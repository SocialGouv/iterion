package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeFakeMarketplace builds a bare git repository that mimics the
// claude-plugins-official layout, with a single plugin/skill plus an
// external-source plugin so we can exercise both happy and refusal paths.
func makeFakeMarketplace(t *testing.T) (bareURL string) {
	t.Helper()
	root := t.TempDir()

	// Workspace where we author the marketplace contents.
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	// Skill: claude-md-management:claude-md-improver (Anthropic-direct).
	skillDir := filepath.Join(work, "plugins", "claude-md-management", "skills", "claude-md-improver")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillContent := `---
name: claude-md-improver
description: Test installed skill
---

Body.`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// External plugin: present in manifest but with non-"./plugins/..." source.
	externalDir := filepath.Join(work, "plugins", "aikido", "skills", "aikido-scan")
	if err := os.MkdirAll(externalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(externalDir, "SKILL.md"), []byte("external"), 0o644); err != nil {
		t.Fatal(err)
	}

	manifest := `{
  "name": "claude-plugins-official",
  "plugins": [
    {"name": "claude-md-management", "source": "./plugins/claude-md-management"},
    {"name": "aikido", "source": {"source": "url", "url": "https://example.com/aikido.git"}}
  ]
}`
	manifestDir := filepath.Join(work, ".claude-plugin")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "marketplace.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	// Init a regular repo in `work` and commit.
	gitConfig := []string{
		"-c", "user.email=test@example.com",
		"-c", "user.name=Test",
		"-c", "init.defaultBranch=main",
		"-c", "commit.gpgsign=false",
	}
	runGit := func(args ...string) {
		t.Helper()
		full := append(gitConfig, args...)
		cmd := exec.Command("git", full...)
		cmd.Dir = work
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q")
	runGit("add", ".")
	runGit("commit", "-qm", "init")
	// Rename HEAD's branch to "main" to satisfy origin/HEAD lookup after clone.

	// Create a bare clone to serve as the remote.
	bare := filepath.Join(root, "remote.git")
	cmd := exec.Command("git", "clone", "--bare", work, bare)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone bare: %v\n%s", err, out)
	}
	// Set HEAD on the bare repo so origin/HEAD resolves.
	cmd = exec.Command("git", "-C", bare, "symbolic-ref", "HEAD", "refs/heads/main")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git symbolic-ref: %v\n%s", err, out)
	}

	return bare
}

func TestSkillInstall_NamespacedAnthropic(t *testing.T) {
	bare := makeFakeMarketplace(t)
	cache := t.TempDir()
	dest := t.TempDir()

	installed, err := InstallSkillFromMarketplace(
		"claude-md-management:claude-md-improver",
		InstallOptions{
			MarketplaceURL: bare,
			CacheRoot:      cache,
			Destination:    dest,
		},
	)
	if err != nil {
		t.Fatalf("install failed: %v", err)
	}
	want := filepath.Join(dest, "claude-md-management", "skills", "claude-md-improver")
	if installed != want {
		t.Errorf("installed path: got %q, want %q", installed, want)
	}
	content, err := os.ReadFile(filepath.Join(installed, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "claude-md-improver") {
		t.Errorf("SKILL.md content unexpected: %s", content)
	}
}

func TestSkillInstall_BareName(t *testing.T) {
	bare := makeFakeMarketplace(t)
	cache := t.TempDir()
	dest := t.TempDir()

	installed, err := InstallSkillFromMarketplace(
		"claude-md-improver",
		InstallOptions{
			MarketplaceURL: bare,
			CacheRoot:      cache,
			Destination:    dest,
		},
	)
	if err != nil {
		t.Fatalf("install failed: %v", err)
	}
	if !strings.HasSuffix(installed, filepath.Join("claude-md-management", "skills", "claude-md-improver")) {
		t.Errorf("unexpected installed path: %s", installed)
	}
}

func TestSkillInstall_RejectsExternalPlugin(t *testing.T) {
	bare := makeFakeMarketplace(t)
	cache := t.TempDir()
	dest := t.TempDir()

	_, err := InstallSkillFromMarketplace(
		"aikido:aikido-scan",
		InstallOptions{
			MarketplaceURL: bare,
			CacheRoot:      cache,
			Destination:    dest,
		},
	)
	if err == nil {
		t.Fatal("expected refusal for external plugin")
	}
	if !strings.Contains(err.Error(), "external plugin repo") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSkillInstall_RefuseExisting(t *testing.T) {
	bare := makeFakeMarketplace(t)
	cache := t.TempDir()
	dest := t.TempDir()

	if _, err := InstallSkillFromMarketplace(
		"claude-md-management:claude-md-improver",
		InstallOptions{
			MarketplaceURL: bare,
			CacheRoot:      cache,
			Destination:    dest,
		},
	); err != nil {
		t.Fatal(err)
	}

	_, err := InstallSkillFromMarketplace(
		"claude-md-management:claude-md-improver",
		InstallOptions{
			MarketplaceURL: bare,
			CacheRoot:      cache,
			Destination:    dest,
		},
	)
	if err == nil {
		t.Fatal("expected refusal when target exists")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("unexpected error: %v", err)
	}

	// Force overwrite must work.
	if _, err := InstallSkillFromMarketplace(
		"claude-md-management:claude-md-improver",
		InstallOptions{
			MarketplaceURL: bare,
			CacheRoot:      cache,
			Destination:    dest,
			Force:          true,
		},
	); err != nil {
		t.Errorf("force install should succeed: %v", err)
	}
}

func TestMarketplaceCache_Refresh(t *testing.T) {
	bare := makeFakeMarketplace(t)
	cache := t.TempDir()

	if err := ensureMarketplaceClone(cache, bare); err != nil {
		t.Fatal(err)
	}
	// Age the cache so the next call triggers a fetch.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(cache, old, old); err != nil {
		t.Fatal(err)
	}
	if err := ensureMarketplaceClone(cache, bare); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cache)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(info.ModTime()) > marketplaceCacheTTL {
		t.Errorf("cache mtime should have been refreshed; got %v", info.ModTime())
	}
}
