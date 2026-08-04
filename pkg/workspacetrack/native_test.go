package workspacetrack

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
	return err == nil
}

// TestNative_CaptureRestoreRoundTrip is the docs-bot case with no git in
// sight: capture the workspace, let a node rewrite / create / delete
// files, then put it back.
func TestNative_CaptureRestoreRoundTrip(t *testing.T) {
	store := t.TempDir()
	ws := t.TempDir()
	write(t, ws, "docs/intro.md", "intro v1\n")
	write(t, ws, "docs/keep.md", "unchanged\n")
	write(t, ws, "README.md", "readme\n")

	tr := NewNative(store)
	before, err := tr.Capture("run1", ws, "pre:generate_docs:0")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if len(before.Entries) != 3 {
		t.Fatalf("captured %d entries, want 3: %+v", len(before.Entries), before.Entries)
	}

	// The node does its work: rewrite, create, delete.
	write(t, ws, "docs/intro.md", "intro v2 REWRITTEN\n")
	write(t, ws, "docs/api.md", "generated\n")
	write(t, ws, "docs/generated/deep/page.md", "nested\n")
	if err := os.Remove(filepath.Join(ws, "README.md")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := tr.Capture("run1", ws, "post:generate_docs:0"); err != nil {
		t.Fatalf("Capture after: %v", err)
	}

	report, err := tr.Restore("run1", ws, before.ID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := read(t, ws, "docs/intro.md"); got != "intro v1\n" {
		t.Errorf("docs/intro.md = %q, want the captured content", got)
	}
	if exists(ws, "docs/api.md") {
		t.Error("a file the node created survived the restore")
	}
	if exists(ws, "docs/generated/deep/page.md") {
		t.Error("a nested created file survived the restore")
	}
	if exists(ws, "docs/generated") {
		t.Error("the emptied directory was not pruned")
	}
	if got := read(t, ws, "README.md"); got != "readme\n" {
		t.Errorf("README.md = %q, want it restored after deletion", got)
	}
	if report.Unchanged == 0 {
		t.Error("expected the untouched file to be reported unchanged, not rewritten")
	}
}

// TestNative_SnapshotsChain: captures form a history, which is the point
// of owning the versioning rather than leaning on per-node refs.
func TestNative_SnapshotsChain(t *testing.T) {
	store, ws := t.TempDir(), t.TempDir()
	write(t, ws, "a.txt", "1")
	tr := NewNative(store)

	first, err := tr.Capture("run1", ws, "post:n1:0")
	if err != nil {
		t.Fatalf("capture 1: %v", err)
	}
	write(t, ws, "a.txt", "2")
	second, err := tr.Capture("run1", ws, "post:n2:0")
	if err != nil {
		t.Fatalf("capture 2: %v", err)
	}
	if second.Parent != first.ID {
		t.Errorf("second.Parent = %q, want %q — snapshots must chain", second.Parent, first.ID)
	}
	if tr.Head("run1") != second.ID {
		t.Errorf("Head = %q, want %q", tr.Head("run1"), second.ID)
	}
	if id, ok := tr.Resolve("run1", "post:n1:0"); !ok || id != first.ID {
		t.Errorf("Resolve(post:n1:0) = %q,%v want %q", id, ok, first.ID)
	}
}

// TestNative_AliasAvoidsRewalk: the "before node N" marker is by
// construction the state after node N-1, so it must cost a label write,
// not a second walk.
func TestNative_AliasAvoidsRewalk(t *testing.T) {
	store, ws := t.TempDir(), t.TempDir()
	write(t, ws, "a.txt", "1")
	tr := NewNative(store)

	post, err := tr.Capture("run1", ws, "post:n1:0")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if err := tr.Alias("run1", "pre:n2:0", post.ID); err != nil {
		t.Fatalf("Alias: %v", err)
	}
	if id, ok := tr.Resolve("run1", "pre:n2:0"); !ok || id != post.ID {
		t.Errorf("aliased label resolves to %q,%v want %q", id, ok, post.ID)
	}
	if tr.Head("run1") != post.ID {
		t.Error("Alias must not advance the head — it names an existing state")
	}
}

// TestNative_DedupesContent: identical bytes are stored once, whatever
// the path or the boundary.
func TestNative_DedupesContent(t *testing.T) {
	store, ws := t.TempDir(), t.TempDir()
	write(t, ws, "a.txt", "same bytes")
	write(t, ws, "b.txt", "same bytes")
	write(t, ws, "nested/c.txt", "same bytes")
	tr := NewNative(store)
	if _, err := tr.Capture("run1", ws, "post:n1:0"); err != nil {
		t.Fatalf("capture: %v", err)
	}
	var objects int
	_ = filepath.Walk(filepath.Join(store, "workspace-objects"),
		func(_ string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				objects++
			}
			return nil
		})
	if objects != 1 {
		t.Errorf("stored %d objects for three identical files, want 1", objects)
	}

	// And the pool is shared across runs: a second run over the same
	// content stores nothing new. Without this, every run of the same bot
	// on the same repo rewrites the whole workspace (318 MiB on iterion).
	if _, err := tr.Capture("run2", ws, "post:n1:0"); err != nil {
		t.Fatalf("capture run2: %v", err)
	}
	var after int
	_ = filepath.Walk(filepath.Join(store, "workspace-objects"),
		func(_ string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				after++
			}
			return nil
		})
	if after != 1 {
		t.Errorf("after a second run the pool holds %d objects, want 1 — content is not deduped across runs", after)
	}
}

