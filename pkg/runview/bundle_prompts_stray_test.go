package runview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// TestMergeBundlePrompts_SkipsStrayEntriesBeforeReading: only a .md in
// prompts/ becomes a prompt, and nothing else there is READ — a broken
// symlink, a fifo, a .DS_Store on a mounted share must not fail a launch,
// nor make the studio's live validation answer 422. The filter sits
// before the I/O; MergePromptFiles keeps its own as the rule's.
func TestMergeBundlePrompts_SkipsStrayEntriesBeforeReading(t *testing.T) {
	dir := t.TempDir()
	prompts := filepath.Join(dir, bundle.DirPrompts)
	if err := os.MkdirAll(prompts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prompts, "mission.md"), []byte("Do the thing."), 0o644); err != nil {
		t.Fatal(err)
	}
	// A broken symlink: unreadable, and not a prompt.
	if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(prompts, "stray.txt")); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	// Still prompts: a .md reached through a symlink to a file, and the
	// suffix in any case.
	if err := os.WriteFile(filepath.Join(dir, "shared.md"), []byte("Shared."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "shared.md"), filepath.Join(prompts, "linked.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prompts, "UPPER.MD"), []byte("Loud."), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &ast.File{}
	b := &bundle.Bundle{Dir: dir, PromptsDir: prompts}
	if err := MergeBundlePrompts(f, b); err != nil {
		t.Fatalf("a stray unreadable entry failed the merge: %v", err)
	}
	var names []string
	for _, p := range f.Prompts {
		names = append(names, p.Name)
	}
	if got := strings.Join(names, ","); got != "UPPER,linked,mission" {
		t.Fatalf("prompts = %s, want UPPER,linked,mission (every .md, in path order, and nothing else)", got)
	}
	// A .md whose name says prompt but which cannot be read must still
	// fail, loudly: that one IS a prompt the bundle promised.
	if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(prompts, "promised.md")); err != nil {
		t.Fatal(err)
	}
	if err := MergeBundlePrompts(&ast.File{}, b); err == nil {
		t.Fatal("an unreadable .md the bundle promised merged in silence")
	}
}
