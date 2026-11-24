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
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string]fakeObject
	authOK  int
}

type fakeObject struct {
	body     []byte
	modified time.Time
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string]fakeObject{}} }

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
	key := strings.Join(parts[1:], "/")
	q := r.URL.Query()
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
