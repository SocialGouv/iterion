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

// Sidecar stored next to each attachment so DeleteRunAttachments is
// one rm -rf and ListAttachments is one directory scan.
const attachmentMetaFilename = "meta.json"

// HMAC signing key for presigned attachment URLs, generated lazily and
// cached at the store root. Survives process restarts so URLs minted
// before a reboot remain valid until their `exp` lapses.
const attachmentSigningKeyFile = ".attachment-signing-key"

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
	if err := s.guardNotDeleted(runID); err != nil {
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
	// fsync before rename: matches WriteFileAtomic semantics. Without
	// this, a crash between rename landing in the directory and the
	// data blocks being flushed can surface a zero-length or partial
	// file at dstPath while meta.json records the full size + sha256.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("store: sync attachment: %w", err)
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
	r, err := s.loadRunRaw(runID)
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
	// Re-sanitise rec.OriginalFilename even though WriteAttachment
	// canonicalises it via filepath.Base: meta.json is on the same
	// filesystem as the caller, so an attacker who can write the
	// sidecar could otherwise plant `../../etc/passwd` and read
	// arbitrary host files through the open endpoint. Defence-in-depth
	// is cheap here.
	if err := sanitizePathComponent("attachment filename", rec.OriginalFilename); err != nil {
		return nil, rec, err
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

// RemoveAttachment deletes a single attachment by name from disk and
// from Run.Attachments. Used by callers performing transactional
// promotion (e.g. promoteStaged) to roll back partial writes. Safe to
// call on a name that was never persisted.
func (s *FilesystemRunStore) RemoveAttachment(ctx context.Context, runID, name string) error {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return err
	}
	if err := sanitizePathComponent("attachment name", name); err != nil {
		return err
	}
	dir := s.attachmentDir(runID, name)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("store: rm attachment dir: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.loadRunRaw(runID)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if r.Attachments == nil {
		return nil
	}
	if _, ok := r.Attachments[name]; !ok {
		return nil
	}
	delete(r.Attachments, name)
	return s.writeRun(r)
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
	r, err := s.loadRunRaw(runID)
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
	key, err := s.presignKey()
	if err != nil {
		return "", fmt.Errorf("presign attachment: %w", err)
	}
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
	// The HMAC plaintext joins runID and name with newlines; if either
	// component carries a newline (or any control char), the issuer
	// and the verifier could disagree on which delimiter is real,
	// making one valid signature match a different (runID, name) pair.
	// Reject the same shapes PresignAttachment rejects so the contract
	// matches on both sides.
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return false
	}
	if err := sanitizePathComponent("attachment name", name); err != nil {
		return false
	}
	expN, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || expN < time.Now().Unix() {
		return false
	}
	key, err := s.presignKey()
	if err != nil {
		return false
	}
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
// use, so URLs survive process restarts.
//
// If crypto/rand is unavailable (a misconfigured container without
// /dev/urandom, a kernel CSPRNG failure), presignKey returns an error
// rather than falling back to a deterministic key. A path-derived
// fallback would be persisted forever — making any third party that
// learns the store path able to forge URLs for every run in the store.
// Fail-closed is the correct posture: presigning becomes unavailable
// until the operator fixes the entropy source.
func (s *FilesystemRunStore) presignKey() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.signingKey) > 0 {
		return s.signingKey, nil
	}
	keyPath := filepath.Join(s.root, attachmentSigningKeyFile)
	if data, err := os.ReadFile(keyPath); err == nil && len(data) >= 32 {
		s.signingKey = data
		return s.signingKey, nil
	}
	buf := make([]byte, 32)
	if _, err := readRandom(buf); err != nil {
		return nil, fmt.Errorf("generate attachment signing key: %w", err)
	}
	// Atomic write so a crash between truncate and the data flush
	// doesn't leave a zero-byte file — that would silently invalidate
	// every outstanding presigned URL on the next boot.
	if err := writeFileAtomic(keyPath, buf, 0o600); err != nil {
		return nil, fmt.Errorf("persist attachment signing key: %w", err)
	}
	s.signingKey = buf
	return s.signingKey, nil
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
