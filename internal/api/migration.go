package api

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gocracker/gocracker/pkg/vmm"
)

type MigrateRequest struct {
	DestinationURL string `json:"destination_url"`
	TargetVMID     string `json:"target_vm_id,omitempty"`
	TargetTapName  string `json:"target_tap_name,omitempty"`
	ResumeTarget   *bool  `json:"resume_target,omitempty"`
}

type MigrationPrepareResponse struct {
	SessionID string `json:"session_id"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
}

type MigrationFinalizeRequest struct {
	SessionID string `json:"session_id"`
	VMID      string `json:"vm_id,omitempty"`
	TapName   string `json:"tap_name,omitempty"`
	Resume    bool   `json:"resume"`
}

type MigrationLoadRequest struct {
	VMID    string `json:"vm_id,omitempty"`
	TapName string `json:"tap_name,omitempty"`
	Resume  bool   `json:"resume"`
}

type MigrationResponse struct {
	SourceID  string `json:"source_id,omitempty"`
	TargetID  string `json:"target_id"`
	State     string `json:"state"`
	BundleDir string `json:"bundle_dir,omitempty"`
	Message   string `json:"message,omitempty"`
}

type migrationCapable interface {
	PrepareMigrationBundle(string) error
	FinalizeMigrationBundle(string) (*vmm.Snapshot, *vmm.MigrationPatchSet, error)
	ResetMigrationTracking() error
}

func (s *Server) handleMigrateVM(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req MigrateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.DestinationURL == "" {
		apiErr(w, http.StatusBadRequest, "destination_url is required")
		return
	}

	s.mu.RLock()
	entry, ok := s.vms[id]
	s.mu.RUnlock()
	if !ok {
		apiErr(w, http.StatusNotFound, "VM not found")
		return
	}
	sourceArch := defaultVMArch(entry.handle.VMConfig().Arch)
	if err := ensureSameArchMigrationTarget(r.Context(), req.DestinationURL, sourceArch); err != nil {
		apiErr(w, http.StatusBadGateway, err.Error())
		return
	}
	workDir, err := os.MkdirTemp("", "gocracker-migrate-src-*")
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.RemoveAll(workDir)

	prepareDir := filepath.Join(workDir, "prepare")
	finalDir := filepath.Join(workDir, "final")
	if err := os.MkdirAll(prepareDir, 0o755); err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.MkdirAll(finalDir, 0o755); err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resumeTarget := true
	if req.ResumeTarget != nil {
		resumeTarget = *req.ResumeTarget
	}

	// Build the base bundle locally BEFORE shipping it. PrepareMigrationBundle
	// writes mem.bin + kernel + initrd + disk into prepareDir while the source
	// VM is still running, then enables dirty tracking so FinalizeMigrationBundle
	// can capture only the delta.
	migrator, hasMigrator := entry.handle.(migrationCapable)
	if hasMigrator {
		if err := migrator.PrepareMigrationBundle(prepareDir); err != nil {
			apiErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	prepareResp, err := sendMigrationPrepare(r.Context(), req.DestinationURL, prepareDir)
	if err != nil {
		if hasMigrator {
			_ = migrator.ResetMigrationTracking()
		}
		apiErr(w, http.StatusBadGateway, err.Error())
		return
	}

	finalizeReq := MigrationFinalizeRequest{
		SessionID: prepareResp.SessionID,
		VMID:      req.TargetVMID,
		TapName:   req.TargetTapName,
		Resume:    resumeTarget,
	}
	targetResp, migrateMode, err := s.migrateVMHandle(r.Context(), entry, req.DestinationURL, prepareDir, finalDir, finalizeReq)
	if err != nil {
		_ = sendMigrationAbort(r.Context(), req.DestinationURL, prepareResp.SessionID)
		apiErr(w, http.StatusBadGateway, err.Error())
		return
	}

	entry.handle.Stop()
	s.mu.Lock()
	delete(s.vms, id)
	delete(s.vmDirs, id)
	s.mu.Unlock()

	resp := MigrationResponse{
		SourceID: id,
		TargetID: targetResp.TargetID,
		State:    "migrated",
		Message:  migrateMode,
	}
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) migrateVMHandle(ctx context.Context, entry *vmEntry, destination string, prepareDir, finalDir string, finalizeReq MigrationFinalizeRequest) (MigrationResponse, string, error) {
	_ = prepareDir // already shipped before this call; only finalize delta is sent here
	if migrator, ok := entry.handle.(migrationCapable); ok {
		if _, _, err := migrator.FinalizeMigrationBundle(finalDir); err != nil {
			_ = migrator.ResetMigrationTracking()
			if vm, ok := entry.handle.(*vmm.VM); ok {
				if resumeErr := vm.Resume(); resumeErr != nil {
					err = fmt.Errorf("%w (resume source failed: %v)", err, resumeErr)
				}
			}
			return MigrationResponse{}, "", err
		}
		resp, err := sendMigrationFinalize(ctx, destination, finalizeReq, finalDir)
		if err != nil {
			_ = migrator.ResetMigrationTracking()
			if vm, ok := entry.handle.(*vmm.VM); ok {
				if resumeErr := vm.Resume(); resumeErr != nil {
					err = fmt.Errorf("%w (resume source failed: %v)", err, resumeErr)
				}
			}
			return MigrationResponse{}, "", err
		}
		return resp, "VM migrated with pre-copy handoff", nil
	}
	if _, err := entry.handle.TakeSnapshot(finalDir); err != nil {
		return MigrationResponse{}, "", err
	}
	resp, err := sendMigrationLoad(ctx, destination, MigrationLoadRequest{
		VMID:    finalizeReq.VMID,
		TapName: finalizeReq.TapName,
		Resume:  finalizeReq.Resume,
	}, finalDir)
	if err != nil {
		return MigrationResponse{}, "", err
	}
	return resp, "VM migrated with snapshot handoff", nil
}

func sendMigrationLoad(ctx context.Context, destination string, req MigrationLoadRequest, bundleDir string) (MigrationResponse, error) {
	var resp MigrationResponse
	targetURL, err := migrationEndpointURL(destination)
	if err != nil {
		return resp, err
	}
	if err := postBundle(ctx, targetURL, req, bundleDir, &resp); err != nil {
		return resp, err
	}
	return resp, nil
}

func (s *Server) handleMigrationPrepare(w http.ResponseWriter, r *http.Request) {
	bundleDir, err := readBundleUpload(r)
	if err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sessionID := fmt.Sprintf("mig-%d", time.Now().UnixNano())
	s.mu.Lock()
	s.migrationSessions[sessionID] = bundleDir
	s.mu.Unlock()

	json.NewEncoder(w).Encode(MigrationPrepareResponse{
		SessionID: sessionID,
		State:     "prepared",
		Message:   "migration base bundle received",
	})
}

func (s *Server) handleMigrationFinalize(w http.ResponseWriter, r *http.Request) {
	req, bundleDir, err := s.readMigrationFinalizeUpload(r)
	if err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}

	handle, cleanup, err := s.restoreManagedVMM(bundleDir, vmm.RestoreOptions{
		OverrideID:  req.VMID,
		OverrideTap: req.TapName,
	}, true, req.Resume)
	if err != nil {
		s.dropMigrationSession(req.SessionID, true)
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.mu.Lock()
	if _, exists := s.vms[handle.ID()]; exists {
		s.mu.Unlock()
		handle.Stop()
		s.dropMigrationSession(req.SessionID, true)
		apiErr(w, http.StatusConflict, "VM already exists on target")
		return
	}
	delete(s.migrationSessions, req.SessionID)
	s.mu.Unlock()
	entry := s.newVMEntry(handle, cleanup)
	entry.bundleDir = bundleDir
	s.registerVMEntry(handle.ID(), entry)

	json.NewEncoder(w).Encode(MigrationResponse{
		TargetID:  handle.ID(),
		State:     handle.State().String(),
		BundleDir: bundleDir,
		Message:   "migration finalized",
	})
}

func (s *Server) handleMigrationAbort(w http.ResponseWriter, r *http.Request) {
	var req MigrationFinalizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.SessionID == "" {
		apiErr(w, http.StatusBadRequest, "session_id is required")
		return
	}
	s.dropMigrationSession(req.SessionID, true)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMigrationLoad(w http.ResponseWriter, r *http.Request) {
	loadReq, bundleDir, err := readMigrationUpload(r)
	if err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}

	handle, cleanup, err := s.restoreManagedVMM(bundleDir, vmm.RestoreOptions{
		OverrideID:  loadReq.VMID,
		OverrideTap: loadReq.TapName,
	}, true, loadReq.Resume)
	if err != nil {
		_ = os.RemoveAll(bundleDir)
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.mu.Lock()
	if _, exists := s.vms[handle.ID()]; exists {
		s.mu.Unlock()
		handle.Stop()
		_ = os.RemoveAll(bundleDir)
		apiErr(w, http.StatusConflict, "VM already exists on target")
		return
	}
	s.mu.Unlock()
	entry := s.newVMEntry(handle, cleanup)
	entry.bundleDir = bundleDir
	s.registerVMEntry(handle.ID(), entry)

	json.NewEncoder(w).Encode(MigrationResponse{
		TargetID:  handle.ID(),
		State:     handle.State().String(),
		BundleDir: bundleDir,
		Message:   "migration bundle loaded",
	})
}

func sendMigrationPrepare(ctx context.Context, destination, bundleDir string) (MigrationPrepareResponse, error) {
	var resp MigrationPrepareResponse
	targetURL, err := migrationStageURL(destination, "/migrations/prepare")
	if err != nil {
		return resp, err
	}
	if err := postBundle(ctx, targetURL, nil, bundleDir, &resp); err != nil {
		return resp, err
	}
	return resp, nil
}

func sendMigrationFinalize(ctx context.Context, destination string, req MigrationFinalizeRequest, bundleDir string) (MigrationResponse, error) {
	var resp MigrationResponse
	targetURL, err := migrationStageURL(destination, "/migrations/finalize")
	if err != nil {
		return resp, err
	}
	if err := postBundle(ctx, targetURL, req, bundleDir, &resp); err != nil {
		return resp, err
	}
	return resp, nil
}

func sendMigrationAbort(ctx context.Context, destination, sessionID string) error {
	targetURL, err := migrationStageURL(destination, "/migrations/abort")
	if err != nil {
		return err
	}
	body, err := json.Marshal(MigrationFinalizeRequest{SessionID: sessionID})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	applyEnvBearerToken(req)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("destination returned %s", resp.Status)
	}
	return nil
}

func postBundle(ctx context.Context, targetURL string, reqBody any, bundleDir string, respOut any) error {
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)

	go func() {
		defer pw.Close()
		defer writer.Close()

		if reqBody != nil {
			reqPart, err := writer.CreateFormField("request")
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if err := json.NewEncoder(reqPart).Encode(reqBody); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}

		bundlePart, err := writer.CreateFormFile("bundle", "bundle.tar.gz")
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if err := writeTarGz(bundlePart, bundleDir); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
	}()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, pr)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())
	applyEnvBearerToken(httpReq)

	httpResp, err := (&http.Client{Timeout: 15 * time.Minute}).Do(httpReq)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode >= 400 {
		var apiErrResp APIError
		if err := json.NewDecoder(httpResp.Body).Decode(&apiErrResp); err == nil && apiErrResp.FaultMessage != "" {
			return fmt.Errorf("destination returned %s: %s", httpResp.Status, apiErrResp.FaultMessage)
		}
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return fmt.Errorf("destination returned %s: %s", httpResp.Status, strings.TrimSpace(string(body)))
	}
	if respOut != nil {
		if err := json.NewDecoder(httpResp.Body).Decode(respOut); err != nil {
			return err
		}
	}
	return nil
}

func applyEnvBearerToken(req *http.Request) {
	token := strings.TrimSpace(os.Getenv("GOCRACKER_API_TOKEN"))
	if token == "" || req == nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
}

func migrationEndpointURL(raw string) (string, error) {
	return migrationStageURL(raw, "/migrations/load")
}

func instanceInfoURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" {
		return "", fmt.Errorf("destination_url must include a scheme")
	}
	base := strings.TrimRight(u.Path, "/")
	if base == "" {
		u.Path = "/"
	} else {
		u.Path = base
	}
	return u.String(), nil
}

func migrationStageURL(raw, suffix string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" {
		return "", fmt.Errorf("destination_url must include a scheme")
	}
	base := strings.TrimRight(u.Path, "/")
	u.Path = base + suffix
	return u.String(), nil
}

func ensureSameArchMigrationTarget(ctx context.Context, destination, sourceArch string) error {
	info, err := fetchDestinationInstanceInfo(ctx, destination)
	if err != nil {
		return err
	}
	destArch := strings.TrimSpace(info.HostArch)
	if destArch == "" {
		return fmt.Errorf("destination instance info did not report host_arch; upgrade the destination server before migrating")
	}
	if destArch != sourceArch {
		return fmt.Errorf("destination host arch %q is not compatible with source VM arch %q (same-arch only)", destArch, sourceArch)
	}
	return nil
}

func fetchDestinationInstanceInfo(ctx context.Context, destination string) (InstanceInfo, error) {
	targetURL, err := instanceInfoURL(destination)
	if err != nil {
		return InstanceInfo{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return InstanceInfo{}, err
	}
	applyEnvBearerToken(req)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return InstanceInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var apiErrResp APIError
		if err := json.NewDecoder(resp.Body).Decode(&apiErrResp); err == nil && apiErrResp.FaultMessage != "" {
			return InstanceInfo{}, fmt.Errorf("destination returned %s: %s", resp.Status, apiErrResp.FaultMessage)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return InstanceInfo{}, fmt.Errorf("destination returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var info InstanceInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return InstanceInfo{}, fmt.Errorf("decode destination instance info: %w", err)
	}
	return info, nil
}

func (s *Server) readMigrationFinalizeUpload(r *http.Request) (MigrationFinalizeRequest, string, error) {
	var req MigrationFinalizeRequest
	req.Resume = true

	reader, err := r.MultipartReader()
	if err != nil {
		return req, "", err
	}

	sessionDir := ""
	seenBundle := false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return req, "", err
		}

		switch part.FormName() {
		case "request":
			if err := json.NewDecoder(part).Decode(&req); err != nil {
				return req, "", err
			}
			if req.SessionID == "" {
				return req, "", fmt.Errorf("session_id is required")
			}
			s.mu.RLock()
			sessionDir = s.migrationSessions[req.SessionID]
			s.mu.RUnlock()
			if sessionDir == "" {
				return req, "", fmt.Errorf("migration session not found")
			}
		case "bundle":
			if sessionDir == "" {
				return req, "", fmt.Errorf("request part must precede bundle")
			}
			if err := extractTarGz(part, sessionDir); err != nil {
				return req, "", err
			}
			seenBundle = true
		}
		part.Close()
	}

	if req.SessionID == "" {
		return req, "", fmt.Errorf("session_id is required")
	}
	if !seenBundle {
		return req, "", fmt.Errorf("bundle file is required")
	}
	return req, sessionDir, nil
}

func readMigrationUpload(r *http.Request) (MigrationLoadRequest, string, error) {
	var req MigrationLoadRequest
	req.Resume = true

	reader, err := r.MultipartReader()
	if err != nil {
		return req, "", err
	}

	bundleDir, err := os.MkdirTemp("", "gocracker-migrate-dst-*")
	if err != nil {
		return req, "", err
	}

	seenBundle := false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = os.RemoveAll(bundleDir)
			return req, "", err
		}

		switch part.FormName() {
		case "request":
			if err := json.NewDecoder(part).Decode(&req); err != nil {
				_ = os.RemoveAll(bundleDir)
				return req, "", err
			}
		case "bundle":
			if err := extractTarGz(part, bundleDir); err != nil {
				_ = os.RemoveAll(bundleDir)
				return req, "", err
			}
			seenBundle = true
		}
		part.Close()
	}
	if !seenBundle {
		_ = os.RemoveAll(bundleDir)
		return req, "", fmt.Errorf("bundle file is required")
	}
	return req, bundleDir, nil
}

func readBundleUpload(r *http.Request) (string, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return "", err
	}

	bundleDir, err := os.MkdirTemp("", "gocracker-migrate-dst-*")
	if err != nil {
		return "", err
	}
	seenBundle := false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = os.RemoveAll(bundleDir)
			return "", err
		}
		if part.FormName() == "bundle" {
			if err := extractTarGz(part, bundleDir); err != nil {
				_ = os.RemoveAll(bundleDir)
				return "", err
			}
			seenBundle = true
		}
		part.Close()
	}
	if !seenBundle {
		_ = os.RemoveAll(bundleDir)
		return "", fmt.Errorf("bundle file is required")
	}
	return bundleDir, nil
}

func writeTarGz(w io.Writer, dir string) error {
	gzw := gzip.NewWriter(w)
	defer gzw.Close()
	tw := tar.NewWriter(gzw)
	defer tw.Close()

	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

func extractTarGz(r io.Reader, dir string) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		target := filepath.Join(dir, filepath.Clean(hdr.Name))
		if !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) && filepath.Clean(target) != filepath.Clean(dir) {
			return fmt.Errorf("invalid bundle path %q", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported tar entry type %d", hdr.Typeflag)
		}
	}
}

func (s *Server) dropMigrationSession(sessionID string, removeDir bool) {
	s.mu.Lock()
	dir := s.migrationSessions[sessionID]
	delete(s.migrationSessions, sessionID)
	s.mu.Unlock()
	if removeDir && dir != "" {
		_ = os.RemoveAll(dir)
	}
}
