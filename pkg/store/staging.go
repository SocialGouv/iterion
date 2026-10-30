package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// A staged upload is an upload the server received and no run has claimed
// yet: the launch form uploads each file as it is picked, the launch names
// the ids, and the promotion moves the bytes to the run. Where the bytes
// wait is the store's business — a local store has a directory, the cloud
// store has no disk at all — so staging is a capability of the run store,
// optional like the run-files area, and the server stages through it or
// refuses typed.

// StagedUpload is the record of an upload received and not yet promoted.
// Its JSON form is the wire answer of POST /api/runs/uploads; the principal
// that staged it is stamped by a tenant-scoped store from the context and
// never leaves the store.
type StagedUpload struct {
	UploadID         string    `json:"upload_id" bson:"_id"`
	OriginalFilename string    `json:"original_filename" bson:"original_filename"`
	MIME             string    `json:"mime" bson:"mime"`
	Size             int64     `json:"size" bson:"size"`
	SHA256           string    `json:"sha256" bson:"sha256"`
	CreatedAt        time.Time `json:"created_at" bson:"created_at"`
	// TenantID and OwnerID are the tenant and the user the upload was
	// staged under. A promotion from another tenant does not find it.
	TenantID string `json:"-" bson:"tenant_id,omitempty"`
	OwnerID  string `json:"-" bson:"owner_id,omitempty"`
}

// ErrUploadNotFound: no staged upload has this id — never staged, already
// promoted, reaped, or staged by another tenant.
var ErrUploadNotFound = errors.New("store: staged upload not found")

// UploadStagingSubdir is the directory under a filesystem store's root
// where staged uploads wait: one directory per upload id, holding the
// bytes under the original filename and the record under `.iterion/`.
const UploadStagingSubdir = "uploads"

// The record sidecar lives OUT of the bytes' namespace since #2863: under
// a dotted subdirectory of the bytes' directory, where no upload can land
// — an uploaded filename is sanitised to a single path component beside
// the bytes, so `meta.json` is an ordinary filename again. (A file named
// exactly `.iterion` still collides with the record's directory; the write
// fails loudly, never reads back as the record.) Records written before
// the move sit beside the bytes as `meta.json`; every reader falls back
// to that legacy path so existing runs and staged uploads stay readable.
func recordPath(dir string) string { return filepath.Join(dir, ".iterion", "meta.json") }

func legacyRecordPath(dir string) string { return filepath.Join(dir, "meta.json") }

// writeRecord persists the record sidecar at its current location.
func writeRecord(dir string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(recordPath(dir)), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(recordPath(dir), data, perm)
}