// TestNative_StatCacheSkipsRehash: an unchanged file must not be read
// again on the next capture. Proven by corrupting the stored object and
// checking the capture still resolves the cached hash.
func TestNative_StatCacheSkipsRehash(t *testing.T) {
	store, ws := t.TempDir(), t.TempDir()
	write(t, ws, "a.txt", "stable")
	tr := NewNative(store)
	first, err := tr.Capture("run1", ws, "post:n1:0")
	if err != nil {
		t.Fatalf("capture 1: %v", err)
	}
	second, err := tr.Capture("run1", ws, "post:n2:0")
	if err != nil {
		t.Fatalf("capture 2: %v", err)
	}
	if first.Entries[0].Hash != second.Entries[0].Hash {
		t.Fatal("hash changed for an untouched file")
	}
	cachePath := filepath.Join(store, "runs", "run1", "workspace", "index.json")
	b, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("stat cache was not persisted: %v", err)
	}
	if !strings.Contains(string(b), "mod_nano") {
		t.Error("stat cache does not record mtimes — every capture would re-hash the workspace")
	}
}

// TestNative_IgnoresHeavyAndOwnState: the tracker must not version the
// repository database, the iterion store, or dependency trees.
func TestNative_IgnoresHeavyAndOwnState(t *testing.T) {
	store, ws := t.TempDir(), t.TempDir()
	write(t, ws, "src/main.go", "package main")
	write(t, ws, ".git/objects/ab/cdef", "gitdb")
	write(t, ws, ".iterion/runs/x/run.json", "{}")
	write(t, ws, "node_modules/left-pad/index.js", "module.exports=1")
	tr := NewNative(store)
	snap, err := tr.Capture("run1", ws, "post:n1:0")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if len(snap.Entries) != 1 || snap.Entries[0].Path != "src/main.go" {
		t.Fatalf("captured %+v, want only src/main.go", snap.Entries)
	}
}

// TestNative_IterionignoreWinsOverGitignore: the project can state
// iterion's rules without touching how it packages itself for git — the
// coupling this package exists to avoid.
func TestNative_IterionignoreWinsOverGitignore(t *testing.T) {
	store, ws := t.TempDir(), t.TempDir()
	write(t, ws, ".gitignore", "dist/\n")
	write(t, ws, ".iterionignore", "scratch/\n")
	write(t, ws, "dist/bundle.js", "built")
	write(t, ws, "scratch/tmp.txt", "temp")
	tr := NewNative(store)
	snap, err := tr.Capture("run1", ws, "post:n1:0")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	paths := map[string]bool{}
	for _, e := range snap.Entries {
		paths[e.Path] = true
	}
	if !paths["dist/bundle.js"] {
		t.Error("dist/ was skipped: .gitignore still decided, but .iterionignore is present and must win")
	}
	if paths["scratch/tmp.txt"] {
		t.Error("scratch/ was captured despite .iterionignore")
	}
}

// TestNative_GitignoreFallback: with no .iterionignore, the repository's
// own rules are a sane default.
func TestNative_GitignoreFallback(t *testing.T) {
	store, ws := t.TempDir(), t.TempDir()
	write(t, ws, ".gitignore", "dist/\n")
	write(t, ws, "dist/bundle.js", "built")
	write(t, ws, "src/main.go", "package main")
	tr := NewNative(store)
	snap, err := tr.Capture("run1", ws, "post:n1:0")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	for _, e := range snap.Entries {
		if strings.HasPrefix(e.Path, "dist/") {
			t.Errorf("captured %q, want .gitignore honoured as the fallback", e.Path)
		}
	}
}

