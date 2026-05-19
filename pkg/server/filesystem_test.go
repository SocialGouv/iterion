package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrowseRoot_EnvUnsetReturnsEmpty(t *testing.T) {
	t.Setenv(browseRootEnv, "")
	if got := browseRoot(); got != "" {
		t.Errorf("expected empty when unset, got %q", got)
	}
}

func TestBrowseRoot_TrimsWhitespace(t *testing.T) {
	t.Setenv(browseRootEnv, "  /tmp/iter-root  ")
	if got := browseRoot(); got != "/tmp/iter-root" {
		t.Errorf("got %q", got)
	}
}

func TestResolveBrowsePath_HappyPath(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveBrowsePath(root, "/sub")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// EvalSymlinks normalises both — accept either as long as it matches sub.
	if !strings.HasSuffix(got, "/sub") {
		t.Errorf("got %q, want suffix /sub", got)
	}
}

func TestResolveBrowsePath_RootItself(t *testing.T) {
	root := t.TempDir()
	got, err := resolveBrowsePath(root, "/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == "" {
		t.Errorf("got empty result")
	}
}

func TestResolveBrowsePath_TraversalRejected(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Try to escape via ".."
	_, err := resolveBrowsePath(root, "/../etc")
	if err == nil {
		t.Fatal("expected error on '..' traversal")
	}
}

func TestResolveBrowsePath_MissingPath(t *testing.T) {
	root := t.TempDir()
	_, err := resolveBrowsePath(root, "/no-such-subdir")
	if err == nil {
		t.Fatal("expected error for missing path")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error wording: %v", err)
	}
}

func TestResolveBrowsePath_SymlinkEscapeRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	// Create a symlink inside root pointing outside.
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation failed (filesystem restriction?): %v", err)
	}
	// Real-rooted resolution: pass the EvalSymlinks-resolved root.
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks root: %v", err)
	}
	_, err = resolveBrowsePath(rootReal, "/escape")
	if err == nil {
		t.Error("symlink escaping the root should be rejected")
	}
}
