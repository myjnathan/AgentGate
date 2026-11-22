package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/attest"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/distrib"
	"github.com/myjnathan/agentgate/internal/store"
)

// SetPackageRegistry wires the package distribution registry
// (backlog F20). A nil registry disables the endpoints (503).
func (a *API) SetPackageRegistry(reg *distrib.Registry) {
	a.pkgs = reg
}

// packageView is the admin/employee-facing package shape. The full
// SHA-256 is the package's public checksum (employees verify their
// download against it); it is never written to the audit log, which
// records only names, kinds and decisions.
type packageView struct {
	ID                 string         `json:"id"`
	Name               string         `json:"name"`
	Kind               distrib.Kind   `json:"kind"`
	Version            string         `json:"version"`
	Filename           string         `json:"filename"`
	SizeBytes          int64          `json:"size_bytes"`
	SHA256             string         `json:"sha256"`
	Status             distrib.Status `json:"status"`
	Source             distrib.Source `json:"source"`
	AgentType          string         `json:"agent_type,omitempty"`
	BaselineRegistered bool           `json:"baseline_registered"`
	ExpiresAt          *time.Time     `json:"expires_at,omitempty"`
	CreatedAt          time.Time      `json:"created_at"`
	DownloadURL        string         `json:"download_url"`
}

func toPackageView(p *distrib.Package) packageView {
	return packageView{
		ID: p.ID, Name: p.Name, Kind: p.Kind, Version: p.Version,
		Filename: p.Filename, SizeBytes: p.SizeBytes, SHA256: p.SHA256,
		Status: p.Status, Source: p.Source, AgentType: p.AgentType,
		BaselineRegistered: p.BaselineRegistered, ExpiresAt: p.ExpiresAt,
		CreatedAt: p.CreatedAt, DownloadURL: "/api/packages/" + p.ID + "/download",
	}
}

func (a *API) requirePackages(w http.ResponseWriter) bool {
	if a.pkgs == nil {
		writeErr(w, http.StatusServiceUnavailable, "package distribution not available")
		return false
	}
	return true
}

func (a *API) listPackages(w http.ResponseWriter, r *http.Request, u *store.User) {
	if !a.requirePackages(w) {
		return
	}
	out := []packageView{}
	a.st.Read(func(st *store.State) {
		for _, p := range st.Packages {
			out = append(out, toPackageView(p))
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"packages": out})
}

type uploadInit struct {
	Name           string     `json:"name"`
	Kind           string     `json:"kind"`
	Version        string     `json:"version"`
	Filename       string     `json:"filename"`
	ExpectedSize   int64      `json:"expected_size"`
	ExpectedSHA256 string     `json:"expected_sha256"`
	Source         string     `json:"source"`
	AgentType      string     `json:"agent_type"`
	ExpiresAt      *time.Time `json:"expires_at"`
}

func (in uploadInit) toUpload(actor string) *distrib.Upload {
	kind, _ := distrib.ParseKind(in.Kind)
	src := distrib.SourceUpload
	if in.Source != "" {
		src, _ = distrib.ParseSource(in.Source)
	}
	return &distrib.Upload{
		Name: in.Name, Kind: kind, Version: in.Version, Filename: in.Filename,
		ExpectedSize: in.ExpectedSize, ExpectedSHA256: strings.ToLower(in.ExpectedSHA256),
		Source: src, AgentType: in.AgentType, ExpiresAt: in.ExpiresAt,
		CreatedAt: time.Now(), UploadedBy: actor,
	}
}

func (a *API) initUpload(w http.ResponseWriter, r *http.Request, u *store.User) {
	if !a.requirePackages(w) {
		return
	}
	var in uploadInit
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	up := in.toUpload(u.Username)
	if err := distrib.ValidateUpload(up); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if up.ExpiresAt != nil && !up.ExpiresAt.After(time.Now()) {
		writeErr(w, http.StatusBadRequest, "expires_at must be in the future")
		return
	}
	if err := a.pkgs.InitUpload(up); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.st.WithLock(func(st *store.State) { st.PackageUploads[up.ID] = up })
	_ = a.st.Save()
	writeJSON(w, http.StatusCreated, map[string]any{
		"upload_id": up.ID, "received_bytes": 0, "expected_size": up.ExpectedSize,
	})
}

func (a *API) getUpload(r *http.Request) (*distrib.Upload, bool) {
	var up *distrib.Upload
	a.st.Read(func(st *store.State) { up = st.PackageUploads[r.PathValue("id")] })
	return up, up != nil
}

func (a *API) uploadStatus(w http.ResponseWriter, r *http.Request, u *store.User) {
	if !a.requirePackages(w) {
		return
	}
	up, ok := a.getUpload(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "upload not found")
		return
	}
	received, err := a.pkgs.ReceivedBytes(up)
	if err != nil {
		writeErr(w, http.StatusNotFound, "upload not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"upload_id": up.ID, "received_bytes": received, "expected_size": up.ExpectedSize,
		"status": "uploading",
	})
}

