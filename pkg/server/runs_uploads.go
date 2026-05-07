package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// stagedUpload is the on-disk metadata sidecar for an upload that
// has been received but not yet promoted to a run. The bytes live
// alongside it in the same staging directory.
type stagedUpload struct {
	UploadID         string    `json:"upload_id"`
	OriginalFilename string    `json:"original_filename"`
	MIME             string    `json:"mime"`
	Size             int64     `json:"size"`
	SHA256           string    `json:"sha256"`
	CreatedAt        time.Time `json:"created_at"`
}

const (
	uploadStagingSubdir = "uploads"
	uploadMetaFilename  = "meta.json"
	// uploadStagingTTL bounds how long an unreferenced upload sits
	// in the staging area before it is reaped. The launch path
	// promotes within seconds of the upload completing; a generous
	// TTL accommodates a user closing the modal without a leak.
	uploadStagingTTL = 1 * time.Hour
)

// stagingRoot returns the directory housing pending uploads.
func (s *Server) stagingRoot() (string, error) {
	if s.runs == nil {
		return "", errors.New("uploads disabled: run store not configured")
	}
	root := s.runs.StoreRoot()
	if root == "" {
		// Cloud / non-FS stores currently lack staging support.
		return "", errors.New("uploads disabled: store has no filesystem root")
	}
	dir := filepath.Join(root, uploadStagingSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create staging root: %w", err)
	}
	return dir, nil
}

