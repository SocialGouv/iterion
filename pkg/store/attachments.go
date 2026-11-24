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

// The record sidecar sits next to each attachment (under `.iterion/`, out
// of the bytes' namespace — see staging.go) so DeleteRunAttachments is one
// rm -rf and ListAttachments is one directory scan.

// HMAC signing key for presigned attachment URLs, generated lazily and
// cached at the store root. Survives process restarts so URLs minted
// before a reboot remain valid until their `exp` lapses.
const attachmentSigningKeyFile = ".attachment-signing-key"

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
	// Every caller-supplied field that can fail JSON encoding is persisted;
	// reject it before draining a reader or replacing an existing body.
	if _, err := json.Marshal(rec); err != nil {
		return fmt.Errorf("store: marshal attachment meta: %w", err)
	}
	hasher := sha256.New()
	bound, staged, written, err := s.stageRunMetadataBody(ctx, runID, body, hasher)
	if err != nil {
		return fmt.Errorf("store: stage attachment: %w", err)
	}
	defer staged.Close()
	r, unlock, err := s.lockRunMetadataMutation(bound, runID)
	if err != nil {
		return err
	}
	defer unlock()
	dir, err := s.openRunPayloadDirectory(bound, runID, true, "attachments", rec.Name)
	if err != nil {
		return err
	}
	defer dir.Close()
	storageRef, err := filepath.Rel(s.root, filepath.Join(dir.parent.key, filepath.Base(filename)))
	if err != nil {
		return err
	}
	payload, err := dir.file(filepath.Base(filename))
	if err != nil {
		return err
	}
	if err = staged.publish(bound, payload, filePerm); err != nil {
		return fmt.Errorf("store: publish attachment: %w", err)
	}

	rec.OriginalFilename = filepath.Base(filename)
	rec.Size = written
	rec.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	rec.StorageRef = filepath.ToSlash(storageRef)

	// Persist meta sidecar (used by Open/List even if Run.Attachments
	// is corrupted).
	metaData, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal attachment meta: %w", err)
	}
	if err := writeRecordDirectory(bound, dir, metaData); err != nil {
		// No residue: the bytes were renamed in place before the record,
		// so a failed record would strand them on disk — a bytes file
		// named exactly `.iterion` blocks the record's directory with a
		// plain file (staging.go), which a later listing then trips over.
		_ = dir.Remove(context.WithoutCancel(bound), filepath.Base(filename))
		return err
	}

	// The sidecar remains the historical authority for a legacy orphan.
	// Existing parents share the same stable lock with body publication.
	if r == nil {
		return nil
	}
	if r.Attachments == nil {
		r.Attachments = make(map[string]AttachmentRecord)
	}
	r.Attachments[rec.Name] = rec
	return s.writeRunLockedContext(bound, r, false, nil)
}

// OpenAttachment admits and opens metadata plus bytes under the stable run
// lock. The returned descriptor pins the original inode; transfer happens
// after unlocking. The caller must Close the reader.
func (s *FilesystemRunStore) OpenAttachment(ctx context.Context, runID, name string) (io.ReadCloser, AttachmentRecord, error) {
	_, unlock, err := s.lockAttachmentRead(ctx, runID)
	if err != nil {
		return nil, AttachmentRecord{}, err
	}
	defer unlock()
	if err := sanitizePathComponent("attachment name", name); err != nil {
		return nil, AttachmentRecord{}, err
	}
	dir, err := s.openRunPayloadDirectory(ctx, runID, false, "attachments", name)
	if err != nil {
		return nil, AttachmentRecord{}, err
	}
	defer dir.Close()
	rec, err := loadAttachmentMetaDirectory(ctx, dir)
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
	payload, err := dir.file(rec.OriginalFilename)
	if err != nil {
		return nil, rec, err
	}
	f, err := payload.Open()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, rec, ErrAttachmentNotFound
		}
		return nil, rec, fmt.Errorf("store: open attachment: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = f.Close()
		return nil, AttachmentRecord{}, err
	}
	return f, rec, nil
}