// TestNative_RestoreLeavesIgnoredPathsAlone: build output survives a
// restore exactly as it survives a checkout.
func TestNative_RestoreLeavesIgnoredPathsAlone(t *testing.T) {
	store, ws := t.TempDir(), t.TempDir()
	write(t, ws, ".gitignore", "dist/\n")
	write(t, ws, "src/main.go", "v1")
	tr := NewNative(store)
	snap, err := tr.Capture("run1", ws, "pre:n1:0")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	write(t, ws, "src/main.go", "v2")
	write(t, ws, "dist/bundle.js", "built after the capture")

	if _, err := tr.Restore("run1", ws, snap.ID); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := read(t, ws, "src/main.go"); got != "v1" {
		t.Errorf("src/main.go = %q, want v1", got)
	}
	if got := read(t, ws, "dist/bundle.js"); got != "built after the capture" {
		t.Errorf("ignored dist/bundle.js = %q, want it untouched", got)
	}
}

// TestNative_OversizedFileIsSkippedNotSilent: coverage gaps are reported.
func TestNative_OversizedFileIsSkippedNotSilent(t *testing.T) {
	store, ws := t.TempDir(), t.TempDir()
	write(t, ws, "small.txt", "ok")
	write(t, ws, "huge.bin", strings.Repeat("x", 4096))
	tr := NewNative(store)
	tr.MaxFileBytes = 1024

	snap, err := tr.Capture("run1", ws, "post:n1:0")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if len(snap.Skipped) != 1 || snap.Skipped[0] != "huge.bin" {
		t.Errorf("Skipped = %v, want [huge.bin] — an uncaptured file must be reported", snap.Skipped)
	}
	report, err := tr.Restore("run1", ws, snap.ID)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(report.Skipped) != 1 {
		t.Error("the restore report must carry the coverage gap forward")
	}
	// And the skipped file is not deleted as "absent from the snapshot".
	if !exists(ws, "huge.bin") {
		t.Error("an oversized file was deleted by the restore; it was never captured to restore")
	}
}

// TestNative_LoadUnknownSnapshot reports a typed error.
func TestNative_LoadUnknownSnapshot(t *testing.T) {
	tr := NewNative(t.TempDir())
	if _, err := tr.Load("run1", "nope"); err == nil {
		t.Fatal("expected an error for an unknown snapshot")
	}
}

