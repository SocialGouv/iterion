package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

func TestBotsSyncMaterializesAndVerifiesLock(t *testing.T) {
	workdir := t.TempDir()
	source := filepath.Join(workdir, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "main.bot"), []byte("workflow shared {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := "name: shared-planner\nversion: test\nschema_version: 1\nenabled: false\n"
	if err := os.WriteFile(filepath.Join(source, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := bundle.ContentHashDir(source)
	if err != nil {
		t.Fatal(err)
	}
	lock := fmt.Sprintf("version: 1\ndependencies:\n  shared-planner:\n    source: ./source\n    ref: local-test\n    bundle_sha256: %s\n", hash)
	if err := os.WriteFile(filepath.Join(workdir, "bots.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}

	first, err := BotsSync(context.Background(), workdir)
	if err != nil {
		t.Fatalf("BotsSync: %v", err)
	}
	if len(first) != 1 || !first[0].Changed {
		t.Fatalf("first sync = %+v", first)
	}
	second, err := BotsSync(context.Background(), workdir)
	if err != nil {
		t.Fatalf("second BotsSync: %v", err)
	}
	if len(second) != 1 || second[0].Changed {
		t.Fatalf("second sync = %+v", second)
	}
}

func TestBotsSyncDoesNotOverwriteOnHashMismatch(t *testing.T) {
	workdir := t.TempDir()
	source := filepath.Join(workdir, "source")
	target := filepath.Join(workdir, ".botz", "shared-planner")
	for _, dir := range []string{source, target} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{source, target} {
		if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("workflow shared {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("name: shared-planner\nschema_version: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(target, "keep-me")
	if err := os.WriteFile(marker, []byte("known-good"), 0o644); err != nil {
		t.Fatal(err)
	}
	lock := "version: 1\ndependencies:\n  shared-planner:\n    source: ./source\n    ref: local-test\n    bundle_sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"
	if err := os.WriteFile(filepath.Join(workdir, "bots.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := BotsSync(context.Background(), workdir); err == nil {
		t.Fatal("expected mismatch")
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "known-good" {
		t.Fatalf("existing install was changed: body=%q err=%v", body, err)
	}
}
