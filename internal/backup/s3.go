package backup

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ErrNotFound is returned by GetObject when the key does not exist.
var ErrNotFound = errors.New("backup: object not found")

// StatusError is a non-2xx S3 response. The body is an S3 XML error
// document (no credentials); it is truncated, so it is safe to log.
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("s3 status %d: %s", e.Status, e.Body)
}

// Object is one listed bucket object.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Client is a minimal S3-compatible client (path-style addressing,
// SigV4 signing) over the standard library. It speaks the subset of
// the S3 API the backup needs: PUT/GET/HEAD/LIST/DELETE. Bodies are
// buffered in memory — state snapshots are kilobytes and audit
// segments are gzip NDJSON at night-one scale; GetObject refuses
// bodies over maxObjectBytes rather than growing without bound.
type Client struct {
	endpoint  string
	bucket    string
	region    string
	accessKey string
	secretKey string
	http      *http.Client
}

// maxObjectBytes caps one downloaded object (512 MiB).
const maxObjectBytes = 512 << 20

// NewClient builds a client from a validated config.
func NewClient(cfg Config) *Client {
	return &Client{
		endpoint:  strings.TrimSuffix(cfg.Endpoint, "/"),
		bucket:    cfg.Bucket,
		region:    cfg.Region,
		accessKey: cfg.AccessKey,
		secretKey: cfg.SecretKey,
		http:      &http.Client{Timeout: 5 * time.Minute},
	}
}

// HeadBucket reports whether the bucket exists: a 404 means it
// does not. Other non-2xx statuses are errors (a 403 means the
// credentials may not list this bucket, not that it is missing).
func (c *Client) HeadBucket(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodHead, "", nil, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode/100 != 2 {
		return false, c.statusErr(resp)
	}
	return true, nil
}

// CreateBucket creates the bucket with a bare PUT (no
// LocationConstraint body — the local-only servers this project
// targets, rclone serve s3 / Garage / MinIO, all accept it). It
// reports whether the bucket was created by this call: a 409 means
// it already exists (another process created it in the meantime)
// and is treated as success with created=false.
func (c *Client) CreateBucket(ctx context.Context) (bool, error) {
	req, err := c.newRequest(ctx, http.MethodPut, "", nil, nil)
	if err != nil {
		return false, err
	}
	_, err = c.do(req)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Status == http.StatusConflict {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// EnsureBucket makes sure the bucket exists, creating it when it
// does not. It reports whether this call created it. Uploads
// (RunOnce) call this first: a fresh self-hosted endpoint has no
// bucket yet, and without it every PUT fails with NoSuchBucket —
// the failure the 2026-11-24 drill hit before the bucket was
// created by hand. Read paths (restore/list/verify) deliberately do
// not call it: a missing bucket there must stay a visible error,
// not a silently created empty one.
func (c *Client) EnsureBucket(ctx context.Context) (bool, error) {
	exists, err := c.HeadBucket(ctx)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	return c.CreateBucket(ctx)
}

// PutObject uploads body as key.
func (c *Client) PutObject(ctx context.Context, key string, body []byte, contentType string) error {
	req, err := c.newRequest(ctx, http.MethodPut, key, nil, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = int64(len(body))
	_, err = c.do(req)
	return err
}

// GetObject downloads key.
func (c *Client) GetObject(ctx context.Context, key string) ([]byte, error) {
	req, err := c.newRequest(ctx, http.MethodGet, key, nil, nil)
	if err != nil {
		return nil, err
	}
	body, err := c.do(req)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Status == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return body, nil
}

// HeadObject reports whether key exists and its size.
func (c *Client) HeadObject(ctx context.Context, key string) (size int64, ok bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodHead, key, nil, nil)
	if err != nil {
		return 0, false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return 0, false, nil
	}
	if resp.StatusCode/100 != 2 {
		return 0, false, c.statusErr(resp)
	}
	return resp.ContentLength, true, nil
}

// ListObjects lists keys under prefix (ListObjectsV2, paginated).
func (c *Client) ListObjects(ctx context.Context, prefix string) ([]Object, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out []Object
	var token string
	for {
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("prefix", prefix)
		q.Set("max-keys", "1000")
		if token != "" {
			q.Set("continuation-token", token)
		}
		req, err := c.newRequest(ctx, http.MethodGet, "", q, nil)
		if err != nil {
			return nil, err
		}
		body, err := c.do(req)
		if err != nil {
			return nil, err
		}
		var res listResult
		if err := xml.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("parse list response: %w", err)
		}
		out = append(out, res.Contents...)
		if len(out) > 10000 {
			return nil, errors.New("bucket listing exceeds 10000 objects")
		}
		if !res.IsTruncated {
			return out, nil
		}
		token = res.NextContinuationToken
		if token == "" {
			return nil, errors.New("truncated list response without continuation token")
		}
	}
}

// DeleteObject removes key; a missing key is not an error.
func (c *Client) DeleteObject(ctx context.Context, key string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, key, nil, nil)
	if err != nil {
		return err
	}
	_, err = c.do(req)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Status == http.StatusNotFound {
			return nil
		}
		return err
	}
	return nil
}

type listResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool     `xml:"IsTruncated"`
	Contents              []Object `xml:"Contents"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
}

// newRequest builds a signed request. The URL is path-style:
// <endpoint>/<bucket>/<key>, query from q. body is signed (its
// SHA-256 travels in X-Amz-Content-Sha256); a nil body signs the
// empty payload.
func (c *Client) newRequest(ctx context.Context, method, key string, q url.Values, body io.Reader) (*http.Request, error) {
	raw := c.endpoint + "/" + c.bucket
	if key != "" {
		raw += "/" + escapeKey(key)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if len(q) > 0 {
		u.RawQuery = canonicalQueryString(q)
	}
	var payload []byte
	if body != nil {
		payload, err = io.ReadAll(body)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	payloadHash := sha256Hex(payload)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	signV4(req, payloadHash, c.accessKey, c.secretKey, c.region, "s3", time.Now(), "x-amz-content-sha256")
	return req, nil
}

// do executes req and returns the response body. A non-2xx status
// becomes a *StatusError; a body over maxObjectBytes is refused.
func (c *Client) do(req *http.Request) ([]byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, c.statusErr(resp)
	}
	if resp.ContentLength > maxObjectBytes {
		return nil, fmt.Errorf("object %s exceeds %d bytes", req.URL.Path, maxObjectBytes)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxObjectBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxObjectBytes {
		return nil, fmt.Errorf("object %s exceeds %d bytes", req.URL.Path, maxObjectBytes)
	}
	return body, nil
}

// statusErr builds a *StatusError from a non-2xx response, reading
// at most 512 bytes of the error document.
func (c *Client) statusErr(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	body := strings.Join(strings.Fields(string(raw)), " ")
	return &StatusError{Status: resp.StatusCode, Body: body}
}

// ---- SigV4 ----

// signV4 signs req in place (Authorization header). extraHeaders
// lists header names beyond host and x-amz-date to include in the
// signature (for S3: x-amz-content-sha256; the AWS documentation
// vector test passes none). The payload hash is supplied by the
// caller and must match the X-Amz-Content-Sha256 header when set.
func signV4(req *http.Request, payloadHash, accessKey, secretKey, region, service string, now time.Time, extraHeaders ...string) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)

	signed := []string{"host", "x-amz-date"}
	for _, h := range extraHeaders {
		signed = append(signed, strings.ToLower(h))
	}
	sort.Strings(signed)

	// Header values come from the request itself (host falls back
	// to the URL when req.Host is unset); x-amz-date was set above.
	var headers strings.Builder
	for _, h := range signed {
		v := ""
		if h == "host" {
			v = req.Host
			if v == "" {
				v = req.URL.Host
			}
		} else {
			v = req.Header.Get(http.CanonicalHeaderKey(h))
		}
		headers.WriteString(h + ":" + strings.TrimSpace(v) + "\n")
	}
	signedHeaders := strings.Join(signed, ";")

	canonicalURI := escapePath(req.URL.Path)
	canonicalQuery := canonicalQueryString(req.URL.Query())
	canonicalRequest := strings.Join([]string{
		req.Method, canonicalURI, canonicalQuery, headers.String(), signedHeaders, payloadHash,
	}, "\n")

	scope := strings.Join([]string{date, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+secretKey), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)
}

// escapePath percent-encodes each path segment (RFC 3986), keeping
// "/" separators. An empty path becomes "/".
func escapePath(path string) string {
	if path == "" {
		return "/"
	}
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = strictEscape(p)
	}
	return strings.Join(parts, "/")
}

// escapeKey percent-encodes each key segment, keeping "/" separators.
func escapeKey(key string) string { return escapePath(key) }

// canonicalQueryString renders query values as the canonical query
// string: keys sorted, values sorted per key, RFC 3986 encoding
// (never "+" for space, unlike url.Values.Encode).
func canonicalQueryString(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, strictEscape(k)+"="+strictEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

// strictEscape percent-encodes everything outside the RFC 3986
// unreserved set (A-Z a-z 0-9 - _ . ~), with uppercase hex digits.
func strictEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}