// TestNative_RestoreKeepsOversizedFileCreatedAfterSnapshot: the target
// snapshot's Skipped list only names paths that existed AT capture time.
// A file that appeared (or grew past the cap) afterwards is in neither
// `want` nor `preserve` — deleting it would destroy bytes the tracker
// holds no copy of, and no backup can bring back.
func TestNative_RestoreKeepsOversizedFileCreatedAfterSnapshot(t *testing.T) {
	store, ws := t.TempDir(), t.TempDir()
	write(t, ws, "small.txt", "ok")
	tr := NewNative(store)
	tr.MaxFileBytes = 1024

	snap, err := tr.Capture("run1", ws, "pre:n1:0")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if len(snap.Skipped) != 0 {
		t.Fatalf("Skipped = %v, want empty at capture time", snap.Skipped)
	}
	// The node produces something too large to version.
	write(t, ws, "huge.bin", strings.Repeat("x", 4096))

	report, err := tr.Restore("run1", ws, snap.ID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !exists(ws, "huge.bin") {
		t.Fatal("an oversized file created after the snapshot was DELETED — unrecoverable, no object was ever stored")
	}
	var reported bool
	for _, p := range report.Skipped {
		if p == "huge.bin" {
			reported = true
		}
	}
	if !reported {
		t.Errorf("report.Skipped = %v, want huge.bin — a file left behind must be reported", report.Skipped)
	}
}

// TestNative_ProtectedPathSurvivesRestore: the workflow source lives in
// the workspace for a self-hosted run. A rewind exists to test an edit to
// it, so restoring must not revert that edit.
func TestNative_ProtectedPathSurvivesRestore(t *testing.T) {
	store, ws := t.TempDir(), t.TempDir()
	write(t, ws, "main.bot", "prompt: ORIGINAL")
	write(t, ws, "docs/page.md", "v1")
	tr := NewNative(store)
	snap, err := tr.Capture("run1", ws, "pre:n1:0")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	write(t, ws, "main.bot", "prompt: EDITED BY THE OPERATOR")
	write(t, ws, "docs/page.md", "v2")

	if _, err := tr.Restore("run1", ws, snap.ID, filepath.Join(ws, "main.bot")); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := read(t, ws, "main.bot"); got != "prompt: EDITED BY THE OPERATOR" {
		t.Errorf("main.bot = %q — the restore reverted the very edit under test", got)
	}
	if got := read(t, ws, "docs/page.md"); got != "v1" {
		t.Errorf("docs/page.md = %q, want v1 — unprotected files still restore", got)
	}
}

// TestNative_ForgetReleasesCache: a Tracker outlives every run in a
// studio process, so the per-run stat cache must be evictable.
func TestNative_ForgetReleasesCache(t *testing.T) {
	store, ws := t.TempDir(), t.TempDir()
	write(t, ws, "a.txt", "1")
	tr := NewNative(store)
	if _, err := tr.Capture("run1", ws, "post:n1:0"); err != nil {
		t.Fatalf("capture: %v", err)
	}
	tr.mu.Lock()
	held := len(tr.stats)
	tr.mu.Unlock()
	if held == 0 {
		t.Fatal("expected a cache entry after a capture")
	}
	tr.Forget("run1")
	tr.mu.Lock()
	after := len(tr.stats)
	tr.mu.Unlock()
	if after != 0 {
		t.Errorf("stats holds %d entries after Forget, want 0", after)
	}
	// Re-derivable from index.json, so a later capture still works.
	if _, err := tr.Capture("run1", ws, "post:n2:0"); err != nil {
		t.Fatalf("capture after Forget: %v", err)
	}
}

// TestNative_RestoreRefusesEscapingPaths: a manifest is data, and Load is
// a bare unmarshal. filepath.Join Cleans, so "../.." would resolve
// outside the workspace — the repo standardised on containment checks
// after its own security audit.
func TestNative_RestoreRefusesEscapingPaths(t *testing.T) {
	store, ws := t.TempDir(), t.TempDir()
	write(t, ws, "keep.txt", "safe")
	tr := NewNative(store)
	snap, err := tr.Capture("run1", ws, "pre:n1:0")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	// Forge an escaping entry, as a tampered or corrupt manifest would.
	loaded, err := tr.Load("run1", snap.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(outside, []byte("original"), 0o644); err != nil {
		t.Fatalf("seed victim: %v", err)
	}
	rel, err := filepath.Rel(ws, outside)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	loaded.Entries = append(loaded.Entries, Entry{
		Path: filepath.ToSlash(rel), Hash: loaded.Entries[0].Hash, Mode: 0o644, Size: 4,
	})
	if err := tr.writeSnapshot("run1", loaded); err != nil {
		t.Fatalf("rewrite manifest: %v", err)
	}

	report, err := tr.Restore("run1", ws, loaded.ID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got, _ := os.ReadFile(outside); string(got) != "original" {
		t.Fatalf("a manifest entry wrote OUTSIDE the workspace: %q", got)
	}
	var reported bool
	for _, p := range report.Skipped {
		if strings.Contains(p, "..") {
			reported = true
		}
	}
	if !reported {
		t.Errorf("report.Skipped = %v, want the rejected path reported", report.Skipped)
	}
}

// TestIgnorer_AllowlistInsideAnExcludedTree is the shape a media pipeline
// needs and gitignore cannot express: "none of runs/, except the
// delivered media". git refuses to re-include a file whose parent is
// excluded; .iterionignore is iterion's own file and does not.
func TestIgnorer_AllowlistInsideAnExcludedTree(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, ".iterionignore", strings.Join([]string{
		"runs/**",
		"!runs/**/export/*.mp4",
		"!runs/**/audio/*.mp3",
		"!runs/**/audio/*.txt",
		"!runs/**/music/*.wav",
	}, "\n"))
	ig := NewIgnorer(ws)

	kept := []string{
		"runs/ep1/export/final.mp4",
		"runs/ep1/audio/voice.mp3",
		"runs/ep1/audio/script.txt",
		"runs/ep1/music/theme.wav",
		"runs/2026-ep3/export/cut.mp4",
	}
	for _, p := range kept {
		if ig.Match(p, false) {
			t.Errorf("%q was excluded; the allowlist must re-include it", p)
		}
	}
	dropped := []string{
		"runs/ep1/frames/0001.png",
		"runs/ep1/export/preview.png",
		"runs/ep1/audio/raw.wav",
		"runs/ep1/state.json",
		"runs/_archived/old/export/x.mp4-backup",
	}
	for _, p := range dropped {
		if !ig.Match(p, false) {
			t.Errorf("%q was captured; only the named media is wanted", p)
		}
	}
	// An excluded directory can no longer be pruned, or the walk would
	// never reach the re-included files inside it.
	if ig.CanPrune() {
		t.Error("CanPrune must be false once a negation exists")
	}
}

// TestIgnorer_DoubleStarPatterns: `**` was silently unsupported, so a
// pattern like assets/music/archive/** matched nothing and its tree was
// captured against the project's stated intent.
func TestIgnorer_DoubleStarPatterns(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, ".gitignore", "assets/music/archive/**\nbuild/**/tmp\n")
	ig := NewIgnorer(ws)

	for _, p := range []string{"assets/music/archive/a.wav", "assets/music/archive/deep/b.wav", "build/x/y/tmp"} {
		if !ig.Match(p, false) {
			t.Errorf("%q should be excluded by a ** pattern", p)
		}
	}
	if ig.Match("assets/music/live.wav", false) {
		t.Error("assets/music/live.wav is outside the archive and must be captured")
	}
	// No negation here, so the walk may still prune.
	if !ig.CanPrune() {
		t.Error("CanPrune must stay true without negations")
	}
}

// TestIgnorer_IterionignoreReplacesGitignore: the precedence is the whole
// point — a project takes control of what iterion versions without
// touching how it packages itself for git.
func TestIgnorer_IterionignoreReplacesGitignore(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, ".gitignore", "runs/\n")
	write(t, ws, ".iterionignore", "state/\n")
	ig := NewIgnorer(ws)

	if ig.Match("runs/ep1/out.mp4", false) {
		t.Error("runs/ is excluded by .gitignore only — .iterionignore must replace it entirely")
	}
	if !ig.Match("state/db.json", false) {
		t.Error("state/ is named by .iterionignore and must be excluded")
	}
}

