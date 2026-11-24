package store

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #2863 — the record sidecar lives under `.iterion/`, out of the bytes'
// namespace, so `meta.json` is an ordinary filename; records written
// before the move sit beside the bytes and every reader falls back.

// The new layout: bytes beside the record's dotted directory, never the
// record beside the bytes.
func TestTheRecordSitsOutOfTheBytesNamespace(t *testing.T) {
	ctx := context.Background()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run-layout"
	if _, err := s.CreateRun(ctx, runID, "demo", nil); err != nil {
		t.Fatal(err)
	}
	rec := AttachmentRecord{Name: "spec", OriginalFilename: "meta.json", MIME: "application/json"}
	if err := s.WriteAttachment(ctx, runID, rec, strings.NewReader(`{"user":"payload"}`)); err != nil {
		t.Fatalf("a file named meta.json is refused: %v", err)
	}
	dir := filepath.Join(s.root, "runs", runID, "attachments", "spec")
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
		t.Fatalf("the bytes are not at the uploaded name: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".iterion", "meta.json")); err != nil {
		t.Fatalf("the record is not under .iterion/: %v", err)
	}
	rc, got, err := s.OpenAttachment(ctx, runID, "spec")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != `{"user":"payload"}` || got.OriginalFilename != "meta.json" {
		t.Fatalf("read back %q / %q, want the payload under its own name", data, got.OriginalFilename)
	}
}

// A run written before the move holds its record beside the bytes; the
// reader falls back to it.
func TestALegacyAttachmentRecordIsStillRead(t *testing.T) {
	ctx := context.Background()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run-legacy"
	if _, err := s.CreateRun(ctx, runID, "demo", nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.root, "runs", runID, "attachments", "spec")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "spec.md"), []byte("# spec"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := AttachmentRecord{Name: "spec", OriginalFilename: "spec.md", MIME: "text/markdown", Size: 6, SHA256: "x", CreatedAt: time.Now().UTC()}
	meta, _ := json.Marshal(rec)
	// The legacy path, spelled out rather than through legacyRecordPath:
	// this test must still compile (and pass) against the pre-#2863 code,
	// where the legacy layout is the only one.
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListAttachments(ctx, runID)
	if err != nil || len(list) != 1 || list[0].Name != "spec" {
		t.Fatalf("ListAttachments = %+v, %v; want the legacy record", list, err)
	}
	rc, got, err := s.OpenAttachment(ctx, runID, "spec")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "# spec" || got.OriginalFilename != "spec.md" {
		t.Fatalf("read back %q / %q through the fallback", data, got.OriginalFilename)
	}
}

// Same fallback for a staged upload whose record predates the move, and
// the reaper still reads the legacy record's clock.
func TestALegacyStagedUploadRecordIsStillRead(t *testing.T) {
	ctx := context.Background()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.root, UploadStagingSubdir, "up_legacy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), mustJSON(t, StagedUpload{
		UploadID: "up_legacy", OriginalFilename: "old.txt", MIME: "text/plain", Size: 3, SHA256: "o",
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, rc, err := s.OpenStagedUpload(ctx, "up_legacy")
	if err != nil {
		t.Fatalf("open the legacy staged upload: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if rec.OriginalFilename != "old.txt" || string(data) != "old" {
		t.Fatalf("read back %q / %q through the fallback", rec.OriginalFilename, data)
	}
	if n, err := s.ReapStagedUploads(ctx, time.Now().Add(-time.Hour)); err != nil || n != 1 {
		t.Fatalf("reap by the legacy record's clock = %d, %v; want 1, nil", n, err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Pre-#2863, `.iterion` was a LEGAL filename — only meta.json was
// reserved: the record sits at the legacy path beside bytes named
// exactly `.iterion`, which the new record path now reads as ENOTDIR.
// The reader must fall back to the legacy record — the only place it can
// be — rather than lose an intact attachment.
func TestALegacyAttachmentNamedDotIterionIsStillRead(t *testing.T) {
	ctx := context.Background()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run-legacy-dotiterion"
	if _, err := s.CreateRun(ctx, runID, "demo", nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.root, "runs", runID, "attachments", "conf")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".iterion"), []byte("conf-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := AttachmentRecord{Name: "conf", OriginalFilename: ".iterion", MIME: "application/octet-stream", Size: 10, SHA256: "x", CreatedAt: time.Now().UTC()}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), mustJSON(t, rec), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListAttachments(ctx, runID)
	if err != nil || len(list) != 1 || list[0].Name != "conf" {
		t.Fatalf("ListAttachments = %+v, %v; want the legacy .iterion attachment", list, err)
	}
	rc, got, err := s.OpenAttachment(ctx, runID, "conf")
	if err != nil {
		t.Fatalf("open the legacy .iterion attachment: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "conf-bytes" || got.OriginalFilename != ".iterion" {
		t.Fatalf("read back %q / %q through the fallback", data, got.OriginalFilename)
	}
}

// Same pre-move shape on a staged upload: bytes `.iterion`, legacy
// record beside them. OpenStagedUpload must read it — the upload stays
// promotable.
func TestALegacyStagedUploadNamedDotIterionIsStillRead(t *testing.T) {
	ctx := context.Background()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.root, UploadStagingSubdir, "up_dotiterion")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), mustJSON(t, StagedUpload{
		UploadID: "up_dotiterion", OriginalFilename: ".iterion", MIME: "application/octet-stream", Size: 7, SHA256: "x",
		CreatedAt: time.Now().UTC(),
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".iterion"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, rc, err := s.OpenStagedUpload(ctx, "up_dotiterion")
	if err != nil {
		t.Fatalf("open the legacy .iterion staged upload: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if rec.OriginalFilename != ".iterion" || string(data) != "payload" {
		t.Fatalf("read back %q / %q through the fallback", rec.OriginalFilename, data)
	}
}

// Without ANY record — neither layout's — a stranded `.iterion` bytes
// file still reads as not-found, on the staged side as on the
// attachment side (TestListAttachmentsSkipsIterionFileResidue).
func TestAStrandedDotIterionFileStaysNotFound(t *testing.T) {
	ctx := context.Background()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.root, UploadStagingSubdir, "up_stranded")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".iterion"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.OpenStagedUpload(ctx, "up_stranded"); err != ErrUploadNotFound {
		t.Fatalf("OpenStagedUpload on a stranded .iterion file = %v, want ErrUploadNotFound", err)
	}
}
