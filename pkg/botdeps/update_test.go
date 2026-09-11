package botdeps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
