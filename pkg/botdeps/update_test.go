package botdeps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botlock"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

func TestUpdateRestoresOriginalLockBytesWhenMaterializationFails(t *testing.T) {
	workdir := t.TempDir()
	source := filepath.Join(workdir, "source")
	writeUpdateTestBundle(t, source, "0.1.0", "workflow shared {}\n")
	oldHash, err := bundle.ContentHashDir(source)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(workdir, botlock.FileName)
	original := []byte(fmt.Sprintf("version: 1\ndependencies:\n  shared-planner:\n    source: ./source\n    ref: local-test\n    bundle_sha256: %s\n", oldHash))
	if err := os.WriteFile(lockPath, original, 0o640); err != nil {
		t.Fatal(err)
	}
	writeUpdateTestBundle(t, source, "0.2.0", "workflow refreshed {}\n")
	// The updater writes the new lock before materializing. Make that second
	// step fail, then prove recovery restored bytes and mode rather than YAML
	// reserialization of the old model.
	if err := os.Mkdir(filepath.Join(workdir, ".botz"), 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(context.Background(), UpdateOptions{Workdir: workdir, Name: "shared-planner"}); err == nil {
		t.Fatal("Update succeeded despite an unwritable materialization root")
	}
	got, err := os.ReadFile(lockPath)
	if err != nil || string(got) != string(original) {
		t.Fatalf("restored lock = %q, err=%v; want %q", got, err, original)
	}
	info, err := os.Stat(lockPath)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("restored mode = %v, err=%v; want 0640", info.Mode(), err)
	}
}

func writeUpdateTestBundle(t *testing.T, dir, version, workflow string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(workflow), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(fmt.Sprintf("name: shared-planner\nversion: %s\nschema_version: 1\nenabled: false\n", version)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateStageLeavesCatalogsWhileSyncRetainsDefaultRegeneration(t *testing.T) {
	workdir := t.TempDir()
	source := filepath.Join(workdir, "source")
	writeUpdateTestBundle(t, source, "0.1.0", "workflow shared {}\n")
	hash, err := bundle.ContentHashDir(source)
	if err != nil {
		t.Fatal(err)
	}
	owner := filepath.Join(workdir, "bots", "router")
	if err := os.MkdirAll(filepath.Join(owner, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"main.bot": "workflow router {}\n", "manifest.yaml": "name: router\nschema_version: 1\n",
		"iterion-bot-catalog-static.md": "<!-- ITERION:CATALOG:GENERATED:BEGIN -->\nstale\n<!-- ITERION:CATALOG:GENERATED:END -->\n",
		"skills/iterion-bot-catalog.md": "untouched\n",
	} {
		if err := os.WriteFile(filepath.Join(owner, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dep := botlock.Dependency{Source: "./source", Ref: "local-test", BundleSHA256: hash}
	stage := t.TempDir()
	if _, err := StageOne(t.Context(), workdir, stage, "shared-planner", dep); err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(owner, "skills", "iterion-bot-catalog.md")
	body, err := os.ReadFile(catalogPath)
	if err != nil || string(body) != "untouched\n" {
		t.Fatalf("private staging changed live catalog: %q %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".botz")); !os.IsNotExist(err) {
		t.Fatalf("private staging touched live cache: %v", err)
	}
	if _, err := SyncOne(t.Context(), workdir, "shared-planner", dep); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(catalogPath)
	if err != nil || !strings.Contains(string(body), "## The team") {
		t.Fatalf("normal sync no longer regenerates catalogs: %q %v", body, err)
	}
}
