package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// wfWithSkills builds a minimal workflow with one agent node referencing the
// given skills, plus a workflow-level default.
func wfWithSkills(nodeSkills, wfSkills []string) *ir.Workflow {
	return &ir.Workflow{
		Name:   "w",
		Skills: wfSkills,
		Nodes: map[string]ir.Node{
			"a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, Skills: nodeSkills},
		},
	}
}

func TestMirrorLibrarySkills_MirrorsAndHints(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	// Author a global library skill (directory form).
	skillDir := filepath.Join(home, "skills", "changelog-writer")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: changelog-writer\ndescription: Writes changelogs\n---\n# body\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	hints, _, _, err := mirrorLibrarySkills(workDir, "", wfWithSkills([]string{"changelog-writer"}, nil), nil, nil, nil)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	// Mirrored into .claude/skills/<name>/SKILL.md + marker.
	if _, err := os.Stat(filepath.Join(workDir, ".claude", "skills", "changelog-writer", "SKILL.md")); err != nil {
		t.Fatalf("skill not mirrored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".claude", "skills", bundleMirrorMarkerDir, "changelog-writer.SKILL.md.sha256")); err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	if hints["changelog-writer"] != "Writes changelogs" {
		t.Errorf("hint = %q, want description", hints["changelog-writer"])
	}
}

// A skill the target repository pre-empted is still HINTED — claude_code and
// claw read the directory natively, so the agent sees whatever is there — but it
// must not be reported as owned. A backend that passes skills explicitly would
// otherwise hand attacker-authored prompt text over as a trusted skill, which is
// exactly what the explicit-path gate exists to prevent.
func TestMirrorLibrarySkills_ShadowedIsHintedButNotOwned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	src := filepath.Join(home, "skills", "changelog-writer")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: changelog-writer\ndescription: Writes changelogs\n---\n# ours\n"
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// The checkout already ships a same-named skill with different content, so
	// the workspace-wins policy keeps it.
	workDir := t.TempDir()
	repoSkill := filepath.Join(workDir, ".claude", "skills", "changelog-writer")
	if err := os.MkdirAll(repoSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoSkill, "SKILL.md"), []byte("the repo's own"), 0o644); err != nil {
		t.Fatal(err)
	}

	hints, owned, _, err := mirrorLibrarySkills(workDir, "", wfWithSkills([]string{"changelog-writer"}, nil), nil, nil, nil)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if _, ok := hints["changelog-writer"]; !ok {
		t.Error("no hint — the agent still sees the skill, so it should be described")
	}
	if len(owned) != 0 {
		t.Errorf("owned = %v — that content is the repo's, and a backend must not pass it", owned)
	}
	// And the repo's file was genuinely left in place.
	got, _ := os.ReadFile(filepath.Join(repoSkill, "SKILL.md"))
	if string(got) != "the repo's own" {
		t.Errorf("workspace file = %q, want it untouched", got)
	}
}

func TestMirrorLibrarySkills_UnknownRefSkipped(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	workDir := t.TempDir()
	hints, _, _, err := mirrorLibrarySkills(workDir, "", wfWithSkills([]string{"nope"}, nil), nil, nil, nil)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if len(hints) != 0 {
		t.Errorf("hints = %v, want empty for unknown ref", hints)
	}
	// Nothing created.
	if _, err := os.Stat(filepath.Join(workDir, ".claude", "skills", "nope")); !os.IsNotExist(err) {
		t.Error("unknown skill should not have been mirrored")
	}
}

func TestMirrorLibrarySkills_NoRefsNoOp(t *testing.T) {
	hints, _, _, err := mirrorLibrarySkills(t.TempDir(), "", wfWithSkills(nil, nil), nil, nil, nil)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if hints != nil {
		t.Errorf("hints = %v, want nil when no skills referenced", hints)
	}
}

func TestCollectSkillRefs_UnionDedup(t *testing.T) {
	wf := wfWithSkills([]string{"a", "b"}, []string{"b", "c"})
	got := collectSkillRefs(wf)
	// workflow defaults first (b, c), then node refs (a, b→deduped).
	want := map[string]bool{"a": true, "b": true, "c": true}
	if len(got) != 3 {
		t.Fatalf("refs = %v, want 3 unique", got)
	}
	for _, r := range got {
		if !want[r] {
			t.Errorf("unexpected ref %q", r)
		}
	}
}

// #1478: a library skill lands in BOTH discovery shapes — the directory
// form claude_code's Skill tool discovers and the flat alias a prompt
// Reading by path resolves — matching the bundle and plugin tiers, markers
// included. owned reports the directory-form path: the one a backend is
// handed explicitly; the flat alias stays a path-Read convenience.
func TestMirrorLibrarySkills_LandsInBothDiscoveryShapes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	skillDir := filepath.Join(home, "skills", "deploy-target")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: deploy-target\ndescription: Deploy to onyxia\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	_, owned, _, err := mirrorLibrarySkills(workDir, "", wfWithSkills([]string{"deploy-target"}, nil), nil, nil, nil)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	dest := filepath.Join(workDir, ".claude", "skills")
	dirForm := filepath.Join(dest, "deploy-target", "SKILL.md")
	flat := filepath.Join(dest, "deploy-target.md")
	if _, err := os.Stat(dirForm); err != nil {
		t.Fatalf("directory form missing: %v", err)
	}
	if _, err := os.Stat(flat); err != nil {
		t.Fatalf("flat alias missing (parity with bundle/plugin tiers broken): %v", err)
	}
	if len(owned) != 1 || owned[0] != dirForm {
		t.Fatalf("owned = %v, want [%s]", owned, dirForm)
	}
	markerDir := filepath.Join(dest, bundleMirrorMarkerDir)
	for _, m := range []string{"deploy-target.SKILL.md.sha256", "deploy-target.md.sha256"} {
		if _, err := os.Stat(filepath.Join(markerDir, m)); err != nil {
			t.Errorf("marker %s missing: %v", m, err)
		}
	}
}

