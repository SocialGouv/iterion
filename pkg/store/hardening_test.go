package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Regression tests locking in the pkg/store production-readiness hardening
// surfaced by a whole-improve-loop (Willy) cross-family review of the
// run-persistence layer (2026-06-14, run 019ec7ed). Each test corresponds to
// a reviewer blocker; see docs/bot-runs/whole-improve-loop.md:
//
//	B1 — TeeRunLog must reject an unsafe run ID before touching the FS and
//	     create the run dir / run.log with the store's private perms (run
//	     logs hold prompts, model output, and secrets).
//	B2 — AppendEvent must sanitize the run ID (traversal-defense parity with
//	     LoadRun / LoadEvents / Artifact / Interaction, which already did).
//	B3 — CreateRun must be exclusive (no-clobber) so a re-used run ID cannot
//	     reset an existing run's metadata / checkpoint.
//	B4 — AppendEvent must repair a torn final JSONL line left by a prior
//	     crash, so the first post-resume event is not lost to concatenation.

// B3: CreateRun is a no-clobber exclusive create.
func TestCreateRunIsExclusive(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()

	if _, err := s.CreateRun(ctx, "dup", "wf", map[string]any{"k": "v1"}); err != nil {
		t.Fatalf("first CreateRun: %v", err)
	}

	_, err := s.CreateRun(ctx, "dup", "wf", map[string]any{"k": "v2"})
	if err == nil {
		t.Fatal("second CreateRun with a re-used ID: expected error, got nil (run was clobbered)")
	}
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second CreateRun: error = %v, want fs.ErrExist in the chain", err)
	}

	// The original run's metadata must survive the rejected re-create.
	r, err := s.LoadRun(ctx, "dup")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got, _ := r.Inputs["k"].(string); got != "v1" {
		t.Fatalf("inputs[k] = %q, want v1 (original was clobbered by the second create)", got)
	}
}

// B2: AppendEvent rejects an unsafe run ID (the traversal-defense gap that
// LoadRun / LoadEvents / Artifact / Interaction already closed).
func TestAppendEventRejectsUnsafeRunID(t *testing.T) {
	s := tmpStore(t)
	if _, err := s.AppendEvent(context.Background(), "../../escape", Event{Type: EventRunStarted}); err == nil {
		t.Fatal("AppendEvent with a traversal run ID: expected error, got nil")
	}
}

