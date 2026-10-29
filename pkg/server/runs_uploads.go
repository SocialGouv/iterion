package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/store"
)

// stagedUpload is the record of an upload received and not yet promoted to
// a run — the wire answer of POST /api/runs/uploads. Where it waits is the
// run store's business (store.StagingStore): a directory under a local
// store, the staged_uploads collection and the blob backend under the
// cloud one. The server stages through the seam or refuses typed.
type stagedUpload = store.StagedUpload

// uploadStagingTTL bounds how long an unreferenced upload sits in staging
// before it is reaped. The launch path promotes within seconds of the
// upload completing; a generous TTL accommodates a user closing the modal
// without a leak.
const uploadStagingTTL = 1 * time.Hour

// stagingStore is the run store's staging seam, or the typed reason there
// is none: no run store, or a store that stages no uploads.
func (s *Server) stagingStore() (store.StagingStore, error) {
	if s.runs == nil {
		return nil, errors.New("uploads disabled: run store not configured")
	}
	st := store.AsStagingStore(s.runs.RunStore())
	if st == nil {
		return nil, errors.New("uploads disabled: the run store stages no uploads")
	}
	return st, nil
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
	stager, err := s.stagingStore()
	if err != nil {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "%v", err)
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

	// Sanitise filename: strip any path component the browser tucked
	// in, refuse traversal attempts, control bytes (incl. NUL — Windows
	// stream-name truncation, log smuggling), and invalid UTF-8 (which
	// later JSON-marshals to the manifest unescaped).
	filename := filepath.Base(hdr.Filename)
	if !validUploadFilename(filename) {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid filename")
		return
	}

	// One pass over the part measures it — the size, the SHA-256 and the
	// sniffed type, never the declared one — and the stager reads it again
	// from the start: the part is seekable, spooled by the multipart parser.
	written, sniffed, sha256Hex, err := measureUpload(file, s.cfg.MaxUploadSize)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "read upload: %v", err)
		return
	}
	if written > s.cfg.MaxUploadSize {
		s.httpErrorFor(w, r, http.StatusRequestEntityTooLarge,
			"file exceeds max_upload_size (%d bytes)", s.cfg.MaxUploadSize)
		return
	}
	if written == 0 {
		s.httpErrorFor(w, r, http.StatusBadRequest, "empty upload")
		return
	}
	if !mimeAllowed(sniffed, s.cfg.AllowedUploadMIMEs) {
		s.httpErrorFor(w, r, http.StatusUnsupportedMediaType,
			"file type %q not in allowlist; declared was %q", sniffed, declaredMIME)
		return
	}

	uploadID, err := newUploadID()
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "generate upload id: %v", err)
		return
	}
	rec := stagedUpload{
		UploadID:         uploadID,
		OriginalFilename: filename,
		MIME:             sniffed,
		Size:             written,
		SHA256:           sha256Hex,
		CreatedAt:        time.Now().UTC(),
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "rewind upload: %v", err)
		return
	}
	if err := stager.StageUpload(r.Context(), rec, io.LimitReader(file, written)); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "stage upload: %v", err)
		return
	}

	s.writeJSONFor(w, r, rec)
}

// measureUpload reads the part once: the byte count (bounded at max+1, so
// an oversize part is known without being read whole), the SHA-256, and
// the content type sniffed from the first 512 bytes.
func measureUpload(part io.Reader, max int64) (size int64, ctype, sha256Hex string, err error) {
	h := sha256.New()
	head := make([]byte, 0, 512)
	limited := io.LimitReader(part, max+1)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := limited.Read(buf)
		if n > 0 {
			size += int64(n)
			h.Write(buf[:n])
			if room := 512 - len(head); room > 0 {
				head = append(head, buf[:min(n, room)]...)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return 0, "", "", rerr
		}
	}
	return size, http.DetectContentType(head), hex.EncodeToString(h.Sum(nil)), nil
}

