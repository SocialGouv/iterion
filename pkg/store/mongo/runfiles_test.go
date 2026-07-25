package mongo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestRunFiles_ScratchToS3Bridge models the two-pod cloud reality without
// a live Mongo: run-file methods touch only the blob backend + the local
// scratch dir, so a bare Store (no Mongo collections wired) exercises the
// full seam.
//
// A "runner" store owns a scratch dir + shared blob; a "server" store has
// NO scratch but the SAME blob. Tools write into the runner's scratch,
// the runner uploads to S3, and the server — which never saw the runner's
// disk — lists + opens the files from S3. This is exactly how a finished
// cloud run's Artifacts panel is served.
func TestRunFiles_ScratchToS3Bridge(t *testing.T) {
	ctx := context.Background()
	b := newInMemoryBlob()
	runner := &Store{blob: b, runFilesScratch: t.TempDir()}
	server := &Store{blob: b} // no scratch — pure S3 reader

	const runID = "run_bridge"
	dir, err := runner.EnsureRunFilesDir(ctx, runID)
	if err != nil {
		t.Fatalf("EnsureRunFilesDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("# hi\n"), 0o644); err != nil {
		t.Fatalf("write report.md: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sbom"), 0o755); err != nil {
		t.Fatalf("mkdir sbom: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sbom", "deps.json"), []byte(`{"n":1}`), 0o644); err != nil {
		t.Fatalf("write deps.json: %v", err)
	}

	// Before the upload, the server sees nothing.
	if files, err := server.ListRunFiles(ctx, runID); err != nil || len(files) != 0 {
		t.Fatalf("server ListRunFiles(pre-upload) = %v, %v; want empty", files, err)
	}

	n, err := runner.UploadRunFiles(ctx, runID)
	if err != nil {
		t.Fatalf("UploadRunFiles: %v", err)
	}
	if n != 2 {
		t.Fatalf("UploadRunFiles count = %d; want 2", n)
	}

	// A clean upload drops the redundant runner-local scratch dir so it
	// can't accumulate on a long-lived runner pod (DeleteRun's scratch
	// sweep runs on the SERVER store, never here).
	if _, err := os.Stat(runner.runFilesScratchDir(runID)); !os.IsNotExist(err) {
		t.Errorf("scratch dir after successful upload: Stat err = %v; want IsNotExist", err)
	}

	// Server now serves both files from S3, sorted.
	files, err := server.ListRunFiles(ctx, runID)
	if err != nil {
		t.Fatalf("server ListRunFiles: %v", err)
	}
	if len(files) != 2 || files[0].Path != "report.md" || files[1].Path != "sbom/deps.json" {
		t.Fatalf("server ListRunFiles = %+v; want report.md, sbom/deps.json", files)
	}
	rc, info, err := server.OpenRunFile(ctx, runID, "sbom/deps.json")
	if err != nil {
		t.Fatalf("server OpenRunFile: %v", err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != `{"n":1}` || info.Path != "sbom/deps.json" {
		t.Fatalf("server OpenRunFile = %q / %q; want {\"n\":1} / sbom/deps.json", body, info.Path)
	}

	// Empty upload (no scratch dir) is a clean (0, nil).
	if got, err := runner.UploadRunFiles(ctx, "never_ran"); err != nil || got != 0 {
		t.Errorf("UploadRunFiles(no scratch) = %d, %v; want 0, nil", got, err)
	}

	// Traversal is rejected at the blob key layer.
	if _, _, err := server.OpenRunFile(ctx, runID, "../escape"); err == nil {
		t.Errorf("OpenRunFile(../escape): expected rejection")
	}

	// DeleteRun (via the blob sweep + scratch removal) clears everything.
	if err := b.DeleteRunFiles(ctx, runID); err != nil {
		t.Fatalf("DeleteRunFiles: %v", err)
	}
	if err := os.RemoveAll(runner.runFilesScratchDir(runID)); err != nil {
		t.Fatalf("remove scratch: %v", err)
	}
	if files, err := server.ListRunFiles(ctx, runID); err != nil || len(files) != 0 {
		t.Errorf("server ListRunFiles(post-delete) = %v, %v; want empty", files, err)
	}
}

// TestUploadRunFiles_StreamsPastAttachmentCap proves artifact files are not
// governed by the buffered-attachment cap. Review videos commonly exceed that
// cap; they must be streamed durably before the scratch tree is removed.
func TestUploadRunFiles_StreamsPastAttachmentCap(t *testing.T) {
	ctx := context.Background()
	b := newInMemoryBlob()
	runner := &Store{blob: b, runFilesScratch: t.TempDir(), maxAttachmentBytes: 8}

	const runID = "run_cap"
	dir, err := runner.EnsureRunFilesDir(ctx, runID)
	if err != nil {
		t.Fatalf("EnsureRunFilesDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "small.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write small.md: %v", err)
	}
	largeBody := make([]byte, 64)
	for i := range largeBody {
		largeBody[i] = byte(i)
	}
	if err := os.WriteFile(filepath.Join(dir, "review.mp4"), largeBody, 0o644); err != nil {
		t.Fatalf("write review.mp4: %v", err)
	}

	n, err := runner.UploadRunFiles(ctx, runID)
	if err != nil {
		t.Fatalf("UploadRunFiles: %v", err)
	}
	if n != 2 {
		t.Errorf("UploadRunFiles count = %d; want 2", n)
	}
	server := &Store{blob: b}
	files, err := server.ListRunFiles(ctx, runID)
	if err != nil {
		t.Fatalf("ListRunFiles: %v", err)
	}
	if len(files) != 2 || files[0].Path != "review.mp4" || files[0].Size != int64(len(largeBody)) || files[1].Path != "small.md" {
		t.Errorf("ListRunFiles = %+v; want review.mp4 + small.md", files)
	}
	rc, _, err := server.OpenRunFile(ctx, runID, "review.mp4")
	if err != nil {
		t.Fatalf("OpenRunFile(review.mp4): %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read review.mp4: %v", err)
	}
	if !bytes.Equal(got, largeBody) {
		t.Fatalf("review.mp4 body mismatch: got %d bytes", len(got))
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("scratch dir should be removed after durable upload, stat err = %v", err)
	}
}

type failingRunFileBlob struct {
	*inMemoryBlob
}

var errInjectedRunFilePut = errors.New("injected put failure")

func (b *failingRunFileBlob) PutRunFile(context.Context, string, string, string, io.Reader, int64) error {
	return errInjectedRunFilePut
}

// A transient blob failure must retain the local bytes for the runner's
// post-return retry. In particular, a review checkpoint may already reference
// this path, so deleting the only copy would turn a temporary outage into a
// permanent broken attachment.
func TestUploadRunFiles_FailureKeepsScratch(t *testing.T) {
	ctx := context.Background()
	base := newInMemoryBlob()
	runner := &Store{
		blob:            &failingRunFileBlob{inMemoryBlob: base},
		runFilesScratch: t.TempDir(),
	}

	const runID = "run_retry"
	dir, err := runner.EnsureRunFilesDir(ctx, runID)
	if err != nil {
		t.Fatalf("EnsureRunFilesDir: %v", err)
	}
	want := []byte("video bytes")
	path := filepath.Join(dir, "review.mp4")
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatalf("write review.mp4: %v", err)
	}

	n, err := runner.UploadRunFiles(ctx, runID)
	if !errors.Is(err, errInjectedRunFilePut) {
		t.Fatalf("UploadRunFiles error = %v, want injected put failure", err)
	}
	if n != 0 {
		t.Fatalf("UploadRunFiles count = %d, want 0", n)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("scratch review.mp4 should survive failed upload: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("scratch review.mp4 changed: got %q", got)
	}
}
