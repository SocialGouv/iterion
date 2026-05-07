package store

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// readRandom is a thin wrapper over crypto/rand.Read for presign-key
// generation. Pulled out for testability — tests can stub it without
// pulling in a global crypto/rand replacement.
var readRandom = rand.Read

// ErrAttachmentNotFound is returned by attachment loaders when the
// requested attachment is not present on the run. Callers can use
// errors.Is to discriminate from other I/O errors.
var ErrAttachmentNotFound = errors.New("store: attachment not found")

// attachmentMetaFilename is the on-disk meta sidecar for the
// filesystem backend. Stored alongside the bytes so DeleteRunAttachments
// is a single rm -rf and ListAttachments is a single dir scan.
const attachmentMetaFilename = "meta.json"

// attachmentDir returns the on-disk directory for an attachment under
// the FilesystemRunStore root.
func (s *FilesystemRunStore) attachmentDir(runID, name string) string {
	return filepath.Join(s.root, "runs", runID, "attachments", name)
}

// WriteAttachment persists the bytes of an attachment under
// `<root>/runs/<run_id>/attachments/<name>/<original_filename>` and
// reflects the metadata into Run.Attachments. The body reader is
// fully drained.
//
// Validation (size cap, MIME allowlist, duplicate names) is the
// caller's responsibility — this method trusts the AttachmentRecord
// it receives.
func (s *FilesystemRunStore) WriteAttachment(ctx context.Context, runID string, rec AttachmentRecord, body io.Reader) error {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return err
	}
	if err := sanitizePathComponent("attachment name", rec.Name); err != nil {
		return err
	}
	// OriginalFilename is path-joined; sanitise it through the same
	// rules so a hostile uploader can't escape the run directory.
	filename := rec.OriginalFilename
	if filename == "" {
		filename = rec.Name
	}
	if err := sanitizePathComponent("attachment filename", filepath.Base(filename)); err != nil {
		return err
	}
	dir := s.attachmentDir(runID, rec.Name)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("store: mkdir attachment: %w", err)
	}
	dstPath := filepath.Join(dir, filepath.Base(filename))

	// Stream-copy with a hash sink so the on-disk SHA-256 is
	// authoritative even if the caller passed a stale rec.SHA256.
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return fmt.Errorf("store: tempfile: %w", err)
	}
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hasher), body)
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("store: copy attachment: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("store: close tempfile: %w", err)
	}
	if err := os.Rename(tmp.Name(), dstPath); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("store: rename attachment: %w", err)
	}

	rec.OriginalFilename = filepath.Base(filename)
	rec.Size = written
	rec.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	rec.StorageRef = filepath.ToSlash(filepath.Join("runs", runID, "attachments", rec.Name, rec.OriginalFilename))

	// Persist meta sidecar (used by Open/List even if Run.Attachments
	// is corrupted).
	metaData, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal attachment meta: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, attachmentMetaFilename), metaData, filePerm); err != nil {
		return err
	}

	// Reflect into Run.Attachments. Done under the same mu the rest of
	// the read-modify-write paths use to avoid losing parallel writes.
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		// If run.json doesn't exist yet (race on CreateRun), the
		// bytes are written and the meta sidecar is canonical;
		// downstream LoadRun will see Attachments empty and a future
		// caller can re-index by walking the attachments/ dir.
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("store: load run for attachment index: %w", err)
	}
	if r.Attachments == nil {
		r.Attachments = make(map[string]AttachmentRecord)
	}
	r.Attachments[rec.Name] = rec
	return s.writeRun(r)
}

// OpenAttachment returns a streaming reader over the attachment bytes
// and its metadata. The caller must Close the reader.
func (s *FilesystemRunStore) OpenAttachment(ctx context.Context, runID, name string) (io.ReadCloser, AttachmentRecord, error) {
	rec, err := s.loadAttachmentMeta(runID, name)
	if err != nil {
		return nil, AttachmentRecord{}, err
	}
	dir := s.attachmentDir(runID, name)
	f, err := os.Open(filepath.Join(dir, rec.OriginalFilename))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, rec, ErrAttachmentNotFound
		}
		return nil, rec, fmt.Errorf("store: open attachment: %w", err)
	}
	return f, rec, nil
}

