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
	hints, _, err := mirrorLibrarySkills(workDir, "", wfWithSkills([]string{"changelog-writer"}, nil), nil, nil)
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

	hints, owned, err := mirrorLibrarySkills(workDir, "", wfWithSkills([]string{"changelog-writer"}, nil), nil, nil)
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
	hints, _, err := mirrorLibrarySkills(workDir, "", wfWithSkills([]string{"nope"}, nil), nil, nil)
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
	hints, _, err := mirrorLibrarySkills(t.TempDir(), "", wfWithSkills(nil, nil), nil, nil)
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
