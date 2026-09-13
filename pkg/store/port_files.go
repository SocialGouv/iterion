package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
)

// PortFilesStore extends the run-files seam with immutable, per-invocation
// capture. It never marks an output published; only SavePortExecution does.
// Unlike UploadRunFiles, this operation does not sweep a shared scratch tree.
type PortFilesStore interface {
	PutPortFile(ctx context.Context, ref PortFileRef, content io.Reader) error
}

var ErrPortFileCorrupt = errors.New("store: native file content is missing or corrupt")

// PublishedPortFileRefs is the only authority for native file visibility.
// PutPortFile may have persisted bytes before an interrupted checkpoint CAS;
// those bytes remain private until a successful publication references them.
func PublishedPortFileRefs(ctx context.Context, s RunStore, runID string) (map[string]PortFileRef, error) {
	run, err := s.LoadRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	refs := map[string]PortFileRef{}
	if run.PortExecution == nil {
		return refs, nil
	}
	for _, value := range run.PortExecution.Publications {
		for _, ref := range value.Files {
			if ref.RunID == runID {
				refs[ref.Path] = ref
			}
		}
	}
	return refs, nil
}

func AsPortFilesStore(s RunStore) PortFilesStore {
	publisher, _ := s.(PortFilesStore)
	return publisher
}

func PortFilePath(producer string, attempt int, digest string) (string, error) {
	if err := SanitizePathComponent("file producer", producer); err != nil {
		return "", err
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != digest || attempt < 0 {
		return "", fmt.Errorf("store: invalid native file digest or attempt")
	}
	return "published/" + producer + "/" + strconv.Itoa(attempt) + "/" + digest, nil
}

func ValidatePortFileRef(ref PortFileRef) error {
	if err := ValidateRunID(ref.RunID); err != nil {
		return err
	}
	if !IsNativeRunID(ref.RunID) {
		return ErrRunSemantics
	}
	path, err := PortFilePath(ref.Producer, ref.Attempt, ref.SHA256)
	if err != nil || ref.Path != path || ref.Size < 0 || ref.Size == math.MaxInt64 {
		return fmt.Errorf("store: invalid native file reference")
	}
	return nil
}

type portContextReader struct {
	ctx context.Context
	io.Reader
}

func (r portContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

// VerifyPortFile checks the actual bytes, independently of supplied metadata.
// The extra byte distinguishes the declared size from a truncated view of a
// larger body. Callers close their reader, including on validation failure.
func VerifyPortFile(ctx context.Context, ref PortFileRef, body io.Reader) error {
	if err := ValidatePortFileRef(ref); err != nil {
		return err
	}
	if body == nil {
		return fmt.Errorf("store: native file body is missing: %w", ErrPortFileCorrupt)
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(portContextReader{ctx, body}, ref.Size+1))
	if err != nil {
		return err
	}
	if n != ref.Size || hex.EncodeToString(hash.Sum(nil)) != ref.SHA256 {
		return fmt.Errorf("store: native file %s has unexpected size or content: %w", ref.Path, ErrPortFileCorrupt)
	}
	return nil
}

// PortFileSnapshot is an owned temporary snapshot, never the producer's
// mutable source file. Close deletes precisely this temporary file.
type PortFileSnapshot struct {
	*os.File
}

func (s *PortFileSnapshot) Close() error {
	return errors.Join(s.File.Close(), os.Remove(s.File.Name()))
}

// PreparePortFile snapshots and verifies content before a backend can expose
// a key that may already name a committed result. Private staging belongs to
// the run's native namespace; it is never a sandbox output directory.
func PreparePortFile(ctx context.Context, stageDir string, ref PortFileRef, content io.Reader) (*PortFileSnapshot, error) {
	if err := ValidatePortFileRef(ref); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if content == nil {
		return nil, fmt.Errorf("store: native file body is missing")
	}
	if err := os.MkdirAll(stageDir, dirPerm); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(stageDir, ".port-snapshot-*")
	if err != nil {
		return nil, err
	}
	snapshot := &PortFileSnapshot{File: f}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = snapshot.Close()
		}
	}()
	// Tee into a private file while validating, with no payload-sized memory
	// buffer. No caller-owned reader is used by the backend after this point.
	if err := VerifyPortFile(ctx, ref, io.TeeReader(content, f)); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	succeeded = true
	return snapshot, nil
}

func (s *FilesystemRunStore) portFilesDir(id string) string {
	return filepath.Join(s.runDir(id), "port_files")
}

func (s *FilesystemRunStore) PutPortFile(ctx context.Context, ref PortFileRef, content io.Reader) error {
	if err := s.guardNativeRun(ref.RunID); err != nil {
		return err
	}
	snapshot, err := PreparePortFile(ctx, filepath.Join(s.runDir(ref.RunID), ".port-staging"), ref, content)
	if err != nil {
		return err
	}
	defer func() { _ = snapshot.Close() }()
	root := s.portFilesDir(ref.RunID)
	if err := os.MkdirAll(root, dirPerm); err != nil {
		return err
	}
	confined, err := os.OpenRoot(s.runDir(ref.RunID))
	if err != nil {
		return err
	}
	defer func() { _ = confined.Close() }()
	rel := filepath.Join("port_files", filepath.FromSlash(ref.Path))
	if err := confined.MkdirAll(filepath.Dir(rel), dirPerm); err != nil {
		return err
	}
	staged, err := filepath.Rel(s.runDir(ref.RunID), snapshot.Name())
	if err != nil {
		return err
	}
	if err := confined.Link(staged, rel); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		body, _, err := s.openNativeCapturedFile(ref.RunID, ref.Path)
		if err != nil {
			return err
		}
		verifyErr := VerifyPortFile(ctx, ref, body)
		if err := errors.Join(verifyErr, body.Close()); err != nil {
			return err
		}
	}
	// Flush the link and every newly created directory edge up to the
	// already-durable run directory, not just the final leaf directory.
	for dir := filepath.Dir(rel); ; dir = filepath.Dir(dir) {
		f, err := confined.Open(dir)
		if err != nil {
			return err
		}
		if err := errors.Join(f.Sync(), f.Close()); err != nil {
			return err
		}
		if dir == "." {
			break
		}
	}
	return nil
}