func (a *API) uploadChunk(w http.ResponseWriter, r *http.Request, u *store.User) {
	if !a.requirePackages(w) {
		return
	}
	up, ok := a.getUpload(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "upload not found")
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		writeErr(w, http.StatusBadRequest, "offset query parameter required")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, distrib.MaxChunkSize+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "chunk too large (max 8 MiB)")
		return
	}
	received, err := a.pkgs.AppendChunk(up, offset, data)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.st.WithLock(func(st *store.State) {
		if s, ok := st.PackageUploads[up.ID]; ok {
			s.ReceivedBytes = received
		}
	})
	_ = a.st.Save()
	writeJSON(w, http.StatusOK, map[string]any{
		"upload_id": up.ID, "received_bytes": received, "expected_size": up.ExpectedSize,
	})
}

func (a *API) completeUpload(w http.ResponseWriter, r *http.Request, u *store.User) {
	if !a.requirePackages(w) {
		return
	}
	up, ok := a.getUpload(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "upload not found")
		return
	}
	pkg, err := a.pkgs.CompleteUpload(up)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.st.WithLock(func(st *store.State) {
		delete(st.PackageUploads, up.ID)
		st.Packages[pkg.ID] = pkg
	})
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "package_uploaded",
		Detail: map[string]string{"package": pkg.Name, "kind": string(pkg.Kind),
			"version": pkg.Version, "status": string(pkg.Status), "source": string(pkg.Source)}})
	writeJSON(w, http.StatusCreated, map[string]any{"package": toPackageView(pkg)})
}

func (a *API) abortUpload(w http.ResponseWriter, r *http.Request, u *store.User) {
	if !a.requirePackages(w) {
		return
	}
	up, ok := a.getUpload(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "upload not found")
		return
	}
	_ = a.pkgs.RemoveUpload(up)
	a.st.WithLock(func(st *store.State) { delete(st.PackageUploads, up.ID) })
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "package_upload_aborted",
		Detail: map[string]string{"package": up.Name, "kind": string(up.Kind), "version": up.Version}})
	writeJSON(w, http.StatusOK, map[string]any{"aborted": up.ID})
}

// uploadPackage is the single-shot upload: one multipart request
// with the file and its metadata. It reuses the resumable flow
// (init → chunks → complete) so both paths share the same checksum
// and status rules; an offline import is the same request with
// source=offline-import and the vendor manifest's checksum.
func (a *API) uploadPackage(w http.ResponseWriter, r *http.Request, u *store.User) {
	if !a.requirePackages(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, distrib.MaxPackageSize+1<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "multipart parse failed (max 256 MiB)")
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "file field required")
		return
	}
	defer f.Close()
	var expires *time.Time
	if s := r.FormValue("expires_at"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "expires_at must be RFC3339")
			return
		}
		if !t.After(time.Now()) {
			writeErr(w, http.StatusBadRequest, "expires_at must be in the future")
			return
		}
		expires = &t
	}
	filename := r.FormValue("filename")
	if filename == "" {
		filename = hdr.Filename
	}
	in := uploadInit{
		Name: r.FormValue("name"), Kind: r.FormValue("kind"), Version: r.FormValue("version"),
		Filename: filename, ExpectedSize: hdr.Size,
		ExpectedSHA256: r.FormValue("expected_sha256"), Source: r.FormValue("source"),
		AgentType: r.FormValue("agent_type"), ExpiresAt: expires,
	}
	up := in.toUpload(u.Username)
	if err := distrib.ValidateUpload(up); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.pkgs.InitUpload(up); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Stream the file through the chunk path in 32 KiB pieces.
	received := int64(0)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			var err error
			received, err = a.pkgs.AppendChunk(up, received, buf[:n])
			if err != nil {
				_ = a.pkgs.RemoveUpload(up)
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = a.pkgs.RemoveUpload(up)
			writeErr(w, http.StatusBadRequest, "read failed")
			return
		}
	}
	pkg, err := a.pkgs.CompleteUpload(up)
	if err != nil {
		_ = a.pkgs.RemoveUpload(up)
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.st.WithLock(func(st *store.State) { st.Packages[pkg.ID] = pkg })
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "package_uploaded",
		Detail: map[string]string{"package": pkg.Name, "kind": string(pkg.Kind),
			"version": pkg.Version, "status": string(pkg.Status), "source": string(pkg.Source)}})
	writeJSON(w, http.StatusCreated, map[string]any{"package": toPackageView(pkg)})
}

