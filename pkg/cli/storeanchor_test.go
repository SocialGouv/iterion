package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A bot that lives inside the project it drives must not key the run store on
// its own subdirectory: the studio, the board and every follow-up command
// resolve the store from the working directory, so a run launched that way was
// written somewhere none of them ever look.
func TestStoreAnchorDir_BotInsideProjectResolvesProjectStore(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	project := t.TempDir()
	projectStore := filepath.Join(project, store.StoreDirName)
	if err := os.MkdirAll(filepath.Join(projectStore, "runs"), 0o755); err != nil {
		t.Fatalf("seed project store: %v", err)
	}
	botDir := filepath.Join(project, "bots", "town-dev")
	if err := os.MkdirAll(botDir, 0o755); err != nil {
		t.Fatalf("seed bot dir: %v", err)
	}
	bot := filepath.Join(botDir, "main.bot")

	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	if err := os.Chdir(project); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	got := store.ResolveStoreDir(storeAnchorDir(bot), "")
	// t.TempDir may hand back a symlinked path (/var -> /private/var on
	// macOS); compare resolved forms so the assertion is about the anchor.
	want, err := filepath.EvalSymlinks(projectStore)
	if err != nil {
		t.Fatalf("evalsymlinks want: %v", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(got); resolveErr == nil {
		got = resolved
	}
	if got != want {
		t.Fatalf("run store anchored outside the project: got %q, want %q", got, want)
	}
}

func TestStoreAnchorDir_UsesWorkingDirectoryEvenWithoutBotPath(t *testing.T) {
	project := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	if err := os.Chdir(project); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	got, err := filepath.EvalSymlinks(storeAnchorDir(""))
	if err != nil {
		t.Fatalf("evalsymlinks got: %v", err)
	}
	want, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatalf("evalsymlinks want: %v", err)
	}
	if got != want {
		t.Fatalf("anchor should be the working directory: got %q, want %q", got, want)
	}
}