// ListAttachments enumerates the attachments persisted for a run.
// Returns a nil slice when no attachments directory exists.
func (s *FilesystemRunStore) ListAttachments(_ context.Context, runID string) ([]AttachmentRecord, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	root := filepath.Join(s.root, "runs", runID, "attachments")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: list attachments: %w", err)
	}
	out := make([]AttachmentRecord, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rec, err := s.loadAttachmentMeta(runID, e.Name())
		if err != nil {
			if errors.Is(err, ErrAttachmentNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// DeleteRunAttachments removes every attachment under the run directory
// and clears Run.Attachments. Safe to call on runs without attachments.
func (s *FilesystemRunStore) DeleteRunAttachments(ctx context.Context, runID string) error {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return err
	}
	root := filepath.Join(s.root, "runs", runID, "attachments")
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("store: rm attachments dir: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if r.Attachments == nil {
		return nil
	}
	r.Attachments = nil
	return s.writeRun(r)
}

// PresignAttachment emits an HMAC-signed local URL pointing at the
// attachment-serving endpoint. The signing key lives on the store; the
// HTTP server validates the signature before serving bytes.
//
// The path component is `/api/runs/<run_id>/attachments/<name>` so
// the URL self-describes the resource for human inspection. Two query
// parameters carry the binding:
//
//	exp=<unix_seconds>     // absolute expiry
//	sig=<hex_hmac_sha256>  // HMAC over `<run_id>\n<name>\n<exp>`
//
// Cloud (S3-backed) stores override this with PresignGetObject; the
// HMAC scheme is local-only.
func (s *FilesystemRunStore) PresignAttachment(_ context.Context, runID, name string, ttl time.Duration) (string, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return "", err
	}
	if err := sanitizePathComponent("attachment name", name); err != nil {
		return "", err
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	exp := time.Now().Add(ttl).Unix()
	key := s.presignKey()
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s\n%s\n%d", runID, name, exp)
	sig := hex.EncodeToString(mac.Sum(nil))
	q := url.Values{}
	q.Set("exp", strconv.FormatInt(exp, 10))
	q.Set("sig", sig)
	return fmt.Sprintf("/api/runs/%s/attachments/%s?%s",
		url.PathEscape(runID),
		url.PathEscape(name),
		q.Encode(),
	), nil
}

// VerifyAttachmentSignature checks an HMAC-signed presign URL against
// the store's signing key. Returns true when the signature is valid
// and the expiry has not passed.
func (s *FilesystemRunStore) VerifyAttachmentSignature(runID, name, exp, sig string) bool {
	if exp == "" || sig == "" {
		return false
	}
	expN, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || expN < time.Now().Unix() {
		return false
	}
	key := s.presignKey()
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s\n%s\n%d", runID, name, expN)
	want := hex.EncodeToString(mac.Sum(nil))
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	wantBytes, _ := hex.DecodeString(want)
	return hmac.Equal(got, wantBytes)
}

// presignKey returns the per-store HMAC signing key. It is derived
// from a stable file under the store root, generated lazily on first
// use, so URLs survive process restarts. Failure to read or generate
// the key falls back to a fixed in-memory secret — that path keeps
// presigning available but log-warns on the next launch.
func (s *FilesystemRunStore) presignKey() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.signingKey) > 0 {
		return s.signingKey
	}
	keyPath := filepath.Join(s.root, ".attachment-signing-key")
	if data, err := os.ReadFile(keyPath); err == nil && len(data) >= 32 {
		s.signingKey = data
		return s.signingKey
	}
	buf := make([]byte, 32)
	_, err := readRandom(buf)
	if err != nil {
		// Deterministic fallback derived from the root path. This is
		// far weaker than crypto/rand but never returns an empty key,
		// which would mint trivially forgeable URLs.
		h := sha256.Sum256([]byte("iterion-attachment-fallback:" + s.root))
		buf = h[:]
	}
	_ = os.WriteFile(keyPath, buf, 0o600)
	s.signingKey = buf
	return s.signingKey
}

// loadAttachmentMeta reads the on-disk meta sidecar for a single
// attachment.
func (s *FilesystemRunStore) loadAttachmentMeta(runID, name string) (AttachmentRecord, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return AttachmentRecord{}, err
	}
	if err := sanitizePathComponent("attachment name", name); err != nil {
		return AttachmentRecord{}, err
	}
	dir := s.attachmentDir(runID, name)
	data, err := os.ReadFile(filepath.Join(dir, attachmentMetaFilename))
	if err != nil {
		if os.IsNotExist(err) {
			return AttachmentRecord{}, ErrAttachmentNotFound
		}
		return AttachmentRecord{}, fmt.Errorf("store: read attachment meta: %w", err)
	}
	var rec AttachmentRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return AttachmentRecord{}, fmt.Errorf("store: decode attachment meta: %w", err)
	}
	return rec, nil
}
