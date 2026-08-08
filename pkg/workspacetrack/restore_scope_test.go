package workspacetrack

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRestoreOnly_LeavesOutOfScopePathsAlone is the guarantee the whole
// scoped path exists for: a restore on a workspace iterion does not own
// must not reach past what it was asked about.
//
// Both directions matter and they fail differently. A file the snapshot
// HOLDS but that is out of scope must not be rewritten (the operator's
// edit is overwritten — recoverable only from the bank). A file the
// snapshot does NOT hold and that is out of scope must not be deleted
// (bytes gone, and on a workspace that is not a git repo there is no
// other copy).
func TestRestoreOnly_LeavesOutOfScopePathsAlone(t *testing.T) {
	store := t.TempDir()
	ws := t.TempDir()
	write(t, ws, "src/touched.go", "v1\n")
	write(t, ws, "src/untouched.go", "human v1\n")

	tr := NewNative(store)
	before, err := tr.Capture("run1", ws, "pre:implement:0")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// The node rewrites one file and creates one. The operator, in
	// another terminal, edits a second file and creates a third.
	write(t, ws, "src/touched.go", "v2 by the node\n")
	write(t, ws, "src/node-made.go", "node output\n")
	write(t, ws, "src/untouched.go", "human v2 — MY WORK\n")
	write(t, ws, "notes/human.md", "my notes\n")

	// Scope: only what the run is recorded to have changed.
	report, err := tr.RestoreOnly("run1", ws, before.ID, []string{"src/touched.go", "src/node-made.go"})
	if err != nil {
		t.Fatalf("RestoreOnly: %v", err)
	}

	if got := read(t, ws, "src/touched.go"); got != "v1\n" {
		t.Errorf("in-scope rewrite: src/touched.go = %q, want the captured content", got)
	}
	if exists(ws, "src/node-made.go") {
		t.Error("in-scope creation survived: a file the node created must be removed")
	}
	if got := read(t, ws, "src/untouched.go"); got != "human v2 — MY WORK\n" {
		t.Errorf("out-of-scope rewrite: src/untouched.go = %q, want the operator's edit kept", got)
	}
	if !exists(ws, "notes/human.md") {
		t.Error("out-of-scope creation was DELETED — this is issue #380")
	}
	if report.Written != 1 || report.Deleted != 1 {
		t.Errorf("report = %d written / %d deleted, want 1/1", report.Written, report.Deleted)
	}
	if len(report.WrittenPaths) != 1 || report.WrittenPaths[0] != "src/touched.go" {
		t.Errorf("WrittenPaths = %v, want the one path actually rewritten", report.WrittenPaths)
	}
	if len(report.DeletedPaths) != 1 || report.DeletedPaths[0] != "src/node-made.go" {
		t.Errorf("DeletedPaths = %v, want the one path actually removed", report.DeletedPaths)
	}
}

// TestRestoreOnly_EmptyScopeTouchesNothing pins the literal reading of an
// empty set. A caller whose scope computation legitimately came back
// empty must get a no-op, never the full-tree restore — the two differ by
// the entire workspace.
func TestRestoreOnly_EmptyScopeTouchesNothing(t *testing.T) {
	store := t.TempDir()
	ws := t.TempDir()
	write(t, ws, "a.txt", "v1\n")

	tr := NewNative(store)
	before, err := tr.Capture("run1", ws, "pre:n:0")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	write(t, ws, "a.txt", "v2\n")
	write(t, ws, "b.txt", "new\n")

	report, err := tr.RestoreOnly("run1", ws, before.ID, nil)
	if err != nil {
		t.Fatalf("RestoreOnly: %v", err)
	}
	if report.Written != 0 || report.Deleted != 0 {
		t.Errorf("report = %d written / %d deleted, want a no-op", report.Written, report.Deleted)
	}
	if got := read(t, ws, "a.txt"); got != "v2\n" {
		t.Errorf("a.txt = %q, want it untouched", got)
	}
	if !exists(ws, "b.txt") {
		t.Error("b.txt was deleted by an empty scope")
	}
}