// B4: a torn final JSONL line (a partial write from a prior crash) is
// separated from the next event so the first post-resume event survives the
// replay instead of being concatenated into a single corrupt line.
func TestAppendEventRepairsTornTail(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()

	if _, err := s.CreateRun(ctx, "torn", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := s.AppendEvent(ctx, "torn", Event{Type: EventRunStarted}); err != nil {
		t.Fatalf("append event 1: %v", err)
	}

	// Simulate a crash mid-write: a partial JSONL record with no trailing
	// newline left at the end of events.jsonl.
	f, err := os.OpenFile(s.eventsPath("torn"), os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		t.Fatalf("open events for torn write: %v", err)
	}
	if _, err := f.WriteString(`{"seq":99,"type":"node_star`); err != nil {
		t.Fatalf("write torn tail: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A fresh store (a new process, e.g. on resume) appends the next event.
	s2, err := New(s.Root())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s2.AppendEvent(ctx, "torn", Event{Type: EventNodeStarted}); err != nil {
		t.Fatalf("append after torn tail: %v", err)
	}

	evts, err := s2.LoadEvents(ctx, "torn")
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	// The torn partial line is skipped, but BOTH valid events survive. Without
	// the repair, event 2 concatenates onto the torn bytes and both vanish
	// (len would be 1).
	if len(evts) != 2 {
		var got []string
		for _, e := range evts {
			got = append(got, string(e.Type))
		}
		t.Fatalf("LoadEvents returned %d events %v, want 2 (torn line skipped, both valid events kept)", len(evts), got)
	}
	if evts[0].Type != EventRunStarted || evts[1].Type != EventNodeStarted {
		t.Fatalf("events = [%s, %s], want [run_started, node_started]", evts[0].Type, evts[1].Type)
	}
	if evts[0].Seq != 0 || evts[1].Seq != 1 {
		t.Fatalf("seqs = [%d, %d], want [0, 1] (monotonic across the torn tail)", evts[0].Seq, evts[1].Seq)
	}
}

// B1: TeeRunLog refuses an unsafe run ID before touching the FS, and creates
// the run dir + run.log with the store's private perms.
func TestTeeRunLogHardening(t *testing.T) {
	root := t.TempDir()
	logger := iterlog.New(iterlog.LevelError, io.Discard)

	// Unsafe run ID: no tee, and no filesystem is touched under the store.
	if _, closer := TeeRunLog(logger, iterlog.LevelError, root, "../escape"); closer != nil {
		_ = closer.Close()
		t.Fatal("TeeRunLog with an unsafe run ID returned a non-nil closer")
	}
	if _, err := os.Stat(filepath.Join(root, "runs")); !os.IsNotExist(err) {
		t.Fatalf("TeeRunLog with an unsafe run ID created %s/runs (stat err = %v)", root, err)
	}

	// Safe run ID: tee set up with private perms on both the dir and the file.
	_, closer := TeeRunLog(logger, iterlog.LevelError, root, "safe")
	if closer == nil {
		t.Fatal("TeeRunLog with a safe run ID returned a nil closer")
	}
	_ = closer.Close()

	runDir := filepath.Join(root, "runs", "safe")
	if di, err := os.Stat(runDir); err != nil {
		t.Fatalf("stat run dir: %v", err)
	} else if di.Mode().Perm() != dirPerm {
		t.Errorf("run dir perm = %#o, want %#o", di.Mode().Perm(), dirPerm)
	}
	if fi, err := os.Stat(filepath.Join(runDir, "run.log")); err != nil {
		t.Fatalf("stat run.log: %v", err)
	} else if fi.Mode().Perm() != filePerm {
		t.Errorf("run.log perm = %#o, want %#o", fi.Mode().Perm(), filePerm)
	}
}

// TestLoadRunDoesNotWriteBack: a read has no side effect on disk. LoadRun
// heals a legacy or in-flight shape IN MEMORY (an empty Name gets its
// deterministic label) and leaves run.json byte-identical. A read that wrote
// would race the run's owner: the engine writes from another store instance
// — usually another process — and no lock covers a reader's
// read-modify-write against it. Every new run passes through the nameless
// window between CreateRun and the engine's first stamping SaveRun, so a
// reader landing there could erase the stamp (ParentRunID, FilePath,
// WorkflowHash, Name…) for good — the engine's later writes are
// load-patch-write on the disk copy. Observed on a dispatched subbot child:
// listed with no parent link for the rest of its life, 3 of 120 dispatches.
func TestLoadRunDoesNotWriteBack(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "read-only"

	r, err := s.CreateRun(ctx, runID, "wf", nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// The in-flight shape: a run persisted before its owner stamped a Name.
	r.Name = ""
	if err := s.writeRun(r); err != nil {
		t.Fatalf("seed nameless run: %v", err)
	}
	before, err := os.ReadFile(s.runJSONPath(runID))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}

	got, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got.Name == "" {
		t.Fatal("LoadRun must heal the empty Name in memory")
	}
	after, err := os.ReadFile(s.runJSONPath(runID))
	if err != nil {
		t.Fatalf("re-read run.json: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("LoadRun wrote run.json back — a read must not race the run's owner\nbefore: %s\nafter:  %s", before, after)
	}
}

// TestLoadRunNeverClobbersAnotherInstancesStamp is the production shape:
// two store instances over one directory — the engine that owns the run
// and a reader (studio, inspect, a dispatcher poll) — with the reader's
// load landing inside the nameless window. The engine's stamp must be what
// every later read, and the disk, report.
func TestLoadRunNeverClobbersAnotherInstancesStamp(t *testing.T) {
	dir := t.TempDir()
	engine, err := New(dir)
	if err != nil {
		t.Fatalf("New(engine): %v", err)
	}
	reader, err := New(dir)
	if err != nil {
		t.Fatalf("New(reader): %v", err)
	}
	ctx := context.Background()
	const runID = "child"

	created, err := engine.CreateRun(ctx, runID, "wf", nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := reader.LoadRun(ctx, runID); err != nil {
		t.Fatalf("reader LoadRun inside the window: %v", err)
	}
	created.ParentRunID = "parent"
	created.Name = "engine-name"
	if err := engine.SaveRun(ctx, created); err != nil {
		t.Fatalf("engine SaveRun: %v", err)
	}

	got, err := reader.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("reader LoadRun after the stamp: %v", err)
	}
	if got.ParentRunID != "parent" || got.Name != "engine-name" {
		t.Fatalf("reader sees parent=%q name=%q, want the engine's stamp", got.ParentRunID, got.Name)
	}
	raw, err := os.ReadFile(engine.runJSONPath(runID))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var onDisk Run
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("decode run.json: %v", err)
	}
	if onDisk.ParentRunID != "parent" {
		t.Fatalf("disk has parent=%q, want the engine's stamp to survive the reader", onDisk.ParentRunID)
	}
}

func TestEnsureRunFilesDirReplacesSymlink(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-files-symlink"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	outside := t.TempDir()
	dir := s.runFilesDir(runID)
	if err := os.Symlink(outside, dir); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable on windows: %v", err)
		}
		t.Fatalf("Symlink: %v", err)
	}

	got, err := s.EnsureRunFilesDir(ctx, runID)
	if err != nil {
		t.Fatalf("EnsureRunFilesDir: %v", err)
	}
	if got != dir {
		t.Fatalf("dir = %q, want %q", got, dir)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("artifact_files remained a symlink")
	}
	if !info.IsDir() {
		t.Fatalf("artifact_files mode = %v, want directory", info.Mode())
	}
}

func TestOpenRunFileRejectsIntermediateSymlinkSwap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix openat/O_NOFOLLOW hardening is not available on windows")
	}
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-files-openat"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	dir, err := s.EnsureRunFilesDir(ctx, runID)
	if err != nil {
		t.Fatalf("EnsureRunFilesDir: %v", err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("SHOULD-NOT-LEAK"), 0o644); err != nil {
		t.Fatalf("write outside secret: %v", err)
	}
	mid := filepath.Join(dir, "mid")
	if err := os.Mkdir(mid, 0o755); err != nil {
		t.Fatalf("mkdir mid: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mid, "secret.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatalf("write inside file: %v", err)
	}

	// Simulate the vulnerable post-validation race: an intermediate directory
	// is replaced with a symlink to an outside tree. OpenRunFile must fail at
	// the intermediate O_DIRECTORY|O_NOFOLLOW open, not follow it and stream the
	// outside secret.
	if err := os.RemoveAll(mid); err != nil {
		t.Fatalf("remove mid: %v", err)
	}
	if err := os.Symlink(outside, mid); err != nil {
		t.Fatalf("symlink mid: %v", err)
	}
	if rc, _, err := s.OpenRunFile(ctx, runID, "mid/secret.txt"); err == nil {
		defer rc.Close()
		body, _ := io.ReadAll(rc)
		t.Fatalf("OpenRunFile unexpectedly succeeded and returned %q", body)
	}
}

// TestWriteRunEventsSyncBarrier pins the write-ahead ordering barrier:
// once an AppendEvent fsync failure flags a run, the next run.json write
// re-syncs events.jsonl first and clears the flag — the checkpoint can
// never be durably ahead of its event log.
func TestWriteRunEventsSyncBarrier(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := s.CreateRun(ctx, "run-barrier", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, "run-barrier", Event{Type: EventRunStarted}); err != nil {
		t.Fatal(err)
	}
	// Simulate a prior fsync failure (fault injection at the syscall level
	// isn't portable; the flag is the observable contract).
	s.mu.Lock()
	s.eventsUnsynced["run-barrier"] = true
	s.mu.Unlock()

	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun with pending events re-sync: %v", err)
	}
	s.mu.Lock()
	still := s.eventsUnsynced["run-barrier"]
	s.mu.Unlock()
	if still {
		t.Fatal("writeRun did not clear the unsynced flag after re-syncing events.jsonl")
	}

	// A flagged run with NO events file (append failed before creating it)
	// must not block the checkpoint write.
	r2, err := s.CreateRun(ctx, "run-barrier-noevents", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.eventsUnsynced["run-barrier-noevents"] = true
	s.mu.Unlock()
	if err := s.SaveRun(ctx, r2); err != nil {
		t.Fatalf("SaveRun with missing events file: %v", err)
	}
	// A successful AppendEvent fsync clears the flag on its own.
	s.mu.Lock()
	s.eventsUnsynced["run-barrier"] = true
	s.mu.Unlock()
	if _, err := s.AppendEvent(ctx, "run-barrier", Event{Type: EventNodeStarted}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	still = s.eventsUnsynced["run-barrier"]
	s.mu.Unlock()
	if still {
		t.Fatal("a successful AppendEvent fsync must clear the unsynced flag")
	}
}
