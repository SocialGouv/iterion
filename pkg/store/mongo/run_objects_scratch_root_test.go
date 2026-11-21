package mongo

import (
	"bytes"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRunObjectsScratchTrailingSeparatorDoesNotOverlapLegacy(t *testing.T) {
	t.Parallel()
	s, ctx := valueFormatMongo(t, true)
	s.runFilesScratch = filepath.Join(t.TempDir(), "scratch") + string(filepath.Separator)
	if _, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", Format: 2}); e != nil {
		t.Fatal(e)
	}
	exact, e := s.EnsureRunFilesDir(ctx, "r")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(exact, "keep.txt"), []byte("new"), 0600); e != nil {
		t.Fatal(e)
	}
	// This is a valid distinct legacy run ID, not a synthetic production node ID.
	if _, e = s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "-v2", Format: 1}); e != nil {
		t.Fatal(e)
	}
	legacy, e := s.EnsureRunFilesDir(ctx, "-v2")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(legacy, "old.txt"), []byte("old"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = s.UploadRunFiles(ctx, "-v2"); e != nil {
		t.Fatal(e)
	}
	p, e := os.ReadFile(filepath.Join(exact, "keep.txt"))
	if e != nil || string(p) != "new" {
		t.Fatalf("legacy scratch cleanup erased exact run: path=%s bytes=%q err=%v", exact, p, e)
	}
	if n, e := s.UploadRunFiles(ctx, "r"); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	rc, _, e := s.OpenRunFile(ctx, "r", "keep.txt")
	if e != nil {
		t.Fatal(e)
	}
	p, e = io.ReadAll(rc)
	_ = rc.Close()
	if e != nil || !bytes.Equal(p, []byte("new")) {
		t.Fatal(string(p), e)
	}
}

func TestRunObjectsScratchPhysicalRoots(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if e := os.MkdirAll(real, 0700); e != nil {
		t.Fatal(e)
	}
	alias := filepath.Join(root, "alias")
	if e := os.Symlink(real, alias); e != nil {
		t.Fatal(e)
	}
	s := &Store{runFilesScratch: alias + string(filepath.Separator)}
	got, e := s.runFilesScratchDirForFormat("r", 2)
	if e != nil || got != filepath.Join(real+"-v2", "r") {
		t.Fatal(got, e)
	}
	s.runFilesScratch = filepath.Join(alias, "not", "created")
	got, e = s.runFilesScratchDirForFormat("r", 2)
	if e != nil || got != filepath.Join(real, "not", "created-v2", "r") {
		t.Fatal(got, e)
	}
	// Never write to the volume root: compute/refuse only.
	s.runFilesScratch = string(filepath.Separator)
	if _, e = s.runFilesScratchDirForFormat("r", 2); !errors.Is(e, store.ErrValueFormatCapability) {
		t.Fatal(e)
	}
	s.runFilesScratch = real
	if e = os.Symlink(real, real+"-v2"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.runFilesScratchDirForFormat("r", 2); !errors.Is(e, store.ErrValueFormatCapability) {
		t.Fatal(e)
	}
	if got, e = s.runFilesScratchDirForFormat("r", 1); e != nil || got != filepath.Join(real, "r") {
		t.Fatal(got, e)
	}
}

func TestRunObjectsScratchDanglingAliasIsNotAbsentSuffix(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	alias := filepath.Join(root, "alias")
	if e := os.Symlink(filepath.Join(root, "missing"), alias); e != nil {
		t.Fatal(e)
	}
	s := &Store{runFilesScratch: filepath.Join(alias, "child")}
	if _, e := s.runFilesScratchDirForFormat("r", 2); !os.IsNotExist(e) {
		t.Fatalf("dangling physical root was guessed: %v", e)
	}
	if _, e := os.Stat(filepath.Join(root, "missing")); !os.IsNotExist(e) {
		t.Fatal(e)
	}
}
