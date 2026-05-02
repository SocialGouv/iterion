package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveStoreDir_OverrideWins(t *testing.T) {
	got := ResolveStoreDir("/some/start", "/explicit/store")
	if got != "/explicit/store" {
		t.Fatalf("override should be returned verbatim, got %q", got)
	}
}

func TestResolveStoreDir_EmptyStartFallsBack(t *testing.T) {
	got := ResolveStoreDir("", "")
	if got != StoreDirName {
		t.Fatalf("empty start should fall back to %q, got %q", StoreDirName, got)
	}
}

func TestResolveStoreDir_FindsAncestor(t *testing.T) {
	root := t.TempDir()
	storeAt := filepath.Join(root, StoreDirName)
	if err := os.Mkdir(storeAt, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	got := ResolveStoreDir(deep, "")
	if got != storeAt {
		t.Fatalf("should find ancestor store at %q, got %q", storeAt, got)
	}
}

func TestResolveStoreDir_FindsAtSelf(t *testing.T) {
	root := t.TempDir()
	storeAt := filepath.Join(root, StoreDirName)
	if err := os.Mkdir(storeAt, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}

	got := ResolveStoreDir(root, "")
	if got != storeAt {
		t.Fatalf("should find store at start dir, got %q", got)
	}
}

func TestResolveStoreDir_NoAncestorCreatesNextToStart(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	got := ResolveStoreDir(deep, "")

	absDeep, _ := filepath.Abs(deep)
	want := filepath.Join(absDeep, StoreDirName)
	if got != want {
		t.Fatalf("no ancestor → expected %q, got %q", want, got)
	}
}

func TestResolveStoreDir_IgnoresFileNamedDotIterion(t *testing.T) {
	root := t.TempDir()
	// A regular file named .iterion must NOT be confused with the store dir.
	if err := os.WriteFile(filepath.Join(root, StoreDirName), []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	deep := filepath.Join(root, "sub")
	if err := os.Mkdir(deep, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	got := ResolveStoreDir(deep, "")

	absDeep, _ := filepath.Abs(deep)
	want := filepath.Join(absDeep, StoreDirName)
	if got != want {
		t.Fatalf("file (not dir) named .iterion must be skipped — expected %q, got %q", want, got)
	}
}

func TestResolveStoreDir_NearestAncestorWins(t *testing.T) {
	root := t.TempDir()
	// Two .iterion dirs in the chain — the nearest (deepest) must win.
	outer := filepath.Join(root, StoreDirName)
	inner := filepath.Join(root, "project", StoreDirName)
	if err := os.MkdirAll(outer, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}
	deep := filepath.Join(root, "project", "sub", "leaf")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	got := ResolveStoreDir(deep, "")
	if got != inner {
		t.Fatalf("nearest ancestor should win, expected %q got %q", inner, got)
	}
}
