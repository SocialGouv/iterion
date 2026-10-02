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
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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

// #2060: ClearMirroredTierMarkers runs BEFORE mirrorBundleSkills at every
// run-start site, and its os.Remove of the `.tier` sidecars had no symlink
// check — through a `.claude` link, the wipe deleted iterion bookkeeping in
// the link's TARGET before the mirror's refusal could fire. The wipe now
// short-circuits on the same refusal; the witness `.tier` in the target
// must survive — and the skip WARNS, so a future caller sees it.
func TestClearMirroredTierMarkersSkipsAClaudeSymlink(t *testing.T) {
	ws, target := claudeSymlinkWorkspace(t)
	markerDir := filepath.Join(target, "skills", bundleMirrorMarkerDir)
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	witness := filepath.Join(markerDir, "keep.md.sha256.tier")
	writeFile(t, witness, "bundle")

	var buf bytes.Buffer
	ClearMirroredTierMarkers(ws, iterlog.New(iterlog.LevelWarn, &buf))

	if _, err := os.Lstat(witness); err != nil {
		t.Fatalf("the tier sidecar in the link's target was wiped through the symlink: %v", err)
	}
	if logs := buf.String(); !strings.Contains(logs, ".claude") || !strings.Contains(logs, "target-dir") {
		t.Fatalf("the skip did not warn naming the link and its target; logs = %q", logs)
	}
}

// #2061: mergePluginHooks writes `.claude/settings.json` and had no symlink
// guard. Ambient writer, so the #2044 split gives it the soft-fail: a
// warning naming the link, no error — and complete=false so the caller
// vetoes the orphan pruner, which walks `.claude/<kind>` itself and must
// not follow the link into the target.
func TestMergePluginHooksSkipsAClaudeSymlink(t *testing.T) {
	ws, target := claudeSymlinkWorkspace(t)
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &buf)

	complete, err := mergePluginHooks(ws, logger)
	if err != nil {
		t.Fatalf("mergePluginHooks through a .claude symlink = %v, want a soft skip, not an error", err)
	}
	if complete {
		t.Fatal("complete = true on a skipped merge — the pruner would walk .claude through the link")
	}
	logs := buf.String()
	for _, want := range []string{".claude", "target-dir"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the skip warning does not name %q; logs = %q", want, logs)
		}
	}
	assertLinkTargetUntouched(t, target)
}

// #2061: the library mirror had no symlink guard at all, and in the
// mid-sequence window it wrote through the link AND reported
// libraryComplete=true. Library skills are `.bot`-declared — run-critical —
// so a declared ref gets the typed refusal, and even with NO declared ref
// the pass reports complete=false: the pruner must not walk the link.
func TestMirrorLibrarySkillsRefusesAClaudeSymlinkWithDeclaredSkills(t *testing.T) {
	ws, target := claudeSymlinkWorkspace(t)
	wf := &ir.Workflow{Skills: []string{"some-skill"}}

	_, _, complete, err := mirrorLibrarySkills(ws, t.TempDir(), wf, nil, nil, nil)
	var linkErr *claudeSymlinkError
	if !errors.As(err, &linkErr) {
		t.Fatalf("mirrorLibrarySkills with declared skills through a .claude symlink = %v, want the typed *claudeSymlinkError refusal", err)
	}
	if complete {
		t.Fatal("complete = true on a refused pass — the pruner would walk .claude through the link")
	}
	assertLinkTargetUntouched(t, target)
}

func TestMirrorLibrarySkillsSymlinkWithoutDeclaredSkillsVetoesThePruner(t *testing.T) {
	ws, target := claudeSymlinkWorkspace(t)
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &buf)

	_, _, complete, err := mirrorLibrarySkills(ws, t.TempDir(), &ir.Workflow{}, nil, nil, logger)
	if err != nil {
		t.Fatalf("mirrorLibrarySkills with no declared skills through a .claude symlink = %v, want a soft skip, not an error", err)
	}
	if complete {
		t.Fatal("complete = true against a symlink — the pruner would walk .claude through the link")
	}
	if logs := buf.String(); !strings.Contains(logs, ".claude") {
		t.Errorf("the skip warning does not name the link; logs = %q", logs)
	}
	assertLinkTargetUntouched(t, target)
}

// #2061: the cloud twin takes the typed refusal directly — the payload
// carries skills the `.bot` declares, and before this guard it wrote
// through the link and reported success.
func TestMirrorInjectedLibrarySkillsRefusesAClaudeSymlink(t *testing.T) {
	ws, target := claudeSymlinkWorkspace(t)

	_, _, err := mirrorInjectedLibrarySkills(ws, []LibrarySkillFile{
		{Name: "some-skill", Content: []byte("# an injected library skill\n")},
	}, nil)
	var linkErr *claudeSymlinkError
	if !errors.As(err, &linkErr) {
		t.Fatalf("mirrorInjectedLibrarySkills through a .claude symlink = %v, want the typed *claudeSymlinkError refusal", err)
	}
	assertLinkTargetUntouched(t, target)
}

// The pruner DELETES, so it carries the same in-function guard as the wipe
// (#2060): called directly with a `.claude` symlink — whatever the
// call-site vetoes forgot — it must not follow the link and delete in the
// target. The fixture plants a prunable orphan (iterion-wrote sidecar,
// marker-matching content, no fresh tier) in the target — and the skip
// WARNS, so a future caller sees it.
func TestPruneWorkspaceMirrorSkipsAClaudeSymlink(t *testing.T) {
	ws, target := claudeSymlinkWorkspace(t)
	orphan := filepath.Join(target, "skills", "gone.md")
	installMirroredFile(t, orphan, filepath.Join(target, "skills", bundleMirrorMarkerDir, "gone.md.sha256"), "orphan\n", "")

	var buf bytes.Buffer
	pruneWorkspaceMirror(ws, true, iterlog.New(iterlog.LevelWarn, &buf))

	raw, err := os.ReadFile(orphan)
	if err != nil || string(raw) != "orphan\n" {
		t.Fatalf("the pruner deleted through the symlink: orphan=%q, %v", raw, err)
	}
	if logs := buf.String(); !strings.Contains(logs, ".claude") || !strings.Contains(logs, "target-dir") {
		t.Fatalf("the skip did not warn naming the link and its target; logs = %q", logs)
	}
}

// The injected payload is authoritative on the cloud path: a workflow the
// pod re-reads WITHOUT the refs the launching instance shipped must not
// degrade the symlink refusal to a soft skip while the payload still
// carries declared skills.
func TestMirrorLibrarySkillsRefusesAClaudeSymlinkOnAnInjectedPayload(t *testing.T) {
	ws, target := claudeSymlinkWorkspace(t)
	inj := &Contributions{Library: []LibrarySkillFile{
		{Name: "some-skill", Content: []byte("# shipped by the launching instance\n")},
	}}

	_, _, complete, err := mirrorLibrarySkills(ws, t.TempDir(), &ir.Workflow{}, nil, inj, nil)
	var linkErr *claudeSymlinkError
	if !errors.As(err, &linkErr) {
		t.Fatalf("mirrorLibrarySkills with an injected payload through a .claude symlink = %v, want the typed *claudeSymlinkError refusal", err)
	}
	if complete {
		t.Fatal("complete = true on a refused pass — the pruner would walk .claude through the link")
	}
	assertLinkTargetUntouched(t, target)
}