// TestRestoreOnly_HonoursProtectedPaths guards the seam a scoped restore
// could plausibly bypass: protection must gate the DELETE as well as the
// write. A protected path present now and absent from the snapshot is
// exactly the .bot an earlier node created — deleting it would be
// strictly worse than the over-restore being fixed.
func TestRestoreOnly_HonoursProtectedPaths(t *testing.T) {
	store := t.TempDir()
	ws := t.TempDir()
	write(t, ws, "keep.md", "v1\n")

	tr := NewNative(store)
	before, err := tr.Capture("run1", ws, "pre:n:0")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	write(t, ws, "keep.md", "v2\n")
	write(t, ws, "main.bot", "edited by the operator\n")

	// Both in scope, both protected.
	if _, err := tr.RestoreOnly("run1", ws, before.ID,
		[]string{"keep.md", "main.bot"},
		filepath.Join(ws, "keep.md"), filepath.Join(ws, "main.bot"),
	); err != nil {
		t.Fatalf("RestoreOnly: %v", err)
	}
	if got := read(t, ws, "keep.md"); got != "v2\n" {
		t.Errorf("protected keep.md = %q, want it NOT rewritten", got)
	}
	if !exists(ws, "main.bot") {
		t.Error("a protected path absent from the snapshot was deleted")
	}
}

// TestRestoreOnly_ReportsOnlyItsOwnCoverageGaps: an oversized file the
// snapshot could not store is a real coverage gap — but only when the
// restore was actually asked about it. Reporting the whole snapshot's
// gaps on a scoped restore makes the loudest line the CLI prints fire on
// every rewind of any repo holding one large file, which trains operators
// to ignore it.
func TestRestoreOnly_ReportsOnlyItsOwnCoverageGaps(t *testing.T) {
	store := t.TempDir()
	ws := t.TempDir()
	write(t, ws, "small.txt", "v1\n")
	write(t, ws, "big.bin", "0123456789ABCDEF")

	tr := NewNative(store)
	tr.MaxFileBytes = 8 // big.bin is over the cap and is recorded as skipped
	before, err := tr.Capture("run1", ws, "pre:n:0")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if len(before.Skipped) != 1 || before.Skipped[0] != "big.bin" {
		t.Fatalf("Skipped = %v, want big.bin recorded as uncaptured", before.Skipped)
	}
	write(t, ws, "small.txt", "v2\n")

	report, err := tr.RestoreOnly("run1", ws, before.ID, []string{"small.txt"})
	if err != nil {
		t.Fatalf("RestoreOnly: %v", err)
	}
	if len(report.Skipped) != 0 {
		t.Errorf("Skipped = %v, want no gap reported for a path outside the scope", report.Skipped)
	}
	// The oversized file is still physically untouched — the scope only
	// governs what is REPORTED, never whether an uncaptured file is safe.
	if !exists(ws, "big.bin") {
		t.Error("an uncaptured oversized file was deleted")
	}

	full, err := tr.Restore("run1", ws, before.ID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(full.Skipped) != 1 {
		t.Errorf("full restore Skipped = %v, want the gap still reported there", full.Skipped)
	}
}

// TestRestoreOnly_DeletesFromTheWalkNotTheManifest pins WHY the scope is
// a filter over the workspace walk rather than a delete list derived from
// a manifest: a manifest is on-disk JSON unmarshalled bare, so a path in
// it is untrusted input. The walk cannot produce a candidate outside the
// workspace; a manifest can.
func TestRestoreOnly_DeletesFromTheWalkNotTheManifest(t *testing.T) {
	store := t.TempDir()
	outside := t.TempDir()
	ws := filepath.Join(t.TempDir(), "workspace")
	write(t, ws, "a.txt", "v1\n")
	victim := filepath.Join(outside, "precious.txt")
	if err := os.WriteFile(victim, []byte("not yours to delete\n"), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}

	tr := NewNative(store)
	before, err := tr.Capture("run1", ws, "pre:n:0")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	// A scope naming an escaping path, as a corrupted or hand-copied
	// manifest could. Nothing outside the workspace may be reached.
	if _, err := tr.RestoreOnly("run1", ws, before.ID,
		[]string{"../" + filepath.Base(outside) + "/precious.txt", "a.txt"},
	); err != nil {
		t.Fatalf("RestoreOnly: %v", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("a path outside the workspace was reached: %v", err)
	}
}