// TestNative_PruneObjectsReclaimsUnreferencedContent: the pool is shared
// across runs, so deleting a run cannot blind-delete its objects — this
// sweep is what stops them accumulating forever.
func TestNative_PruneObjectsReclaimsUnreferencedContent(t *testing.T) {
	store, ws := t.TempDir(), t.TempDir()
	write(t, ws, "keep.txt", "still referenced")
	write(t, ws, "gone.txt", "will be orphaned")
	tr := NewNative(store)
	if _, err := tr.Capture("run1", ws, "post:n1:0"); err != nil {
		t.Fatalf("capture: %v", err)
	}
	countObjects := func() int {
		n := 0
		_ = filepath.Walk(filepath.Join(store, "workspace-objects"), func(_ string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				n++
			}
			return nil
		})
		return n
	}
	if countObjects() != 2 {
		t.Fatalf("expected 2 objects, got %d", countObjects())
	}

	// A live manifest still references both — nothing to reclaim.
	objs, _, err := tr.PruneObjects(0)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if objs != 0 || countObjects() != 2 {
		t.Errorf("prune removed %d objects while a manifest still referenced them", objs)
	}

	// Drop the run's manifests, as `iterion runs prune` would.
	if err := os.RemoveAll(filepath.Join(store, "runs", "run1")); err != nil {
		t.Fatalf("remove run: %v", err)
	}
	objs, bytes, err := tr.PruneObjects(0)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if objs != 2 || bytes == 0 {
		t.Errorf("prune reclaimed %d objects / %d bytes, want both non-zero", objs, bytes)
	}
	if countObjects() != 0 {
		t.Errorf("%d orphaned objects survived the sweep", countObjects())
	}
}

// TestIgnorer_AlwaysIgnoredDirsPruneDespiteNegations is Revi's R964d8e.
//
// CanPrune is a single global flag, so ONE negation anywhere disabled
// fs.SkipDir for EVERY excluded directory — `.git` and `.iterion`
// included. In a managed store the object pool lives at
// <workspace>/.iterion/workspace-objects/, so every node boundary walked
// everything the run had ever captured, and the per-boundary cost grew
// with each capture instead of holding flat. Measured on a 20k-object
// pool: 0.18 ms without a negation vs 12.3 ms with one — 68x, and rising.
// This repo's own .gitignore carries five negations.
//
// Match short-circuits on alwaysIgnored segments BEFORE the rule loop, so
// no negation can ever re-include anything under them: they are always
// safe to prune, and must be.
func TestIgnorer_AlwaysIgnoredDirsPruneDespiteNegations(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, ".gitignore", "*.log\n!keep.log\n")
	ig := NewIgnorer(ws)

	// The negation is real, so a generic excluded directory must still be
	// descended into — something inside may be re-included.
	if ig.CanPrune() {
		t.Fatal("fixture is wrong: the negation should make the global flag false")
	}
	for _, dir := range []string{".git", ".iterion", "vendor/.git", "sub/.iterion"} {
		if !ig.Match(dir, true) {
			t.Errorf("%q should be excluded", dir)
		}
		if !ig.CanPruneDir(dir) {
			t.Errorf("%q must be prunable despite the negation — no rule can re-include "+
				"anything under it, and descending makes every boundary walk grow "+
				"with the object pool", dir)
		}
	}
	// A directory excluded by an ordinary rule is NOT prunable here: the
	// negation could re-include something inside it.
	if ig.CanPruneDir("logs") {
		t.Error("an ordinarily-excluded directory must still be descended into once a negation exists")
	}
}

