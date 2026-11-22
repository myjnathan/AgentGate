package api_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/store"
)

func shaHexStr(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func pkgCall(t *testing.T, srvURL, method, path, sess string, body io.Reader, ctype string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srvURL+path, body)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if sess != "" {
		req.Header.Set("Authorization", "Bearer "+sess)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func jsonCall(t *testing.T, srvURL, method, path, sess string, body any) (int, map[string]any) {
	t.Helper()
	var buf *bytes.Buffer
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewBuffer(b)
	} else {
		buf = bytes.NewBuffer(nil)
	}
	return pkgCall(t, srvURL, method, path, sess, buf, "application/json")
}

// multipartUpload builds a single-shot package upload request body.
func multipartUpload(t *testing.T, fields map[string]string, filename string, data []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func uploadVerifiedPackage(t *testing.T, srvURL, adminSess, name, agentType string, data []byte) map[string]any {
	t.Helper()
	body, ctype := multipartUpload(t, map[string]string{
		"name": name, "kind": "user", "version": "1.0.0",
		"expected_sha256": shaHexStr(data), "source": "upload", "agent_type": agentType,
	}, name+"-1.0.0.bin", data)
	code, out := pkgCall(t, srvURL, "POST", "/api/packages", adminSess, body, ctype)
	if code != 201 {
		t.Fatalf("upload %s: got %d (%v), want 201", name, code, out)
	}
	pkg, _ := out["package"].(map[string]any)
	if pkg == nil {
		t.Fatalf("upload %s: no package in response: %v", name, out)
	}
	return pkg
}

func TestPackageUploadLifecycle(t *testing.T) {
	srv, _, adminSess, empSess := setupTokensAPI(t)

	code, listed := jsonCall(t, srv.URL, "GET", "/api/packages", adminSess, nil)
	if code != 200 {
		t.Fatalf("list packages: got %d, want 200", code)
	}
	if pkgs, _ := listed["packages"].([]any); len(pkgs) != 0 {
		t.Fatalf("packages = %v, want empty", listed)
	}
	if code, _ := jsonCall(t, srv.URL, "GET", "/api/packages", empSess, nil); code != 403 {
		t.Fatalf("employee list packages: got %d, want 403", code)
	}

	guardData := []byte("fake-ag-guard-binary-v1")
	pkg := uploadVerifiedPackage(t, srv.URL, adminSess, "ag-guard-xinghe", "dsh", guardData)
	if pkg["status"] != "verified" {
		t.Errorf("status = %v, want verified", pkg["status"])
	}
	if pkg["sha256"] != shaHexStr(guardData) {
		t.Errorf("sha256 = %v, want %s", pkg["sha256"], shaHexStr(guardData))
	}
	pkgID, _ := pkg["id"].(string)

	// No expected checksum: hosted, but unverified.
	body, ctype := multipartUpload(t, map[string]string{
		"name": "ag-nocheck", "kind": "user", "version": "1.0.0",
	}, "nocheck.bin", []byte("bytes"))
	code, out := pkgCall(t, srv.URL, "POST", "/api/packages", adminSess, body, ctype)
	if code != 201 {
		t.Fatalf("unverified upload: got %d (%v), want 201", code, out)
	}
	unverified, _ := out["package"].(map[string]any)
	if unverified["status"] != "unverified" {
		t.Errorf("no-checksum status = %v, want unverified", unverified["status"])
	}
	unverifiedID, _ := unverified["id"].(string)

	// Validation: bad version, bad kind.
	body, ctype = multipartUpload(t, map[string]string{
		"name": "bad", "kind": "user", "version": "latest",
	}, "bad.bin", []byte("x"))
	if code, _ := pkgCall(t, srv.URL, "POST", "/api/packages", adminSess, body, ctype); code != 400 {
		t.Fatalf("bad version: got %d, want 400", code)
	}

	// Employee self-service download: body + checksum header match.
	req, _ := http.NewRequest("GET", srv.URL+"/api/packages/"+pkgID+"/download", nil)
	req.Header.Set("Authorization", "Bearer "+empSess)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(got, guardData) {
		t.Fatalf("employee download: status %d, body %q", resp.StatusCode, got)
	}
	if resp.Header.Get("X-Package-SHA256") != shaHexStr(guardData) {
		t.Errorf("download sha header = %q", resp.Header.Get("X-Package-SHA256"))
	}
	if resp.Header.Get("X-Package-Status") != "verified" {
		t.Errorf("download status header = %q", resp.Header.Get("X-Package-Status"))
	}
	// Anonymous download is refused.
	req, _ = http.NewRequest("GET", srv.URL+"/api/packages/"+pkgID+"/download", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous download: got %d, want 401", resp.StatusCode)
	}

	// Baseline: unverified package is refused; verified registers.
	if code, _ := jsonCall(t, srv.URL, "POST", "/api/packages/"+unverifiedID+"/baseline", adminSess,
		map[string]any{"agent_type": "dsh"}); code != 400 {
		t.Fatalf("unverified baseline: got %d, want 400", code)
	}
	code, out = jsonCall(t, srv.URL, "POST", "/api/packages/"+pkgID+"/baseline", adminSess,
		map[string]any{"agent_type": "dsh"})
	if code != 200 {
		t.Fatalf("register baseline: got %d (%v), want 200", code, out)
	}
	if bp, _ := out["package"].(map[string]any); bp["baseline_registered"] != true {
		t.Errorf("baseline_registered = %v, want true", bp["baseline_registered"])
	}
	// The dsh attestation policy now carries the package hash, and a
	// second registration does not duplicate it.
	code, pols := jsonCall(t, srv.URL, "GET", "/api/attestation/policies", adminSess, nil)
	if code != 200 {
		t.Fatalf("list policies: got %d", code)
	}
	found := false
	for _, p := range pols["policies"].([]any) {
		pm, _ := p.(map[string]any)
		if pm["agent_type"] == "dsh" {
			hashes, _ := pm["allowed_binary_hashes"].([]any)
			if len(hashes) == 1 && hashes[0] == shaHexStr(guardData) {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("dsh policy lacks the baseline hash: %v", pols)
	}
	if code, _ := jsonCall(t, srv.URL, "POST", "/api/packages/"+pkgID+"/baseline", adminSess,
		map[string]any{"agent_type": "dsh"}); code != 200 {
		t.Fatalf("re-register baseline: got %d, want 200", code)
	}
	code, pols = jsonCall(t, srv.URL, "GET", "/api/attestation/policies", adminSess, nil)
	for _, p := range pols["policies"].([]any) {
		pm, _ := p.(map[string]any)
		if pm["agent_type"] == "dsh" {
			if hashes, _ := pm["allowed_binary_hashes"].([]any); len(hashes) != 1 {
				t.Errorf("baseline duplicated: %v", hashes)
			}
		}
	}

	// Delete removes the record and the file.
	if code, _ := jsonCall(t, srv.URL, "DELETE", "/api/packages/"+pkgID, adminSess, nil); code != 200 {
		t.Fatalf("delete package: got %d, want 200", code)
	}
	req, _ = http.NewRequest("GET", srv.URL+"/api/packages/"+pkgID+"/download", nil)
	req.Header.Set("Authorization", "Bearer "+empSess)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("download after delete: got %d, want 404", resp.StatusCode)
	}
	// Employee may not delete.
	if code, _ := jsonCall(t, srv.URL, "DELETE", "/api/packages/"+unverifiedID, empSess, nil); code != 403 {
		t.Fatalf("employee delete: got %d, want 403", code)
	}
}

func TestResumableUploadFlow(t *testing.T) {
	srv, _, adminSess, _ := setupTokensAPI(t)
	data := []byte("0123456789abcdefghijklmnopqrstuvwxyz")

	code, up := jsonCall(t, srv.URL, "POST", "/api/packages/uploads", adminSess, map[string]any{
		"name": "ag-resume", "kind": "user", "version": "2.0.0", "filename": "resume.bin",
		"expected_size": len(data), "expected_sha256": shaHexStr(data),
		"source": "offline-import", "agent_type": "codex",
	})
	if code != 201 {
		t.Fatalf("init upload: got %d (%v), want 201", code, up)
	}
	uploadID, _ := up["upload_id"].(string)

	code, st := jsonCall(t, srv.URL, "GET", "/api/packages/uploads/"+uploadID+"/status", adminSess, nil)
	if code != 200 || st["received_bytes"].(float64) != 0 {
		t.Fatalf("upload status: got %d %v, want 200/0", code, st)
	}

	chunk := func(offset int, b []byte) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("PUT", srv.URL+"/api/packages/uploads/"+uploadID+"?offset="+strconv.Itoa(offset), bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+adminSess)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := chunk(0, data[:10]); code != 200 {
		t.Fatalf("first chunk: got %d, want 200", code)
	}
	// Wrong offset: the server refuses to corrupt the file.
	if code, _ := chunk(0, data[:5]); code != 400 {
		t.Fatalf("overlapping chunk: got %d, want 400", code)
	}
	if code, _ := chunk(20, data[10:20]); code != 400 {
		t.Fatalf("gapped chunk: got %d, want 400", code)
	}
	// Completing early is refused.
	if code, _ := jsonCall(t, srv.URL, "POST", "/api/packages/uploads/"+uploadID+"/complete", adminSess, nil); code != 400 {
		t.Fatalf("early complete: got %d, want 400", code)
	}
	if code, st := chunk(10, data[10:]); code != 200 || st["received_bytes"].(float64) != float64(len(data)) {
		t.Fatalf("second chunk: got %d %v", code, st)
	}
	code, out := jsonCall(t, srv.URL, "POST", "/api/packages/uploads/"+uploadID+"/complete", adminSess, nil)
	if code != 201 {
		t.Fatalf("complete upload: got %d (%v), want 201", code, out)
	}
	pkg, _ := out["package"].(map[string]any)
	if pkg["status"] != "verified" || pkg["source"] != "offline-import" {
		t.Errorf("completed package wrong: %v", pkg)
	}
	// Status of a finished upload is gone.
	if code, _ := jsonCall(t, srv.URL, "GET", "/api/packages/uploads/"+uploadID+"/status", adminSess, nil); code != 404 {
		t.Fatalf("finished upload status: got %d, want 404", code)
	}

	// Abort removes the session and its partial bytes.
	code, up2 := jsonCall(t, srv.URL, "POST", "/api/packages/uploads", adminSess, map[string]any{
		"name": "ag-abort", "kind": "user", "version": "1.0.0", "filename": "abort.bin",
		"expected_size": 10, "source": "upload",
	})
	if code != 201 {
		t.Fatalf("init abort upload: got %d", code)
	}
	abortID, _ := up2["upload_id"].(string)
	if code, _ := chunkAbort(t, srv.URL, adminSess, abortID); code != 200 {
		t.Fatalf("abort upload: got %d, want 200", code)
	}
	if code, _ := jsonCall(t, srv.URL, "GET", "/api/packages/uploads/"+abortID+"/status", adminSess, nil); code != 404 {
		t.Fatalf("aborted upload status: got %d, want 404", code)
	}
}

func chunkAbort(t *testing.T, srvURL, sess, id string) (int, map[string]any) {
	t.Helper()
	return jsonCall(t, srvURL, "DELETE", "/api/packages/uploads/"+id, sess, nil)
}

func TestGenerateUserBundle(t *testing.T) {
	srv, _, adminSess, empSess := setupTokensAPI(t)

	guard := uploadVerifiedPackage(t, srv.URL, adminSess, "comp-guard", "codex", []byte("guard-binary"))
	setup := uploadVerifiedPackage(t, srv.URL, adminSess, "comp-setup", "codex", []byte("setup-binary"))

	code, out := jsonCall(t, srv.URL, "POST", "/api/packages/user-bundle", adminSess, map[string]any{
		"name": "ag-user-xinghe", "version": "1.0.0", "agent_type": "codex",
		"gateway_url": "https://gw.xinghe.example", "max_data_level": "internal",
		"egress_allowlist": []string{"gw.xinghe.example"},
		"components": []map[string]any{
			{"package_id": guard["id"], "role": "ag-guard"},
			{"package_id": setup["id"], "role": "ag-setup"},
		},
		"register_baseline": true,
	})
	if code != 201 {
		t.Fatalf("generate bundle: got %d (%v), want 201", code, out)
	}
	bundle, _ := out["package"].(map[string]any)
	if bundle["kind"] != "user" || bundle["source"] != "generated" || bundle["status"] != "verified" {
		t.Errorf("bundle record wrong: %v", bundle)
	}
	if bundle["baseline_registered"] != true {
		t.Errorf("bundle baseline_registered = %v, want true", bundle["baseline_registered"])
	}
	bundleID, _ := bundle["id"].(string)
	bundleSHA, _ := bundle["sha256"].(string)

	// Download the zip and inspect it.
	req, _ := http.NewRequest("GET", srv.URL+"/api/packages/"+bundleID+"/download", nil)
	req.Header.Set("Authorization", "Bearer "+empSess)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	zb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("download bundle: got %d", resp.StatusCode)
	}
	if shaHexStr(zb) != bundleSHA {
		t.Errorf("downloaded zip sha mismatch")
	}
	zr, err := zip.NewReader(bytes.NewReader(zb), int64(len(zb)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string][]byte{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		names[f.Name] = b
	}
	if !bytes.Equal(names["ag-guard"], []byte("guard-binary")) || !bytes.Equal(names["ag-setup"], []byte("setup-binary")) {
		t.Errorf("bundle binaries wrong: %v", names)
	}
	var guardCfg map[string]any
	if err := json.Unmarshal(names["guard.json"], &guardCfg); err != nil {
		t.Fatalf("guard.json: %v", err)
	}
	if guardCfg["gateway_url"] != "https://gw.xinghe.example" || guardCfg["max_data_level"] != "internal" {
		t.Errorf("guard.json wrong: %v", guardCfg)
	}
	var manifest map[string]any
	if err := json.Unmarshal(names["MANIFEST.json"], &manifest); err != nil {
		t.Fatalf("MANIFEST.json: %v", err)
	}
	if manifest["name"] != "ag-user-xinghe" {
		t.Errorf("manifest wrong: %v", manifest)
	}
	// The bundle hash is now the codex attestation baseline.
	code, pols := jsonCall(t, srv.URL, "GET", "/api/attestation/policies", adminSess, nil)
	found := false
	for _, p := range pols["policies"].([]any) {
		pm, _ := p.(map[string]any)
		if pm["agent_type"] == "codex" {
			for _, h := range pm["allowed_binary_hashes"].([]any) {
				if h == bundleSHA {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("codex policy lacks bundle baseline hash: %v", pols)
	}

	// Validation: one component, unknown component, http gateway,
	// employee caller.
	if code, _ := jsonCall(t, srv.URL, "POST", "/api/packages/user-bundle", adminSess, map[string]any{
		"name": "bad", "version": "1.0.0", "agent_type": "codex",
		"gateway_url": "https://gw.example", "components": []map[string]any{
			{"package_id": guard["id"], "role": "ag-guard"},
		},
	}); code != 400 {
		t.Fatalf("one component: got %d, want 400", code)
	}
	if code, _ := jsonCall(t, srv.URL, "POST", "/api/packages/user-bundle", adminSess, map[string]any{
		"name": "bad", "version": "1.0.0", "agent_type": "codex",
		"gateway_url": "http://gw.example", "components": []map[string]any{
			{"package_id": guard["id"], "role": "ag-guard"},
			{"package_id": setup["id"], "role": "ag-setup"},
		},
	}); code != 400 {
		t.Fatalf("http gateway: got %d, want 400", code)
	}
	if code, _ := jsonCall(t, srv.URL, "POST", "/api/packages/user-bundle", empSess, map[string]any{
		"name": "bad", "version": "1.0.0", "agent_type": "codex",
		"gateway_url": "https://gw.example", "components": []map[string]any{
			{"package_id": guard["id"], "role": "ag-guard"},
			{"package_id": setup["id"], "role": "ag-setup"},
		},
	}); code != 403 {
		t.Fatalf("employee generate: got %d, want 403", code)
	}
}

func TestPackageCleanup(t *testing.T) {
	srv, st, adminSess, _ := setupTokensAPI(t)

	pkg := uploadVerifiedPackage(t, srv.URL, adminSess, "ag-old", "dsh", []byte("old-bytes"))
	pkgID, _ := pkg["id"].(string)
	// Backdate the expiry in the store (the API refuses a past
	// expiry at upload time; cleanup is what retires it).
	past := time.Now().Add(-time.Hour)
	st.WithLock(func(s *store.State) {
		if p, ok := s.Packages[pkgID]; ok {
			p.ExpiresAt = &past
		}
	})
	_ = st.Save()

	code, out := jsonCall(t, srv.URL, "POST", "/api/packages/cleanup", adminSess, nil)
	if code != 200 {
		t.Fatalf("cleanup: got %d (%v), want 200", code, out)
	}
	if out["count"].(float64) != 1 {
		t.Errorf("cleanup count = %v, want 1", out["count"])
	}
	code, listed := jsonCall(t, srv.URL, "GET", "/api/packages", adminSess, nil)
	for _, p := range listed["packages"].([]any) {
		if pm, _ := p.(map[string]any); pm["id"] == pkgID {
			t.Errorf("expired package still listed: %v", listed)
		}
	}
	// Second cleanup is a no-op.
	code, out = jsonCall(t, srv.URL, "POST", "/api/packages/cleanup", adminSess, nil)
	if code != 200 || out["count"].(float64) != 0 {
		t.Errorf("second cleanup: got %d %v, want 200/0", code, out)
	}
	// Upload with a past expiry is refused outright.
	body, ctype := multipartUpload(t, map[string]string{
		"name": "ag-past", "kind": "user", "version": "1.0.0",
		"expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339),
	}, "past.bin", []byte("x"))
	if code, _ := pkgCall(t, srv.URL, "POST", "/api/packages", adminSess, body, ctype); code != 400 {
		t.Fatalf("past expiry upload: got %d, want 400", code)
	}
}