// readRecord reads the record sidecar from its current location, falling
// back to the layout records existing before #2863 were written in.
func readRecord(dir string) ([]byte, error) {
	data, err := os.ReadFile(recordPath(dir))
	if err == nil {
		return data, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return os.ReadFile(legacyRecordPath(dir))
}

// StagingStore is the optional run-store capability that holds uploads
// between their arrival and their promotion to a run. FilesystemRunStore
// stages under <root>/uploads/; the Mongo (cloud) store stages the record
// in its staged_uploads collection and the bytes in the blob backend under
// uploads/<upload_id>/<filename>. AsStagingStore answers nil for a store
// that stages nothing — the server then refuses uploads typed.
type StagingStore interface {
	// StageUpload persists body under rec.UploadID with rec as its record.
	// rec.Size is the byte length the caller measured; a body that yields
	// a different length is refused and nothing is kept. The body is
	// SEEKABLE because a blob backend rewinds it to retry and to checksum
	// (an S3 PUT over plaintext refuses an unseekable stream outright).
	StageUpload(ctx context.Context, rec StagedUpload, body io.ReadSeeker) error
	// OpenStagedUpload returns the record and a reader on the bytes, or
	// ErrUploadNotFound. The caller closes the reader.
	OpenStagedUpload(ctx context.Context, uploadID string) (StagedUpload, io.ReadCloser, error)
	// DiscardStagedUpload removes a staged upload. Idempotent: an upload
	// that is not there is nothing to remove.
	DiscardStagedUpload(ctx context.Context, uploadID string) error
	// ReapStagedUploads removes every staged upload older than cutoff —
	// uploads a closed launch form never named — and returns how many it
	// removed. A platform sweep: it sees every tenant's uploads.
	ReapStagedUploads(ctx context.Context, cutoff time.Time) (int, error)
}

// AsStagingStore returns s as StagingStore when the backend stages
// uploads, or nil otherwise.
func AsStagingStore(s RunStore) StagingStore {
	return capability[StagingStore](s)
}

var _ StagingStore = (*FilesystemRunStore)(nil)

func (s *FilesystemRunStore) stagedUploadDir(uploadID string) (string, error) {
	if err := sanitizePathComponent("upload ID", uploadID); err != nil {
		return "", err
	}
	return filepath.Join(s.root, UploadStagingSubdir, uploadID), nil
}

// StageUpload implements StagingStore under <root>/uploads/<upload_id>/:
// the bytes under the original filename, the record beside them. A
// failure anywhere leaves no directory behind.
func (s *FilesystemRunStore) StageUpload(_ context.Context, rec StagedUpload, body io.ReadSeeker) error {
	dir, err := s.stagedUploadDir(rec.UploadID)
	if err != nil {
		return err
	}
	if err := sanitizePathComponent("upload filename", rec.OriginalFilename); err != nil {
		return err
	}
	if rec.Size <= 0 {
		return fmt.Errorf("store: stage upload %s: the record carries no size", rec.UploadID)
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("store: stage upload %s: %w", rec.UploadID, err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()
	dst, err := os.Create(filepath.Join(dir, rec.OriginalFilename))
	if err != nil {
		return fmt.Errorf("store: stage upload %s: %w", rec.UploadID, err)
	}
	// One byte past the record's size is enough to know the body is longer.
	n, err := io.Copy(dst, io.LimitReader(body, rec.Size+1))
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("store: stage upload %s: write: %w", rec.UploadID, err)
	}
	if n != rec.Size {
		return fmt.Errorf("store: stage upload %s: the body yields %d bytes, the record says %d", rec.UploadID, n, rec.Size)
	}
	meta, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("store: stage upload %s: encode record: %w", rec.UploadID, err)
	}
	if err := writeRecord(dir, meta, 0o600); err != nil {
		return fmt.Errorf("store: stage upload %s: write record: %w", rec.UploadID, err)
	}
	keep = true
	return nil
}

// OpenStagedUpload implements StagingStore.
func (s *FilesystemRunStore) OpenStagedUpload(_ context.Context, uploadID string) (StagedUpload, io.ReadCloser, error) {
	dir, err := s.stagedUploadDir(uploadID)
	if err != nil {
		return StagedUpload{}, nil, err
	}
	raw, err := readRecord(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return StagedUpload{}, nil, ErrUploadNotFound
		}
		return StagedUpload{}, nil, fmt.Errorf("store: open staged upload %s: %w", uploadID, err)
	}
	var rec StagedUpload
	if err := json.Unmarshal(raw, &rec); err != nil {
		return StagedUpload{}, nil, fmt.Errorf("store: open staged upload %s: decode record: %w", uploadID, err)
	}
	if err := sanitizePathComponent("upload filename", rec.OriginalFilename); err != nil {
		return StagedUpload{}, nil, fmt.Errorf("store: open staged upload %s: %w", uploadID, err)
	}
	f, err := os.Open(filepath.Join(dir, rec.OriginalFilename))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return StagedUpload{}, nil, ErrUploadNotFound
		}
		return StagedUpload{}, nil, fmt.Errorf("store: open staged upload %s: %w", uploadID, err)
	}
	return rec, f, nil
}

// DiscardStagedUpload implements StagingStore.
func (s *FilesystemRunStore) DiscardStagedUpload(_ context.Context, uploadID string) error {
	dir, err := s.stagedUploadDir(uploadID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("store: discard staged upload %s: %w", uploadID, err)
	}
	return nil
}

// ReapStagedUploads implements StagingStore: the record says when an
// upload was staged — the same clock the Mongo twin reaps on — and one
// older than cutoff goes. A record that cannot be read dates the upload
// by its directory instead, so an unreadable sidecar is still swept.
func (s *FilesystemRunStore) ReapStagedUploads(_ context.Context, cutoff time.Time) (int, error) {
	root := filepath.Join(s.root, UploadStagingSubdir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("store: reap staged uploads: %w", err)
	}
	reaped := 0
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		stagedAt, err := s.stagedUploadTime(filepath.Join(root, e.Name()), e)
		if err != nil {
			errs = append(errs, fmt.Errorf("stat %s: %w", e.Name(), err))
			continue
		}
		if !stagedAt.Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", e.Name(), err))
			continue
		}
		reaped++
	}
	if len(errs) > 0 {
		return reaped, fmt.Errorf("store: reap staged uploads: %w", errors.Join(errs...))
	}
	return reaped, nil
}

// stagedUploadTime is when an upload was staged: its record's CreatedAt,
// or the directory's modification time when the record is unreadable.
func (s *FilesystemRunStore) stagedUploadTime(dir string, entry fs.DirEntry) (time.Time, error) {
	if raw, err := readRecord(dir); err == nil {
		var rec StagedUpload
		if json.Unmarshal(raw, &rec) == nil && !rec.CreatedAt.IsZero() {
			return rec.CreatedAt, nil
		}
	}
	info, err := entry.Info()
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}
