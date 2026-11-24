package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitializeRepoCreatesExpectedFiles(t *testing.T) {
	root := t.TempDir()
	// Create a rust workspace to trigger detection
	rustDir := filepath.Join(root, "rust")
	if err := os.MkdirAll(rustDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rustDir, "Cargo.toml"), []byte("[workspace]\n"), 0644); err != nil {
		t.Fatal(err)
	}

	report, err := InitializeRepo(root)
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}

	rendered := report.Render()
	if !strings.Contains(rendered, ".claw/") {
		t.Error("expected .claw/ in report")
	}
	if !strings.Contains(rendered, ".claw.json") {
		t.Error("expected .claw.json in report")
	}
	if !strings.Contains(rendered, "created") {
		t.Error("expected 'created' status in report")
	}
	if !strings.Contains(rendered, ".gitignore       created") {
		t.Error("expected '.gitignore       created' in report")
	}
	if !strings.Contains(rendered, "CLAUDE.md        created") {
		t.Error("expected 'CLAUDE.md        created' in report")
	}

	// Verify files exist
	if _, err := os.Stat(filepath.Join(root, ".claw")); err != nil {
		t.Error("expected .claw/ dir to exist")
	}
	if _, err := os.Stat(filepath.Join(root, ".claw.json")); err != nil {
		t.Error("expected .claw.json to exist")
	}
	if _, err := os.Stat(filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Error("expected CLAUDE.md to exist")
	}

	// Verify .claw.json content
	clawContent, _ := os.ReadFile(filepath.Join(root, ".claw.json"))
	if string(clawContent) != starterClawJSON {
		t.Errorf("unexpected .claw.json content: %q", string(clawContent))
	}

	// Verify .gitignore entries
	gitignore, _ := os.ReadFile(filepath.Join(root, ".gitignore"))
	if !strings.Contains(string(gitignore), ".claw/settings.local.json") {
		t.Error("expected .claw/settings.local.json in .gitignore")
	}
	if !strings.Contains(string(gitignore), ".claw/sessions/") {
		t.Error("expected .claw/sessions/ in .gitignore")
	}

	// Verify CLAUDE.md contains detected language
	claudeMD, _ := os.ReadFile(filepath.Join(root, "CLAUDE.md"))
	if !strings.Contains(string(claudeMD), "Languages: Rust.") {
		t.Error("expected 'Languages: Rust.' in CLAUDE.md")
	}
	if !strings.Contains(string(claudeMD), "cargo clippy --workspace --all-targets -- -D warnings") {
		t.Error("expected cargo clippy command in CLAUDE.md")
	}
}

func TestInitializeRepoIdempotent(t *testing.T) {
	root := t.TempDir()
	// Pre-create CLAUDE.md with custom content
	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("custom guidance\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Pre-create .gitignore with one entry
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".claw/settings.local.json\n"), 0644); err != nil {
		t.Fatal(err)
	}

	first, err := InitializeRepo(root)
	if err != nil {
		t.Fatalf("first init failed: %v", err)
	}
	if !strings.Contains(first.Render(), "CLAUDE.md        skipped (already exists)") {
		t.Error("expected CLAUDE.md skipped on first run")
	}

	second, err := InitializeRepo(root)
	if err != nil {
		t.Fatalf("second init failed: %v", err)
	}
	secondRendered := second.Render()
	if !strings.Contains(secondRendered, "skipped (already exists)") {
		t.Error("expected skipped on second run")
	}
	if !strings.Contains(secondRendered, ".gitignore       skipped (already exists)") {
		t.Error("expected .gitignore skipped")
	}

	// CLAUDE.md should not be overwritten
	claudeContent, _ := os.ReadFile(filepath.Join(root, "CLAUDE.md"))
	if string(claudeContent) != "custom guidance\n" {
		t.Errorf("CLAUDE.md was modified: %q", string(claudeContent))
	}

	// .gitignore should not duplicate entries
	gitignore, _ := os.ReadFile(filepath.Join(root, ".gitignore"))
	if count := strings.Count(string(gitignore), ".claw/settings.local.json"); count != 1 {
		t.Errorf("expected 1 occurrence of .claw/settings.local.json, got %d", count)
	}
	if count := strings.Count(string(gitignore), ".claw/sessions/"); count != 1 {
		t.Errorf("expected 1 occurrence of .claw/sessions/, got %d", count)
	}
}

func TestRenderInitClaudeMDDetection(t *testing.T) {
	root := t.TempDir()
	// Create Python and Next.js markers
	if err := os.WriteFile(filepath.Join(root, "pyproject.toml"), []byte("[project]\nname = \"demo\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"dependencies":{"next":"14.0.0","react":"18.0.0"},"devDependencies":{"typescript":"5.0.0"}}`), 0644); err != nil {
		t.Fatal(err)
	}

	rendered := RenderInitClaudeMD(root)
	if !strings.Contains(rendered, "Languages: Python, TypeScript.") {
		t.Errorf("expected Python, TypeScript in languages, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Frameworks/tooling markers: Next.js, React.") {
		t.Errorf("expected Next.js, React in frameworks, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "pyproject.toml") {
		t.Error("expected pyproject.toml mention")
	}
	if !strings.Contains(rendered, "Next.js detected") {
		t.Error("expected Next.js detected note")
	}
}
