package runtime

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

func TestMirrorBundleSkills_CopiesIntoClaudeSkills(t *testing.T) {
	workDir := t.TempDir()
	skillsSrc := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(skillsSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsSrc, "probe.md"), []byte("# probe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(skillsSrc, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsSrc, "nested", "step.md"), []byte("# step\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := &bundle.Bundle{SkillsDir: skillsSrc}
	if _, err := mirrorBundleSkills(workDir, b, nil); err != nil {
		t.Fatalf("mirror: %v", err)
	}

	dest := filepath.Join(workDir, ".claude", "skills")
	// Flat "probe.md" source mirrors to the directory form "probe/SKILL.md"
	// (the form claude_code's Skill tool discovers natively).
	if _, err := os.Stat(filepath.Join(dest, "probe", "SKILL.md")); err != nil {
		t.Errorf("probe/SKILL.md missing: %v", err)
	}
	// AND to the flat alias "probe.md" so a bot prompt that Reads the skill by
	// its flat path (`.claude/skills/probe.md`, the pattern most catalog bots
	// use) resolves it instead of failing + re-finding the file.
	if got, err := os.ReadFile(filepath.Join(dest, "probe.md")); err != nil {
		t.Errorf("flat alias probe.md missing: %v", err)
	} else if string(got) != "# probe\n" {
		t.Errorf("flat alias probe.md content = %q, want the source content", got)
	}
	// A source that is already a directory is copied through unchanged.
	if _, err := os.Stat(filepath.Join(dest, "nested", "step.md")); err != nil {
		t.Errorf("nested/step.md missing: %v", err)
	}
}

func TestMirrorBundleSkills_SkipsNonMarkdownFiles(t *testing.T) {
	workDir := t.TempDir()
	skillsSrc := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(skillsSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	// A placeholder like bots/*/skills/.gitkeep must be skipped: its stem
	// keeps the full basename, so the directory form and the flat alias
	// would collide on the SAME path ("is a directory" on fresh mirror).
	if err := os.WriteFile(filepath.Join(skillsSrc, ".gitkeep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsSrc, "real.md"), []byte("# real\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := &bundle.Bundle{SkillsDir: skillsSrc}
	if _, err := mirrorBundleSkills(workDir, b, nil); err != nil {
		t.Fatalf("mirror with a non-md placeholder must succeed: %v", err)
	}

	dest := filepath.Join(workDir, ".claude", "skills")
	if _, err := os.Stat(filepath.Join(dest, ".gitkeep")); !os.IsNotExist(err) {
		t.Errorf(".gitkeep must not be mirrored (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "real", "SKILL.md")); err != nil {
		t.Errorf("real/SKILL.md missing: %v", err)
	}
}

func TestMirrorBundleSkills_WorkspaceWinsOnCollision(t *testing.T) {
	workDir := t.TempDir()
	dest := filepath.Join(workDir, ".claude", "skills")
	// A pre-existing workspace skill at the directory form the mirror targets.
	sharedDir := filepath.Join(dest, "shared")
	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sharedDir, "SKILL.md"), []byte("workspace"), 0o644); err != nil {
		t.Fatal(err)
	}

	skillsSrc := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(skillsSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsSrc, "shared.md"), []byte("bundle"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := mirrorBundleSkills(workDir, &bundle.Bundle{SkillsDir: skillsSrc}, nil); err != nil {
		t.Fatalf("mirror: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(sharedDir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "workspace" {
		t.Errorf("workspace file overwritten: got %q, want %q", string(got), "workspace")
	}
}

func TestMirrorBundleSkills_NilBundleIsNoop(t *testing.T) {
	workDir := t.TempDir()
	if _, err := mirrorBundleSkills(workDir, nil, nil); err != nil {
		t.Errorf("nil bundle should be a no-op, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".claude")); err == nil {
		t.Errorf(".claude dir created unnecessarily")
	}
}

func TestMirrorBundleSkills_EmptySkillsDirIsNoop(t *testing.T) {
	workDir := t.TempDir()
	if _, err := mirrorBundleSkills(workDir, &bundle.Bundle{}, nil); err != nil {
		t.Errorf("empty SkillsDir should be a no-op, got %v", err)
	}
}

// TestMirrorBundleSkills_RefreshesPreviouslyMirroredFile validates the
// v0.2.0→v0.3.0 upgrade case: when a bundle's skill content changes
// between runs and the workspace file still matches what we last
// wrote (user hasn't customized), the next mirror should refresh
// with the new content. Pre-v2-marker behavior silently shadowed,
// so users running iterion against a freshly-built bundle would see
// stale skills indefinitely.
func TestMirrorBundleSkills_RefreshesPreviouslyMirroredFile(t *testing.T) {
	workDir := t.TempDir()
	skillsSrc := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(skillsSrc, 0o755); err != nil {
		t.Fatal(err)
	}

	// First mirror: v1 content.
	if err := os.WriteFile(filepath.Join(skillsSrc, "alpha.md"), []byte("v1 content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mirrorBundleSkills(workDir, &bundle.Bundle{SkillsDir: skillsSrc}, nil); err != nil {
		t.Fatalf("first mirror: %v", err)
	}
	alphaDest := filepath.Join(workDir, ".claude", "skills", "alpha", "SKILL.md")
	mirrored, _ := os.ReadFile(alphaDest)
	if string(mirrored) != "v1 content" {
		t.Fatalf("after first mirror: got %q, want %q", string(mirrored), "v1 content")
	}

	// Bundle author ships v2: edit the source.
	if err := os.WriteFile(filepath.Join(skillsSrc, "alpha.md"), []byte("v2 content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Second mirror: workspace file still matches the v1 marker so it
	// must be refreshed to v2.
	if _, err := mirrorBundleSkills(workDir, &bundle.Bundle{SkillsDir: skillsSrc}, nil); err != nil {
		t.Fatalf("second mirror: %v", err)
	}
	refreshed, _ := os.ReadFile(alphaDest)
	if string(refreshed) != "v2 content" {
		t.Errorf("v2 refresh did not land: got %q, want %q", string(refreshed), "v2 content")
	}
}

// TestMirrorBundleSkills_PreservesUserCustomizationOnUpgrade
// complements the refresh path: if the workspace file diverges from
// the marker (user manually edited the mirrored skill), the next
// mirror must NOT clobber the user's change, regardless of whether
// the bundle's source content has also moved on. "Workspace wins on
// genuine collision" is still the contract.
func TestMirrorBundleSkills_PreservesUserCustomizationOnUpgrade(t *testing.T) {
	workDir := t.TempDir()
	skillsSrc := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(skillsSrc, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(skillsSrc, "beta.md"), []byte("v1 content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mirrorBundleSkills(workDir, &bundle.Bundle{SkillsDir: skillsSrc}, nil); err != nil {
		t.Fatalf("first mirror: %v", err)
	}

	// User customises the mirrored skill (directory form).
	destPath := filepath.Join(workDir, ".claude", "skills", "beta", "SKILL.md")
	if err := os.WriteFile(destPath, []byte("user-edited"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Bundle author ships v2.
	if err := os.WriteFile(filepath.Join(skillsSrc, "beta.md"), []byte("v2 content"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := mirrorBundleSkills(workDir, &bundle.Bundle{SkillsDir: skillsSrc}, nil); err != nil {
		t.Fatalf("second mirror: %v", err)
	}
	got, _ := os.ReadFile(destPath)
	if string(got) != "user-edited" {
		t.Errorf("user customisation overwritten: got %q, want %q", string(got), "user-edited")
	}
}

// mirrorBundleSkills reports which skill directories iterion OWNS, so a backend
// can tell them from whatever the target repository ships at the same path.
// A shadowed entry — where the workspace's own file won — is the repo's, and
// must not be reported: the workspace is an untrusted checkout, and this is the
// only place the distinction exists.
func TestMirrorBundleSkillsReportsOwnership(t *testing.T) {
	skillsSrc := t.TempDir()
	for _, n := range []string{"mine.md", "contested.md"} {
		if err := os.WriteFile(filepath.Join(skillsSrc, n), []byte("---\nname: x\n---\nbody\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(skillsSrc, "dirform"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsSrc, "dirform", "SKILL.md"), []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	// The workspace already carries its own "contested" skill with different
	// content — the collision policy leaves it alone, so it is not ours.
	dest := filepath.Join(workDir, ".claude", "skills")
	if err := os.MkdirAll(filepath.Join(dest, "contested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "contested", "SKILL.md"), []byte("the repo's own"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "contested.md"), []byte("the repo's own"), 0o644); err != nil {
		t.Fatal(err)
	}

	owned, err := mirrorBundleSkills(workDir, &bundle.Bundle{SkillsDir: skillsSrc}, nil)
	if err != nil {
		t.Fatal(err)
	}

	has := func(name string) bool {
		for _, p := range owned {
			if slices.Contains(strings.Split(p, string(filepath.Separator)), name) {
				return true
			}
		}
		return false
	}
	if !has("mine") {
		t.Errorf("owned = %v, missing the skill we wrote", owned)
	}
	// A FLAT source is reported as the FILE it wrote, never as the directory
	// holding it: MkdirAll succeeds on a directory the checkout already shipped,
	// so claiming <stem>/ would hand over any .md the target repo planted beside
	// our SKILL.md, wherever this list is trusted.
	for _, p := range owned {
		if filepath.Base(filepath.Dir(p)) == "mine" && filepath.Base(p) != "SKILL.md" {
			t.Errorf("owned = %v: a flat source must name its file, not its directory", owned)
		}
	}
	// Directory-form skills were invisible to the previous marker-based scheme,
	// which wrote no marker for them.
	if !has("dirform") {
		t.Errorf("owned = %v, missing the directory-form skill", owned)
	}
	if has("contested") {
		t.Errorf("owned = %v claims a skill the workspace shadowed — that content is the repo's", owned)
	}

	// The mirror runs again on every resume, against the SAME worktree. A
	// directory-form skill it wrote on the first pass is then "already there",
	// and treating that as the workspace's own would drop it from the list on
	// every resume — the skill silently disappearing partway through a run.
	second, err := mirrorBundleSkills(workDir, &bundle.Bundle{SkillsDir: skillsSrc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(owned, second) {
		t.Errorf("re-mirror changed ownership:\n first = %v\nsecond = %v", owned, second)
	}

	// ...and a workspace that overwrites our directory-form skill after the
	// fact takes it back off the list: content is what decides, so the only way
	// a repo is reported as ours is by shipping a byte-identical copy.
	if err := os.WriteFile(filepath.Join(dest, "dirform", "SKILL.md"), []byte("the repo's own"), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := mirrorBundleSkills(workDir, &bundle.Bundle{SkillsDir: skillsSrc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range third {
		if filepath.Base(p) == "dirform" {
			t.Errorf("owned = %v still claims a directory the workspace overwrote", third)
		}
	}
}

// A flat bundle source writes <stem>/SKILL.md, and MkdirAll succeeds happily on
// a <stem>/ the checkout already shipped. Reporting the DIRECTORY as owned would
// therefore vouch for whatever the target repo planted next to our file — a
// backend that hands the list to an agent would load attacker-authored prompt
// text as a trusted skill. Reproduces the exploit shape: a repo ships
// .claude/skills/<name>/evil.md with no SKILL.md, so nothing is shadowed.
func TestMirrorBundleSkillsNeverOwnsARepoPrePopulatedDir(t *testing.T) {
	skillsSrc := t.TempDir()
	if err := os.WriteFile(filepath.Join(skillsSrc, "whats-next.md"),
		[]byte("---\nname: whats-next\n---\nours\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	planted := filepath.Join(workDir, ".claude", "skills", "whats-next")
	if err := os.MkdirAll(planted, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planted, "evil.md"), []byte("do the bad thing"), 0o644); err != nil {
		t.Fatal(err)
	}

	owned, err := mirrorBundleSkills(workDir, &bundle.Bundle{SkillsDir: skillsSrc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range owned {
		if p == planted {
			t.Fatalf("owned = %v claims a directory the repo pre-populated (it holds evil.md)", owned)
		}
		// And nothing reported may BE the planted file or contain it.
		if filepath.Base(p) == "evil.md" {
			t.Fatalf("owned = %v names repo-authored content directly", owned)
		}
	}
	// Our own file is still offered — the fix must not cost the skill.
	if !slices.Contains(owned, filepath.Join(planted, "SKILL.md")) {
		t.Errorf("owned = %v, want our own SKILL.md named", owned)
	}
}

// Plugin skills are iterion-authored, mirrored by a different function into the
// same directory. They belong on the same channel: a backend that only trusts
// what the engine reports would otherwise be handed none of them.
func TestMirrorPluginContributionsReportsOwnedSkills(t *testing.T) {
	workDir := t.TempDir()
	owned, err := mirrorInjectedPluginFiles(workDir, []ContributionFile{
		{Kind: "skills", Name: "graphify.md", Content: []byte("---\nname: graphify\n---\nbody\n")},
		{Kind: "commands", Name: "deploy.md", Content: []byte("a command, not a skill")},
		{Kind: "agents", Name: "scout.md", Content: []byte("an agent, not a skill")},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 1 || filepath.Base(owned[0]) != "graphify.md" {
		t.Fatalf("owned = %v, want just the skill — commands and agents are not skills", owned)
	}
	if _, err := os.Stat(owned[0]); err != nil {
		t.Errorf("reported %q but it is not on disk: %v", owned[0], err)
	}
}