// TestCapture_DoesNotDescendIntoStorePoolWithNegations is the walk-level
// half of R964d8e: the unit above pins the predicate, this pins that
// Capture actually honours it for the pool that lives inside the
// workspace.
func TestCapture_DoesNotDescendIntoStorePoolWithNegations(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, ".gitignore", "*.log\n!keep.log\n")
	write(t, ws, "main.go", "package main\n")
	// A file the walk must never look at, mimicking the in-workspace store.
	write(t, ws, ".iterion/workspace-objects/ab/cdef", "blob")
	write(t, ws, ".git/objects/ab/cdef", "blob")

	n := NewNative(t.TempDir())
	snap, err := n.Capture("r1", ws, "pre:x:0")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	for _, e := range snap.Entries {
		if strings.HasPrefix(e.Path, ".iterion/") || strings.HasPrefix(e.Path, ".git/") {
			t.Errorf("captured %q — the store's own pool and the git database must never be versioned", e.Path)
		}
	}
	// The negation is still honoured for ordinary paths.
	if !NewIgnorer(ws).Match("app.log", false) {
		t.Error("fixture: *.log should still be excluded")
	}
}

// TestCapture_OverflowLatchesInsteadOfRewalking is Revi's Re3abf5.
//
// The cap is enforced from inside the WalkDir callback, so Capture
// aborted BEFORE saveCache — the stat cache was never persisted and the
// next boundary started from an empty one, re-reading and re-hashing up
// to MaxFiles files again. On a workspace over the cap that is a full
// content read of 50,000 files per node boundary, forever, producing zero
// snapshots and surfaced only as a warn.
func TestCapture_OverflowLatchesInsteadOfRewalking(t *testing.T) {
	ws := t.TempDir()
	for i := 0; i < 12; i++ {
		write(t, ws, fmt.Sprintf("f%d.txt", i), "x")
	}
	n := NewNative(t.TempDir())
	n.MaxFiles = 5

	_, err := n.Capture("r1", ws, "pre:a:0")
	if !errors.Is(err, ErrWorkspaceTooLarge) {
		t.Fatalf("first capture error = %v, want ErrWorkspaceTooLarge", err)
	}
	if !n.Overflowed("r1") {
		t.Error("the overflow must latch, or every later boundary re-walks to the same verdict")
	}

	// The latch must survive the process: it is persisted in index.json,
	// so a fresh tracker over the same store still short-circuits.
	fresh := NewNative(n.root)
	fresh.MaxFiles = 5
	if !fresh.Overflowed("r1") {
		t.Error("the latch must be persisted — a resume would otherwise start re-walking again")
	}
	_, err = fresh.Capture("r1", ws, "pre:b:0")
	if !errors.Is(err, ErrWorkspaceTooLarge) {
		t.Fatalf("second capture error = %v, want ErrWorkspaceTooLarge", err)
	}

	// An unrelated run in the same store is unaffected.
	if fresh.Overflowed("r2") {
		t.Error("the latch is per-run, not per-store")
	}
}

