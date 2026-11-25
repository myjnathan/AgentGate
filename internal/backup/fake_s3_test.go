package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is an in-memory S3-compatible server for tests: it stores
// objects, answers ListObjectsV2 with real S3 XML (including the
// xmlns namespace), pages at two objects per response to exercise
// continuation tokens, and checks that requests carry a well-formed
// SigV4 Authorization header and a payload hash matching the body.
// It does not re-derive the signature (the AWS vector test pins the
// signing math, and the drill checks against a real server).
// Buckets are modelled like a real server: a bucket must exist
// before any object operation in it succeeds (otherwise
// NoSuchBucket), and creating one is a PUT on the bucket itself.
// This strictness is deliberate — the pre-EnsureBucket fake
// accepted objects in any bucket, which is why the tests passed
// while the 2026-11-24 drill failed on a fresh endpoint.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string]fakeObject
	buckets map[string]bool
	// createConflict makes the next bucket creation answer 409
	// (another process created it first) while still recording
	// the bucket as existing.
	createConflict bool
	authOK         int
}

type fakeObject struct {
	body     []byte
	modified time.Time
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string]fakeObject{}, buckets: map[string]bool{}}
}

func (f *fakeS3) setOld(key string, t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o, ok := f.objects[key]; ok {
		o.modified = t
		f.objects[key] = o
	}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// SigV4 structural checks.
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=") ||
		!strings.Contains(auth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date") ||
		!strings.Contains(auth, "Signature=") {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	if sig := auth[strings.LastIndex(auth, "Signature=")+len("Signature="):]; len(sig) != 64 {
		http.Error(w, "bad signature length", http.StatusForbidden)
		return
	}
	if r.Header.Get("X-Amz-Date") == "" || r.Header.Get("X-Amz-Content-Sha256") == "" {
		http.Error(w, "missing amz headers", http.StatusForbidden)
		return
	}
	body, _ := io.ReadAll(r.Body)
	h := sha256.Sum256(body)
	if hex.EncodeToString(h[:]) != r.Header.Get("X-Amz-Content-Sha256") {
		http.Error(w, "payload hash mismatch", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authOK++
	// Path: /<bucket>/<key...>; list has no key and list-type=2.
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		http.Error(w, "missing bucket", http.StatusBadRequest)
		return
	}
	bucket := parts[0]
	key := strings.Join(parts[1:], "/")
	q := r.URL.Query()
	// Bucket-level operations (empty key): HEAD checks existence,
	// PUT creates, GET with list-type=2 lists.
	if key == "" {
		switch r.Method {
		case http.MethodHead:
			if f.buckets[bucket] {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
			return
		case http.MethodPut:
			if f.createConflict {
				f.buckets[bucket] = true
				http.Error(w, "<Error><Code>BucketAlreadyExists</Code></Error>", http.StatusConflict)
				return
			}
			f.buckets[bucket] = true
			w.WriteHeader(http.StatusOK)
			return
		case http.MethodGet:
			if q.Get("list-type") == "2" {
				if !f.buckets[bucket] {
					http.Error(w, "<Error><Code>NoSuchBucket</Code></Error>", http.StatusNotFound)
					return
				}
				f.list(w, r, q.Get("prefix"), q.Get("continuation-token"))
				return
			}
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	}
	// Object operations in a missing bucket fail like a real
	// server: NoSuchBucket, not NoSuchKey.
	if !f.buckets[bucket] {
		http.Error(w, "<Error><Code>NoSuchBucket</Code></Error>", http.StatusNotFound)
		return
	}
	if r.Method == http.MethodGet && q.Get("list-type") == "2" {
		f.list(w, r, q.Get("prefix"), q.Get("continuation-token"))
		return
	}
	switch r.Method {
	case http.MethodPut:
		f.objects[key] = fakeObject{body: append([]byte(nil), body...), modified: time.Now()}
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		o, ok := f.objects[key]
		if !ok {
			http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(o.body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(o.body)
	case http.MethodHead:
		o, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(o.body)))
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request, prefix, token string) {
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	start := 0
	if token != "" {
		start, _ = strconv.Atoi(token)
	}
	// Page at two objects: a server may return fewer than max-keys.
	end := start + 2
	truncated := false
	if end < len(keys) {
		truncated = true
	} else {
		end = len(keys)
	}
	type contents struct {
		Key          string    `xml:"Key"`
		LastModified time.Time `xml:"LastModified"`
		Size         int64     `xml:"Size"`
	}
	res := struct {
		XMLName               xml.Name   `xml:"ListBucketResult"`
		Xmlns                 string     `xml:"xmlns,attr"`
		IsTruncated           bool       `xml:"IsTruncated"`
		Contents              []contents `xml:"Contents"`
		NextContinuationToken string     `xml:"NextContinuationToken,omitempty"`
	}{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", IsTruncated: truncated}
	for _, k := range keys[start:end] {
		o := f.objects[k]
		res.Contents = append(res.Contents, contents{Key: k, LastModified: o.modified, Size: int64(len(o.body))})
	}
	if truncated {
		res.NextContinuationToken = strconv.Itoa(end)
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(res)
}

func TestClientAgainstFakeS3(t *testing.T) {
	srv := newFakeS3()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	cfg := Config{Endpoint: ts.URL, Bucket: "bkt", Region: "us-east-1",
		AccessKey: "ak", SecretKey: "sk"}.WithDefaults()
	c := NewClient(cfg)
	ctx := t.Context()
	// The bucket starts missing on a fresh fake: EnsureBucket must
	// create it before any object operation succeeds.
	created, err := c.EnsureBucket(ctx)
	if err != nil || !created {
		t.Fatalf("EnsureBucket = %v %v, want created=true", created, err)
	}
	if err := c.PutObject(ctx, "state/state-2026-11-24T00-00-00Z.json", []byte(`{"users":{}}`), "application/json"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := c.PutObject(ctx, "audit/audit-archive-"+strconv.Itoa(i)+".jsonl.gz", []byte("gz"), "application/gzip"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.GetObject(ctx, "state/state-2026-11-24T00-00-00Z.json")
	if err != nil || string(got) != `{"users":{}}` {
		t.Fatalf("get = %q, err %v", got, err)
	}
	if _, err := c.GetObject(ctx, "state/nope.json"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing get must be ErrNotFound, got %v", err)
	}
	size, ok, err := c.HeadObject(ctx, "audit/audit-archive-0.jsonl.gz")
	if err != nil || !ok || size != 2 {
		t.Fatalf("head = %d %v %v", size, ok, err)
	}
	if _, ok, err := c.HeadObject(ctx, "audit/nope"); err != nil || ok {
		t.Fatalf("missing head = %v %v", ok, err)
	}
	// Pagination: 5 audit objects at 2 per page.
	objs, err := c.ListObjects(ctx, "audit/")
	if err != nil || len(objs) != 5 {
		t.Fatalf("list = %d, err %v", len(objs), err)
	}
	if objs[0].Key != "audit/audit-archive-0.jsonl.gz" || objs[4].Key != "audit/audit-archive-4.jsonl.gz" {
		t.Fatalf("list order wrong: %v", objs)
	}
	if err := c.DeleteObject(ctx, "audit/audit-archive-4.jsonl.gz"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.HeadObject(ctx, "audit/audit-archive-4.jsonl.gz"); err != nil || ok {
		t.Fatalf("deleted head = %v %v", ok, err)
	}
	if err := c.DeleteObject(ctx, "audit/nope"); err != nil {
		t.Fatalf("delete missing must succeed, got %v", err)
	}
	if srv.authOK == 0 {
		t.Fatal("fake server saw no authenticated request")
	}
}

func TestClientAuthFailure(t *testing.T) {
	// A server that rejects every signature must surface a 403
	// class, not a generic error.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "<Error><Code>SignatureDoesNotMatch</Code></Error>", http.StatusForbidden)
	}))
	defer ts.Close()
	cfg := Config{Endpoint: ts.URL, Bucket: "bkt", Region: "us-east-1",
		AccessKey: "ak", SecretKey: "wrong"}.WithDefaults()
	c := NewClient(cfg)
	err := c.PutObject(t.Context(), "state/x.json", []byte("{}"), "application/json")
	if err == nil {
		t.Fatal("expected auth error")
	}
	if ErrorClass(err) != "s3_auth" {
		t.Fatalf("error class = %q, want s3_auth (err %v)", ErrorClass(err), err)
	}
}

func TestEnsureBucketCreatesMissingBucket(t *testing.T) {
	srv := newFakeS3()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	cfg := Config{Endpoint: ts.URL, Bucket: "bkt", Region: "us-east-1",
		AccessKey: "ak", SecretKey: "sk"}.WithDefaults()
	c := NewClient(cfg)
	ctx := t.Context()

	// Missing bucket: first ensure creates it.
	created, err := c.EnsureBucket(ctx)
	if err != nil || !created {
		t.Fatalf("EnsureBucket = %v %v, want created=true", created, err)
	}
	// Objects now work, and a second ensure is a no-op.
	if err := c.PutObject(ctx, "state/x.json", []byte("{}"), "application/json"); err != nil {
		t.Fatal(err)
	}
	created, err = c.EnsureBucket(ctx)
	if err != nil || created {
		t.Fatalf("second EnsureBucket = %v %v, want created=false", created, err)
	}
	if exists, err := c.HeadBucket(ctx); err != nil || !exists {
		t.Fatalf("HeadBucket = %v %v, want true", exists, err)
	}
}

func TestEnsureBucketRaceConflict(t *testing.T) {
	// Another process creates the bucket between our HEAD and PUT:
	// the server answers 409, which EnsureBucket treats as success.
	srv := newFakeS3()
	srv.createConflict = true
	ts := httptest.NewServer(srv)
	defer ts.Close()
	cfg := Config{Endpoint: ts.URL, Bucket: "bkt", Region: "us-east-1",
		AccessKey: "ak", SecretKey: "sk"}.WithDefaults()
	c := NewClient(cfg)
	created, err := c.EnsureBucket(t.Context())
	if err != nil || created {
		t.Fatalf("EnsureBucket on 409 = %v %v, want created=false, nil", created, err)
	}
	if exists, err := c.HeadBucket(t.Context()); err != nil || !exists {
		t.Fatalf("HeadBucket after 409 = %v %v, want true", exists, err)
	}
}

func TestPutObjectMissingBucketFails(t *testing.T) {
	// The 2026-11-24 drill failure, pinned: without EnsureBucket,
	// an upload to a fresh endpoint fails with NoSuchBucket (404),
	// classed s3_not_found — never a silent success.
	srv := newFakeS3()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	cfg := Config{Endpoint: ts.URL, Bucket: "bkt", Region: "us-east-1",
		AccessKey: "ak", SecretKey: "sk"}.WithDefaults()
	c := NewClient(cfg)
	err := c.PutObject(t.Context(), "state/x.json", []byte("{}"), "application/json")
	if err == nil {
		t.Fatal("PutObject into a missing bucket must fail")
	}
	if ErrorClass(err) != "s3_not_found" {
		t.Fatalf("error class = %q, want s3_not_found (err %v)", ErrorClass(err), err)
	}
	if exists, err := c.HeadBucket(t.Context()); err != nil || exists {
		t.Fatalf("HeadBucket = %v %v, want false", exists, err)
	}
}