// ListAttachments enumerates the attachments persisted for a run.
// Returns a nil slice when no attachments directory exists.
func (s *FilesystemRunStore) ListAttachments(ctx context.Context, runID string) ([]AttachmentRecord, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	_, unlock, err := s.lockAttachmentRead(ctx, runID)
	if errors.Is(err, ErrAttachmentNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer unlock()
	root, err := s.openRunPayloadDirectory(ctx, runID, false, "attachments")
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := root.ReadDir()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("store: list attachments: %w", err)
	}
	out := make([]AttachmentRecord, 0, len(entries))
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !e.IsDir() {
			continue
		}
		child, err := root.OpenDirectory(ctx, e.Name(), false)
		if err != nil {
			return nil, err
		}
		rec, err := loadAttachmentMetaDirectory(ctx, child)
		closeErr := child.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			if errors.Is(err, ErrAttachmentNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, rec)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
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
	r, unlock, err := s.lockRunMetadataMutation(ctx, runID)
	if err != nil {
		return err
	}
	defer unlock()
	dir, err := s.openRunPayloadPath(ctx, runID, false, "attachments", name)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.removeAll(ctx); err != nil {
		return fmt.Errorf("store: rm attachment dir: %w", err)
	}
	if r == nil {
		return nil
	}
	if r.Attachments == nil {
		return nil
	}
	if _, ok := r.Attachments[name]; !ok {
		return nil
	}
	delete(r.Attachments, name)
	return s.writeRunLockedContext(ctx, r, false, nil)
}

// DeleteRunAttachments removes every attachment under the run directory
// and clears Run.Attachments. Safe to call on runs without attachments.
func (s *FilesystemRunStore) DeleteRunAttachments(ctx context.Context, runID string) error {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return err
	}
	r, unlock, err := s.lockRunMetadataMutation(ctx, runID)
	if err != nil {
		return err
	}
	defer unlock()
	root, err := s.openRunPayloadFile(ctx, runID, "attachments", false)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.removeAll(ctx); err != nil {
		return fmt.Errorf("store: rm attachments dir: %w", err)
	}
	if r == nil {
		return nil
	}
	if r.Attachments == nil {
		return nil
	}
	r.Attachments = nil
	return s.writeRunLockedContext(ctx, r, false, nil)
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
// Exact URLs additionally carry vf=2, authenticated as a suffix of the MAC
// plaintext. Legacy URLs keep their historical shape and bind format one.
// Cloud (S3-backed) stores override this with PresignGetObject; the
// HMAC scheme is local-only.
func (s *FilesystemRunStore) PresignAttachment(ctx context.Context, runID, name string, ttl time.Duration) (string, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return "", err
	}
	if err := sanitizePathComponent("attachment name", name); err != nil {
		return "", err
	}
	format, unlock, err := s.lockAttachmentRead(ctx, runID)
	// Historically URLs may be minted before bytes exist, including after
	// deletion. Preserve that contract, retaining the original format intent.
	if err != nil && !errors.Is(err, ErrAttachmentNotFound) {
		return "", err
	}
	if unlock != nil {
		unlock()
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	exp := time.Now().Add(ttl).Unix()
	key, err := s.presignKey()
	if err != nil {
		return "", fmt.Errorf("presign attachment: %w", err)
	}
	sig := attachmentSignature(key, runID, name, exp, format)
	q := url.Values{}
	q.Set("exp", strconv.FormatInt(exp, 10))
	q.Set("sig", sig)
	if format == ExactValueFormat {
		q.Set("vf", "2")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
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
	return s.verifyAttachmentSignature(runID, name, exp, sig, LegacyValueFormat)
}

func (s *FilesystemRunStore) verifyAttachmentSignature(runID, name, exp, sig string, format ValueFormat) bool {
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
	want := attachmentSignature(key, runID, name, expN, format)
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	wantBytes, _ := hex.DecodeString(want)
	return hmac.Equal(got, wantBytes)
}

// attachmentSignature preserves the legacy plaintext byte-for-byte. The exact
// domain includes the value format; stripping vf cannot produce a legacy MAC.
func attachmentSignature(key []byte, runID, name string, exp int64, format ValueFormat) string {
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s\n%s\n%d", runID, name, exp)
	if format == ExactValueFormat {
		fmt.Fprint(mac, "\nvalue-format:2")
	}
	return hex.EncodeToString(mac.Sum(nil))
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
