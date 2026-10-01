package runtime

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
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

// claudeSymlinkWorkspace returns a workspace whose `.claude` is a symlink
// to a sibling directory holding one committed file, plus the target's
// path — the #1569 shape every mirror writer is tested against.
func claudeSymlinkWorkspace(t *testing.T) (ws, target string) {
	t.Helper()
	ws = t.TempDir()
	target = filepath.Join(ws, "target-dir")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(target, "keep.md"), "the checkout's own\n")
	if err := os.Symlink("target-dir", filepath.Join(ws, ".claude")); err != nil {
		t.Fatalf("symlink .claude: %v", err)
	}
	return ws, target
}

// assertLinkTargetUntouched verifies a mirror wrote NOTHING through the
// `.claude` symlink: the target holds exactly its one committed file.
func assertLinkTargetUntouched(t *testing.T, target string) {
	t.Helper()
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "keep.md" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("target-dir = %v, want [keep.md] — the mirror wrote through the symlink", names)
	}
}

// #2044: MirrorSingleSkill runs INSIDE a run (the chatbox attach path),
// after the run-start guard already passed — a checkout rewrite or an
// operator edit can turn `.claude` into a symlink mid-run. The attach
// mirror must refuse with the same typed error as the run-start mirror
// rather than write through the link.
func TestMirrorSingleSkillRefusesAClaudeSymlink(t *testing.T) {
	ws, target := claudeSymlinkWorkspace(t)
	skillsSrc := t.TempDir()
	writeFile(t, filepath.Join(skillsSrc, "skill.md"), "# a bundle skill\n")

	err := MirrorSingleSkill(ws, &bundle.Bundle{SkillsDir: skillsSrc}, "skill.md", nil)
	var linkErr *claudeSymlinkError
	if !errors.As(err, &linkErr) {
		t.Fatalf("MirrorSingleSkill through a .claude symlink = %v, want the typed *claudeSymlinkError refusal", err)
	}
	if !strings.Contains(linkErr.Error(), "target-dir") {
		t.Fatalf("the refusal must name the resolved target: %v", linkErr)
	}
	assertLinkTargetUntouched(t, target)
}

// #2044: the local plugin-contribution mirror keeps its soft-fail
// semantics on the same symlink shape — ambient plugin enablement must
// never brick a run. The whole pass is SKIPPED with a warning naming the
// link and its target, no error is returned, and complete drops to false
// so the orphan pruner stays out (last pass's sidecars were not
// refreshed, so nothing may be deleted).
func TestMirrorPluginContributionsSkipsAClaudeSymlink(t *testing.T) {
	ws, target := claudeSymlinkWorkspace(t)
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &buf)

	owned, complete, err := mirrorPluginContributions(ws, nil, false, logger)
	if err != nil {
		t.Fatalf("mirrorPluginContributions through a .claude symlink = %v, want a soft skip, not an error", err)
	}
	if complete {
		t.Fatal("complete = true on a skipped pass — the pruner would delete last pass's plugin files")
	}
	if len(owned) != 0 {
		t.Fatalf("owned = %v, want none — nothing was mirrored", owned)
	}
	logs := buf.String()
	for _, want := range []string{".claude", "target-dir"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the skip warning does not name %q; logs = %q", want, logs)
		}
	}
	assertLinkTargetUntouched(t, target)
}

// #2044: the cloud-injected twin takes the same soft skip — it is guarded
// one level up in mirrorPluginContributions, but a direct caller must get
// the same behaviour: no error, a warning naming the link and target,
// complete=false, nothing written through the link.
func TestMirrorInjectedPluginFilesSkipsAClaudeSymlink(t *testing.T) {
	ws, target := claudeSymlinkWorkspace(t)
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &buf)

	owned, complete, err := mirrorInjectedPluginFiles(ws, []ContributionFile{
		{Kind: "skills", Name: "plug.md", Content: []byte("# an injected plugin skill\n")},
	}, logger)
	if err != nil {
		t.Fatalf("mirrorInjectedPluginFiles through a .claude symlink = %v, want a soft skip, not an error", err)
	}
	if complete {
		t.Fatal("complete = true on a skipped pass — the pruner would delete last pass's plugin files")
	}
	if len(owned) != 0 {
		t.Fatalf("owned = %v, want none — nothing was mirrored", owned)
	}
	logs := buf.String()
	for _, want := range []string{".claude", "target-dir"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the skip warning does not name %q; logs = %q", want, logs)
		}
	}
	assertLinkTargetUntouched(t, target)
}

func TestMirrorInjectedPluginFilesEmptyPayloadStillVetoesPruneOnAClaudeSymlink(t *testing.T) {
	ws, target := claudeSymlinkWorkspace(t)
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &buf)

	owned, complete, err := mirrorInjectedPluginFiles(ws, nil, logger)
	if err != nil {
		t.Fatalf("mirrorInjectedPluginFiles with no files through a .claude symlink = %v, want a soft skip, not an error", err)
	}
	if complete {
		t.Fatal("complete = true on an empty pass against a symlink — the pruner would walk .claude through the link")
	}
	if len(owned) != 0 {
		t.Fatalf("owned = %v, want none", owned)
	}
	if logs := buf.String(); !strings.Contains(logs, ".claude") {
		t.Errorf("the skip warning does not name the link; logs = %q", logs)
	}
	assertLinkTargetUntouched(t, target)
}