// promoteStaged moves uploads from staging into a run-scoped
// destination by calling RunStore.WriteAttachment for each. Returns
// the populated map[name]AttachmentRecord and the cumulative bytes.
//
// Validation against the workflow's IR (declared names, accept_mime,
// required) is the caller's responsibility. This helper is concerned
// only with the wire format and the staging seam.
func (s *Server) promoteStaged(ctx context.Context, runID string, mapping map[string]string) (map[string]store.AttachmentRecord, int64, error) {
	if len(mapping) == 0 {
		return nil, 0, nil
	}
	if len(mapping) > s.cfg.MaxUploadsPerRun {
		return nil, 0, fmt.Errorf("too many attachments: %d > %d", len(mapping), s.cfg.MaxUploadsPerRun)
	}
	stager, err := s.stagingStore()
	if err != nil {
		return nil, 0, err
	}
	out := make(map[string]store.AttachmentRecord, len(mapping))
	written := make([]string, 0, len(mapping))  // names persisted, used for rollback
	promoted := make([]string, 0, len(mapping)) // upload ids to discard only on full success
	var cumulative int64

	// rollback removes every attachment we persisted before the error so
	// the run doesn't end up with a half-populated Attachments map after
	// the Launch handler returns 400. Best-effort; logs are emitted by
	// the store on partial failure.
	rollback := func() {
		for _, name := range written {
			_ = s.runs.RemoveAttachment(ctx, runID, name)
		}
	}

	for name, uploadID := range mapping {
		if err := store.SanitizePathComponent("attachment_name", name); err != nil {
			rollback()
			return nil, 0, fmt.Errorf("invalid attachment name %q: %w", name, err)
		}
		if err := store.SanitizePathComponent("upload_id", uploadID); err != nil {
			rollback()
			return nil, 0, fmt.Errorf("invalid upload id %q: %w", uploadID, err)
		}
		meta, body, err := stager.OpenStagedUpload(ctx, uploadID)
		if err != nil {
			rollback()
			if errors.Is(err, store.ErrUploadNotFound) {
				return nil, 0, fmt.Errorf("upload %q not found", uploadID)
			}
			return nil, 0, fmt.Errorf("open upload %q: %w", uploadID, err)
		}
		cumulative += meta.Size
		if cumulative > s.cfg.MaxTotalUploadSize {
			body.Close()
			rollback()
			return nil, 0, fmt.Errorf("cumulative upload size exceeds max_total_upload_size (%d bytes)", s.cfg.MaxTotalUploadSize)
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
			rollback()
			return nil, 0, fmt.Errorf("persist attachment %q: %w", name, err)
		}
		body.Close()
		out[name] = rec
		written = append(written, name)
		promoted = append(promoted, uploadID)
	}
	// All attachments persisted — only now is it safe to clear staging.
	for _, id := range promoted {
		if err := stager.DiscardStagedUpload(ctx, id); err != nil && s.logger != nil {
			s.logger.Warn("discard staged upload %s after its promotion: %v", id, err)
		}
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
// allowlist that gates the rest of the studio API.
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
	// Verifying both preserves the studio's UX (no signature in
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
	// RFC 6266: quote the filename parameter. mime.FormatMediaType
	// handles quoting + escaping for us; a filename with spaces or
	// semicolons would otherwise be parsed as multiple disposition
	// parameters by strict consumers.
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("inline", map[string]string{"filename": rec.OriginalFilename}),
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
		// Media. Human gates collect these directly from the operator
		// (a soundtrack handed to a video-editing workflow, a screen
		// recording attached to a bug report), which was impossible
		// while the allowlist stopped at images: the upload was
		// rejected and the operator had to go find the right folder on
		// disk by hand — the exact chore the gate upload removes.
		// Wildcarded because the audio/video container zoo (mpeg, mp4,
		// ogg, wav, webm, quicktime, x-m4a…) is not worth enumerating,
		// and MIME here is SNIFFED server-side, never trusted from the
		// client.
		"audio/*", "video/*",
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

// validUploadFilename returns true when filename is acceptable as the
// on-disk component of a staged upload. The browser already submitted
// it via multipart so filepath.Base has stripped any path prefix; the
// remaining concerns are:
//
//   - reserved navigation names ("", ".", "..") — confuse downstream
//     readers and the manifest layer
//   - embedded separators (/ or \) that survived Base on cross-platform
//     edge cases (Windows uploads with a Unix server)
//   - control bytes including NUL — log-smuggling, manifest-JSON
//     corruption, and on Windows alternate-stream truncation
//   - invalid UTF-8 — the manifest is JSON, and unescaped bad bytes
//     in a string field corrupt the file
//
// The size cap (255 bytes) matches what most filesystems will accept
// as a single path component on disk.
func validUploadFilename(filename string) bool {
	if filename == "" || filename == "." || filename == ".." {
		return false
	}
	if len(filename) > 255 {
		return false
	}
	if strings.ContainsAny(filename, `/\`) {
		return false
	}
	if !utf8.ValidString(filename) {
		return false
	}
	for _, r := range filename {
		// All C0 controls (incl. NUL) and DEL.
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// newUploadID generates a URL-safe upload identifier of the form
// `up_<unix_seconds>_<32 hex random>`: opaque to clients, and with 128
// bits of randomness no one guesses another tenant's staged upload — the
// tenant-scoped record is the guard, the entropy is the floor beneath it.
func newUploadID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("up_%d_%s", time.Now().Unix(), hex.EncodeToString(buf)), nil
}

// reapStagedUploads sweeps abandoned uploads through the staging seam. A
// store that stages nothing has nothing to sweep; a sweep that fails is
// said at warn level, never dropped in silence. Called periodically by
// runStagedUploadReaper.
func (s *Server) reapStagedUploads(ctx context.Context) {
	stager, err := s.stagingStore()
	if err != nil {
		return
	}
	if _, err := stager.ReapStagedUploads(ctx, time.Now().Add(-uploadStagingTTL)); err != nil && s.logger != nil {
		s.logger.Warn("reap staged uploads: %v", err)
	}
}

// uploadReaperInterval governs how often the staged-upload reaper
// scans the staging root. Shorter = lower max disk usage; longer =
// less filesystem churn. The TTL itself (uploadStagingTTL) bounds the
// staleness window, so the interval just trades off how quickly we
// reclaim expired dirs.
const uploadReaperInterval = 10 * time.Minute

// runStagedUploadReaper drives reapStagedUploads on a fixed interval
// until the server shuts down. Spawned as a goroutine from
// ListenAndServe.
func (s *Server) runStagedUploadReaper(ctx context.Context) {
	// Fire once on startup so a freshly restarted server reclaims dirs
	// older than the TTL without waiting a full interval.
	s.reapStagedUploads(ctx)
	t := time.NewTicker(uploadReaperInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reapStagedUploads(ctx)
		}
	}
}
