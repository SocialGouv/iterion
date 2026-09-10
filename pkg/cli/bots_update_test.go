package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/botlock"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

func TestBotsUpdateRefreshesLockAndMaterialization(t *testing.T) {
	workdir := t.TempDir()
	source := filepath.Join(workdir, "source")
	writeUpdateBundle(t, source, "0.1.0", "workflow shared {}\n")
	oldHash, err := bundle.ContentHashDir(source)
	if err != nil {
		t.Fatal(err)
	}
	writeUpdateLock(t, workdir, "./source", "local-test", oldHash)
	if _, err := BotsSync(context.Background(), workdir); err != nil {
		t.Fatal(err)
	}
	writeUpdateBundle(t, source, "0.2.0", "workflow changed {}\n")

	result, err := BotsUpdate(context.Background(), BotsUpdateOptions{Workdir: workdir, Name: "shared-planner"})
	if err != nil {
		t.Fatal(err)
	}
	wantHash, err := bundle.ContentHashDir(source)
	if err != nil {
		t.Fatal(err)
	}
	if result.BundleSHA256 != wantHash {
		t.Fatalf("hash = %s, want %s", result.BundleSHA256, wantHash)
	}
	lock, err := botlock.Load(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Dependencies["shared-planner"].BundleSHA256 != wantHash {
		t.Fatalf("lock = %#v", lock.Dependencies["shared-planner"])
	}
	installedHash, err := bundle.ContentHashDir(filepath.Join(workdir, ".botz", "shared-planner"))
	if err != nil || installedHash != wantHash {
		t.Fatalf("installed hash = %s, err=%v", installedHash, err)
	}
}

func TestBotsUpdateRejectsDirtyLocalGitBundle(t *testing.T) {
	repo := t.TempDir()
	source := filepath.Join(repo, "bots", "shared-planner")
	writeUpdateBundle(t, source, "0.1.0", "workflow shared {}\n")
	runUpdateGit(t, repo, "init")
	runUpdateGit(t, repo, "config", "user.email", "test@example.invalid")
	runUpdateGit(t, repo, "config", "user.name", "Test")
	runUpdateGit(t, repo, "add", ".")
	runUpdateGit(t, repo, "commit", "-m", "initial")
	head := strings.TrimSpace(runUpdateGit(t, repo, "rev-parse", "HEAD"))
	hash, err := bundle.ContentHashDir(source)
	if err != nil {
		t.Fatal(err)
	}
	writeUpdateLock(t, repo, ".", head, hash)
	writeUpdateBundle(t, source, "0.2.0", "workflow changed {}\n")

	_, err = BotsUpdate(context.Background(), BotsUpdateOptions{Workdir: repo, Name: "shared-planner"})
	if err == nil || !strings.Contains(err.Error(), "uncommitted bundle changes") {
		t.Fatalf("error = %v", err)
	}
	lock, loadErr := botlock.Load(repo)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if lock.Dependencies["shared-planner"].BundleSHA256 != hash {
		t.Fatalf("dirty update changed lock: %#v", lock.Dependencies["shared-planner"])
	}
}

func TestBotsUpdateRecordsCleanLocalHead(t *testing.T) {
	repo := t.TempDir()
	source := filepath.Join(repo, "bots", "shared-planner")
	writeUpdateBundle(t, source, "0.1.0", "workflow shared {}\n")
	runUpdateGit(t, repo, "init")
	runUpdateGit(t, repo, "config", "user.email", "test@example.invalid")
	runUpdateGit(t, repo, "config", "user.name", "Test")
	runUpdateGit(t, repo, "add", ".")
	runUpdateGit(t, repo, "commit", "-m", "initial")
	head := strings.TrimSpace(runUpdateGit(t, repo, "rev-parse", "HEAD"))
	hash, err := bundle.ContentHashDir(source)
	if err != nil {
		t.Fatal(err)
	}
	writeUpdateLock(t, repo, ".", "stale-ref", hash)

	result, err := BotsUpdate(context.Background(), BotsUpdateOptions{Workdir: repo, Name: "shared-planner"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Ref != head {
		t.Fatalf("ref = %q, want HEAD %q", result.Ref, head)
	}
}

func writeUpdateBundle(t *testing.T, dir, version, source string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("name: shared-planner\nversion: %s\nschema_version: 1\nenabled: false\n", version)
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeUpdateLock(t *testing.T, workdir, source, ref, hash string) {
	t.Helper()
	body := fmt.Sprintf("version: 1\ndependencies:\n  shared-planner:\n    source: %s\n    ref: %s\n    path: bots/shared-planner\n    bundle_sha256: %s\n", source, ref, hash)
	if source == "./source" {
		body = fmt.Sprintf("version: 1\ndependencies:\n  shared-planner:\n    source: %s\n    ref: %s\n    bundle_sha256: %s\n", source, ref, hash)
	}
	if err := os.WriteFile(filepath.Join(workdir, botlock.FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runUpdateGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := gittest.Cmd(dir, args...)
	body, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, body)
	}
	return string(body)
}
