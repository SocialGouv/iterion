package safepath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardNameRefusesWhatCannotBeWrittenUnderARoot(t *testing.T) {
	for _, name := range []string{"/etc/passwd", "../up", "a/../../b", "./a/../../b", "/"} {
		if err := GuardName(name); err == nil {
			t.Errorf("GuardName(%q) = nil, want a refusal", name)
		}
	}
	for _, name := range []string{"a/b.txt", "./a/b.txt", ".", "", "a/./b", "a/..b/c"} {
		if err := GuardName(name); err != nil {
			t.Errorf("GuardName(%q) = %v, want nil", name, err)
		}
	}
}

// The lexical check and the filesystem one answer different questions: a name
// with no ".." in it still escapes when a component of the path it names is a
// symlink out of the root. The OS follows that link at open time.
func TestJoinRefusesAPathThroughASymlinkOutOfTheRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink("real", filepath.Join(root, "inside")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Join(root, "out/landed.txt"); err == nil {
		t.Error("Join accepted a path through a symlink leaving the root")
	}
	if _, err := Join(root, "inside/kept.txt"); err != nil {
		t.Errorf("Join refused a path through a link that stays inside the root: %v", err)
	}
	got, err := Join(root, "a/b.txt")
	if err != nil {
		t.Fatalf("Join(a/b.txt) = %v", err)
	}
	if want := filepath.Join(root, "a", "b.txt"); got != want {
		t.Errorf("Join = %q, want %q", got, want)
	}
	if _, err := Join(root, "../escaped.txt"); err == nil {
		t.Error("Join accepted a lexical escape")
	}
}

// Landing answers with the file a write REACHES, which is what a decision
// taken from a path — an exclusion list, an allow-list — has to be about: a
// link turns one name into another file, and the name says nothing about it.
func TestLandingResolvesTheParentChain(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hooks", filepath.Join(root, "x")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	abs, err := Join(root, "x/pre-push")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Landing(root, abs)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hooks/pre-push" {
		t.Errorf("Landing = %q, want %q: a decision taken on the name would miss the file", got, "hooks/pre-push")
	}
	// A path whose parent does not exist yet lands where it is named.
	abs, err = Join(root, "new/dir/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Landing(root, abs); err != nil || got != "new/dir/file.txt" {
		t.Errorf("Landing = %q (%v), want the name itself", got, err)
	}
}

func TestAssertNoEscapingSymlinkIgnoresWhatDoesNotExistYet(t *testing.T) {
	root := t.TempDir()
	if err := AssertNoEscapingSymlink(root, filepath.Join(root, "not", "created", "yet.txt")); err != nil {
		t.Errorf("AssertNoEscapingSymlink = %v, want nil: a component that does not exist cannot be a symlink", err)
	}
	if err := AssertNoEscapingSymlink(root, filepath.Join(filepath.Dir(root), "elsewhere")); err == nil ||
		!strings.Contains(err.Error(), "outside root") {
		t.Errorf("AssertNoEscapingSymlink on a path outside the root = %v, want a refusal", err)
	}
}
