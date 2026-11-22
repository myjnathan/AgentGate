package distrib_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/distrib"
)

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newUpload(t *testing.T, size int64, expectedSHA string) *distrib.Upload {
	t.Helper()
	return &distrib.Upload{
		Name: "ag-user-win", Kind: distrib.KindUser, Version: "1.0.0",
		Filename: "agentgate-user-1.0.0.zip", ExpectedSize: size,
		ExpectedSHA256: expectedSHA, Source: distrib.SourceUpload,
		AgentType: "dsh", UploadedBy: "admin",
	}
}

func TestValidateUpload(t *testing.T) {
	good := newUpload(t, 10, shaHex([]byte("0123456789")))
	if err := distrib.ValidateUpload(good); err != nil {
		t.Fatalf("valid upload rejected: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*distrib.Upload)
	}{
		{"empty name", func(u *distrib.Upload) { u.Name = "" }},
		{"name with separator", func(u *distrib.Upload) { u.Name = "a/b" }},
		{"bad kind", func(u *distrib.Upload) { u.Kind = "other" }},
		{"bad version", func(u *distrib.Upload) { u.Version = "latest" }},
		{"filename with separator", func(u *distrib.Upload) { u.Filename = "../x" }},
		{"zero size", func(u *distrib.Upload) { u.ExpectedSize = 0 }},
		{"oversize", func(u *distrib.Upload) { u.ExpectedSize = distrib.MaxPackageSize + 1 }},
		{"bad sha", func(u *distrib.Upload) { u.ExpectedSHA256 = "xyz" }},
		{"generated source rejected", func(u *distrib.Upload) { u.Source = distrib.SourceGenerated }},
	}
	for _, tc := range cases {
		up := newUpload(t, 10, shaHex([]byte("0123456789")))
		tc.mut(up)
		if err := distrib.ValidateUpload(up); err == nil {
			t.Errorf("%s: expected rejection", tc.name)
		}
	}
	// Offline import is a legitimate source.
	up := newUpload(t, 10, "")
	up.Source = distrib.SourceOfflineImport
	if err := distrib.ValidateUpload(up); err != nil {
		t.Errorf("offline-import rejected: %v", err)
	}
}

func TestSingleShotUploadVerifiedAndUnverified(t *testing.T) {
	reg := distrib.NewRegistry(t.TempDir())
	data := []byte("fake-installer-bytes-0123456789")

	// With a matching expected checksum the package is verified.
	up := newUpload(t, int64(len(data)), shaHex(data))
	if err := reg.InitUpload(up); err != nil {
		t.Fatal(err)
	}
	if n, err := reg.AppendChunk(up, 0, data); err != nil || n != int64(len(data)) {
		t.Fatalf("append: n=%d err=%v", n, err)
	}
	pkg, err := reg.CompleteUpload(up)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Status != distrib.StatusVerified {
		t.Errorf("status = %s, want verified", pkg.Status)
	}
	if pkg.SHA256 != shaHex(data) || pkg.SizeBytes != int64(len(data)) {
		t.Errorf("package record wrong: %+v", pkg)
	}
	if got, err := reg.ReadFile(pkg); err != nil || !bytes.Equal(got, data) {
		t.Errorf("stored file mismatch: err=%v", err)
	}

	// Without an expected checksum the same bytes stay unverified:
	// hosted, but never eligible as an attestation baseline.
	up2 := newUpload(t, int64(len(data)), "")
	up2.Name = "ag-user-nocheck"
	if err := reg.InitUpload(up2); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.AppendChunk(up2, 0, data); err != nil {
		t.Fatal(err)
	}
	pkg2, err := reg.CompleteUpload(up2)
	if err != nil {
		t.Fatal(err)
	}
	if pkg2.Status != distrib.StatusUnverified {
		t.Errorf("status = %s, want unverified", pkg2.Status)
	}

	// A wrong expected checksum also lands unverified (not an error:
	// the file is what it is, the manifest disagrees with it).
	up3 := newUpload(t, int64(len(data)), shaHex([]byte("something else")))
	up3.Name = "ag-user-badcheck"
	if err := reg.InitUpload(up3); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.AppendChunk(up3, 0, data); err != nil {
		t.Fatal(err)
	}
	pkg3, err := reg.CompleteUpload(up3)
	if err != nil {
		t.Fatal(err)
	}
	if pkg3.Status != distrib.StatusUnverified {
		t.Errorf("mismatched checksum: status = %s, want unverified", pkg3.Status)
	}
}

func TestResumableUpload(t *testing.T) {
	root := t.TempDir()
	reg := distrib.NewRegistry(root)
	data := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	up := newUpload(t, int64(len(data)), shaHex(data))
	if err := reg.InitUpload(up); err != nil {
		t.Fatal(err)
	}
	// First half arrives, then the connection "drops".
	if _, err := reg.AppendChunk(up, 0, data[:10]); err != nil {
		t.Fatal(err)
	}
	// A fresh registry over the same root resumes from the part
	// file on disk: the received count is the truth, not memory.
	reg2 := distrib.NewRegistry(root)
	if n, err := reg2.ReceivedBytes(up); err != nil || n != 10 {
		t.Fatalf("resume: received=%d err=%v", n, err)
	}
	// Wrong offset is rejected (overlap / gap would corrupt).
	if _, err := reg2.AppendChunk(up, 0, data[:5]); err == nil {
		t.Fatal("overlapping chunk accepted")
	}
	if _, err := reg2.AppendChunk(up, 20, data[10:20]); err == nil {
		t.Fatal("gapped chunk accepted")
	}
	// Oversized chunk rejected.
	if _, err := reg2.AppendChunk(up, 10, make([]byte, distrib.MaxChunkSize+1)); err == nil {
		t.Fatal("oversized chunk accepted")
	}
	// Resume from the right offset and finish.
	if _, err := reg2.AppendChunk(up, 10, data[10:]); err != nil {
		t.Fatal(err)
	}
	pkg, err := reg2.CompleteUpload(up)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Status != distrib.StatusVerified {
		t.Errorf("resumed package status = %s, want verified", pkg.Status)
	}
	// Completing twice fails: the part file is gone.
	if _, err := reg2.CompleteUpload(up); err == nil {
		t.Fatal("second complete accepted")
	}
}

func TestIncompleteUploadRejected(t *testing.T) {
	reg := distrib.NewRegistry(t.TempDir())
	data := []byte("0123456789")
	up := newUpload(t, 20, shaHex(data))
	if err := reg.InitUpload(up); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.AppendChunk(up, 0, data); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CompleteUpload(up); err == nil {
		t.Fatal("incomplete upload completed")
	}
}

func TestRemoveUploadAndPackage(t *testing.T) {
	reg := distrib.NewRegistry(t.TempDir())
	data := []byte("0123456789")
	up := newUpload(t, int64(len(data)), shaHex(data))
	if err := reg.InitUpload(up); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.AppendChunk(up, 0, data); err != nil {
		t.Fatal(err)
	}
	if err := reg.RemoveUpload(up); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(reg.PartPath(up)); !os.IsNotExist(err) {
		t.Errorf("part file still present after abort")
	}

	pkg, err := func() (*distrib.Package, error) {
		up2 := newUpload(t, int64(len(data)), shaHex(data))
		if err := reg.InitUpload(up2); err != nil {
			return nil, err
		}
		if _, err := reg.AppendChunk(up2, 0, data); err != nil {
			return nil, err
		}
		return reg.CompleteUpload(up2)
	}()
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.RemovePackage(pkg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(reg.PathFor(pkg))); !os.IsNotExist(err) {
		t.Errorf("package dir still present after delete")
	}
}

func TestExpiredFilter(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	pkgs := []*distrib.Package{
		{ID: "a", ExpiresAt: &past},
		{ID: "b", ExpiresAt: &future},
		{ID: "c"},
	}
	got := distrib.Expired(pkgs, now)
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("expired = %v, want [a]", got)
	}
}