func (a *API) getPackage(r *http.Request) (*distrib.Package, bool) {
	var pkg *distrib.Package
	a.st.Read(func(st *store.State) { pkg = st.Packages[r.PathValue("id")] })
	return pkg, pkg != nil
}

// downloadPackage streams a hosted package to any logged-in user:
// employees self-serve their install from here. The audit event
// records who downloaded which package, never the checksum.
func (a *API) downloadPackage(w http.ResponseWriter, r *http.Request, u *store.User) {
	if !a.requirePackages(w) {
		return
	}
	pkg, ok := a.getPackage(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "package not found")
		return
	}
	f, err := a.pkgs.OpenFile(pkg)
	if err != nil {
		writeErr(w, http.StatusNotFound, "package file missing")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", pkg.Filename))
	w.Header().Set("Content-Length", strconv.FormatInt(pkg.SizeBytes, 10))
	w.Header().Set("X-Package-SHA256", pkg.SHA256)
	w.Header().Set("X-Package-Status", string(pkg.Status))
	_, _ = io.Copy(w, f)
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "package", Action: "package_downloaded",
		Detail: map[string]string{"package": pkg.Name, "kind": string(pkg.Kind),
			"version": pkg.Version, "status": string(pkg.Status)}})
}

func (a *API) deletePackage(w http.ResponseWriter, r *http.Request, u *store.User) {
	if !a.requirePackages(w) {
		return
	}
	pkg, ok := a.getPackage(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "package not found")
		return
	}
	_ = a.pkgs.RemovePackage(pkg)
	a.st.WithLock(func(st *store.State) { delete(st.Packages, pkg.ID) })
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "package_deleted",
		Detail: map[string]string{"package": pkg.Name, "kind": string(pkg.Kind), "version": pkg.Version}})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": pkg.ID})
}

// registerPackageBaseline registers one package's SHA-256 as the
// F14 attestation reference value for an agent type: future logins
// whose binary hash differs are downgraded. Only a verified package
// may become a baseline — an unverified file proves nothing. The
// hash itself never enters the audit log (only category facts do,
// per F14); the policy record is the place the hash lives.
func (a *API) registerPackageBaseline(pkg *distrib.Package, agentType string) error {
	if pkg.Status != distrib.StatusVerified {
		return fmt.Errorf("only a verified package can be registered as a baseline")
	}
	if agentType == "" {
		agentType = pkg.AgentType
	}
	if agentType == "" {
		return fmt.Errorf("agent_type required")
	}
	if len(agentType) > 64 {
		return fmt.Errorf("agent_type too long")
	}
	var pol *attest.Policy
	a.st.WithLock(func(st *store.State) {
		pol = st.AttestationPolicies[agentType]
		if pol == nil {
			pol = &attest.Policy{AgentType: agentType}
			st.AttestationPolicies[agentType] = pol
		}
		found := false
		for _, h := range pol.AllowedBinaryHashes {
			if strings.EqualFold(h, pkg.SHA256) {
				found = true
				break
			}
		}
		if !found {
			pol.AllowedBinaryHashes = append(pol.AllowedBinaryHashes, pkg.SHA256)
		}
	})
	if err := pol.Validate(); err != nil {
		return err
	}
	a.st.WithLock(func(st *store.State) {
		if p, ok := st.Packages[pkg.ID]; ok {
			p.BaselineRegistered = true
		}
	})
	return a.st.Save()
}

