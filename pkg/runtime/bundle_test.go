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
	if _, err := os.Stat(filepath.Join(dest, "probe.md")); err != nil {
		t.Errorf("probe.md missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "nested", "step.md")); err != nil {
		t.Errorf("nested/step.md missing: %v", err)
	}
}

func TestMirrorBundleSkills_WorkspaceWinsOnCollision(t *testing.T) {
	workDir := t.TempDir()
	dest := filepath.Join(workDir, ".claude", "skills")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "shared.md"), []byte("workspace"), 0o644); err != nil {
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

	got, err := os.ReadFile(filepath.Join(dest, "shared.md"))
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
