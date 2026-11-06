package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestMirrorBundleSkills_RefusesASkillsDirThatLeavesTheWorkspace.
//
// git checks out a symlink with whatever target it was committed with, so a
// fork can ship `.claude/skills` — or `.claude` itself — as a link into the
// operator's own tree. The untrusted branch of the mirror REPLACES what it
// finds there, which turns that link into a delete and an overwrite OUTSIDE
// the workspace, under file names the public bundle catalog already tells the
// attacker.
//
// The primitive is the floor's own: before it, a diverged collision was
// shadowed and nothing outside was ever touched. So this is the regression
// test for a fix, and the operator's file surviving is the whole assertion.
func TestMirrorBundleSkills_RefusesASkillsDirThatLeavesTheWorkspace(t *testing.T) {
	for _, linked := range []string{".claude/skills", ".claude"} {
		t.Run("the checkout committed "+linked+" as a symlink", func(t *testing.T) {
			outside := t.TempDir() // stands in for the operator's own ~/.claude
			victim := filepath.Join(outside, "skills", "audit", "operator-notes.md")
			writeSkill(t, filepath.Dir(victim), filepath.Base(victim), "# the operator's own notes\n")

			workDir := t.TempDir()
			if linked == ".claude/skills" {
				if err := os.MkdirAll(filepath.Join(workDir, ".claude"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, "skills"), filepath.Join(workDir, ".claude", "skills")); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(outside, filepath.Join(workDir, ".claude")); err != nil {
				t.Fatal(err)
			}

			bundleDir := t.TempDir()
			writeSkill(t, filepath.Join(bundleDir, "skills"), "audit/SKILL.md", "---\nname: audit\n---\nbundle-authored\n")
			b := &bundle.Bundle{Dir: bundleDir, SkillsDir: filepath.Join(bundleDir, "skills")}

			_, err := mirrorBundleSkills(workDir, b, testMirrorPass(t, workDir, store.RunTrustFork), iterlog.Nop())
			if err == nil {
				t.Error("the mirror accepted a skills directory that resolves outside the workspace")
			} else {
				// The handle refuses typed; a `.claude` link is ALSO caught
				// earlier by the #1569 symlink veto, whose refusal is its own
				// type. Either way the refusal is typed, never a raw I/O error.
				var linkErr *claudeSymlinkError
				if !errors.Is(err, ErrSkillsDestEscapesWorkspace) && !errors.As(err, &linkErr) {
					t.Errorf("refusal is not typed: %v", err)
				}
			}

			// The assertion that matters: nothing outside the workspace moved.
			if _, statErr := os.Stat(victim); statErr != nil {
				t.Errorf("a file OUTSIDE the workspace was removed: %v", statErr)
			}
			if got, _ := os.ReadFile(victim); string(got) != "# the operator's own notes\n" {
				t.Errorf("a file OUTSIDE the workspace was overwritten: %q", got)
			}
		})
	}
}

// TestSkillsDest_ATrustedRunKeepsItsSymlinkedSkillsDirectory: the check is
// about provenance, not about symlinks. An operator who arranged their own
// checkout that way is doing something that works today, and the mirror there
// only ever copies or shadows — it never removes.
func TestSkillsDest_ATrustedRunKeepsItsSymlinkedSkillsDirectory(t *testing.T) {
	outside := t.TempDir()
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workDir, ".claude", "skills")); err != nil {
		t.Fatal(err)
	}
	dest, err := skillsDest(workDir, testMirrorPass(t, workDir, store.RunTrustDefault))
	if err != nil {
		t.Fatalf("a trusted run was refused its own arrangement: %v", err)
	}
	if dest != filepath.Join(workDir, ".claude", "skills") {
		t.Errorf("dest = %q", dest)
	}
	// And the untrusted arm on the SAME fixture refuses — without this the
	// test above could pass on a build that never checks anything.
	if _, err := skillsDest(workDir, testMirrorPass(t, workDir, store.RunTrustFork)); !errors.Is(err, ErrSkillsDestEscapesWorkspace) {
		t.Errorf("untrusted arm returned %v, want %v", err, ErrSkillsDestEscapesWorkspace)
	}
}

// TestMirrorBundleSkills_ASymlinkOneLevelDeeperCannotEscape.
//
// Proving the destination once and returning a STRING leaves every later
// operation to re-resolve that string through the OS, so a symlink one
// component deeper escapes past a PASSING proof. The guarantee has to BE the
// object writes go through.
//
// Here `.claude/skills` is a real directory (the proof passes) and the skill's
// MARKER directory inside it is the link. Nothing outside may move.
func TestMirrorBundleSkills_ASymlinkOneLevelDeeperCannotEscape(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "operator-notes.md")
	writeSkill(t, outside, "operator-notes.md", "# the operator's own notes\n")

	workDir := t.TempDir()
	skills := filepath.Join(workDir, ".claude", "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	// One level DEEPER than the proof, and at the name that actually escapes:
	// the MARKER directory. A link at `<skills>/<name>` proves nothing —
	// RemoveAll removes the link itself, so nothing can land beyond it — and
	// an earlier version of this test pointed there and stayed green with the
	// floor removed. The marker dir is never removed, only written into.
	if err := os.Symlink(outside, filepath.Join(skills, bundleMirrorMarkerDir)); err != nil {
		t.Fatal(err)
	}

	bundleDir := t.TempDir()
	// FLAT source: the form whose mirror writes MARKERS, which is what lands
	// in the linked directory. A tree-source skill writes no marker, so a
	// fixture using one cannot observe this escape at all.
	writeSkill(t, filepath.Join(bundleDir, "skills"), "audit.md", "---\nname: audit\n---\nbundle-authored\n")
	b := &bundle.Bundle{Dir: bundleDir, SkillsDir: filepath.Join(bundleDir, "skills")}

	// The destination proof itself passes — that is the point of this test.
	if _, err := skillsDest(workDir, testMirrorPass(t, workDir, store.RunTrustFork)); err != nil {
		t.Fatalf("the destination proof failed, so this test would not be exercising what it claims: %v", err)
	}

	_, _ = mirrorBundleSkills(workDir, b, testMirrorPass(t, workDir, store.RunTrustFork), iterlog.Nop())

	if got, err := os.ReadFile(victim); err != nil || string(got) != "# the operator's own notes\n" {
		t.Errorf("a file OUTSIDE the workspace was written or removed through a symlink one level below the proof (err=%v):\n%s", err, got)
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the mirror created %d entries OUTSIDE the workspace: %v", len(entries)-1, names)
	}
}
