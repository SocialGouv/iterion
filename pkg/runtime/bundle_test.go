package runtime

import (
	"os"
	"path/filepath"
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
	if err := mirrorBundleSkills(workDir, b, nil); err != nil {
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
	if err := mirrorBundleSkills(workDir, b, nil); err != nil {
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

	if err := mirrorBundleSkills(workDir, &bundle.Bundle{SkillsDir: skillsSrc}, nil); err != nil {
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
	if err := mirrorBundleSkills(workDir, nil, nil); err != nil {
		t.Errorf("nil bundle should be a no-op, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".claude")); err == nil {
		t.Errorf(".claude dir created unnecessarily")
	}
}

func TestMirrorBundleSkills_EmptySkillsDirIsNoop(t *testing.T) {
	workDir := t.TempDir()
	if err := mirrorBundleSkills(workDir, &bundle.Bundle{}, nil); err != nil {
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
	if err := mirrorBundleSkills(workDir, &bundle.Bundle{SkillsDir: skillsSrc}, nil); err != nil {
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
	if err := mirrorBundleSkills(workDir, &bundle.Bundle{SkillsDir: skillsSrc}, nil); err != nil {
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
	if err := mirrorBundleSkills(workDir, &bundle.Bundle{SkillsDir: skillsSrc}, nil); err != nil {
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

	if err := mirrorBundleSkills(workDir, &bundle.Bundle{SkillsDir: skillsSrc}, nil); err != nil {
		t.Fatalf("second mirror: %v", err)
	}
	got, _ := os.ReadFile(destPath)
	if string(got) != "user-edited" {
		t.Errorf("user customisation overwritten: got %q, want %q", string(got), "user-edited")
	}
}