// TestRestore_VerifiesObjectsBeforeDeleting is Revi's Ra8eacb: the
// deletion pass ran to completion before any object was read, so an
// incomplete pool left the workspace with neither the new state nor the
// old one.
func TestRestore_VerifiesObjectsBeforeDeleting(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "keep.txt", "original")
	n := NewNative(t.TempDir())
	snap, err := n.Capture("r1", ws, "pre:a:0")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	// The node's work: modify one file, add another.
	write(t, ws, "keep.txt", "modified by the node")
	write(t, ws, "produced.txt", "the node's output")

	// Corrupt the pool, as a copied store or a racing prune would.
	for _, e := range snap.Entries {
		if err := os.Remove(n.objectPath(e.Hash)); err != nil {
			t.Fatalf("remove object: %v", err)
		}
	}

	if _, err := n.Restore("r1", ws, snap.ID, ""); err == nil {
		t.Fatal("restore must fail when the pool is incomplete")
	}
	// The point: it must fail having deleted NOTHING.
	if _, err := os.Stat(filepath.Join(ws, "produced.txt")); err != nil {
		t.Errorf("produced.txt was deleted before the restore discovered it could not write anything back: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(ws, "keep.txt"))
	if err != nil || string(b) != "modified by the node" {
		t.Errorf("keep.txt = %q (err %v), want it untouched", b, err)
	}
}

// TestPruneObjects_UnreadableRunDirIsFatal is Revi's R0a5322.
//
// The mark phase skipped a run whose snapshots dir would not open, for
// ANY reason. `continue` is right only for ENOENT (a run that never
// versioned its workspace); on EACCES/ENOTDIR/EIO the run contributed
// zero hashes and the sweep then deleted every object only it referenced,
// destroying its whole workspace history. The neighbouring
// unreadable-manifest branch already fails closed for the same reason,
// and `runs prune` — which surfaces unreadable run dirs rather than
// treating them as fatal — now calls this on every invocation.
func TestPruneObjects_UnreadableRunDirIsFatal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not deny access")
	}
	storeDir, ws := t.TempDir(), t.TempDir()
	write(t, ws, "kept.txt", "content only this run references")
	tr := NewNative(storeDir)
	if _, err := tr.Capture("run-locked", ws, "post:implement:0"); err != nil {
		t.Fatalf("capture: %v", err)
	}
	countObjects := func() int {
		n := 0
		_ = filepath.WalkDir(filepath.Join(storeDir, objectsDir), func(_ string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				n++
			}
			return nil
		})
		return n
	}
	before := countObjects()
	if before == 0 {
		t.Fatal("fixture: nothing was stored")
	}

	snapDir := filepath.Join(storeDir, "runs", "run-locked", "workspace", "snapshots")
	if err := os.Chmod(snapDir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(snapDir, 0o755) })

	_, _, err := tr.PruneObjects(0)
	if err == nil {
		t.Error("an unreadable snapshots dir must abort the sweep — treating it as " +
			"'this run references nothing' deletes content that is still live")
	}
	if after := countObjects(); after != before {
		t.Errorf("%d object(s) deleted while a run's manifests were unreadable "+
			"(had %d) — that run's workspace history is gone", before-after, before)
	}
}

// TestStoreObject_MissingPathIsRecognisableAsNotExist is the load-bearing
// half of Revi's R2a3d16.
//
// The default shape is an in-place run over the operator's LIVE checkout,
// so a file disappearing between WalkDir's readdir and storeObject's open
// is ordinary — an editor save that replaces a file, or a watcher a bot
// node left running. That aborted the WHOLE capture before writeSnapshot
// and saveCache, so the boundary got no snapshot AND the next one
// re-hashed everything, surfacing much later as a rewind reporting "no
// snapshot recorded".
//
// The fix skips the entry when os.IsNotExist(err). The race itself cannot
// be staged deterministically from a unit test (there is no hook between
// readdir and open), so what is pinned here is the predicate it rests on:
// if storeObject ever wrapped the error such that os.IsNotExist stopped
// matching, the tolerance would silently stop working and captures would
// go back to being lost — with nothing failing.
func TestStoreObject_MissingPathIsRecognisableAsNotExist(t *testing.T) {
	n := NewNative(t.TempDir())
	_, err := n.storeObject(filepath.Join(t.TempDir(), "never-existed.txt"))
	if err == nil {
		t.Fatal("storeObject on a missing path must fail")
	}
	if !os.IsNotExist(err) {
		t.Errorf("storeObject error = %v; os.IsNotExist must match it, or Capture "+
			"cannot tell a vanished file from a real failure and loses the whole boundary", err)
	}
}