// handleUploadAttachment answers POST /api/runs/uploads. The body is
// `multipart/form-data` with a single `file` field plus optional
// `declared_mime` form field.
func (s *Server) handleUploadAttachment(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.cfg.MaxUploadSize <= 0 {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "uploads not configured on this server")
		return
	}
	staging, err := s.stagingRoot()
	if err != nil {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "uploads disabled: %v", err)
		return
	}

	// MaxBytesReader covers the body even when ParseMultipartForm
	// streams. We add ~16 KB headroom for multipart envelope bytes.
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadSize+16<<10)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.httpErrorFor(w, r, http.StatusRequestEntityTooLarge,
				"file exceeds max_upload_size (%d bytes)", s.cfg.MaxUploadSize)
			return
		}
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid multipart form: %v", err)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing 'file' field: %v", err)
		return
	}
	defer file.Close()

	declaredMIME := strings.TrimSpace(r.FormValue("declared_mime"))

	uploadID, err := newUploadID()
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "generate upload id: %v", err)
		return
	}
	dir := filepath.Join(staging, uploadID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "mkdir staging: %v", err)
		return
	}

	// Sanitise filename: strip any path component the browser tucked
	// in, and refuse traversal attempts.
	filename := filepath.Base(hdr.Filename)
	if filename == "" || filename == "." || filename == ".." || strings.ContainsAny(filename, `/\`) {
		os.RemoveAll(dir)
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid filename")
		return
	}

	dst, err := os.Create(filepath.Join(dir, filename))
	if err != nil {
		os.RemoveAll(dir)
		s.httpErrorFor(w, r, http.StatusInternalServerError, "create staging file: %v", err)
		return
	}
	// Streaming copy with size guard (defence-in-depth: form parsing
	// already limited via MaxBytesReader, but ParseMultipartForm may
	// have spilled to a temp file we don't directly observe).
	written, err := io.Copy(dst, io.LimitReader(file, s.cfg.MaxUploadSize+1))
	dst.Close()
	if err != nil {
		os.RemoveAll(dir)
		s.httpErrorFor(w, r, http.StatusInternalServerError, "write staging file: %v", err)
		return
	}
	if written > s.cfg.MaxUploadSize {
		os.RemoveAll(dir)
		s.httpErrorFor(w, r, http.StatusRequestEntityTooLarge,
			"file exceeds max_upload_size (%d bytes)", s.cfg.MaxUploadSize)
		return
	}
	if written == 0 {
		os.RemoveAll(dir)
		s.httpErrorFor(w, r, http.StatusBadRequest, "empty upload")
		return
	}

	// Sniff the actual MIME from the on-disk bytes — never trust the
	// declared form field. We re-open the file to feed http.DetectContentType
	// without rebuffering the upload in memory.
	mime, sha256Hex, err := sniffMIMEAndHash(filepath.Join(dir, filename))
	if err != nil {
		os.RemoveAll(dir)
		s.httpErrorFor(w, r, http.StatusInternalServerError, "sniff/hash: %v", err)
		return
	}
	if !mimeAllowed(mime, s.cfg.AllowedUploadMIMEs) {
		os.RemoveAll(dir)
		s.httpErrorFor(w, r, http.StatusUnsupportedMediaType,
			"file type %q not in allowlist; declared was %q", mime, declaredMIME)
		return
	}

	rec := stagedUpload{
		UploadID:         uploadID,
		OriginalFilename: filename,
		MIME:             mime,
		Size:             written,
		SHA256:           sha256Hex,
		CreatedAt:        time.Now().UTC(),
	}
	metaData, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, uploadMetaFilename), metaData, 0o600); err != nil {
		os.RemoveAll(dir)
		s.httpErrorFor(w, r, http.StatusInternalServerError, "write meta: %v", err)
		return
	}

	s.writeJSONFor(w, r, rec)
}

// promoteStaged moves uploads from staging into a run-scoped
// destination by calling RunStore.WriteAttachment for each. Returns
// the populated map[name]AttachmentRecord and the cumulative bytes.
//
// Validation against the workflow's IR (declared names, accept_mime,
// required) is the caller's responsibility. This helper is concerned
// only with the wire format and the staging fs layout.
func (s *Server) promoteStaged(ctx context.Context, runID string, mapping map[string]string) (map[string]store.AttachmentRecord, int64, error) {
	if len(mapping) == 0 {
		return nil, 0, nil
	}
	if len(mapping) > s.cfg.MaxUploadsPerRun {
		return nil, 0, fmt.Errorf("too many attachments: %d > %d", len(mapping), s.cfg.MaxUploadsPerRun)
	}
	staging, err := s.stagingRoot()
	if err != nil {
		return nil, 0, err
	}
	out := make(map[string]store.AttachmentRecord, len(mapping))
	var cumulative int64
	for name, uploadID := range mapping {
		if err := store.SanitizePathComponent("attachment_name", name); err != nil {
			return nil, 0, fmt.Errorf("invalid attachment name %q: %w", name, err)
		}
		if err := store.SanitizePathComponent("upload_id", uploadID); err != nil {
			return nil, 0, fmt.Errorf("invalid upload id %q: %w", uploadID, err)
		}
		dir := filepath.Join(staging, uploadID)
		metaBytes, err := os.ReadFile(filepath.Join(dir, uploadMetaFilename))
		if err != nil {
			return nil, 0, fmt.Errorf("upload %q not found", uploadID)
		}
		var meta stagedUpload
		if err := json.Unmarshal(metaBytes, &meta); err != nil {
			return nil, 0, fmt.Errorf("decode upload meta: %w", err)
		}
		cumulative += meta.Size
		if cumulative > s.cfg.MaxTotalUploadSize {
			return nil, 0, fmt.Errorf("cumulative upload size exceeds max_total_upload_size (%d bytes)", s.cfg.MaxTotalUploadSize)
		}
		body, err := os.Open(filepath.Join(dir, meta.OriginalFilename))
		if err != nil {
			return nil, 0, fmt.Errorf("open upload %q: %w", uploadID, err)
		}
		rec := store.AttachmentRecord{
			Name:             name,
			OriginalFilename: meta.OriginalFilename,
			MIME:             meta.MIME,
			Size:             meta.Size,
			SHA256:           meta.SHA256,
		}
		if err := s.runs.WriteAttachment(ctx, runID, rec, body); err != nil {
			body.Close()
			return nil, 0, fmt.Errorf("persist attachment %q: %w", name, err)
		}
		body.Close()
		out[name] = rec
		_ = os.RemoveAll(dir)
	}
	// Refresh once after the loop so the returned map carries the
	// canonical StorageRef + any meta WriteAttachment recomputed.
	if list, err := s.runs.ListAttachments(ctx, runID); err == nil {
		for _, a := range list {
			if _, ok := out[a.Name]; ok {
				out[a.Name] = a
			}
		}
	}
	return out, cumulative, nil
}

// handleServeAttachment answers GET /api/runs/{id}/attachments/{name}.
// HMAC signature is required when the request comes from outside a
// safe Origin (i.e. presigned-URL access from a non-CORS-allowed
// caller); browser-origin requests skip the HMAC check via the same
// allowlist that gates the rest of the editor API.
func (s *Server) handleServeAttachment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("name")
	if id == "" || name == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing id or name")
		return
	}
	if s.runs == nil {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "run store not configured")
		return
	}

	// Either the request comes from a safe Origin (browser SPA) OR it
	// presents a valid HMAC signature minted by PresignAttachment.
	// Verifying both preserves the editor's UX (no signature in
	// dev-tools URLs) while still allowing presigned URLs to work
	// from any user agent.
	exp := r.URL.Query().Get("exp")
	sig := r.URL.Query().Get("sig")
	if exp == "" || sig == "" {
		if !s.requireSafeOrigin(w, r) {
			return
		}
	} else if !s.verifyAttachmentSig(id, name, exp, sig) {
		s.httpErrorFor(w, r, http.StatusForbidden, "invalid or expired signature")
		return
	}

	rc, rec, err := s.runs.OpenAttachment(r.Context(), id, name)
	if err != nil {
		if errors.Is(err, store.ErrAttachmentNotFound) {
			s.httpErrorFor(w, r, http.StatusNotFound, "attachment not found")
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "open attachment: %v", err)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", rec.MIME)
	if rec.Size > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", rec.Size))
	}
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("inline; filename=%s",
			url.PathEscape(rec.OriginalFilename),
		),
	)
	if _, err := io.Copy(w, rc); err != nil {
		s.logger.Warn("serve attachment %s/%s: %v", id, name, err)
	}
}

// handlePresignAttachment answers GET /api/runs/{id}/attachments/{name}/url.
// Returns a JSON {"url": "..."} the SPA can hand to a third-party
// fetcher. ttl bounds the URL's lifetime.
func (s *Server) handlePresignAttachment(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	id := r.PathValue("id")
	name := r.PathValue("name")
	if id == "" || name == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing id or name")
		return
	}
	ttl := 10 * time.Minute
	if t := r.URL.Query().Get("ttl"); t != "" {
		if d, err := time.ParseDuration(t + "s"); err == nil && d > 0 && d <= 24*time.Hour {
			ttl = d
		}
	}
	signed, err := s.runs.PresignAttachment(r.Context(), id, name, ttl)
	if err != nil {
		if errors.Is(err, store.ErrAttachmentNotFound) {
			s.httpErrorFor(w, r, http.StatusNotFound, "attachment not found")
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "presign: %v", err)
		return
	}
	s.writeJSONFor(w, r, map[string]string{"url": signed})
}

// verifyAttachmentSig dispatches the signature check to the
// underlying RunStore when the local backend supports it. Cloud
// (S3-presigned) URLs never reach this server — they are signed by
// AWS and consumed by the client directly — so we treat unknown
// stores as "no HMAC support", which forces requireSafeOrigin to
// guard the path.
func (s *Server) verifyAttachmentSig(runID, name, exp, sig string) bool {
	if s.runs == nil {
		return false
	}
	return s.runs.VerifyAttachmentSignature(runID, name, exp, sig)
}

// applyUploadDefaults fills in MaxUploadSize / total / per-run when
// the embedder didn't pick explicit values. Mode-aware: desktop is
// permissive, web/cloud are strict.
func applyUploadDefaults(cfg Config) Config {
	if cfg.MaxUploadSize <= 0 {
		switch cfg.Mode {
		case "desktop":
			cfg.MaxUploadSize = 1 << 30 // 1 GB
		default:
			cfg.MaxUploadSize = 50 << 20 // 50 MB
		}
	}
	if cfg.MaxTotalUploadSize <= 0 {
		cfg.MaxTotalUploadSize = 5 * cfg.MaxUploadSize
	}
	if cfg.MaxUploadsPerRun <= 0 {
		cfg.MaxUploadsPerRun = 20
	}
	if len(cfg.AllowedUploadMIMEs) == 0 {
		cfg.AllowedUploadMIMEs = defaultAllowedMIMEs()
	}
	return cfg
}

func defaultAllowedMIMEs() []string {
	return []string{
		"image/png", "image/jpeg", "image/gif", "image/webp",
		"application/pdf",
		"application/json", "application/yaml",
		"application/zip", "application/gzip", "application/x-tar",
		"text/plain", "text/markdown", "text/csv",
		"application/octet-stream",
	}
}

// mimeAllowed checks `mime` against an allowlist of `type/subtype`
// patterns. `*` is a wildcard segment (e.g. `image/*`).
func mimeAllowed(mime string, allow []string) bool {
	mime = strings.ToLower(strings.TrimSpace(mime))
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	mt, sub := splitMIME(mime)
	if mt == "" {
		return false
	}
	for _, pat := range allow {
		pmt, psub := splitMIME(strings.ToLower(strings.TrimSpace(pat)))
		if pmt != "*" && pmt != mt {
			continue
		}
		if psub == "*" || psub == sub {
			return true
		}
	}
	return false
}

func splitMIME(m string) (string, string) {
	i := strings.Index(m, "/")
	if i <= 0 {
		return "", ""
	}
	return m[:i], m[i+1:]
}

// newUploadID generates a URL-safe upload identifier of the form
// `up_<unix_seconds>_<8 hex random>`. Time prefix lets the TTL
// sweeper compute eligibility without reading the meta file.
func newUploadID() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("up_%d_%s", time.Now().Unix(), hex.EncodeToString(buf)), nil
}

// sniffMIMEAndHash opens the file, computes SHA-256 streaming, and
// returns the http.DetectContentType verdict on the first 512 bytes.
func sniffMIMEAndHash(p string) (string, string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	head := make([]byte, 512)
	n, err := f.Read(head)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", "", err
	}
	mime := http.DetectContentType(head[:n])

	// Reset and stream the rest into the hasher.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", "", err
	}
	return mime, hex.EncodeToString(h.Sum(nil)), nil
}

// reapStagedUploads sweeps abandoned uploads. Best-effort: errors
// are logged but never propagated. Called periodically by the server.
func (s *Server) reapStagedUploads() {
	staging, err := s.stagingRoot()
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-uploadStagingTTL)
	entries, err := os.ReadDir(staging)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		fi, err := os.Stat(path.Join(staging, e.Name()))
		if err != nil {
			continue
		}
		if fi.ModTime().Before(cutoff) {
			_ = os.RemoveAll(path.Join(staging, e.Name()))
		}
	}
}
