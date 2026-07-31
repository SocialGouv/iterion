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

// A run created before the anchor moved must stay resumable: the fallback only
// fires when the caller pinned no store and the current one does not hold it.
func TestLegacyRunStoreDir_FindsPreUpgradeRun(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	project := t.TempDir()
	bot := filepath.Join(project, "bots", "town-dev", "main.bot")
	if err := os.MkdirAll(filepath.Dir(bot), 0o755); err != nil {
		t.Fatalf("seed bot dir: %v", err)
	}
	legacy := store.ResolveStoreDir(filepath.Dir(bot), "")
	if err := os.MkdirAll(filepath.Join(legacy, "runs", "run-1"), 0o755); err != nil {
		t.Fatalf("seed legacy store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "runs", "run-1", "run.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed legacy run: %v", err)
	}
	current := filepath.Join(project, store.StoreDirName)

	if got := legacyRunStoreDir(bot, "", current, "run-1"); got != legacy {
		t.Fatalf("legacy run should be found: got %q, want %q", got, legacy)
	}
	if got := legacyRunStoreDir(bot, "/pinned", current, "run-1"); got != "" {
		t.Fatalf("an explicit --store-dir must win, got %q", got)
	}
	if got := legacyRunStoreDir(bot, "", current, "absent"); got != "" {
		t.Fatalf("unknown run must not fall back, got %q", got)
	}
	if got := legacyRunStoreDir(bot, "", legacy, "run-1"); got != "" {
		t.Fatalf("already-current store must not fall back, got %q", got)
	}
}

// The schedule gate lists live runs from runStoreDir(e.Bot, e.StoreDir) and the
// tick launches through the same call, after chdir'ing into the entry's
// workdir. If the two ever resolved differently, LiveRunsForSchedule would come
// back empty, EvaluateOverlap would always fire, and every entry without an
// explicit --store-dir would silently lose its at-most-one-live guarantee.
func TestRunStoreDir_ScheduleGateAndLaunchAgree(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	workdir := t.TempDir()
	projectStore := filepath.Join(workdir, store.StoreDirName)
	if err := os.MkdirAll(filepath.Join(projectStore, "runs"), 0o755); err != nil {
		t.Fatalf("seed project store: %v", err)
	}
	bot := filepath.Join(workdir, "bots", "sec-audit", "main.bot")
	if err := os.MkdirAll(filepath.Dir(bot), 0o755); err != nil {
		t.Fatalf("seed bot dir: %v", err)
	}

	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	// RunScheduleRun chdirs into e.Workdir before resolving the gate store.
	if err := os.Chdir(workdir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	gate := runStoreDir(bot, "")
	launch := runStoreDir(bot, "")
	if gate != launch {
		t.Fatalf("gate and launch stores diverged: %q vs %q", gate, launch)
	}
	want, err := filepath.EvalSymlinks(projectStore)
	if err != nil {
		t.Fatalf("evalsymlinks want: %v", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(gate); resolveErr == nil {
		gate = resolved
	}
	if gate != want {
		t.Fatalf("schedule gate must see the workdir store: got %q, want %q", gate, want)
	}
}
