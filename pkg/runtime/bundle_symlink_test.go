package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// #1569: a checkout carrying `.claude` as a SYMLINK to another top-level
// directory routes the run-start mirror THROUGH the link — the files
// physically land under the link's target, a path no tree-noise entry names
// (the dir-only `**/.claude/` rule never matches a symlink, and IsNoise sees
// `target-dir/skill.md`, not `.claude/skill.md`). The wip bank and the
// operator's commit-and-finalize then stage the mirrored skills as the run's
// work. The mirror must REFUSE to write through the link: warning and
// continuing leaves the misclassified dirt in place, and unlinking the
// checkout's own symlink is not the engine's to do.
//
// The witness is the ticket's staging argv, executed against the real
// repository after the mirror attempt: nothing the mirror touched may be
// stageable as the run's work under the link's target.
func TestMirrorBundleSkillsRefusesAClaudeSymlink(t *testing.T) {
	ws := t.TempDir()
	gittest.InitRepo(t, ws)
	if err := os.MkdirAll(filepath.Join(ws, "target-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	addCommit(t, ws, filepath.Join("target-dir", "keep.md"), "the checkout's own\n", "track the link's target")
	if err := os.Symlink("target-dir", filepath.Join(ws, ".claude")); err != nil {
		t.Fatalf("symlink .claude: %v", err)
	}

	skillsSrc := t.TempDir()
	writeFile(t, filepath.Join(skillsSrc, "skill.md"), "# a bundle skill\n")

	_, err := mirrorBundleSkills(ws, &bundle.Bundle{SkillsDir: skillsSrc}, nil)
	var linkErr *claudeSymlinkError
	if !errors.As(err, &linkErr) {
		t.Fatalf("mirrorBundleSkills through a .claude symlink = %v, want the typed *claudeSymlinkError refusal", err)
	}
	if !strings.Contains(linkErr.Error(), "target-dir") {
		t.Fatalf("the refusal must name the resolved target: %v", linkErr)
	}

	// Nothing was written through the link: the target holds exactly what
	// the checkout committed.
	entries, readErr := os.ReadDir(filepath.Join(ws, "target-dir"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 1 || entries[0].Name() != "keep.md" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("target-dir = %v, want [keep.md] — the mirror wrote through the symlink", names)
	}

	// The ticket's staging argv, verbatim: pre-fix this staged the mirrored
	// skills as `A target-dir/skills/…` — the run's "work". Post-fix there
	// is nothing of the mirror's to stage.
	gittest.Run(t, ws, "add", "-A", "--", ":/", ":(exclude,top).claude")
	staged := gittest.Run(t, ws, "status", "--porcelain")
	if strings.Contains(staged, "target-dir/skills") || strings.Contains(staged, "target-dir/iterion-skills") {
		t.Fatalf("the mirror's files are staged as the run's work under the link's target:\n%s", staged)
	}
}

// The refusal is about the LINK, not about a workspace reached through one:
// a run whose workDir itself resolves through a symlink keeps mirroring —
// `.claude` there is an ordinary directory in the same tree.
func TestMirrorBundleSkillsAllowsAWorkspaceReachedThroughASymlink(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "ws-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink workspace: %v", err)
	}
	skillsSrc := t.TempDir()
	writeFile(t, filepath.Join(skillsSrc, "skill.md"), "# a bundle skill\n")

	if _, err := mirrorBundleSkills(link, &bundle.Bundle{SkillsDir: skillsSrc}, nil); err != nil {
		t.Fatalf("mirrorBundleSkills into a workspace reached through a symlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(real, ".claude", "skills", "skill", "SKILL.md")); err != nil {
		t.Fatalf("the mirrored skill is missing from the real workspace: %v", err)
	}
}