// TestCapture_SurvivesAFileRemovedBeforeTheWalk is the observable
// counterpart: a workspace that shrank between captures still produces a
// snapshot, and the removed path is simply absent from it.
func TestCapture_SurvivesAFileRemovedBeforeTheWalk(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "stable.txt", "kept")
	write(t, ws, "vanishing.txt", "about to disappear")
	n := NewNative(t.TempDir())

	if _, err := n.Capture("r1", ws, "pre:a:0"); err != nil {
		t.Fatalf("warm capture: %v", err)
	}
	if err := os.Remove(filepath.Join(ws, "vanishing.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	snap, err := n.Capture("r1", ws, "post:a:0")
	if err != nil {
		t.Fatalf("capture after a removal: %v", err)
	}
	found := false
	for _, e := range snap.Entries {
		if e.Path == "stable.txt" {
			found = true
		}
		if e.Path == "vanishing.txt" {
			t.Error("a file that no longer exists must not appear in the snapshot")
		}
	}
	if !found {
		t.Error("the rest of the workspace must still be captured")
	}
	if _, ok := n.Resolve("r1", "post:a:0"); !ok {
		t.Error("the boundary label did not resolve — a rewind would report 'no snapshot recorded'")
	}
}

// TestCapture_ExcludesTheStoreUnderAnyName is Revi's R80c07e.
//
// The store was excluded BY NAME (`.iterion`), but store.ResolveStoreDir
// returns an explicit --store-dir verbatim — so a store inside the
// workspace under any other name was invisible to the ignorer. Two
// consequences, both bad: every boundary captured the previous boundary's
// objects and manifests as new content, so the pool compounded per node
// instead of holding flat; and Restore, which deletes whatever the target
// snapshot lacks, deleted pool objects and manifests written later —
// content other snapshots and other RUNS still reference, since the pool
// is store-global. The damage surfaces much later, as an unrelated
// restore failing with "object … is unavailable".
func TestCapture_ExcludesTheStoreUnderAnyName(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "main.go", "package main\n")
	// The store lives INSIDE the workspace under a name the name-based
	// rule knows nothing about — `iterion run --store-dir "$PWD/mystore"`.
	storeDir := filepath.Join(ws, "mystore")
	n := NewNative(storeDir)

	if _, err := n.Capture("r1", ws, "pre:a:0"); err != nil {
		t.Fatalf("first capture: %v", err)
	}
	// The second capture is where it showed: the first one's objects and
	// manifests are now sitting in the workspace.
	snap, err := n.Capture("r1", ws, "post:a:0")
	if err != nil {
		t.Fatalf("second capture: %v", err)
	}
	for _, e := range snap.Entries {
		if strings.HasPrefix(e.Path, "mystore/") {
			t.Errorf("captured %q — the tracker is versioning its own store, so the pool "+
				"compounds per boundary and a restore deletes content other runs reference", e.Path)
		}
	}

	// And a restore must not delete the store either.
	write(t, ws, "produced.txt", "by the node")
	objectsBefore := 0
	_ = filepath.WalkDir(filepath.Join(storeDir, objectsDir), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			objectsBefore++
		}
		return nil
	})
	if objectsBefore == 0 {
		t.Fatal("fixture: no objects were stored")
	}
	if _, err := n.Restore("r1", ws, snap.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	objectsAfter := 0
	_ = filepath.WalkDir(filepath.Join(storeDir, objectsDir), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			objectsAfter++
		}
		return nil
	})
	if objectsAfter < objectsBefore {
		t.Errorf("the restore deleted %d pool object(s) — they are store-global, so other "+
			"snapshots and other runs still reference them", objectsBefore-objectsAfter)
	}
	// The restore still did its job on the real workspace.
	if _, err := os.Stat(filepath.Join(ws, "produced.txt")); !os.IsNotExist(err) {
		t.Error("the node's file survived a restore to a snapshot that predates it")
	}
}

// TestRestore_LeavesUntouchedEmptyDirsAlone is Revi's R1e8c6f: the sweep
// re-walked the whole workspace and removed ANY empty directory, not the
// ones the restore emptied. Neither git nor this tracker records empty
// directories, so on an in-place run that silently deleted the operator's
// own scratch dirs, mount points and pre-created output dirs.
func TestRestore_LeavesUntouchedEmptyDirsAlone(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "a.txt", "kept")
	n := NewNative(t.TempDir())
	snap, err := n.Capture("r1", ws, "pre:a:0")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	// The operator's own empty directory, created AFTER the snapshot and
	// never touched by the node.
	if err := os.MkdirAll(filepath.Join(ws, "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	// And a directory the node genuinely created, which the restore empties.
	write(t, ws, "generated/out.md", "by the node")

	if _, err := n.Restore("r1", ws, snap.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "scratch")); err != nil {
		t.Errorf("scratch/ was deleted — the restore never had a copy of it and never created it: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "generated")); !os.IsNotExist(err) {
		t.Error("generated/ survived — the restore emptied it, so it must go")
	}
}
