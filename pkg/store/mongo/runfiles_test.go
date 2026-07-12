package mongo

import (
	"context"
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
