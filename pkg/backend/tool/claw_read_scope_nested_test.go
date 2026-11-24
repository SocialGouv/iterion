package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nestedRootFixture lays the #2869 shape: a project scratch (the anchor),
// a run subtree beneath it, and a host directory outside every root that
// holds a file no read may return. It returns a registry whose read_file
// is scoped on a workspace plus the run subtree attached BENEATH the
// anchor, and a reader over that registry.
func nestedRootFixture(t *testing.T) (scope *ReadScope, scratch, outside string, read func(string) (string, error)) {
	t.Helper()
	scratch = t.TempDir()
	outside = t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "host-file.txt"), []byte("HOST ONLY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(scratch, "runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	scope = NewReadScope(t.TempDir())
	reg := NewRegistry()
	if err := RegisterClawBuiltinsWithScope(reg, scope, nil); err != nil {
		t.Fatal(err)
	}
	td, err := reg.Resolve("read_file")
	if err != nil {
		t.Fatal(err)
	}
	read = func(path string) (string, error) {
		raw, _ := json.Marshal(map[string]any{"path": path})
		return td.Execute(context.Background(), raw)
	}
	return scope, scratch, outside, read
}

// A link PLANTED at runs/<id> before the root is attached — another run
// wrote the shared scratch through its bind — must not become the root: a
// plain root canonicalizes through it and widens the scope to the link's
// target. Attached beneath its anchor, the root stays the lexical
// <anchor>/runs/<id>, and nothing behind the link reads, under either
// spelling.
func TestNestedReadRootDoesNotAdoptAPlantedLink(t *testing.T) {
	t.Parallel()
	scope, scratch, outside, read := nestedRootFixture(t)
	runRoot := filepath.Join(scratch, "runs", "run-planted")
	if err := os.Symlink(outside, runRoot); err != nil {
		t.Fatal(err)
	}

	if err := scope.AddExtraRootBeneath(scratch, runRoot); err != nil {
		t.Fatalf("AddExtraRootBeneath: %v", err)
	}
	want := filepath.Join(mustEvalSymlinks(t, scratch), "runs", "run-planted")
	if got := scope.ExtraRoots(); len(got) != 1 || got[0] != want {
		t.Fatalf("ExtraRoots = %v, want the lexical [%q] — never the link's target %q", got, want, outside)
	}
	for _, path := range []string{
		filepath.Join(outside, "host-file.txt"),
		filepath.Join(runRoot, "host-file.txt"),
	} {
		if out, err := read(path); err == nil {
			t.Errorf("read_file(%q) = %q through a link planted at the run root, want a refusal", path, out)
		}
	}
}

// A link SWAPPED in after the root was attached — the subtree was a real
// directory at the push, replaced while the run waits — must not move the
// root either: containment re-resolves the anchor only and keeps the tail
// lexical.
func TestNestedReadRootDoesNotFollowALinkSwappedInLater(t *testing.T) {
	t.Parallel()
	scope, scratch, outside, read := nestedRootFixture(t)
	runRoot := filepath.Join(scratch, "runs", "run-swapped")
	if err := os.MkdirAll(runRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(runRoot, "own.md")
	if err := os.WriteFile(own, []byte("OWN\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := scope.AddExtraRootBeneath(scratch, runRoot); err != nil {
		t.Fatalf("AddExtraRootBeneath: %v", err)
	}
	// Control: the real subtree reads.
	if out, err := read(own); err != nil || out != "OWN\n" {
		t.Fatalf("read_file(%q) under the real subtree = %q, %v", own, out, err)
	}

	if err := os.Rename(runRoot, runRoot+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, runRoot); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(outside, "host-file.txt"),
		filepath.Join(runRoot, "host-file.txt"),
	} {
		if out, err := read(path); err == nil {
			t.Errorf("read_file(%q) = %q after the run root was swapped for a link, want a refusal", path, out)
		}
	}
}

// The anchor relation is checked: a root that is not beneath its anchor is
// refused unattached, and the anchor itself attaches as a plain root.
func TestAddExtraRootBeneathRefusesARootOutsideItsAnchor(t *testing.T) {
	t.Parallel()
	scope, scratch, outside, _ := nestedRootFixture(t)
	if err := scope.AddExtraRootBeneath(scratch, outside); err == nil || !strings.Contains(err.Error(), "not beneath") {
		t.Fatalf("AddExtraRootBeneath(%q, %q) = %v, want a refusal", scratch, outside, err)
	}
	if got := scope.ExtraRoots(); len(got) != 0 {
		t.Fatalf("a refused root was attached: %v", got)
	}
	if err := scope.AddExtraRootBeneath(scratch, scratch); err != nil {
		t.Fatalf("AddExtraRootBeneath(anchor, anchor): %v", err)
	}
	if got := scope.ExtraRoots(); len(got) != 1 || got[0] != mustEvalSymlinks(t, scratch) {
		t.Fatalf("ExtraRoots = %v, want the anchor as a plain root", got)
	}
}

func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
