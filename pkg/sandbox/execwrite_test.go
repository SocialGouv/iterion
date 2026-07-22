package sandbox_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/noop"
)

// noopRun builds a host-passthrough Run so WriteFileExec's sh pipeline
// executes for real against a temp dir — a functional test of the
// stdin-streaming writer script itself.
func noopRun(t *testing.T) sandbox.Run {
	t.Helper()
	d, err := noop.New()
	if err != nil {
		t.Fatal(err)
	}
	p, err := d.Prepare(context.Background(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := d.Start(context.Background(), p, sandbox.RunInfo{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestWriteFileExec_WritesAtomicOwnerOnly(t *testing.T) {
	run := noopRun(t)
	target := filepath.Join(t.TempDir(), "nested", "dir", "iterion-credentials")
	payload := []byte("https://oauth2:tok@github.com\n")

	if err := sandbox.WriteFileExec(context.Background(), run, target, payload); err != nil {
		t.Fatalf("WriteFileExec: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("content = %q, want %q", got, payload)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perms = %o, want 0600 (umask 077)", perm)
	}
	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind: %v", err)
	}

	// Rewrite in place — the rotation path.
	if err := sandbox.WriteFileExec(context.Background(), run, target, []byte("rotated\n")); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	got, _ = os.ReadFile(target)
	if string(got) != "rotated\n" {
		t.Errorf("rotated content = %q", got)
	}
}

func TestWriteFileExec_RejectsBadPaths(t *testing.T) {
	run := noopRun(t)
	for _, p := range []string{"", "relative/path", "/a/../b", "/", "/a\nb"} {
		if err := sandbox.WriteFileExec(context.Background(), run, p, []byte("x")); err == nil {
			t.Errorf("path %q: expected an error", p)
		}
	}
}

func TestWriteFileExec_SurfacesWriterFailure(t *testing.T) {
	run := noopRun(t)
	// Parent path component is a FILE → mkdir -p fails → non-zero exit.
	dir := t.TempDir()
	block := filepath.Join(dir, "block")
	if err := os.WriteFile(block, []byte("f"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := sandbox.WriteFileExec(context.Background(), run, filepath.Join(block, "child"), []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("expected a writer-exited error, got %v", err)
	}
}