func (a *API) registerBaseline(w http.ResponseWriter, r *http.Request, u *store.User) {
	if !a.requirePackages(w) {
		return
	}
	pkg, ok := a.getPackage(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "package not found")
		return
	}
	var in struct {
		AgentType string `json:"agent_type"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if err := a.registerPackageBaseline(pkg, in.AgentType); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	agentType := in.AgentType
	if agentType == "" {
		agentType = pkg.AgentType
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "package_baseline_registered",
		Detail: map[string]string{"package": pkg.Name, "kind": string(pkg.Kind),
			"version": pkg.Version, "agent_type": agentType}})
	writeJSON(w, http.StatusOK, map[string]any{"package": toPackageView(pkg), "agent_type": agentType})
}

func (a *API) cleanupPackages(w http.ResponseWriter, r *http.Request, u *store.User) {
	if !a.requirePackages(w) {
		return
	}
	var expired []*distrib.Package
	a.st.Read(func(st *store.State) {
		all := make([]*distrib.Package, 0, len(st.Packages))
		for _, p := range st.Packages {
			all = append(all, p)
		}
		expired = distrib.Expired(all, time.Now())
	})
	deleted := []string{}
	for _, p := range expired {
		_ = a.pkgs.RemovePackage(p)
		deleted = append(deleted, p.ID)
	}
	a.st.WithLock(func(st *store.State) {
		for _, id := range deleted {
			delete(st.Packages, id)
		}
	})
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "packages_cleaned",
		Detail: map[string]string{"count": strconv.Itoa(len(deleted))}})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "count": len(deleted)})
}

type bundleComponentIn struct {
	PackageID string `json:"package_id"`
	Role      string `json:"role"`
}

// generateUserBundle assembles a user package (ag-guard + ag-setup
// + generated guard.json + MANIFEST.json) from two already-hosted
// component packages and the policy the admin enters. The bundle is
// stored as a verified user-kind package (its hash is computed at
// generation, so expected == actual by construction) and, when
// asked, its hash is registered as the F14 baseline in the same call.
func (a *API) generateUserBundle(w http.ResponseWriter, r *http.Request, u *store.User) {
	if !a.requirePackages(w) {
		return
	}
	var in struct {
		Name             string              `json:"name"`
		Version          string              `json:"version"`
		AgentType        string              `json:"agent_type"`
		GatewayURL       string              `json:"gateway_url"`
		MaxDataLevel     string              `json:"max_data_level"`
		EgressAllowlist  []string            `json:"egress_allowlist"`
		Components       []bundleComponentIn `json:"components"`
		RegisterBaseline bool                `json:"register_baseline"`
		ExpiresAt        *time.Time          `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if in.MaxDataLevel == "" {
		in.MaxDataLevel = "internal"
	}
	if in.ExpiresAt != nil && !in.ExpiresAt.After(time.Now()) {
		writeErr(w, http.StatusBadRequest, "expires_at must be in the future")
		return
	}
	if len(in.Components) != 2 {
		writeErr(w, http.StatusBadRequest, "exactly two components required (ag-guard + ag-setup)")
		return
	}
	components := make([]distrib.BundleComponent, 0, 2)
	for _, c := range in.Components {
		if c.Role != "ag-guard" && c.Role != "ag-setup" {
			writeErr(w, http.StatusBadRequest, "component role must be ag-guard or ag-setup")
			return
		}
		var pkg *distrib.Package
		a.st.Read(func(st *store.State) { pkg = st.Packages[c.PackageID] })
		if pkg == nil {
			writeErr(w, http.StatusNotFound, "component package not found: "+c.PackageID)
			return
		}
		data, err := a.pkgs.ReadFile(pkg)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "component file missing: "+pkg.Name)
			return
		}
		components = append(components, distrib.BundleComponent{
			Role: c.Role, Filename: pkg.Filename, Data: data, SHA256: pkg.SHA256,
		})
	}
	cfg := distrib.UserBundleConfig{
		Name: in.Name, Version: in.Version, AgentType: in.AgentType,
		GatewayURL: in.GatewayURL, MaxDataLevel: in.MaxDataLevel,
		EgressAllowlist: in.EgressAllowlist, GeneratedAt: time.Now(),
	}
	zb, sum, err := distrib.GenerateUserBundle(cfg, components)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	filename := sanitizeBundleName(in.Name) + "-" + in.Version + ".zip"
	pkg := &distrib.Package{
		ID: distrib.NewID("pkg"), Name: in.Name, Kind: distrib.KindUser,
		Version: in.Version, Filename: filename, SizeBytes: int64(len(zb)),
		SHA256: sum, ExpectedSHA256: sum, Status: distrib.StatusVerified,
		Source: distrib.SourceGenerated, AgentType: in.AgentType,
		ExpiresAt: in.ExpiresAt, CreatedAt: time.Now(), UploadedBy: u.Username,
	}
	if err := a.pkgs.StoreBytes(pkg, zb); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.st.WithLock(func(st *store.State) { st.Packages[pkg.ID] = pkg })
	_ = a.st.Save()
	if in.RegisterBaseline {
		if err := a.registerPackageBaseline(pkg, in.AgentType); err != nil {
			writeErr(w, http.StatusBadRequest, "bundle stored, baseline registration failed: "+err.Error())
			return
		}
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "package_bundle_generated",
		Detail: map[string]string{"package": pkg.Name, "kind": "user",
			"version": pkg.Version, "agent_type": in.AgentType,
			"baseline_registered": strconv.FormatBool(in.RegisterBaseline)}})
	writeJSON(w, http.StatusCreated, map[string]any{"package": toPackageView(pkg)})
}

func sanitizeBundleName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.':
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "agentgate-user"
	}
	return b.String()
}
