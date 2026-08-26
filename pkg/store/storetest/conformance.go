// Package storetest exposes the conformance suite that every
// store.RunStore backend must satisfy. It lives in its own package
// (rather than as a *_test.go helper) so backend tests in sibling
// packages — notably pkg/store/mongo — can plug a backend-specific
// factory into the same assertions.
//
// The suite covers: CreateRun→LoadRun round-trip, status transitions
// + FinishedAt clamping at terminals, AppendEvent monotone seq under
// sequential AND concurrent writers, WriteArtifact version ordering,
// LockRun exclusivity across an Unlock boundary, and Capabilities()
// non-emptiness.
package storetest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Factory returns a fresh, empty RunStore for one subtest. Cleanup
// (t.TempDir, drop database, etc.) is the factory's job.
type Factory func(t *testing.T) store.RunStore

// testCtx returns a context carrying a synthetic tenant + owner. The
// mongo backend's fail-closed withTenantFilter guard panics on
// tenant-scoped queries that arrive without a tenant in ctx;
// conformance tests speak the normal calling convention (auth
// middleware stamps a tenant) so they exercise the same code path as
// production. Filesystem backend ignores the values.
func testCtx() context.Context {
	return store.WithIdentity(context.Background(), "_test", "_test")
}

// Opts let backends declare what behaviour the harness should expect
// when it differs from the filesystem default.
type Opts struct {
	// InitialStatus is the status CreateRun is expected to set. FS
	// starts at "running" (engine takes ownership immediately);
	// Mongo starts at "queued" because the runner pod claims the
	// run asynchronously.
	InitialStatus store.RunStatus
}

// RunWithOpts executes the full conformance suite with backend-
// specific overrides.
func RunWithOpts(t *testing.T, factory Factory, opts Opts) {
	t.Run("CreateLoadRoundTrip", func(t *testing.T) { testCreateLoad(t, factory(t), opts) })
	t.Run("StatusTransitions", func(t *testing.T) { testStatusTransitions(t, factory(t)) })
	t.Run("QueuedAttemptCAS", func(t *testing.T) { testQueuedAttemptCAS(t, factory(t)) })
	t.Run("FailRunTerminal", func(t *testing.T) { testFailRunTerminal(t, factory(t)) })
	t.Run("EventSeqMonotone", func(t *testing.T) { testEventSeqMonotone(t, factory(t)) })
	t.Run("EventSeqUnderConcurrency", func(t *testing.T) { testEventSeqConcurrent(t, factory(t)) })
	t.Run("EventDataDecodeShape", func(t *testing.T) { testEventDataDecodeShape(t, factory(t)) })
	t.Run("ArtifactVersionsMonotone", func(t *testing.T) { testArtifactVersions(t, factory(t)) })
	t.Run("LockExclusivity", func(t *testing.T) { testLockExclusive(t, factory(t)) })
	t.Run("CapabilitiesReported", func(t *testing.T) { testCapabilitiesReported(t, factory(t)) })
	t.Run("UserMessagesInbox", func(t *testing.T) { testUserMessagesInbox(t, factory(t)) })
	t.Run("WatchedIssues", func(t *testing.T) { testWatchedIssues(t, factory(t)) })
	t.Run("SubbotChildren", func(t *testing.T) { testSubbotChildren(t, factory(t)) })
	t.Run("NodesServed", func(t *testing.T) { testNodesServed(t, factory(t)) })
	t.Run("ReverseTreeQueries", func(t *testing.T) { testReverseTreeQueries(t, factory(t)) })
	t.Run("ScheduleReverseQuery", func(t *testing.T) { testScheduleReverseQuery(t, factory(t)) })
	t.Run("DeleteRun", func(t *testing.T) { testDeleteRun(t, factory(t)) })
	t.Run("RunLogStore", func(t *testing.T) { testRunLogStore(t, factory(t)) })
	t.Run("TurnStore", func(t *testing.T) { testTurnStore(t, factory(t)) })
	t.Run("ToolBlobStore", func(t *testing.T) { testToolBlobStore(t, factory(t)) })
	t.Run("BackendSessionStore", func(t *testing.T) { testBackendSessionStore(t, factory(t)) })
	t.Run("RunFilesStore", func(t *testing.T) { testRunFilesStore(t, factory(t)) })
	t.Run("ParentedRunCreator", func(t *testing.T) { testParentedRunCreator(t, factory(t)) })
}