func TestGenerateUserBundle(t *testing.T) {
	cfg := distrib.UserBundleConfig{
		Name: "ag-user-xinghe", Version: "1.2.0", AgentType: "dsh",
		GatewayURL: "https://gw.xinghe.example", MaxDataLevel: "internal",
		EgressAllowlist: []string{"gw.xinghe.example", "models.example.com"},
		GeneratedAt:     time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC),
	}
	guard := []byte("fake-ag-guard-binary")
	setup := []byte("fake-ag-setup-binary")
	zb, sum, err := distrib.GenerateUserBundle(cfg, []distrib.BundleComponent{
		{Role: "ag-guard", Filename: "ag-guard", Data: guard},
		{Role: "ag-setup", Filename: "ag-setup", Data: setup},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sum != shaHex(zb) {
		t.Errorf("returned sha does not match zip bytes")
	}
	zr, err := zip.NewReader(bytes.NewReader(zb), int64(len(zb)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rc); err != nil {
			t.Fatal(err)
		}
		rc.Close()
		names[f.Name] = buf.Bytes()
	}
	if !bytes.Equal(names["ag-guard"], guard) || !bytes.Equal(names["ag-setup"], setup) {
		t.Errorf("bundle binaries mismatch: %v", names)
	}
	var guardCfg map[string]any
	if err := json.Unmarshal(names["guard.json"], &guardCfg); err != nil {
		t.Fatalf("guard.json: %v", err)
	}
	if guardCfg["gateway_url"] != "https://gw.xinghe.example" || guardCfg["max_data_level"] != "internal" {
		t.Errorf("guard.json policy wrong: %v", guardCfg)
	}
	var manifest distrib.BundleManifest
	if err := json.Unmarshal(names["MANIFEST.json"], &manifest); err != nil {
		t.Fatalf("MANIFEST.json: %v", err)
	}
	if manifest.Name != "ag-user-xinghe" || len(manifest.Components) != 2 {
		t.Errorf("manifest wrong: %+v", manifest)
	}
	if manifest.Components[0].SHA256 != shaHex(guard) || manifest.Components[1].SHA256 != shaHex(setup) {
		t.Errorf("manifest component hashes wrong: %+v", manifest.Components)
	}
	if manifest.GuardConfigSHA256 != shaHex(names["guard.json"]) {
		t.Errorf("manifest guard config hash wrong")
	}
}

func TestGenerateUserBundleValidation(t *testing.T) {
	cfg := distrib.UserBundleConfig{
		Name: "ag-user-x", Version: "1.0.0", AgentType: "dsh",
		GatewayURL: "https://gw.example", MaxDataLevel: "internal",
	}
	good := []distrib.BundleComponent{
		{Role: "ag-guard", Filename: "ag-guard", Data: []byte("g")},
		{Role: "ag-setup", Filename: "ag-setup", Data: []byte("s")},
	}
	cases := []struct {
		name string
		mut  func(*distrib.UserBundleConfig, *[]distrib.BundleComponent)
	}{
		{"one component", func(c *distrib.UserBundleConfig, cs *[]distrib.BundleComponent) { *cs = (*cs)[:1] }},
		{"duplicate role", func(c *distrib.UserBundleConfig, cs *[]distrib.BundleComponent) { (*cs)[1].Role = "ag-guard" }},
		{"bad role", func(c *distrib.UserBundleConfig, cs *[]distrib.BundleComponent) { (*cs)[1].Role = "ag-other" }},
		{"http gateway", func(c *distrib.UserBundleConfig, cs *[]distrib.BundleComponent) { c.GatewayURL = "http://gw.example" }},
		{"bad level", func(c *distrib.UserBundleConfig, cs *[]distrib.BundleComponent) { c.MaxDataLevel = "topsecret" }},
		{"bad version", func(c *distrib.UserBundleConfig, cs *[]distrib.BundleComponent) { c.Version = "x" }},
		{"bad egress", func(c *distrib.UserBundleConfig, cs *[]distrib.BundleComponent) { c.EgressAllowlist = []string{"a b"} }},
		{"empty component", func(c *distrib.UserBundleConfig, cs *[]distrib.BundleComponent) { (*cs)[0].Data = nil }},
	}
	for _, tc := range cases {
		c := cfg
		cs := append([]distrib.BundleComponent(nil), good...)
		tc.mut(&c, &cs)
		if _, _, err := distrib.GenerateUserBundle(c, cs); err == nil {
			t.Errorf("%s: expected rejection", tc.name)
		}
	}
}

func TestStoreBytesVerifiesHash(t *testing.T) {
	reg := distrib.NewRegistry(t.TempDir())
	data := []byte("bundle-bytes")
	pkg := &distrib.Package{
		ID: distrib.NewID("pkg"), Name: "ag-user", Kind: distrib.KindUser,
		Version: "1.0.0", Filename: "agentgate-user-1.0.0.zip",
		SizeBytes: int64(len(data)), SHA256: shaHex(data),
		ExpectedSHA256: shaHex(data), Source: distrib.SourceGenerated,
		CreatedAt: time.Now(),
	}
	if err := reg.StoreBytes(pkg, data); err != nil {
		t.Fatal(err)
	}
	if pkg.Status != distrib.StatusVerified {
		t.Errorf("stored generated package status = %s, want verified", pkg.Status)
	}
	bad := *pkg
	bad.SHA256 = shaHex([]byte("other"))
	if err := reg.StoreBytes(&bad, data); err == nil {
		t.Fatal("hash mismatch accepted")
	}
}