// The migration case: a workspace where a previous run wrote only
// <name>/SKILL.md (no flat alias, no iterion-wrote sidecar). On the first
// dual-shape run the dir-form entry reads UpToDate (its content still
// hashes to its marker), the flat alias lands fresh, and the dir form
// KEEPS its absent iterion-wrote sidecar — the pruner never touches it,
// so a later source removal leaves a pre-mirror file in place rather than
// pruning an operator's file.
func TestMirrorLibrarySkills_MigrationFromDirOnlyWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	src := filepath.Join(home, "skills", "deploy-target")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: deploy-target\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	dest := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(dest, bundleMirrorMarkerDir)
	preDir := filepath.Join(dest, "deploy-target")
	if err := os.MkdirAll(preDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(preDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	preHash, err := hashFile(filepath.Join(preDir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(markerDir, "deploy-target.SKILL.md.sha256"), []byte(preHash), 0o644); err != nil {
		t.Fatal(err)
	}
	// A pre-#1526 sidecar: present (iterion wrote this dest at some past
	// run) but EMPTY — no recorded form. The first dual-shape pass touches
	// the file UpToDate and upgrades the content, so the pruner resolves
	// the marker to the DIRECTORY form and never to a byte-identical
	// operator file that happens to exist beside it.
	if err := os.WriteFile(filepath.Join(markerDir, "deploy-target.SKILL.md.sha256"+iterionWroteSidecarSuffix), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := mirrorLibrarySkills(workDir, "", wfWithSkills([]string{"deploy-target"}, nil), nil, nil, nil); err != nil {
		t.Fatalf("mirror: %v", err)
	}

	if _, err := os.Stat(filepath.Join(preDir, "SKILL.md")); err != nil {
		t.Fatalf("the pre-existing dir form was pruned or lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "deploy-target.md")); err != nil {
		t.Fatalf("flat alias was not created on migration: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(markerDir, "deploy-target.SKILL.md.sha256"+iterionWroteSidecarSuffix)); err != nil || string(b) != "dir:deploy-target" {
		t.Errorf("the legacy empty sidecar was not upgraded with the recorded form: %q (err %v) — the pruner would fall back to the disk-stat guess", b, err)
	}
	if _, err := os.Stat(filepath.Join(markerDir, "deploy-target.md.sha256"+iterionWroteSidecarSuffix)); err != nil {
		t.Errorf("flat alias should have an iterion-wrote sidecar after Mirrored: %v", err)
	}
}

// A DIRECTORY at a destination path is a shadow, not an error: the
// checkout shipped its own object there and workspace-wins holds. A fatal
// return here aborts every run declaring the skill over a planted
// directory the mirror never needed to touch.
func TestMirrorLibrarySkills_DirectoryAtFlatAliasPathShadows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	src := filepath.Join(home, "skills", "deploy-target")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: deploy-target\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	blocked := filepath.Join(workDir, ".claude", "skills", "deploy-target.md")
	if err := os.MkdirAll(filepath.Join(blocked, "planted"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, owned, _, err := mirrorLibrarySkills(workDir, "", wfWithSkills([]string{"deploy-target"}, nil), nil, nil, nil)
	if err != nil {
		t.Fatalf("a directory at the flat-alias path aborted the pass: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".claude", "skills", "deploy-target", "SKILL.md")); err != nil {
		t.Fatalf("directory form did not land: %v", err)
	}
	if _, err := os.Stat(filepath.Join(blocked, "planted")); err != nil {
		t.Errorf("the workspace's directory was modified: %v", err)
	}
	if len(owned) != 1 {
		t.Errorf("owned = %v, want just the directory-form file", owned)
	}
}

// A declared library skill the store cannot resolve this pass is NOT
// covered (the pass is incomplete, the pruner must not run) — whatever a
// prior pass wrote is un-verifiable exactly then. A ref covered by a fresh
// higher-tier marker (the bundle mirror ran this pass) keeps the pass
// complete.
func TestMirrorLibrarySkills_StoreOutageFlagsIncomplete(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	// The library has NO such skill this run.
	workDir := t.TempDir()
	dest := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(dest, bundleMirrorMarkerDir)

	// Case 1: a stale prior-pass marker (no fresh tier sidecar) — the
	// dangerous middle; the pass is INCOMPLETE.
	preDir := filepath.Join(dest, "own-skill")
	if err := os.MkdirAll(preDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(preDir, "SKILL.md"), []byte("library wrote this\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := hashFile(filepath.Join(preDir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(markerDir, "own-skill.SKILL.md.sha256"), []byte(h), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, complete, err := mirrorLibrarySkills(workDir, "", wfWithSkills([]string{"own-skill"}, nil), nil, nil, nil)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if complete {
		t.Errorf("complete=true despite the store failing to resolve a declared skill — the pruner would delete a still-declared skill")
	}

	// Case 2: the name is covered by a FRESH bundle-tier sidecar (the
	// bundle mirror ran this pass) — the pass IS complete.
	if err := os.WriteFile(filepath.Join(markerDir, "own-skill.SKILL.md.sha256"+tierSidecarSuffix), []byte("bundle"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, complete2, err := mirrorLibrarySkills(workDir, "", wfWithSkills([]string{"own-skill"}, nil), nil, nil, nil)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if !complete2 {
		t.Errorf("complete=false for a fresh higher-tier-covered ref — the pass IS complete")
	}
}
