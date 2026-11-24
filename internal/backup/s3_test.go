package backup

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// TestSignV4AWSExample pins the signer to the AWS-documented SigV4
// example ("Examples of the complete Version 4 signing process",
// IAM ListUsers): canonical request hash, derived signing key and
// signature are published values, so this fails if the signing math
// drifts. Two details the example hinges on: the request carries a
// Content-Type header (it is signed), and the example secret key
// uses "+" ("...K7MDENG+bPxRfiCYEXAMPLEKEY"), not "/". Note the
// signature's tail: some mirrors transcribe it as ...b5b7, but
// ...b5d7 is what follows deterministically from the published
// canonical hash (f536975d...) and signing key (c4afb1cc...), and
// two independent implementations agree on it.
func TestSignV4AWSExample(t *testing.T) {
	req, err := http.NewRequest("GET", "https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	now := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	emptyHash := sha256Hex(nil)
	if emptyHash != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("empty payload hash = %s", emptyHash)
	}
	signV4(req, emptyHash, "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "us-east-1", "iam", now, "content-type")
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-date, Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization = %q\nwant %q", got, want)
	}
	if got := req.Header.Get("X-Amz-Date"); got != "20150830T123600Z" {
		t.Fatalf("X-Amz-Date = %q", got)
	}
}

func TestStrictEscape(t *testing.T) {
	if got := strictEscape("a b/c~d-e_f.g"); got != "a%20b%2Fc~d-e_f.g" {
		t.Fatalf("strictEscape = %q", got)
	}
	if got := strictEscape("audit-archive-1-2.jsonl.gz"); got != "audit-archive-1-2.jsonl.gz" {
		t.Fatalf("strictEscape must leave safe chars alone: %q", got)
	}
}

func TestConfigValidate(t *testing.T) {
	good := Config{Endpoint: "http://127.0.0.1:9000", Bucket: "agentgate-backup",
		Region: "us-east-1", AccessKey: "ak", SecretKey: "sk"}.WithDefaults()
	if err := good.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for _, bad := range []Config{
		{Endpoint: "ftp://x", Bucket: "bkt", AccessKey: "a", SecretKey: "s"},
		{Endpoint: "http://x:9000/prefix", Bucket: "bkt", AccessKey: "a", SecretKey: "s"},
		{Endpoint: "http://user:pass@x:9000", Bucket: "bkt", AccessKey: "a", SecretKey: "s"},
		{Endpoint: "http://x:9000", Bucket: "AB", AccessKey: "a", SecretKey: "s"},
		{Endpoint: "http://x:9000", Bucket: "bkt", AccessKey: "", SecretKey: "s"},
		{Endpoint: "http://x:9000", Bucket: "bkt", AccessKey: "a", SecretKey: ""},
	} {
		if err := bad.WithDefaults().Validate(); err == nil {
			t.Fatalf("invalid config accepted: %+v", bad)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	getenv := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	if _, err := ConfigFromEnv(getenv(nil)); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("empty env must be ErrNotConfigured, got %v", err)
	}
	cfg, err := ConfigFromEnv(getenv(map[string]string{
		"AGENTGATE_BACKUP_S3_ENDPOINT":   "http://127.0.0.1:9000",
		"AGENTGATE_BACKUP_S3_BUCKET":     "agentgate-backup",
		"AGENTGATE_BACKUP_S3_ACCESS_KEY": "ak",
		"AGENTGATE_BACKUP_S3_SECRET_KEY": "sk",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Region != DefaultRegion || cfg.StateInterval != DefaultStateInterval ||
		cfg.StateRetention != DefaultStateRetention || cfg.AuditRetention != DefaultAuditRetention || cfg.Source != "env" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if _, err := ConfigFromEnv(getenv(map[string]string{
		"AGENTGATE_BACKUP_S3_ENDPOINT":          "http://127.0.0.1:9000",
		"AGENTGATE_BACKUP_S3_BUCKET":            "agentgate-backup",
		"AGENTGATE_BACKUP_S3_ACCESS_KEY":        "ak",
		"AGENTGATE_BACKUP_S3_SECRET_KEY":        "sk",
		"AGENTGATE_BACKUP_STATE_INTERVAL_HOURS": "0",
	})); err == nil {
		t.Fatal("zero interval must be rejected")
	}
}