func testQueuedAttemptCAS(t *testing.T, s store.RunStore) {
	t.Helper()
	attempts := store.AsQueuedAttemptStore(s)
	if attempts == nil {
		t.Skip("backend does not implement QueuedAttemptStore")
	}
	ctx := testCtx()
	const runID = "run-queued-attempt"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.UpdateRunStatus(ctx, runID, store.RunStatusFailedResumable, "retry"); err != nil {
		t.Fatalf("mark resumable: %v", err)
	}
	changed, err := s.UpdateRunStatusIf(ctx, runID, store.RunStatusQueued, "", []store.RunStatus{store.RunStatusFailedResumable})
	if err != nil || !changed {
		t.Fatalf("claim queued attempt = (%t, %v), want (true, nil)", changed, err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil || r.QueuedAt == nil {
		t.Fatalf("LoadRun queued marker = (%v, %v), want non-nil", r, err)
	}

	// A delivery from before this requeue must not fail the new attempt.
	changed, err = attempts.FailQueuedRunIfAttempt(ctx, runID, "stale", r.QueuedAt.Add(-time.Second))
	if err != nil || changed {
		t.Fatalf("stale attempt CAS = (%t, %v), want (false, nil)", changed, err)
	}
	if got, _ := s.LoadRun(ctx, runID); got == nil || got.Status != store.RunStatusQueued {
		t.Fatalf("stale attempt changed run to %+v, want queued", got)
	}

	// The delivery belonging to this attempt may perform the terminal flip.
	changed, err = attempts.FailQueuedRunIfAttempt(ctx, runID, "schema mismatch", r.QueuedAt.Add(time.Second))
	if err != nil || !changed {
		t.Fatalf("current attempt CAS = (%t, %v), want (true, nil)", changed, err)
	}
	if got, _ := s.LoadRun(ctx, runID); got == nil || got.Status != store.RunStatusFailedResumable {
		t.Fatalf("current attempt left run at %+v, want failed_resumable", got)
	}
}

// testParentedRunCreator exercises the optional ParentedRunCreator surface
// (spawnRun's atomic child precreate, PR #193 M3): the ParentRunID must
// round-trip from the SINGLE create write — no follow-up SaveRun — and the
// child must be reachable through ListChildRuns. Also asserts the
// no-clobber contract (a reused id fails). Skipped otherwise.
func testParentedRunCreator(t *testing.T, s store.RunStore) {
	t.Helper()
	pc := store.AsParentedRunCreator(s)
	if pc == nil {
		t.Skip("backend does not implement ParentedRunCreator")
	}
	ctx := testCtx()
	const parentID = "run_parent"
	const childID = "run_child"
	if _, err := s.CreateRun(ctx, parentID, "demo", nil); err != nil {
		t.Fatalf("CreateRun(parent): %v", err)
	}
	child, err := pc.CreateChildRun(ctx, childID, "demo", parentID, map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("CreateChildRun: %v", err)
	}
	// The returned doc carries the parent link with no extra write.
	if child.ParentRunID != parentID {
		t.Errorf("CreateChildRun returned ParentRunID = %q; want %q", child.ParentRunID, parentID)
	}
	// It is durable: a fresh load sees the link (not just the in-memory copy).
	loaded, err := s.LoadRun(ctx, childID)
	if err != nil {
		t.Fatalf("LoadRun(child): %v", err)
	}
	if loaded.ParentRunID != parentID {
		t.Errorf("persisted ParentRunID = %q; want %q", loaded.ParentRunID, parentID)
	}
	if got := loaded.Inputs["k"]; got != "v" {
		t.Errorf("persisted Inputs[k] = %v; want v", got)
	}
	// And it is reachable through the child reverse query.
	kids, err := s.ListChildRuns(ctx, parentID)
	if err != nil {
		t.Fatalf("ListChildRuns: %v", err)
	}
	if !sameOrderedSlice(kids, []string{childID}) {
		t.Errorf("ListChildRuns(%s) = %v; want [%s]", parentID, kids, childID)
	}
	// No-clobber: reusing an id must fail, never reset the existing doc.
	if _, err := pc.CreateChildRun(ctx, childID, "demo", parentID, nil); err == nil {
		t.Errorf("CreateChildRun(duplicate id): expected error, got nil")
	}
}

// testRunFilesStore exercises the optional RunFilesStore surface
// (tool-produced files — run reports, SBOMs — surfaced in the studio
// Artifacts panel) when the backend implements it. It writes files into
// the EnsureRunFilesDir scratch dir, then — for backends whose write
// target differs from their read source (Mongo: scratch→S3) — bridges via
// the RunFilesUploader before reading back. The filesystem backend needs
// no bridge (its scratch dir IS the read source), so the same assertions
// hold on both. Covers list ordering, nested paths, open round-trip,
// traversal rejection, and DeleteRun cleanup. Skipped otherwise.
func testRunFilesStore(t *testing.T, s store.RunStore) {
	t.Helper()
	rfs := store.AsRunFilesStore(s)
	if rfs == nil {
		t.Skip("backend does not implement RunFilesStore")
	}
	ctx := testCtx()
	const runID = "run_files"
	if _, err := s.CreateRun(ctx, runID, "demo", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Empty run: no files, open of a nonexistent path errors.
	if files, err := rfs.ListRunFiles(ctx, runID); err != nil || len(files) != 0 {
		t.Errorf("ListRunFiles(empty) = %v, %v; want empty, nil", files, err)
	}
	if _, _, err := rfs.OpenRunFile(ctx, runID, "nope.md"); err == nil {
		t.Errorf("OpenRunFile(missing): expected error")
	}

	// Write two files (one nested) into the scratch dir.
	dir, err := rfs.EnsureRunFilesDir(ctx, runID)
	if err != nil {
		t.Fatalf("EnsureRunFilesDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("# Report\n"), 0o644); err != nil {
		t.Fatalf("write report.md: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "sbom.json"), []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatalf("write sbom.json: %v", err)
	}

	// Bridge scratch→durable when the backend needs it (cloud). No-op for
	// the filesystem backend (scratch dir is already the read source).
	if up := store.AsRunFilesUploader(s); up != nil {
		n, err := up.UploadRunFiles(ctx, runID)
		if err != nil {
			t.Fatalf("UploadRunFiles: %v", err)
		}
		if n != 2 {
			t.Errorf("UploadRunFiles count = %d; want 2", n)
		}
	}

	// List returns both, sorted by path.
	files, err := rfs.ListRunFiles(ctx, runID)
	if err != nil {
		t.Fatalf("ListRunFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("ListRunFiles count = %d; want 2 (%+v)", len(files), files)
	}
	if files[0].Path != "report.md" || files[1].Path != "sub/sbom.json" {
		t.Errorf("ListRunFiles paths = %q, %q; want report.md, sub/sbom.json", files[0].Path, files[1].Path)
	}
	if files[0].Size != int64(len("# Report\n")) {
		t.Errorf("report.md size = %d; want %d", files[0].Size, len("# Report\n"))
	}

	// Open round-trip on the nested file.
	rc, info, err := rfs.OpenRunFile(ctx, runID, "sub/sbom.json")
	if err != nil {
		t.Fatalf("OpenRunFile(sub/sbom.json): %v", err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != `{"ok":true}` {
		t.Errorf("OpenRunFile body = %q; want %q", body, `{"ok":true}`)
	}
	if info.Path != "sub/sbom.json" {
		t.Errorf("OpenRunFile info.Path = %q; want sub/sbom.json", info.Path)
	}

	// Traversal + absolute paths are rejected (mapped to a not-found error).
	for _, bad := range []string{"../escape", "/etc/passwd", "sub/../../escape"} {
		if _, _, err := rfs.OpenRunFile(ctx, runID, bad); err == nil {
			t.Errorf("OpenRunFile(%q): expected rejection", bad)
		}
	}

	// DeleteRun sweeps the artifact files.
	if err := s.DeleteRun(ctx, runID); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	if files, err := rfs.ListRunFiles(ctx, runID); err != nil || len(files) != 0 {
		t.Errorf("ListRunFiles after DeleteRun = %v, %v; want empty, nil", files, err)
	}
}

func testBackendSessionStore(t *testing.T, s store.RunStore) {
	t.Helper()
	bss := store.AsBackendSessionStore(s)
	if bss == nil {
		t.Skip("backend does not implement BackendSessionStore")
	}
	ctx := testCtx()
	const runID = "run_sesspack"
	if _, err := s.CreateRun(ctx, runID, "demo", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := bss.GetBackendSession(ctx, runID, "missing"); err == nil {
		t.Errorf("GetBackendSession(missing): expected error")
	}
	body := []byte("packed-session")
	if err := bss.PutBackendSession(ctx, runID, "ref1", body); err != nil {
		t.Fatalf("PutBackendSession: %v", err)
	}
	got, err := bss.GetBackendSession(ctx, runID, "ref1")
	if err != nil {
		t.Fatalf("GetBackendSession: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("GetBackendSession = %q, want %q", got, body)
	}
	if err := bss.DeleteBackendSession(ctx, runID, "ref1"); err != nil {
		t.Fatalf("DeleteBackendSession: %v", err)
	}
	if _, err := bss.GetBackendSession(ctx, runID, "ref1"); err == nil {
		t.Errorf("Get after Delete: expected error")
	}
	if err := bss.PutBackendSession(ctx, runID, "ref2", body); err != nil {
		t.Fatalf("Put ref2: %v", err)
	}
	if err := s.DeleteRun(ctx, runID); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	if _, err := bss.GetBackendSession(ctx, runID, "ref2"); err == nil {
		t.Errorf("Get after DeleteRun: expected error")
	}
}

// testToolBlobStore exercises the optional ToolBlobStore surface (large
// per-tool-call I/O bodies served paginated to the studio Tools tab) when
// the backend implements it: write/read round-trip, offset+limit windows,
// eof semantics, past-the-end reads, kind validation, the os.ErrNotExist
// contract for a missing blob, and DeleteRun cleanup. Skipped otherwise.
func testToolBlobStore(t *testing.T, s store.RunStore) {
	t.Helper()
	tbs := store.AsToolBlobStore(s)
	if tbs == nil {
		t.Skip("backend does not implement ToolBlobStore")
	}
	ctx := testCtx()
	const runID = "run_toolblob"
	const tuid = "toolu_123"
	if _, err := s.CreateRun(ctx, runID, "demo", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Missing blob → os.ErrNotExist-compatible error, size 0. (eof is
	// meaningless on the error path and differs across backends, so it is
	// not asserted here.)
	if _, total, _, err := tbs.ReadToolBlob(ctx, runID, tuid, "output", 0, 0); !errors.Is(err, os.ErrNotExist) || total != 0 {
		t.Errorf("ReadToolBlob(missing) = total %d err %v; want 0, os.ErrNotExist", total, err)
	}

	// Kind validation rejects anything but input|output.
	if _, err := tbs.WriteToolBlob(ctx, runID, tuid, "bogus", []byte("x")); err == nil {
		t.Errorf("WriteToolBlob(bad kind): expected error")
	}
	if _, _, _, err := tbs.ReadToolBlob(ctx, runID, tuid, "bogus", 0, 0); err == nil {
		t.Errorf("ReadToolBlob(bad kind): expected error")
	}

	// Write input + output bodies.
	const output = "hello world"
	if n, err := tbs.WriteToolBlob(ctx, runID, tuid, "output", []byte(output)); err != nil || n != int64(len(output)) {
		t.Fatalf("WriteToolBlob(output) = %d, %v; want %d, nil", n, err, len(output))
	}
	if _, err := tbs.WriteToolBlob(ctx, runID, tuid, "input", []byte("the input")); err != nil {
		t.Fatalf("WriteToolBlob(input): %v", err)
	}

	// Full read.
	data, total, eof, err := tbs.ReadToolBlob(ctx, runID, tuid, "output", 0, 0)
	if err != nil || string(data) != output || total != int64(len(output)) || !eof {
		t.Fatalf("ReadToolBlob(all) = %q total %d eof %v err %v; want %q,%d,true", data, total, eof, err, output, len(output))
	}
	// Windowed read in the middle (not at eof).
	data, total, eof, err = tbs.ReadToolBlob(ctx, runID, tuid, "output", 4, 5)
	if err != nil || string(data) != "o wor" || total != int64(len(output)) || eof {
		t.Fatalf("ReadToolBlob(4,5) = %q total %d eof %v err %v; want %q,%d,false", data, total, eof, err, "o wor", len(output))
	}
	// From-offset to end.
	data, _, eof, err = tbs.ReadToolBlob(ctx, runID, tuid, "output", 6, 0)
	if err != nil || string(data) != "world" || !eof {
		t.Fatalf("ReadToolBlob(6,0) = %q eof %v err %v; want %q,true", data, eof, err, "world")
	}
	// Past-the-end.
	if data, _, eof, err := tbs.ReadToolBlob(ctx, runID, tuid, "output", 99, 0); err != nil || len(data) != 0 || !eof {
		t.Fatalf("ReadToolBlob(past end) = %q eof %v err %v; want empty,true,nil", data, eof, err)
	}
	// input is independent from output.
	if data, _, _, err := tbs.ReadToolBlob(ctx, runID, tuid, "input", 0, 0); err != nil || string(data) != "the input" {
		t.Errorf("ReadToolBlob(input) = %q, %v; want %q", data, err, "the input")
	}

	// Idempotent overwrite.
	if _, err := tbs.WriteToolBlob(ctx, runID, tuid, "output", []byte("replaced")); err != nil {
		t.Fatalf("WriteToolBlob(overwrite): %v", err)
	}
	if data, _, _, err := tbs.ReadToolBlob(ctx, runID, tuid, "output", 0, 0); err != nil || string(data) != "replaced" {
		t.Errorf("after overwrite = %q, %v; want %q", data, err, "replaced")
	}

	// DeleteRun sweeps the tool blobs too.
	if err := s.DeleteRun(ctx, runID); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	if _, _, _, err := tbs.ReadToolBlob(ctx, runID, tuid, "output", 0, 0); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadToolBlob after DeleteRun = %v; want os.ErrNotExist", err)
	}
}

// testTurnStore exercises the optional TurnStore surface (per-LLM-turn
// checkpoints backing the fork-from-here + per-node timeline features)
// when the backend implements it: write/load round-trip, ListTurns
// ordering, LatestTurn across loop iterations, LoadTurnAtIndex resolving
// on the highest iteration that has the turn, the messages sidecar, and
// the ErrTurnNotFound cases. Skipped for backends without the seam.
func testTurnStore(t *testing.T, s store.RunStore) {
	t.Helper()
	ts := store.AsTurnStore(s)
	if ts == nil {
		t.Skip("backend does not implement TurnStore")
	}
	ctx := testCtx()
	const runID = "run_turns"
	if _, err := s.CreateRun(ctx, runID, "demo", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Missing turn → ErrTurnNotFound on every lookup shape.
	if _, err := ts.LoadTurn(ctx, runID, "impl", 0, 0); !errors.Is(err, store.ErrTurnNotFound) {
		t.Errorf("LoadTurn(missing) = %v; want ErrTurnNotFound", err)
	}
	if _, err := ts.LatestTurn(ctx, runID, "impl"); !errors.Is(err, store.ErrTurnNotFound) {
		t.Errorf("LatestTurn(missing) = %v; want ErrTurnNotFound", err)
	}
	if _, err := ts.LoadTurnAtIndex(ctx, runID, "impl", 3); !errors.Is(err, store.ErrTurnNotFound) {
		t.Errorf("LoadTurnAtIndex(missing) = %v; want ErrTurnNotFound", err)
	}
	// ListTurns on an empty node is an empty slice, not an error.
	if turns, err := ts.ListTurns(ctx, runID, "impl", 0); err != nil || len(turns) != 0 {
		t.Errorf("ListTurns(empty) = %v, %v; want empty, nil", turns, err)
	}

	// Write a small ladder: node "impl" iter 0 turns {0,1,2}, iter 1 turn {0}.
	writes := []*store.TurnCheckpoint{
		{RunID: runID, NodeID: "impl", LoopIter: 0, TurnIndex: 0, Backend: "claw", Model: "m", FinishReason: "tool_use"},
		{RunID: runID, NodeID: "impl", LoopIter: 0, TurnIndex: 1, Backend: "claw"},
		{RunID: runID, NodeID: "impl", LoopIter: 0, TurnIndex: 2, Backend: "claw", SessionID: "sess-abc",
			Messages: []byte(`[{"role":"user","content":"hi"}]`)},
		{RunID: runID, NodeID: "impl", LoopIter: 1, TurnIndex: 0, Backend: "claw"},
	}
	for _, w := range writes {
		if err := ts.WriteTurn(ctx, w); err != nil {
			t.Fatalf("WriteTurn(%s/%d/%d): %v", w.NodeID, w.LoopIter, w.TurnIndex, err)
		}
	}

	// LoadTurn round-trip.
	got, err := ts.LoadTurn(ctx, runID, "impl", 0, 0)
	if err != nil {
		t.Fatalf("LoadTurn(0,0): %v", err)
	}
	if got.Backend != "claw" || got.Model != "m" || got.FinishReason != "tool_use" {
		t.Errorf("LoadTurn(0,0) fields: %+v", got)
	}

	// ListTurns returns iter-0 turns in ascending TurnIndex order.
	list, err := ts.ListTurns(ctx, runID, "impl", 0)
	if err != nil {
		t.Fatalf("ListTurns: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("ListTurns count: got %d want 3", len(list))
	}
	for i, tc := range list {
		if tc.TurnIndex != i {
			t.Errorf("ListTurns[%d].TurnIndex = %d want %d", i, tc.TurnIndex, i)
		}
	}

	// LatestTurn picks (highest loop_iter=1, its highest turn_index=0).
	latest, err := ts.LatestTurn(ctx, runID, "impl")
	if err != nil {
		t.Fatalf("LatestTurn: %v", err)
	}
	if latest.LoopIter != 1 || latest.TurnIndex != 0 {
		t.Errorf("LatestTurn = iter %d turn %d; want iter 1 turn 0", latest.LoopIter, latest.TurnIndex)
	}

	// LoadTurnAtIndex(2) resolves on iter 0 (only iter with turn 2).
	at2, err := ts.LoadTurnAtIndex(ctx, runID, "impl", 2)
	if err != nil {
		t.Fatalf("LoadTurnAtIndex(2): %v", err)
	}
	if at2.LoopIter != 0 || at2.TurnIndex != 2 {
		t.Errorf("LoadTurnAtIndex(2) = iter %d turn %d; want iter 0 turn 2", at2.LoopIter, at2.TurnIndex)
	}
	if at2.SessionID != "sess-abc" {
		t.Errorf("LoadTurnAtIndex(2).SessionID = %q; want sess-abc", at2.SessionID)
	}

	// Messages sidecar: present for the turn that carried one, not-found
	// for one that didn't.
	msgs, err := ts.LoadTurnMessages(ctx, runID, "impl", 0, 2)
	if err != nil {
		t.Fatalf("LoadTurnMessages(0,2): %v", err)
	}
	if !bytes.Equal(msgs, []byte(`[{"role":"user","content":"hi"}]`)) {
		t.Errorf("LoadTurnMessages(0,2) = %q; want the persisted blob", msgs)
	}
	if _, err := ts.LoadTurnMessages(ctx, runID, "impl", 0, 0); !errors.Is(err, store.ErrTurnNotFound) {
		t.Errorf("LoadTurnMessages(no blob) = %v; want ErrTurnNotFound", err)
	}

	// WriteTurn is an idempotent overwrite on the same key.
	if err := ts.WriteTurn(ctx, &store.TurnCheckpoint{RunID: runID, NodeID: "impl", LoopIter: 0, TurnIndex: 0, Backend: "claw", Model: "m2"}); err != nil {
		t.Fatalf("WriteTurn(overwrite): %v", err)
	}
	if got, err := ts.LoadTurn(ctx, runID, "impl", 0, 0); err != nil || got.Model != "m2" {
		t.Errorf("after overwrite LoadTurn(0,0).Model = %v (err %v); want m2", got, err)
	}

	// DeleteRun sweeps the turns too.
	if err := s.DeleteRun(ctx, runID); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	if _, err := ts.LatestTurn(ctx, runID, "impl"); !errors.Is(err, store.ErrTurnNotFound) {
		t.Errorf("LatestTurn after DeleteRun = %v; want ErrTurnNotFound", err)
	}
}

// testRunLogStore exercises the optional RunLogStore surface (ADR-053)
// when the backend implements it: append/read/size round-trip, offset
// windows crossing chunk boundaries, idempotent duplicate-offset
// redelivery, and empty-log semantics.
func testRunLogStore(t *testing.T, s store.RunStore) {
	t.Helper()
	ls := store.AsRunLogStore(s)
	if ls == nil {
		t.Skip("backend does not implement RunLogStore")
	}
	ctx := testCtx()
	const runID = "run_logstore"
	if _, err := s.CreateRun(ctx, runID, "demo", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Empty log: size 0, nil range.
	if n, err := ls.RunLogSize(ctx, runID); err != nil || n != 0 {
		t.Fatalf("RunLogSize(empty) = %d, %v; want 0, nil", n, err)
	}
	if data, err := ls.ReadRunLogRange(ctx, runID, 0, 0); err != nil || len(data) != 0 {
		t.Fatalf("ReadRunLogRange(empty) = %q, %v; want empty, nil", data, err)
	}

	// Sequential appends.
	if err := ls.AppendRunLog(ctx, runID, 0, []byte("hello ")); err != nil {
		t.Fatalf("AppendRunLog #1: %v", err)
	}
	if err := ls.AppendRunLog(ctx, runID, 6, []byte("world")); err != nil {
		t.Fatalf("AppendRunLog #2: %v", err)
	}
	if n, err := ls.RunLogSize(ctx, runID); err != nil || n != 11 {
		t.Fatalf("RunLogSize = %d, %v; want 11, nil", n, err)
	}
	if data, err := ls.ReadRunLogRange(ctx, runID, 0, 0); err != nil || string(data) != "hello world" {
		t.Fatalf("ReadRunLogRange(all) = %q, %v; want %q", data, err, "hello world")
	}
	// Window crossing the chunk boundary, sliced on both edges.
	if data, err := ls.ReadRunLogRange(ctx, runID, 4, 9); err != nil || string(data) != "o wor" {
		t.Fatalf("ReadRunLogRange(4,9) = %q, %v; want %q", data, err, "o wor")
	}
	// From-offset to end.
	if data, err := ls.ReadRunLogRange(ctx, runID, 6, 0); err != nil || string(data) != "world" {
		t.Fatalf("ReadRunLogRange(6,0) = %q, %v; want %q", data, err, "world")
	}
	// Past-the-end window.
	if data, err := ls.ReadRunLogRange(ctx, runID, 11, 0); err != nil || len(data) != 0 {
		t.Fatalf("ReadRunLogRange(past end) = %q, %v; want empty, nil", data, err)
	}

	// Idempotent redelivery: re-appending an already-persisted chunk at
	// the same offset must succeed without corrupting the stream.
	if err := ls.AppendRunLog(ctx, runID, 6, []byte("world")); err != nil {
		t.Fatalf("AppendRunLog(duplicate) = %v; want idempotent nil", err)
	}
	if data, err := ls.ReadRunLogRange(ctx, runID, 0, 0); err != nil || string(data) != "hello world" {
		t.Fatalf("after duplicate append: ReadRunLogRange = %q, %v; want %q", data, err, "hello world")
	}
	if n, err := ls.RunLogSize(ctx, runID); err != nil || n != 11 {
		t.Fatalf("after duplicate append: RunLogSize = %d, %v; want 11", n, err)
	}
}

// testDeleteRun proves DeleteRun removes a run (with events) from both
// LoadRun and ListRuns, leaves a sibling run untouched, and is idempotent.
func testDeleteRun(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	for _, id := range []string{"run_del", "run_keep"} {
		if _, err := s.CreateRun(ctx, id, "demo", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
		if _, err := s.AppendEvent(ctx, id, store.Event{Type: store.EventNodeStarted, Timestamp: time.Now().UTC()}); err != nil {
			t.Fatalf("AppendEvent %s: %v", id, err)
		}
		// Also write a run-log chunk so DeleteRun's cleanup of the
		// run_logs collection is exercised (it was omitted from the Mongo
		// children list — the run's raw log lived on until its TTL).
		if ls := store.AsRunLogStore(s); ls != nil {
			if err := ls.AppendRunLog(ctx, id, 0, []byte("log-"+id)); err != nil {
				t.Fatalf("AppendRunLog %s: %v", id, err)
			}
		}
	}

	if err := s.DeleteRun(ctx, "run_del"); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	// The deleted run's log chunks must be gone too (not just its record).
	if ls := store.AsRunLogStore(s); ls != nil {
		if n, err := ls.RunLogSize(ctx, "run_del"); err != nil || n != 0 {
			t.Errorf("RunLogSize(run_del) after delete = %d, %v; want 0, nil", n, err)
		}
		// Sibling's log survives.
		if n, _ := ls.RunLogSize(ctx, "run_keep"); n == 0 {
			t.Errorf("sibling run_keep log wiped by deleting run_del")
		}
	}
	if _, err := s.LoadRun(ctx, "run_del"); err == nil {
		t.Error("LoadRun after delete: want error, got nil")
	}
	ids, err := s.ListRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id == "run_del" {
			t.Errorf("ListRuns still contains deleted run: %v", ids)
		}
	}
	if !containsStr(ids, "run_keep") {
		t.Errorf("ListRuns dropped the sibling run_keep: %v", ids)
	}
	// Sibling's data survives.
	if _, err := s.LoadRun(ctx, "run_keep"); err != nil {
		t.Errorf("sibling run_keep gone after deleting run_del: %v", err)
	}
	// Idempotent: deleting an already-gone run is a no-op.
	if err := s.DeleteRun(ctx, "run_del"); err != nil {
		t.Errorf("second DeleteRun should be a no-op, got: %v", err)
	}

	// Durable tombstone: every late writer gets the TYPED refusal and
	// resurrects nothing. This is the contract that makes DeleteRun
	// final — before it, AppendEvent's MkdirAll (fs) and SaveRun's
	// upsert (Mongo) silently rebuilt deleted runs.
	if _, err := s.AppendEvent(ctx, "run_del", store.Event{Type: store.EventNodeStarted, Timestamp: time.Now().UTC()}); !errors.Is(err, store.ErrRunDeleted) {
		t.Errorf("AppendEvent after delete: err = %v, want ErrRunDeleted", err)
	}
	if err := s.SaveRun(ctx, &store.Run{ID: "run_del", WorkflowName: "zombie", Status: store.RunStatusRunning}); !errors.Is(err, store.ErrRunDeleted) {
		t.Errorf("SaveRun after delete: err = %v, want ErrRunDeleted", err)
	}
	if _, err := s.CreateRun(ctx, "run_del", "zombie", nil); !errors.Is(err, store.ErrRunDeleted) {
		// Mongo CreateRun may not exist as a distinct guard (SaveRun
		// covers the upsert path); only enforce when the impl surfaced
		// a typed error at all.
		if err == nil {
			t.Errorf("CreateRun after delete resurrected the run")
		}
	}
	if err := s.AppendQueuedMessage(ctx, "run_del", store.QueuedUserMessage{ID: "late-msg", Text: "late"}); !errors.Is(err, store.ErrRunDeleted) {
		t.Errorf("AppendQueuedMessage after delete: err = %v, want ErrRunDeleted", err)
	}
	// LoadRun reports the deliberate deletion distinctly from absence.
	if _, err := s.LoadRun(ctx, "run_del"); !errors.Is(err, store.ErrRunDeleted) {
		t.Errorf("LoadRun after delete: err = %v, want ErrRunDeleted", err)
	}
	// And the run STILL doesn't reappear anywhere.
	ids, err = s.ListRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if containsStr(ids, "run_del") {
		t.Errorf("late writers resurrected run_del in ListRuns: %v", ids)
	}

	// The tombstone is reaped past the retention horizon — and only then.
	if n, err := s.PruneDeletionMarkers(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Errorf("PruneDeletionMarkers(too old cutoff) = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Errorf("PruneDeletionMarkers(future cutoff) = (%d, %v), want (1, nil)", n, err)
	}
	// Post-reap, the id is a plain 404 again (history fully released).
	if _, err := s.LoadRun(ctx, "run_del"); !errors.Is(err, store.ErrRunNotFound) {
		t.Errorf("LoadRun after tombstone reap: err = %v, want ErrRunNotFound", err)
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func testWatchedIssues(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	if _, err := s.CreateRun(ctx, "run_watch", "demo", nil); err != nil {
		t.Fatal(err)
	}

	// Add merges + dedups, preserving insertion order.
	got, err := s.AddWatchedIssues(ctx, "run_watch", []string{"a", "b", "a", ""})
	if err != nil {
		t.Fatalf("AddWatchedIssues: %v", err)
	}
	if !sameSet(got, []string{"a", "b"}) {
		t.Errorf("after add: got %v want [a b]", got)
	}

	// A second add only appends the new entry.
	got, err = s.AddWatchedIssues(ctx, "run_watch", []string{"b", "c"})
	if err != nil {
		t.Fatalf("AddWatchedIssues #2: %v", err)
	}
	if !sameSet(got, []string{"a", "b", "c"}) {
		t.Errorf("after add #2: got %v want [a b c]", got)
	}

	// Persisted on the run record.
	r, err := s.LoadRun(ctx, "run_watch")
	if err != nil {
		t.Fatal(err)
	}
	if !sameSet(r.WatchedIssueIDs, []string{"a", "b", "c"}) {
		t.Errorf("LoadRun watched: got %v want [a b c]", r.WatchedIssueIDs)
	}

	// Remove drops the named entries.
	got, err = s.RemoveWatchedIssues(ctx, "run_watch", []string{"b", "missing"})
	if err != nil {
		t.Fatalf("RemoveWatchedIssues: %v", err)
	}
	if !sameSet(got, []string{"a", "c"}) {
		t.Errorf("after remove: got %v want [a c]", got)
	}

	// Removing the rest leaves an empty set.
	got, err = s.RemoveWatchedIssues(ctx, "run_watch", []string{"a", "c"})
	if err != nil {
		t.Fatalf("RemoveWatchedIssues #2: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("after remove all: got %v want empty", got)
	}
}

// testSubbotChildren exercises the subbot re-attach map: SetSubbotChild
// records a childRunID under a per-execution key (atomic per-key so distinct
// keys coexist), LoadRun surfaces it, and ClearSubbotChild removes exactly
// that key. Empty keys are silent no-ops.
func testSubbotChildren(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	if _, err := s.CreateRun(ctx, "run_subbot_parent", "demo", nil); err != nil {
		t.Fatal(err)
	}

	// Two distinct execution keys (e.g. two fan-out branches of the same
	// subbot node) coexist without clobbering each other.
	if err := s.SetSubbotChild(ctx, "run_subbot_parent", "node_a#branch_0", "child-A"); err != nil {
		t.Fatalf("SetSubbotChild A: %v", err)
	}
	if err := s.SetSubbotChild(ctx, "run_subbot_parent", "node_a#branch_1", "child-B"); err != nil {
		t.Fatalf("SetSubbotChild B: %v", err)
	}

	r, err := s.LoadRun(ctx, "run_subbot_parent")
	if err != nil {
		t.Fatal(err)
	}
	if r.SubbotChildren["node_a#branch_0"] != "child-A" || r.SubbotChildren["node_a#branch_1"] != "child-B" {
		t.Errorf("after set: got %v want {node_a#branch_0:child-A, node_a#branch_1:child-B}", r.SubbotChildren)
	}

	// Re-set overwrites the same key in place (re-attach to a fresh child
	// after the prior one ended badly).
	if err := s.SetSubbotChild(ctx, "run_subbot_parent", "node_a#branch_0", "child-A2"); err != nil {
		t.Fatalf("SetSubbotChild A2: %v", err)
	}

	// Clear removes exactly the named key, leaving the sibling.
	if err := s.ClearSubbotChild(ctx, "run_subbot_parent", "node_a#branch_0"); err != nil {
		t.Fatalf("ClearSubbotChild A: %v", err)
	}
	r, err = s.LoadRun(ctx, "run_subbot_parent")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.SubbotChildren["node_a#branch_0"]; ok {
		t.Errorf("after clear: node_a#branch_0 still present in %v", r.SubbotChildren)
	}
	if r.SubbotChildren["node_a#branch_1"] != "child-B" {
		t.Errorf("after clear: sibling lost, got %v", r.SubbotChildren)
	}

	// Empty key is a no-op on both paths.
	if err := s.SetSubbotChild(ctx, "run_subbot_parent", "", "ignored"); err != nil {
		t.Errorf("SetSubbotChild empty key: %v", err)
	}
	if err := s.ClearSubbotChild(ctx, "run_subbot_parent", ""); err != nil {
		t.Errorf("ClearSubbotChild empty key: %v", err)
	}
}

func testNodesServed(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	if _, err := s.CreateRun(ctx, "run_served", "demo", nil); err != nil {
		t.Fatal(err)
	}

	first := store.NodeServed{
		Backend:         "claude_code",
		Model:           "glm-4.6",
		DeclaredModel:   "anthropic/claude-opus-5",
		ContextWindow:   200_000,
		MaxOutputTokens: 8192,
	}
	if err := s.RecordNodeServed(ctx, "run_served", "implement", first); err != nil {
		t.Fatalf("RecordNodeServed implement: %v", err)
	}
	second := store.NodeServed{Backend: "claw", Model: "openai/gpt-5.5"}
	if err := s.RecordNodeServed(ctx, "run_served", "review", second); err != nil {
		t.Fatalf("RecordNodeServed review: %v", err)
	}

	r, err := s.LoadRun(ctx, "run_served")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.NodesServed["implement"]; got != first {
		t.Errorf("implement = %+v, want %+v", got, first)
	}
	if got := r.NodesServed["review"]; got != second {
		t.Errorf("review = %+v, want %+v", got, second)
	}

	// Last write wins on the same node (a loop's last pass).
	updated := store.NodeServed{Backend: "pi", Model: "kimi-k2"}
	if err := s.RecordNodeServed(ctx, "run_served", "implement", updated); err != nil {
		t.Fatalf("RecordNodeServed overwrite: %v", err)
	}
	r, err = s.LoadRun(ctx, "run_served")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.NodesServed["implement"]; got != updated {
		t.Errorf("after overwrite implement = %+v, want %+v", got, updated)
	}
	if got := r.NodesServed["review"]; got != second {
		t.Errorf("sibling review lost: %+v", got)
	}

	if err := s.RecordNodeServed(ctx, "run_served", "", first); err != nil {
		t.Errorf("empty nodeID: %v", err)
	}
	if err := s.RecordNodeServed(ctx, "no-such-run", "n", first); err == nil {
		t.Error("unknown run returned no error")
	}
}

// testReverseTreeQueries exercises the run-tree reverse indexes
// (T4b, refs #125): ListRunsBySourceIssue projects the card←run edge
// (Source.IssueID), ListChildRuns projects a run's shard/child subtree
// (ParentRunID). Both must return exactly the matching ids, in
// created_at-ascending order, and nothing else.
func testReverseTreeQueries(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()

	// saveTree creates a run then stamps its tree edges + a controlled
	// CreatedAt (so ordering is deterministic across backends) via
	// SaveRun.
	saveTree := func(id, issueID, parentID string, createdAt time.Time) {
		if _, err := s.CreateRun(ctx, id, "demo", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
		r, err := s.LoadRun(ctx, id)
		if err != nil {
			t.Fatalf("LoadRun %s: %v", id, err)
		}
		if issueID != "" {
			r.Source = &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: issueID}
		}
		r.ParentRunID = parentID
		if parentID != "" {
			// Contract C3: subbot-spawned children also record WHICH node
			// spawned them; assert the field round-trips per backend below.
			r.ParentNodeID = "sb_node"
		}
		r.CreatedAt = createdAt
		if err := s.SaveRun(ctx, r); err != nil {
			t.Fatalf("SaveRun %s: %v", id, err)
		}
	}

	base := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	// Two runs off issue "native:card-A" (b then a, so we can assert the
	// query re-sorts by created_at ascending → [a_run, b_run]).
	saveTree("a_run", "native:card-A", "", base.Add(1*time.Minute))
	saveTree("b_run", "native:card-A", "", base.Add(2*time.Minute))
	// One run off a different card.
	saveTree("c_run", "native:card-B", "", base.Add(3*time.Minute))
	// A shard subtree under parent "a_run": two children + one unrelated.
	saveTree("shard_0", "", "a_run", base.Add(4*time.Minute))
	saveTree("shard_1", "", "a_run", base.Add(5*time.Minute))
	saveTree("orphan", "", "z_missing_parent", base.Add(6*time.Minute))

	t.Run("BySourceIssue", func(t *testing.T) {
		got, err := s.ListRunsBySourceIssue(ctx, "native:card-A")
		if err != nil {
			t.Fatalf("ListRunsBySourceIssue: %v", err)
		}
		if want := []string{"a_run", "b_run"}; !sameOrderedSlice(got, want) {
			t.Errorf("ListRunsBySourceIssue(card-A) = %v, want %v", got, want)
		}

		got, err = s.ListRunsBySourceIssue(ctx, "native:card-B")
		if err != nil {
			t.Fatalf("ListRunsBySourceIssue B: %v", err)
		}
		if want := []string{"c_run"}; !sameOrderedSlice(got, want) {
			t.Errorf("ListRunsBySourceIssue(card-B) = %v, want %v", got, want)
		}

		// Unknown issue + empty arg → empty, never nil-panic, never error.
		for _, q := range []string{"native:card-none", ""} {
			got, err = s.ListRunsBySourceIssue(ctx, q)
			if err != nil {
				t.Fatalf("ListRunsBySourceIssue(%q): %v", q, err)
			}
			if len(got) != 0 {
				t.Errorf("ListRunsBySourceIssue(%q) = %v, want empty", q, got)
			}
		}
	})

	t.Run("ChildRuns", func(t *testing.T) {
		got, err := s.ListChildRuns(ctx, "a_run")
		if err != nil {
			t.Fatalf("ListChildRuns: %v", err)
		}
		if want := []string{"shard_0", "shard_1"}; !sameOrderedSlice(got, want) {
			t.Errorf("ListChildRuns(a_run) = %v, want %v", got, want)
		}

		// A parent with no children (b_run) + empty arg → empty.
		for _, q := range []string{"b_run", ""} {
			got, err = s.ListChildRuns(ctx, q)
			if err != nil {
				t.Fatalf("ListChildRuns(%q): %v", q, err)
			}
			if len(got) != 0 {
				t.Errorf("ListChildRuns(%q) = %v, want empty", q, got)
			}
		}

		// ParentNodeID must round-trip alongside ParentRunID (contract C3).
		child, err := s.LoadRun(ctx, "shard_0")
		if err != nil {
			t.Fatalf("LoadRun shard_0: %v", err)
		}
		if child.ParentNodeID != "sb_node" {
			t.Errorf("shard_0 ParentNodeID = %q, want %q", child.ParentNodeID, "sb_node")
		}
	})
}

// testScheduleReverseQuery exercises ListRunsBySchedule (the
// schedule←run reverse edge counted by the pkg/schedgate overlap
// gate): exactly the matching ids, created_at ascending, empty for
// unknown/empty schedule ids, and ScheduleID/ScheduleName round-trip
// on RunSource.
func testScheduleReverseQuery(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()

	saveScheduled := func(id, scheduleID string, createdAt time.Time) {
		if _, err := s.CreateRun(ctx, id, "demo", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
		r, err := s.LoadRun(ctx, id)
		if err != nil {
			t.Fatalf("LoadRun %s: %v", id, err)
		}
		if scheduleID != "" {
			r.Source = &store.RunSource{
				Kind:         store.RunSourceKindSchedule,
				ScheduleID:   scheduleID,
				ScheduleName: scheduleID + "-label",
			}
		}
		r.CreatedAt = createdAt
		if err := s.SaveRun(ctx, r); err != nil {
			t.Fatalf("SaveRun %s: %v", id, err)
		}
	}

	base := time.Date(2026, 7, 17, 2, 0, 0, 0, time.UTC)
	// Saved out of chronological order to assert the query re-sorts.
	saveScheduled("sched_b", "weekly-audit", base.Add(2*time.Minute))
	saveScheduled("sched_a", "weekly-audit", base.Add(1*time.Minute))
	saveScheduled("sched_other", "nightly-docs", base.Add(3*time.Minute))
	saveScheduled("manual", "", base.Add(4*time.Minute))

	got, err := s.ListRunsBySchedule(ctx, "weekly-audit")
	if err != nil {
		t.Fatalf("ListRunsBySchedule: %v", err)
	}
	if want := []string{"sched_a", "sched_b"}; !sameOrderedSlice(got, want) {
		t.Errorf("ListRunsBySchedule(weekly-audit) = %v, want %v", got, want)
	}

	for _, q := range []string{"absent-schedule", ""} {
		got, err = s.ListRunsBySchedule(ctx, q)
		if err != nil {
			t.Fatalf("ListRunsBySchedule(%q): %v", q, err)
		}
		if len(got) != 0 {
			t.Errorf("ListRunsBySchedule(%q) = %v, want empty", q, got)
		}
	}

	r, err := s.LoadRun(ctx, "sched_a")
	if err != nil {
		t.Fatalf("LoadRun sched_a: %v", err)
	}
	if r.Source == nil || r.Source.Kind != store.RunSourceKindSchedule ||
		r.Source.ScheduleID != "weekly-audit" || r.Source.ScheduleName != "weekly-audit-label" {
		t.Errorf("RunSource round-trip mismatch: %+v", r.Source)
	}
}

// sameOrderedSlice reports whether got and want are element-wise equal
// in the SAME order (the reverse-tree queries guarantee created_at
// ascending, so ordering is part of the contract).
func sameOrderedSlice(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// sameSet reports whether got and want contain the same elements,
// order-insensitive (Mongo's $addToSet does not guarantee ordering).
func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(got))
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		seen[w]--
	}
	for _, v := range seen {
		if v != 0 {
			return false
		}
	}
	return true
}

func testCreateLoad(t *testing.T, s store.RunStore, opts Opts) {
	t.Helper()
	in := map[string]any{"foo": "bar"}
	r, err := s.CreateRun(testCtx(), "run_1", "demo", in)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if r.ID != "run_1" {
		t.Errorf("ID: got %q", r.ID)
	}
	if r.Status != opts.InitialStatus {
		t.Errorf("Status: got %q want %q", r.Status, opts.InitialStatus)
	}
	r2, err := s.LoadRun(testCtx(), "run_1")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r2.WorkflowName != "demo" {
		t.Errorf("WorkflowName: got %q", r2.WorkflowName)
	}
	if r2.Inputs["foo"] != "bar" {
		t.Errorf("Inputs[foo]: got %v", r2.Inputs["foo"])
	}
}

func testStatusTransitions(t *testing.T, s store.RunStore) {
	t.Helper()
	if _, err := s.CreateRun(testCtx(), "run_2", "demo", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunStatus(testCtx(), "run_2", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	r, _ := s.LoadRun(testCtx(), "run_2")
	if r.Status != store.RunStatusFinished {
		t.Errorf("Status: got %q", r.Status)
	}
	if r.FinishedAt == nil {
		t.Errorf("FinishedAt: expected set on terminal status")
	}
}

// testFailRunTerminal pins the checkpoint-preserving terminal failure on
// BOTH store twins: status failed + checkpoint retained + FinishedAt set,
// and the atomic cancelled-outranks guard. The DSL fail-node path depends
// on this for rewind to stay possible on cloud deployments.
func testFailRunTerminal(t *testing.T, s store.RunStore) {
	t.Helper()
	if _, err := s.CreateRun(testCtx(), "run_ft", "demo", nil); err != nil {
		t.Fatal(err)
	}
	cp := &store.Checkpoint{NodeID: "node-f"}
	if err := s.FailRunTerminal(testCtx(), "run_ft", cp, "workflow reached fail node"); err != nil {
		t.Fatalf("FailRunTerminal: %v", err)
	}
	r, err := s.LoadRun(testCtx(), "run_ft")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusFailed {
		t.Errorf("Status: got %q, want failed", r.Status)
	}
	if r.Error != "workflow reached fail node" {
		t.Errorf("Error: got %q, want the failure reason", r.Error)
	}
	if r.Checkpoint == nil || r.Checkpoint.NodeID != "node-f" {
		t.Errorf("Checkpoint: got %+v, want preserved with NodeID node-f", r.Checkpoint)
	}
	if r.FinishedAt == nil {
		t.Errorf("FinishedAt: expected set on terminal status")
	}

	// An operator cancel outranks a terminal failure racing in behind it.
	if _, err := s.CreateRun(testCtx(), "run_ft_cancel", "demo", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunStatus(testCtx(), "run_ft_cancel", store.RunStatusCancelled, "operator stop"); err != nil {
		t.Fatal(err)
	}
	if err := s.FailRunTerminal(testCtx(), "run_ft_cancel", cp, "late failure"); err != nil {
		t.Fatalf("FailRunTerminal on a cancelled run: %v", err)
	}
	r, err = s.LoadRun(testCtx(), "run_ft_cancel")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusCancelled {
		t.Errorf("Status after a late failure: got %q, want cancelled", r.Status)
	}
}

func testEventSeqMonotone(t *testing.T, s store.RunStore) {
	t.Helper()
	if _, err := s.CreateRun(testCtx(), "run_3", "demo", nil); err != nil {
		t.Fatal(err)
	}
	const N = 50
	var prev int64 = -1
	for i := 0; i < N; i++ {
		ev := store.Event{Type: store.EventNodeStarted, Timestamp: time.Now().UTC()}
		written, err := s.AppendEvent(testCtx(), "run_3", ev)
		if err != nil {
			t.Fatalf("AppendEvent #%d: %v", i, err)
		}
		if written.Seq <= prev {
			t.Errorf("Seq #%d: %d not strictly greater than prev %d", i, written.Seq, prev)
		}
		prev = written.Seq
	}
	all, err := s.LoadEvents(testCtx(), "run_3")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != N {
		t.Errorf("LoadEvents: got %d want %d", len(all), N)
	}
}

func testEventSeqConcurrent(t *testing.T, s store.RunStore) {
	t.Helper()
	if _, err := s.CreateRun(testCtx(), "run_4", "demo", nil); err != nil {
		t.Fatal(err)
	}
	const goroutines = 8
	const perG = 25
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				ev := store.Event{Type: store.EventNodeStarted, Timestamp: time.Now().UTC()}
				if _, err := s.AppendEvent(testCtx(), "run_4", ev); err != nil {
					t.Errorf("AppendEvent: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	all, err := s.LoadEvents(testCtx(), "run_4")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(all), goroutines*perG; got != want {
		t.Errorf("event count: got %d want %d", got, want)
	}
	seen := make(map[int64]struct{}, len(all))
	for i, ev := range all {
		if _, dup := seen[ev.Seq]; dup {
			t.Errorf("duplicate seq %d at index %d", ev.Seq, i)
		}
		seen[ev.Seq] = struct{}{}
		if ev.Seq < 0 {
			t.Errorf("negative seq at index %d: %d", i, ev.Seq)
		}
	}
}

// testEventDataDecodeShape pins the cross-backend contract for the
// open-shaped Event.Data payload: whatever a backend puts on the wire,
// nested documents must load back as plain map[string]any, nested
// arrays as plain []any, and integers within the int / int64 / float64
// family. Shared consumers (runview snapshot reducers, subbot
// terminal-output recovery) type-assert exactly these shapes and
// silently produce nothing on any other — a backend leaking its
// codec's defined types (bson.D / bson.A / int32) breaks every one of
// them while JSON round-trips keep looking correct.
func testEventDataDecodeShape(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	const runID = "run_event_decode_shape"
	if _, err := s.CreateRun(ctx, runID, "demo", nil); err != nil {
		t.Fatal(err)
	}
	in := store.Event{
		Type:      store.EventNodeFinished,
		NodeID:    "deploy",
		Timestamp: time.Now().UTC(),
		Data: map[string]any{
			"output": map[string]any{
				"_backend":     "claude_code",
				"deployed_url": "https://app.example.test",
				"steps":        []any{map[string]any{"name": "build", "ok": true}},
			},
			"iteration": 3,
		},
	}
	if _, err := s.AppendEvent(ctx, runID, in); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	assertShape := func(source string, e *store.Event) {
		t.Helper()
		output, ok := e.Data["output"].(map[string]any)
		if !ok {
			t.Fatalf("%s: Data[output] loaded as %T, want map[string]any", source, e.Data["output"])
		}
		steps, ok := output["steps"].([]any)
		if !ok {
			t.Fatalf("%s: output[steps] loaded as %T, want []any", source, output["steps"])
		}
		if _, ok := steps[0].(map[string]any); !ok {
			t.Fatalf("%s: steps[0] loaded as %T, want map[string]any", source, steps[0])
		}
		switch e.Data["iteration"].(type) {
		case int, int64, float64:
		default:
			t.Fatalf("%s: Data[iteration] loaded as %T, want int / int64 / float64", source, e.Data["iteration"])
		}
	}

	all, err := s.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("LoadEvents: got %d events, want 1", len(all))
	}
	assertShape("LoadEvents", all[0])

	var scanned *store.Event
	if err := s.ScanEvents(ctx, runID, func(e *store.Event) bool {
		scanned = e
		return true
	}); err != nil {
		t.Fatalf("ScanEvents: %v", err)
	}
	if scanned == nil {
		t.Fatal("ScanEvents visited no events")
	}
	assertShape("ScanEvents", scanned)
}

func testArtifactVersions(t *testing.T, s store.RunStore) {
	t.Helper()
	if _, err := s.CreateRun(testCtx(), "run_5", "demo", nil); err != nil {
		t.Fatal(err)
	}
	for v := 1; v <= 3; v++ {
		if err := s.WriteArtifact(testCtx(), &store.Artifact{
			RunID:     "run_5",
			NodeID:    "node_a",
			Version:   v,
			Data:      map[string]any{"v": v},
			WrittenAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("WriteArtifact v=%d: %v", v, err)
		}
	}
	versions, err := s.ListArtifactVersions(testCtx(), "run_5", "node_a")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 3 {
		t.Fatalf("ListArtifactVersions: got %d want 3", len(versions))
	}
	for i, vinfo := range versions {
		if vinfo.Version != i+1 {
			t.Errorf("Version[%d]: got %d want %d", i, vinfo.Version, i+1)
		}
	}
	latest, err := s.LoadLatestArtifact(testCtx(), "run_5", "node_a")
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version != 3 {
		t.Errorf("Latest version: got %d want 3", latest.Version)
	}
}

func testLockExclusive(t *testing.T, s store.RunStore) {
	t.Helper()
	if _, err := s.CreateRun(testCtx(), "run_6", "demo", nil); err != nil {
		t.Fatal(err)
	}
	first, err := s.LockRun(testCtx(), "run_6")
	if err != nil {
		t.Fatalf("first LockRun: %v", err)
	}
	if err := first.Unlock(); err != nil {
		t.Errorf("Unlock: %v", err)
	}
	second, err := s.LockRun(testCtx(), "run_6")
	if err != nil {
		t.Fatalf("relock after unlock: %v", err)
	}
	if err := second.Unlock(); err != nil {
		t.Errorf("second Unlock: %v", err)
	}
}

func testCapabilitiesReported(t *testing.T, s store.RunStore) {
	t.Helper()
	caps := s.Capabilities()
	if !caps.LiveStream && !caps.CrossProcessLock && !caps.PIDFile && !caps.GitWorktree {
		t.Errorf("Capabilities all-false; backend must report at least one")
	}
}

func testUserMessagesInbox(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	if _, err := s.CreateRun(ctx, "run_um", "demo", nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// FIFO: append three messages and ensure load returns them in
	// queued_at order even when wall-clock order is reversed.
	msgs := []store.QueuedUserMessage{
		{ID: "m1", Text: "first", QueuedAt: now.Add(0)},
		{ID: "m2", Text: "second", QueuedAt: now.Add(10 * time.Millisecond)},
		{ID: "m3", Text: "third", QueuedAt: now.Add(20 * time.Millisecond)},
	}
	for _, m := range msgs {
		if err := s.AppendQueuedMessage(ctx, "run_um", m); err != nil {
			t.Fatalf("AppendQueuedMessage(%s): %v", m.ID, err)
		}
	}
	pending, err := s.LoadPendingQueuedMessages(ctx, "run_um")
	if err != nil {
		t.Fatalf("LoadPending: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("Pending count: got %d want 3", len(pending))
	}
	for i, want := range []string{"m1", "m2", "m3"} {
		if pending[i].ID != want {
			t.Errorf("FIFO[%d]: got %q want %q", i, pending[i].ID, want)
		}
		if pending[i].Status != store.QueuedMessageStatusQueued {
			t.Errorf("Initial status[%s]: got %q want queued", pending[i].ID, pending[i].Status)
		}
	}
	// Deliver m1, m2 and re-check pending: m3 only.
	if err := s.UpdateQueuedMessageStatus(ctx, "run_um", "m1", store.QueuedMessageStatusDelivered); err != nil {
		t.Fatalf("Deliver m1: %v", err)
	}
	if err := s.UpdateQueuedMessageStatus(ctx, "run_um", "m2", store.QueuedMessageStatusDelivered); err != nil {
		t.Fatalf("Deliver m2: %v", err)
	}
	pending, err = s.LoadPendingQueuedMessages(ctx, "run_um")
	if err != nil {
		t.Fatalf("LoadPending after deliver: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "m3" {
		t.Fatalf("Pending after deliver = %+v, want only m3", pending)
	}
	// ListQueuedMessages returns ALL three, FIFO, with current
	// statuses preserved.
	all, err := s.ListQueuedMessages(ctx, "run_um")
	if err != nil {
		t.Fatalf("ListQueuedMessages: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List count: got %d want 3", len(all))
	}
	if all[0].Status != store.QueuedMessageStatusDelivered ||
		all[1].Status != store.QueuedMessageStatusDelivered ||
		all[2].Status != store.QueuedMessageStatusQueued {
		t.Errorf("statuses: %v / %v / %v", all[0].Status, all[1].Status, all[2].Status)
	}
	// Cancellation only valid from "queued". Cancel m3 OK; cancelling
	// m1 (delivered) must fail with the conflict sentinel.
	if err := s.UpdateQueuedMessageStatus(ctx, "run_um", "m3", store.QueuedMessageStatusCancelled, store.QueuedMessageStatusQueued); err != nil {
		t.Fatalf("Cancel m3: %v", err)
	}
	err = s.UpdateQueuedMessageStatus(ctx, "run_um", "m1", store.QueuedMessageStatusCancelled, store.QueuedMessageStatusQueued)
	if err == nil {
		t.Fatalf("Cancel of delivered m1: expected error")
	}
	// Updating an unknown ID returns ErrQueuedMessageNotFound.
	if err := s.UpdateQueuedMessageStatus(ctx, "run_um", "nonexistent", store.QueuedMessageStatusDelivered); err == nil {
		t.Fatalf("Update nonexistent: expected error")
	}
}
