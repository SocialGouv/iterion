package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/memory"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A workspace can pile up gigabytes of ${PROJECT_SCRATCH_DIR} while never
// running a `worktree: auto` bot. Discovery keyed on worktrees/ alone made
// those stores invisible to --all-projects, so the sweep silently covered
// part of the machine and reported success — found by probing the real
// command with an aged scratch entry, not by reading the code.
//
// Mutation coverage: narrow isSweepableStore back to hasWorktreeDir → the
// scratch-only store is no longer discovered and this fails.
func TestSweepableStoreIncludesScratchOnlyStores(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	scratchOnly := filepath.Join(root, "scratch-only")
	if err := os.MkdirAll(filepath.Join(scratchOnly, "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	worktreeOnly := filepath.Join(root, "worktree-only")
	if err := os.MkdirAll(filepath.Join(worktreeOnly, "worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	neither := filepath.Join(root, "neither")
	if err := os.MkdirAll(neither, 0o755); err != nil {
		t.Fatal(err)
	}

	if !isSweepableStore(scratchOnly) {
		t.Error("a store with scratch but no worktrees is not swept — its scratch grows forever")
	}
	if !isSweepableStore(worktreeOnly) {
		t.Error("a store with worktrees is not swept")
	}
	if isSweepableStore(neither) {
		t.Error("a directory holding neither is treated as a store")
	}
}

// The two store layouts put scratch in different places, and assuming one
// is how a sweep quietly misses half of them: a per-project store keeps
// scratch as a sibling of runs/, while a store INSIDE a workspace
// (<repo>/.iterion) has its scratch in the global data dir, keyed by the
// repo root.
func TestScratchRootForStoreHandlesBothLayouts(t *testing.T) {
	repo := t.TempDir()
	localStore := filepath.Join(repo, ".iterion")

	got := scratchRootForStore(localStore)
	want := memory.WorkspaceScratchDir(repo)
	if got != want {
		t.Errorf("workspace store → %q, want %q (scratch never lives inside a local store)", got, want)
	}
}

// The sweep must report what it would take without taking it, like every
// other half of this command.
func TestSweepStoreScratchDryRunReportsWithoutDeleting(t *testing.T) {
	// A per-project store: scratch is a sibling of runs/, which is the
	// layout --all-projects walks. Redirect the data dir so the test never
	// reads or writes the operator's own ~/.iterion.
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	store := filepath.Join(home, "projects", "-some-workspace")
	entry := filepath.Join(store, "scratch", "old-state")
	if err := os.MkdirAll(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(entry, "blob.bin")
	if err := os.WriteFile(blob, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	for _, p := range []string{blob, entry} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	now := func() time.Time { return time.Now() }
	var r CleanResult
	r.DryRun = true
	sweepStoreScratch([]string{store}, CleanOptions{OlderThan: 168 * time.Hour}, now, &r)

	if len(r.Scratch) != 1 || r.ScratchBytes == 0 {
		t.Fatalf("dry run reported %d entries / %d bytes, want the one it would take", len(r.Scratch), r.ScratchBytes)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Error("dry run deleted the entry")
	}

	// …and with --apply it goes.
	var applied CleanResult
	sweepStoreScratch([]string{store}, CleanOptions{OlderThan: 168 * time.Hour, Apply: true}, now, &applied)
	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Error("--apply left the stale entry behind")
	}
}

// The sandbox's own warm caches (#2069) are durable state the guard
// introduced, and until this they had no reclaim at all: a store whose only
// volume is sandbox-caches was invisible to --all-projects, and no sweep
// walked into it. A machine that only ever ran sandboxed bots is exactly that
// store.
//
// Mutation coverage: drop the sandbox-caches arm of isSweepableStore → the
// cache-only store is no longer discovered and this fails.
func TestSweepableStoreIncludesCacheOnlyStores(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cacheOnly := filepath.Join(root, "cache-only")
	if err := os.MkdirAll(filepath.Join(cacheOnly, "sandbox-caches", ".cache", "go-build"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !isSweepableStore(cacheOnly) {
		t.Error("a store whose only volume is the sandbox cache store is not swept — it grows forever, and --all-projects reports success")
	}
}

// And the sweep reclaims them, by the same age rule as scratch: the paths are
// dot-prefixed and nested (.cache/go-build, .local/share/pnpm), which the
// scratch scan skips by design, so the reclaim reads the store layout from the
// code that CREATES it rather than from a listing.
func TestSweepSandboxCachesReportsThenReclaims(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	cache := filepath.Join(home, "sandbox-caches", ".cache", "go-build", "ab")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	object := filepath.Join(cache, "abcdef-d")
	if err := os.WriteFile(object, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	// A cache that is still warm must survive the same sweep.
	fresh := filepath.Join(home, "sandbox-caches", ".npm", "_cacache")
	if err := os.MkdirAll(fresh, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fresh, "index"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	for _, p := range []string{object, cache, filepath.Dir(cache)} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	now := func() time.Time { return time.Now() }
	var dry CleanResult
	dry.DryRun = true
	sweepSandboxCaches(CleanOptions{OlderThan: 168 * time.Hour}, now, &dry)
	if len(dry.Scratch) != 1 || dry.ScratchBytes < 4096 {
		t.Fatalf("dry run reported %d entries / %d bytes, want the one stale cache it would take", len(dry.Scratch), dry.ScratchBytes)
	}
	if _, err := os.Stat(object); err != nil {
		t.Error("dry run deleted the cache")
	}

	var applied CleanResult
	sweepSandboxCaches(CleanOptions{OlderThan: 168 * time.Hour, Apply: true}, now, &applied)
	// The NAMED path goes — `sandbox-caches/.cache/go-build` — and its
	// parent stays: the store's members are named by the mount plan, so a
	// sweep that took `.cache` would take yarn's cache with go's.
	if _, err := os.Stat(filepath.Dir(cache)); !os.IsNotExist(err) {
		t.Error("--apply left the stale cache behind")
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(cache))); err != nil {
		t.Errorf("--apply took the store member's parent, which holds the OTHER caches: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("--apply took a cache that is still warm")
	}
}

// A path under the store that a run replaced with a SYMLINK is refused, not
// followed: a sweep that followed it would delete wherever the run pointed it,
// with --apply, outside any store.
func TestSweepSandboxCachesRefusesASymlinkedCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	outside := t.TempDir()
	victim := filepath.Join(outside, "keep-me")
	if err := os.WriteFile(victim, []byte("operator data"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "sandbox-caches", ".cache", "go-build")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(victim, old, old); err != nil {
		t.Fatal(err)
	}

	var r CleanResult
	sweepSandboxCaches(CleanOptions{OlderThan: 168 * time.Hour, Apply: true}, time.Now, &r)
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("the sweep followed a symlinked cache and deleted outside the store: %v", err)
	}
	if len(r.ScratchErrors) == 0 {
		t.Error("a refused path must be reported — a sweep that silently skips is one nobody fixes")
	}
}

// A planted ITERION_HOME — a project `.env` may set any exportable name —
// must not move the roots these sweeps DELETE under: the anchor and the
// fallback resolve on the home the OPERATOR chose
// (store.InheritedIterionDataDir), the same home the mount plan and the
// engine's end-of-run sweep answer to, and no operator home at all refuses
// the sweep — a root nobody can answer for deletes nothing, out loud.
func TestTheCleanSweepsRefuseAPlantedHome(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)

	base := t.TempDir()
	planted := t.TempDir()
	t.Setenv("ITERION_HOME", planted)
	envtrust.MarkPlanted("ITERION_HOME")

	// The raw material: the LIVE resolution follows the plant.
	if live := memory.WorkspaceScratchDir(base); !strings.HasPrefix(live, planted+string(filepath.Separator)) {
		t.Fatalf("fixture: the live resolution did not follow the planted home (%s) — the witness would prove nothing", live)
	}

	// A store under the planted data dir maps to no planted root.
	plantedStore := filepath.Join(planted, "projects", "k")
	if err := os.MkdirAll(plantedStore, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := scratchRootForStore(plantedStore); strings.HasPrefix(got, planted+string(filepath.Separator)) {
		t.Errorf("scratchRootForStore followed the planted ITERION_HOME: %s", got)
	}

	// The cache sweep scans and deletes nothing under the planted home.
	object := filepath.Join(planted, "sandbox-caches", ".cache", "go-build", "ab", "abcdef-d")
	if err := os.MkdirAll(filepath.Dir(object), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(object, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(object, old, old); err != nil {
		t.Fatal(err)
	}
	var r CleanResult
	sweepSandboxCaches(CleanOptions{OlderThan: 168 * time.Hour, Apply: true}, time.Now, &r)
	if _, err := os.Stat(object); err != nil {
		t.Error("the cache sweep followed the planted ITERION_HOME and deleted under it")
	}

	// No operator home at all: both sweeps refuse — the store mapping
	// answers "" and the cache sweep says so instead of deleting.
	envtrust.MarkPlanted(store.HomeEnvName())
	if got := scratchRootForStore(plantedStore); got != "" {
		t.Errorf("scratchRootForStore resolved %q with no operator home — the refusal must fail closed on \"\"", got)
	}
	var r2 CleanResult
	sweepSandboxCaches(CleanOptions{OlderThan: 168 * time.Hour, Apply: true}, time.Now, &r2)
	if len(r2.Scratch) != 0 {
		t.Errorf("the cache sweep deleted %d entr(y/ies) with no operator home to answer for: %v", len(r2.Scratch), r2.Scratch)
	}
	if len(r2.ScratchErrors) == 0 {
		t.Error("the cache sweep deleted nothing but said nothing — a refused sweep nobody hears about is one nobody fixes")
	}
}
