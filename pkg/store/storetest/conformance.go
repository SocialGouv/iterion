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
	t.Run("OutcomeSeqAndTypedCauses", func(t *testing.T) { testOutcomeSeqAndTypedCauses(t, factory(t)) })
	t.Run("SaveRunHostileValues", func(t *testing.T) { testSaveRunHostileValues(t, factory(t)) })
	t.Run("RoutingPolicyImmutable", func(t *testing.T) { testRoutingPolicyImmutable(t, factory(t)) })
	t.Run("OutputsSurviveTerminal", func(t *testing.T) { testOutputsSurviveTerminal(t, factory(t)) })
	t.Run("RouteDecisionRegistry", func(t *testing.T) { testRouteDecisionRegistry(t, factory(t)) })
	t.Run("QueuedAttemptCAS", func(t *testing.T) { testQueuedAttemptCAS(t, factory(t)) })
	t.Run("MergeClaimCAS", func(t *testing.T) { testMergeClaimCAS(t, factory(t)) })
	t.Run("SaveRunPreservesLiveMergeClaim", func(t *testing.T) { testSaveRunPreservesLiveMergeClaim(t, factory(t)) })
	t.Run("FailRunTerminal", func(t *testing.T) { testFailRunTerminal(t, factory(t)) })
	t.Run("FailureCodeLifecycle", func(t *testing.T) { testFailureCodeLifecycle(t, factory(t)) })
	t.Run("TransitionSideEffects", func(t *testing.T) { testTransitionSideEffects(t, factory(t)) })
	t.Run("TombstoneRefusesWriters", func(t *testing.T) { testTombstoneRefusesWriters(t, factory(t)) })
	t.Run("PausePointerLifecycle", func(t *testing.T) { testPausePointerLifecycle(t, factory(t)) })
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

// testFailureCodeLifecycle pins the ADR-095 persistence discipline on
// BOTH store twins: the typed code lands in the same write as the
// failure status (plain, coded-CAS and FailRun* forms), an UNKNOWN
// code round-trips unharmed (open-world contract), and EVERY
// transition to a non-failure status clears it — the invariant that
// keeps a resumed run from lying about a past failure.
func testFailureCodeLifecycle(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	if _, err := s.CreateRun(ctx, "run_fc", "demo", nil); err != nil {
		t.Fatal(err)
	}
	// FailRunResumable carries the code atomically.
	cp := &store.Checkpoint{NodeID: "n1"}
	if err := s.FailRunResumable(ctx, "run_fc", cp, "quota window shut", store.FailureUsageLimitBlocked); err != nil {
		t.Fatal(err)
	}
	r, err := s.LoadRun(ctx, "run_fc")
	if err != nil {
		t.Fatal(err)
	}
	if r.FailureCode != store.FailureUsageLimitBlocked {
		t.Fatalf("FailureCode after FailRunResumable: got %q", r.FailureCode)
	}
	// Resume (any transition to running) clears code AND error together.
	if err := s.UpdateRunStatus(ctx, "run_fc", store.RunStatusRunning, ""); err != nil {
		t.Fatal(err)
	}
	r, _ = s.LoadRun(ctx, "run_fc")
	if r.FailureCode != "" || r.Error != "" {
		t.Fatalf("resume must clear code+error, got code=%q error=%q", r.FailureCode, r.Error)
	}
	// Coded CAS: code and status land in one write.
	changed, err := s.UpdateRunStatusIfCoded(ctx, "run_fc", store.RunStatusCancelled, "run cancelled", store.FailureCancelled, []store.RunStatus{store.RunStatusRunning})
	if err != nil || !changed {
		t.Fatalf("coded CAS: changed=%v err=%v", changed, err)
	}
	r, _ = s.LoadRun(ctx, "run_fc")
	if r.FailureCode != store.FailureCancelled {
		t.Fatalf("FailureCode after coded CAS: got %q", r.FailureCode)
	}
	// Transition to queued (the cloud resume pre-flip) clears it too —
	// the invariant covers queued, not only running.
	if _, err := s.UpdateRunStatusIf(ctx, "run_fc", store.RunStatusQueued, "", []store.RunStatus{store.RunStatusCancelled}); err != nil {
		t.Fatal(err)
	}
	r, _ = s.LoadRun(ctx, "run_fc")
	if r.FailureCode != "" {
		t.Fatalf("queued must clear the code, got %q", r.FailureCode)
	}
	// A checkpoint-coupled pause (which bypasses the transition choke
	// point) still clears the classification: paused is not a failure.
	if err := s.UpdateRunStatusCoded(ctx, "run_fc", store.RunStatusFailedResumable, "parked", store.FailureInterrupted); err != nil {
		t.Fatal(err)
	}
	if err := s.PauseRun(ctx, "run_fc", &store.Checkpoint{NodeID: "n2"}); err != nil {
		t.Fatal(err)
	}
	r, _ = s.LoadRun(ctx, "run_fc")
	if r.FailureCode != "" {
		t.Fatalf("pause must clear the code, got %q", r.FailureCode)
	}

	// Open-world: an unknown, non-empty code survives persistence.
	if err := s.UpdateRunStatusCoded(ctx, "run_fc", store.RunStatusFailed, "boom", store.FailureCode("SOME_FUTURE_CODE_V9")); err != nil {
		t.Fatal(err)
	}
	r, _ = s.LoadRun(ctx, "run_fc")
	if r.FailureCode != "SOME_FUTURE_CODE_V9" {
		t.Fatalf("unknown code mangled: %q", r.FailureCode)
	}
}

// testTransitionSideEffects pins the transition side effects BOTH twins
// must share (each was a live FS↔Mongo divergence): the running claim
// clears checkpoint AND error, PauseRun clears FinishedAt and the code,
// SaveRun normalizes a stale code, and an empty-expectedFrom CAS is a
// loud error — never a silent no-op on one twin and an unconditional
// write on the other.
func testTransitionSideEffects(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	if _, err := s.CreateRun(ctx, "run_tse", "demo", nil); err != nil {
		t.Fatal(err)
	}
	cp := &store.Checkpoint{NodeID: "n1"}
	if err := s.FailRunResumable(ctx, "run_tse", cp, "boom", store.FailureInterrupted); err != nil {
		t.Fatal(err)
	}
	// The running claim PRESERVES the previous attempt's checkpoint on
	// both twins: the park writers that follow (drain, usage-cap,
	// orphan sweeps) flip running→failed_resumable without a
	// checkpoint of their own, and the resume point must survive that
	// round trip — a pod dying between its claim and its first own
	// checkpoint resumes from the previous attempt's node, never from
	// the workflow entry.
	if err := s.UpdateRunStatus(ctx, "run_tse", store.RunStatusRunning, "should not persist"); err != nil {
		t.Fatal(err)
	}
	r, err := s.LoadRun(ctx, "run_tse")
	if err != nil {
		t.Fatal(err)
	}
	if r.Checkpoint == nil || r.Checkpoint.NodeID != "n1" {
		t.Errorf("running claim destroyed the resume point (checkpoint %v)", r.Checkpoint)
	}
	if r.Error != "" {
		t.Errorf("running run must carry no failure message, got %q", r.Error)
	}
	// PauseRun: not over, so no terminal timestamp and no failure code.
	if err := s.FailRunResumable(ctx, "run_tse", cp, "boom", store.FailureInterrupted); err != nil {
		t.Fatal(err)
	}
	if err := s.PauseRun(ctx, "run_tse", &store.Checkpoint{NodeID: "n2"}); err != nil {
		t.Fatal(err)
	}
	r, _ = s.LoadRun(ctx, "run_tse")
	if r.FinishedAt != nil {
		t.Error("paused run kept a stale FinishedAt")
	}
	if r.FailureCode != "" {
		t.Errorf("paused run kept a stale code %q", r.FailureCode)
	}
	// SaveRun normalizes: a copy loaded before a status change must not
	// resurrect its failure code through the full-document write.
	r.Status = store.RunStatusRunning
	r.FailureCode = store.FailureUsageLimitBlocked
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	r, _ = s.LoadRun(ctx, "run_tse")
	if r.FailureCode != "" {
		t.Errorf("SaveRun resurrected a stale code %q on a running run", r.FailureCode)
	}
	// Finished keeps the checkpoint too: `iterion fork` reads a
	// terminal parent's checkpoint for its upstream outputs.
	if err := s.UpdateRunStatus(ctx, "run_tse", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	r, _ = s.LoadRun(ctx, "run_tse")
	if r.Checkpoint == nil {
		t.Error("finished transition destroyed the checkpoint a fork would read")
	}
	if err := s.UpdateRunStatus(ctx, "run_tse", store.RunStatusRunning, ""); err != nil {
		t.Fatal(err)
	}

	// Empty expectedFrom: a loud error on both twins (FS used to no-op
	// silently while Mongo wrote unconditionally).
	if _, err := s.UpdateRunStatusIf(ctx, "run_tse", store.RunStatusCancelled, "x", nil); err == nil {
		t.Error("empty-expectedFrom CAS must be refused")
	}
	r, _ = s.LoadRun(ctx, "run_tse")
	if r.Status == store.RunStatusCancelled {
		t.Error("empty-expectedFrom CAS wrote anyway")
	}
}

// testTombstoneRefusesWriters pins that a deleted run stays dead on both
// twins: no status/checkpoint/pause/failure writer may mutate the
// tombstone (Mongo used to write status, code and checkpoint onto the
// skeleton and report success).
func testTombstoneRefusesWriters(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	if _, err := s.CreateRun(ctx, "run_tomb", "demo", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRun(ctx, "run_tomb"); err != nil {
		t.Fatal(err)
	}
	cp := &store.Checkpoint{NodeID: "n1"}
	if err := s.FailRunResumable(ctx, "run_tomb", cp, "post-delete", store.FailureFailNode); err == nil {
		t.Error("FailRunResumable succeeded on a tombstone")
	}
	if err := s.FailRunTerminal(ctx, "run_tomb", cp, "post-delete", ""); err == nil {
		t.Error("FailRunTerminal succeeded on a tombstone")
	}
	if err := s.PauseRun(ctx, "run_tomb", cp); err == nil {
		t.Error("PauseRun succeeded on a tombstone")
	}
	if err := s.SaveCheckpoint(ctx, "run_tomb", cp); err == nil {
		t.Error("SaveCheckpoint succeeded on a tombstone")
	}
	if changed, _ := s.UpdateRunStatusIf(ctx, "run_tomb", store.RunStatusCancelled, "x", []store.RunStatus{store.RunStatusRunning, store.RunStatusFailed, store.RunStatusFailedResumable, store.RunStatusQueued, store.RunStatusFinished, store.RunStatusCancelled, store.RunStatusPausedWaitingHuman, store.RunStatusPausedOperator, "deleted"}); changed {
		t.Error("status CAS wrote onto a tombstone")
	}
	if _, err := s.LoadRun(ctx, "run_tomb"); !errors.Is(err, store.ErrRunDeleted) {
		t.Errorf("tombstone must read as ErrRunDeleted, got %v", err)
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
	if err := s.FailRunTerminal(testCtx(), "run_ft", cp, "workflow reached fail node", store.FailureFailNode); err != nil {
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
	if err := s.FailRunTerminal(testCtx(), "run_ft_cancel", cp, "late failure", ""); err != nil {
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

// testPausePointerLifecycle holds both backends to the pause-pointer
// consumption contract (store.CarriesPausePointer): the checkpoint
// survives every transition (ADR-095 §5), but its interaction evidence
// is a consumable — cleared by any transition into a status that
// cannot truthfully carry it, and PRESERVED on the paused → queued
// cloud-resume hop, which the runner's queued router reads to route a
// human-answers resume. Without the consumption, a status-only cancel
// of a paused run left the pointer live and a cloud resume crossed the
// human gate with an empty answer.
func testPausePointerLifecycle(t *testing.T, s store.RunStore) {
	ctx := testCtx()
	cp := func() *store.Checkpoint {
		return &store.Checkpoint{NodeID: "gate", InteractionID: "I1",
			InteractionQuestions: map[string]any{"approve": "yes?"}}
	}

	// Cancel consumes: pause → cancelled clears the pointer, keeps the node.
	if _, err := s.CreateRun(ctx, "pp-cancel", "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.PauseRun(ctx, "pp-cancel", cp()); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunStatus(ctx, "pp-cancel", store.RunStatusCancelled, "operator cancel"); err != nil {
		t.Fatal(err)
	}
	r, err := s.LoadRun(ctx, "pp-cancel")
	if err != nil {
		t.Fatal(err)
	}
	if r.Checkpoint == nil || r.Checkpoint.NodeID != "gate" {
		t.Fatalf("cancel must preserve the checkpoint anchor, got %+v", r.Checkpoint)
	}
	if r.Checkpoint.InteractionID != "" || len(r.Checkpoint.InteractionQuestions) > 0 {
		t.Errorf("cancel left the pause pointer live (%q, %d questions) — a cloud resume would cross the human gate with an empty answer",
			r.Checkpoint.InteractionID, len(r.Checkpoint.InteractionQuestions))
	}

	// The queued hop preserves: pause → queued (SubmitResume with answers)
	// must keep the pointer — it is what routes the runner's Resume into
	// the pause path where the answers are recorded.
	if _, err := s.CreateRun(ctx, "pp-queued", "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.PauseRun(ctx, "pp-queued", cp()); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.UpdateRunStatusIf(ctx, "pp-queued", store.RunStatusQueued, "",
		[]store.RunStatus{store.RunStatusPausedWaitingHuman}); err != nil || !ok {
		t.Fatalf("queued CAS: ok=%v err=%v", ok, err)
	}
	r, err = s.LoadRun(ctx, "pp-queued")
	if err != nil {
		t.Fatal(err)
	}
	if r.Checkpoint == nil || r.Checkpoint.InteractionID != "I1" {
		t.Errorf("the paused → queued hop must PRESERVE the pause pointer (the runner routes the answers resume on it), got %+v", r.Checkpoint)
	}

	// SaveRun on a non-carrying status must not resurrect a consumed
	// pointer through a full-document write.
	r, err = s.LoadRun(ctx, "pp-cancel")
	if err != nil {
		t.Fatal(err)
	}
	r.Checkpoint.InteractionID = "I-resurrected"
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	r, err = s.LoadRun(ctx, "pp-cancel")
	if err != nil {
		t.Fatal(err)
	}
	if r.Checkpoint.InteractionID != "" {
		t.Errorf("SaveRun resurrected a consumed pause pointer on a cancelled run: %q", r.Checkpoint.InteractionID)
	}

	// SaveCheckpoint must not resurrect it either: SaveRun normalizes
	// its own COPY, so a caller that then re-persists its original
	// checkpoint (the rewind shape: SaveRun(run) then SaveCheckpoint(cp))
	// would replay the live pointer. Only PauseRun writes one.
	if err := s.SaveCheckpoint(ctx, "pp-cancel", cp()); err != nil {
		t.Fatal(err)
	}
	r, err = s.LoadRun(ctx, "pp-cancel")
	if err != nil {
		t.Fatal(err)
	}
	if r.Checkpoint.InteractionID != "" || len(r.Checkpoint.InteractionQuestions) > 0 {
		t.Errorf("SaveCheckpoint resurrected the pause pointer (%q) — the rewind write shape replays it", r.Checkpoint.InteractionID)
	}

	// …but on a PAUSED run the write-through is legitimate: budget/
	// bookkeeping updates on a live pause must keep the pointer, or the
	// next resume cannot load its interaction.
	// pp-queued is queued (carries) — reuse it: bump a counter and re-save.
	if ok, cerr := s.UpdateRunStatusIf(ctx, "pp-queued", store.RunStatusPausedWaitingHuman, "",
		[]store.RunStatus{store.RunStatusQueued}); cerr != nil || !ok {
		t.Fatalf("back to paused: ok=%v err=%v", ok, cerr)
	}
	pausedCp := cp()
	pausedCp.BudgetCostUSD = 0.95
	if err := s.SaveCheckpoint(ctx, "pp-queued", pausedCp); err != nil {
		t.Fatal(err)
	}
	r, err = s.LoadRun(ctx, "pp-queued")
	if err != nil {
		t.Fatal(err)
	}
	if r.Checkpoint.InteractionID != "I1" {
		t.Errorf("SaveCheckpoint on a PAUSED run stripped the live pointer (%q) — the next resume cannot load its interaction", r.Checkpoint.InteractionID)
	}
}

// testOutcomeSeqAndTypedCauses drives the outcome bookkeeping: every
// terminal arrival is a new episode, typed metadata travels with the
// transition that carries it (and ONLY that one), and full-document
// savers can neither rewind the counter nor resurrect stale metadata.
func testOutcomeSeqAndTypedCauses(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	const runID = "run-outcome-seq"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	load := func() *store.Run {
		t.Helper()
		r, err := s.LoadRun(ctx, runID)
		if err != nil {
			t.Fatalf("LoadRun: %v", err)
		}
		return r
	}

	// Non-terminal transitions don't count episodes.
	if err := s.UpdateRunStatus(ctx, runID, store.RunStatusRunning, ""); err != nil {
		t.Fatalf("running: %v", err)
	}
	if r := load(); r.OutcomeSeq != 0 {
		t.Fatalf("seq after running = %d, want 0", r.OutcomeSeq)
	}

	// First terminal arrival: episode 1, untyped ⇒ empty metadata.
	if err := s.UpdateRunStatus(ctx, runID, store.RunStatusFailedResumable, "boom"); err != nil {
		t.Fatalf("failed_resumable: %v", err)
	}
	r := load()
	if r.OutcomeSeq != 1 || r.FailureCode != "" || r.ContinuationState != "" {
		t.Fatalf("episode 1 = (seq %d, code %q, cont %q), want (1, \"\", \"\")", r.OutcomeSeq, r.FailureCode, r.ContinuationState)
	}

	// Typed transition: episode 2 carries its cause (ADR-095's
	// failure_code — ONE taxonomy) and continuation.
	changed, err := s.UpdateRunOutcome(ctx, runID, store.RunStatusCancelled, "stopped",
		store.RunOutcomeMeta{Code: store.FailureCancelled, Continuation: store.ContinuationFinal},
		[]store.RunStatus{store.RunStatusFailedResumable})
	if err != nil || !changed {
		t.Fatalf("typed cancel = (%t, %v), want (true, nil)", changed, err)
	}
	r = load()
	if r.OutcomeSeq != 2 || r.FailureCode != store.FailureCancelled || r.ContinuationState != store.ContinuationFinal {
		t.Fatalf("episode 2 = (seq %d, code %q, cont %q)", r.OutcomeSeq, r.FailureCode, r.ContinuationState)
	}
	// The CAS arm still works.
	if changed, err := s.UpdateRunOutcome(ctx, runID, store.RunStatusFailed, "no",
		store.RunOutcomeMeta{Code: store.FailureExecutionFailed}, []store.RunStatus{store.RunStatusRunning}); err != nil || changed {
		t.Fatalf("outcome CAS mismatch = (%t, %v), want (false, nil)", changed, err)
	}

	// Leaving the terminal state clears the metadata: stale metadata
	// must never describe a newer outcome.
	if err := s.UpdateRunStatus(ctx, runID, store.RunStatusRunning, ""); err != nil {
		t.Fatalf("resume: %v", err)
	}
	r = load()
	if r.OutcomeSeq != 2 || r.FailureCode != "" || r.ContinuationState != "" {
		t.Fatalf("post-resume = (seq %d, code %q, cont %q), want (2, \"\", \"\")", r.OutcomeSeq, r.FailureCode, r.ContinuationState)
	}

	// The checkpoint-aware ENGINE writer types its episode — code only:
	// the engine does not know the queue topology, so it never states a
	// continuation (the runner promotes it below).
	if err := s.FailRunResumable(ctx, runID, &store.Checkpoint{NodeID: "n"}, "drained",
		store.FailureInterrupted); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}
	r = load()
	if r.OutcomeSeq != 3 || r.FailureCode != store.FailureInterrupted || r.ContinuationState != "" {
		t.Fatalf("episode 3 = (seq %d, code %q, cont %q), want (3, INTERRUPTED, \"\")", r.OutcomeSeq, r.FailureCode, r.ContinuationState)
	}

	// The RUNNER promotes the continuation at the actual NAK — a
	// same-status write that states ownership WITHOUT inventing an
	// episode (the transition-gated increment).
	if changed, err := s.UpdateRunOutcome(ctx, runID, store.RunStatusFailedResumable, "drained",
		store.RunOutcomeMeta{Code: store.FailureInterrupted, Continuation: store.ContinuationRedeliveryPending},
		[]store.RunStatus{store.RunStatusFailedResumable}); err != nil || !changed {
		t.Fatalf("continuation promote = (%t, %v), want (true, nil)", changed, err)
	}
	r = load()
	if r.OutcomeSeq != 3 || r.ContinuationState != store.ContinuationRedeliveryPending {
		t.Fatalf("promoted = (seq %d, cont %q), want (3, redelivery_pending) — a same-status promote must not invent an episode", r.OutcomeSeq, r.ContinuationState)
	}

	// A full-document save with a STALE in-memory run (same status)
	// keeps the persisted bookkeeping — it can neither rewind the
	// counter nor clear the cause/continuation a transition wrote
	// meanwhile.
	stale := *r
	stale.OutcomeSeq = 0
	stale.FailureCode = ""
	stale.ContinuationState = ""
	if err := s.SaveRun(ctx, &stale); err != nil {
		t.Fatalf("stale SaveRun: %v", err)
	}
	r = load()
	if r.OutcomeSeq != 3 || r.FailureCode != store.FailureInterrupted || r.ContinuationState != store.ContinuationRedeliveryPending {
		t.Fatalf("after stale save = (seq %d, code %q, cont %q), want preserved (3, INTERRUPTED, redelivery_pending)", r.OutcomeSeq, r.FailureCode, r.ContinuationState)
	}

	// A status CHANGE through SaveRun is a transition: metadata clears,
	// and a terminal arrival counts an episode.
	moved := *r
	moved.Status = store.RunStatusRunning
	if err := s.SaveRun(ctx, &moved); err != nil {
		t.Fatalf("SaveRun to running: %v", err)
	}
	if r = load(); r.OutcomeSeq != 3 || r.FailureCode != "" {
		t.Fatalf("save-to-running = (seq %d, code %q), want (3, \"\")", r.OutcomeSeq, r.FailureCode)
	}
	moved = *r
	moved.Status = store.RunStatusFinished
	if err := s.SaveRun(ctx, &moved); err != nil {
		t.Fatalf("SaveRun to finished: %v", err)
	}
	if r = load(); r.OutcomeSeq != 4 {
		t.Fatalf("save-to-finished seq = %d, want 4", r.OutcomeSeq)
	}
}

// testSaveRunHostileValues guards the Mongo pipeline against
// aggregation-expression evaluation of DATA: a $-prefixed string value
// must round-trip verbatim (not be parsed as a field path and dropped
// or substituted), and a dotted map key must not reject the write —
// agent outputs ("$ ./gradlew build") and user inputs ("config.path")
// produce both shapes routinely.
func testSaveRunHostileValues(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	const runID = "run-hostile-values"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.Error = "$workflow_name is not a field path"
	r.Inputs = map[string]any{"config.path": "/etc/app.yml", "cmd": "$JAVA_HOME/bin/java"}
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun with hostile values: %v", err)
	}
	got, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got.Error != r.Error {
		t.Fatalf("Error = %q, want %q (a $-value was evaluated, not stored)", got.Error, r.Error)
	}
	if got.Inputs["config.path"] != "/etc/app.yml" || got.Inputs["cmd"] != "$JAVA_HOME/bin/java" {
		t.Fatalf("Inputs = %v, want the hostile keys/values verbatim", got.Inputs)
	}

	// An upsert-create directly in a terminal status is NOT an episode:
	// nothing transitioned, the document was born that way (fork seeds
	// cancelled children through SaveRun on a run that does not exist).
	born := *got
	born.ID = "run-born-terminal"
	born.Status = store.RunStatusCancelled
	born.OutcomeSeq = 0
	if err := s.SaveRun(ctx, &born); err != nil {
		t.Fatalf("SaveRun upsert-create: %v", err)
	}
	if b, err := s.LoadRun(ctx, "run-born-terminal"); err != nil || b.OutcomeSeq != 0 {
		t.Fatalf("born-terminal = (seq %d, %v), want (0, nil)", b.OutcomeSeq, err)
	}
}

// testMergeClaimCAS drives the merge state machine at the store level:
// the claim CAS (entry), the conditional persist (exit), the stale
// steal with claim-token isolation, and the no-clobber-merged
// invariant that closes the double-squash TOCTOU.
func testMergeClaimCAS(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	const runID = "run-merge-claim"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	notStale := time.Now().Add(-15 * time.Minute)

	// Entry: an unset merge_status is claimable, prior comes back "".
	claimed, prior, tokenA, err := s.ClaimMerge(ctx, runID, notStale)
	if err != nil || !claimed || prior != "" {
		t.Fatalf("first claim = (%t, %q, %v), want (true, \"\", nil)", claimed, prior, err)
	}
	if tokenA.IsZero() {
		t.Fatal("claim must return its token")
	}
	// A held (fresh) claim refuses the second claimant.
	claimed, prior, _, err = s.ClaimMerge(ctx, runID, notStale)
	if err != nil || claimed || prior != store.MergeStatusMerging {
		t.Fatalf("second claim = (%t, %q, %v), want (false, merging, nil)", claimed, prior, err)
	}

	// Exit: the holder persists the outcome conditioned on "merging"
	// AND its own token.
	changed, err := s.UpdateRunMergeIf(ctx, runID, store.RunMergeUpdate{
		Status:          store.MergeStatusMerged,
		MergedCommit:    "abc123",
		MergedInto:      "main",
		MergeStrategy:   store.MergeStrategySquash,
		ExpectClaimedAt: tokenA,
	}, []store.MergeStatus{store.MergeStatusMerging})
	if err != nil || !changed {
		t.Fatalf("persist merged = (%t, %v), want (true, nil)", changed, err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil || r.MergeStatus != store.MergeStatusMerged || r.MergedCommit != "abc123" || r.MergedInto != "main" {
		t.Fatalf("merged bookkeeping = %+v (%v)", r, err)
	}
	if !r.MergeClaimedAt.IsZero() {
		t.Errorf("MergeClaimedAt should be cleared by the exit write, got %v", r.MergeClaimedAt)
	}

	// No-clobber: a late writer still expecting "merging" (the loser of
	// a race, or a stolen-claim holder) cannot overwrite "merged".
	changed, err = s.UpdateRunMergeIf(ctx, runID, store.RunMergeUpdate{Status: store.MergeStatusFailed},
		[]store.MergeStatus{store.MergeStatusMerging})
	if err != nil || changed {
		t.Fatalf("clobber attempt = (%t, %v), want (false, nil)", changed, err)
	}
	if got, _ := s.LoadRun(ctx, runID); got.MergeStatus != store.MergeStatusMerged || got.MergedCommit != "abc123" {
		t.Fatalf("merged record damaged: %+v", got)
	}
	// And "merged" is terminal for the claim too.
	claimed, prior, _, err = s.ClaimMerge(ctx, runID, notStale)
	if err != nil || claimed || prior != store.MergeStatusMerged {
		t.Fatalf("claim on merged = (%t, %q, %v), want (false, merged, nil)", claimed, prior, err)
	}

	// Stale steal: a claim whose stamp predates staleBefore is up for
	// grabs — AND the stolen-from claimant's token consumes nothing
	// afterwards (the claim names an owner, not just a state; without
	// the token check the crashed claimant's late failure write would
	// overwrite the live claimant's outcome).
	const runID2 = "run-merge-claim-stale"
	if _, err := s.CreateRun(ctx, runID2, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	_, _, tokenOld, err := s.ClaimMerge(ctx, runID2, notStale)
	if err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if claimed, _, _, err := s.ClaimMerge(ctx, runID2, notStale); err != nil || claimed {
		t.Fatalf("fresh claim must hold, got steal (%t, %v)", claimed, err)
	}
	claimed, prior, tokenNew, err := s.ClaimMerge(ctx, runID2, time.Now().Add(time.Second))
	if err != nil || !claimed || prior != store.MergeStatusMerging {
		t.Fatalf("stale steal = (%t, %q, %v), want (true, merging, nil)", claimed, prior, err)
	}
	if tokenNew.Equal(tokenOld) {
		t.Fatal("steal must issue a fresh token")
	}
	changed, err = s.UpdateRunMergeIf(ctx, runID2, store.RunMergeUpdate{Status: store.MergeStatusFailed, ExpectClaimedAt: tokenOld},
		[]store.MergeStatus{store.MergeStatusMerging})
	if err != nil || changed {
		t.Fatalf("stolen-from claimant's write = (%t, %v), want (false, nil)", changed, err)
	}
	changed, err = s.UpdateRunMergeIf(ctx, runID2, store.RunMergeUpdate{Status: store.MergeStatusMerged, MergedCommit: "def456", MergedInto: "main", ExpectClaimedAt: tokenNew},
		[]store.MergeStatus{store.MergeStatusMerging})
	if err != nil || !changed {
		t.Fatalf("live claimant's write = (%t, %v), want (true, nil)", changed, err)
	}

	// A "merging" whose stamp is missing entirely (a full-document
	// writer dropped it) is claimable — it must not wedge the run.
	const runID3 = "run-merge-claim-nostamp"
	if _, err := s.CreateRun(ctx, runID3, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if changed, err := s.UpdateRunMergeIf(ctx, runID3, store.RunMergeUpdate{Status: store.MergeStatusMerging},
		[]store.MergeStatus{""}); err != nil || !changed {
		t.Fatalf("seed stampless merging = (%t, %v)", changed, err)
	}
	claimed, _, _, err = s.ClaimMerge(ctx, runID3, notStale)
	if err != nil || !claimed {
		t.Fatalf("stampless merging must be claimable = (%t, %v)", claimed, err)
	}

	// skipped and conflicted stay claimable (/merge is the only path
	// that re-materialises a lost merge clone; a recovered run lands
	// "skipped" and must stay mergeable).
	for _, st := range []store.MergeStatus{store.MergeStatusSkipped, store.MergeStatusConflicted} {
		id := "run-merge-claim-" + string(st)
		if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		if changed, err := s.UpdateRunMergeIf(ctx, id, store.RunMergeUpdate{Status: st}, []store.MergeStatus{""}); err != nil || !changed {
			t.Fatalf("seed %s = (%t, %v)", st, changed, err)
		}
		claimed, prior, _, err := s.ClaimMerge(ctx, id, notStale)
		if err != nil || !claimed || prior != st {
			t.Fatalf("claim on %s = (%t, %q, %v), want (true, %s, nil)", st, claimed, prior, err, st)
		}
	}

	// Exit CAS with the empty status in expectedFrom matches an unset
	// field (a run that never entered the machine).
	const runID4 = "run-merge-claim-virgin"
	if _, err := s.CreateRun(ctx, runID4, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	changed, err = s.UpdateRunMergeIf(ctx, runID4, store.RunMergeUpdate{Status: store.MergeStatusPending},
		[]store.MergeStatus{""})
	if err != nil || !changed {
		t.Fatalf("empty-status CAS = (%t, %v), want (true, nil)", changed, err)
	}

	// A missing run is an ERROR, not a silent (false, nil) — a caller
	// must be able to tell a lost race from a deleted run.
	if _, err := s.UpdateRunMergeIf(ctx, "run-merge-claim-ghost", store.RunMergeUpdate{Status: store.MergeStatusPending},
		[]store.MergeStatus{""}); err == nil {
		t.Fatal("UpdateRunMergeIf on a missing run must error")
	}
}

// testSaveRunPreservesLiveMergeClaim: the merge claim is owned by the
// merge choke points (ClaimMerge / UpdateRunMergeIf); a full-document
// SaveRun from a writer that loaded the run BEFORE the claim (operator
// rename, rewind bookkeeping, cloud publisher stamps) must not disavow
// a live claim — clobbering merge_status+merge_claimed_at re-opens the
// double-squash the claim exists to prevent (the next claimant passes
// immediately while the first is mid-merge).
func testSaveRunPreservesLiveMergeClaim(t *testing.T, s store.RunStore) {
	ctx := testCtx()
	if _, err := s.CreateRun(ctx, "mc-save", "wf", nil); err != nil {
		t.Fatal(err)
	}
	// The stale copy, loaded before the claim.
	stale, err := s.LoadRun(ctx, "mc-save")
	if err != nil {
		t.Fatal(err)
	}
	claimed, _, token, err := s.ClaimMerge(ctx, "mc-save", time.Now().UTC())
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	// The stale writer saves (e.g. a rename): the claim must survive.
	stale.Name = "renamed"
	if err := s.SaveRun(ctx, stale); err != nil {
		t.Fatal(err)
	}
	r, err := s.LoadRun(ctx, "mc-save")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "renamed" {
		t.Errorf("the save's own payload was lost: name=%q", r.Name)
	}
	if r.MergeStatus != store.MergeStatusMerging {
		t.Errorf("SaveRun disavowed a live merge claim: merge_status=%q, want %q — the next claimant would double-squash", r.MergeStatus, store.MergeStatusMerging)
	}
	if !r.MergeClaimedAt.Equal(token) {
		t.Errorf("SaveRun dropped the claim stamp: %v, want %v", r.MergeClaimedAt, token)
	}
}

// testRoutingPolicyImmutable: once the launch persisted the contract,
// no full-document saver — however stale — can drop or replace it.
// Retroactively changing the contract of already-produced work is the
// exact attack the launch-frozen snapshot exists to prevent.
func testRoutingPolicyImmutable(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	const runID = "run-routing-policy"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	launch := &store.RoutingPolicy{Version: 1, SuccessWhen: "outputs.gate.ok", AllowedActions: []string{"merge"}}
	launch.Hash = launch.ComputeHash()
	r.RoutingPolicy = launch
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun launch: %v", err)
	}

	// A stale saver without the field cannot drop it…
	stale, _ := s.LoadRun(ctx, runID)
	stale.RoutingPolicy = nil
	if err := s.SaveRun(ctx, stale); err != nil {
		t.Fatalf("SaveRun stale: %v", err)
	}
	got, _ := s.LoadRun(ctx, runID)
	if got.RoutingPolicy == nil || got.RoutingPolicy.Hash != launch.Hash {
		t.Fatalf("policy dropped by a stale save: %+v", got.RoutingPolicy)
	}

	// …and a saver carrying a DIFFERENT contract cannot swap it.
	evil, _ := s.LoadRun(ctx, runID)
	swapped := &store.RoutingPolicy{Version: 1, SuccessWhen: "outputs.gate.other"}
	swapped.Hash = swapped.ComputeHash()
	evil.RoutingPolicy = swapped
	if err := s.SaveRun(ctx, evil); err != nil {
		t.Fatalf("SaveRun swap: %v", err)
	}
	got, _ = s.LoadRun(ctx, runID)
	if got.RoutingPolicy == nil || got.RoutingPolicy.Hash != launch.Hash {
		t.Fatalf("policy swapped by a save: %+v", got.RoutingPolicy)
	}
	// The first-write window closes at the terminal: a run that
	// finished WITHOUT a contract cannot be given one after the fact —
	// that would decide already-produced work retroactively.
	const lateID = "run-routing-policy-late"
	if _, err := s.CreateRun(ctx, lateID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.UpdateRunStatus(ctx, lateID, store.RunStatusFinished, ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	late, _ := s.LoadRun(ctx, lateID)
	late.RoutingPolicy = launch
	if err := s.SaveRun(ctx, late); err != nil {
		t.Fatalf("SaveRun late: %v", err)
	}
	if got, _ := s.LoadRun(ctx, lateID); got.RoutingPolicy != nil {
		t.Fatalf("a contract was fixed onto already-terminal work: %+v", got.RoutingPolicy)
	}
}

// testOutputsSurviveTerminal: the checkpoint's outputs are the run's
// terminal evidence — the values a routing contract evaluates. They
// must survive the transition INTO finished on every backend (the FS
// store used to clear them there while Mongo kept them: the two
// backends diverged on the very field a decision reads).
func testOutputsSurviveTerminal(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	const runID = "run-outputs-survive"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	cp := &store.Checkpoint{Outputs: map[string]map[string]any{"gate": {"converged": true}}}
	if err := s.SaveCheckpoint(ctx, runID, cp); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := s.UpdateRunStatus(ctx, runID, store.RunStatusFinished, ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Checkpoint == nil || r.Checkpoint.Outputs["gate"]["converged"] != true {
		t.Fatalf("terminal outputs destroyed by the finish transition: %+v", r.Checkpoint)
	}
}

// testRouteDecisionRegistry holds both registry backends to one
// contract: the unique episode claim, the leased steal of an orphaned
// "claimed" row, the bounded retry of "failed", the finish states, the
// audit ordering and the sweep query.
func testRouteDecisionRegistry(t *testing.T, s store.RunStore) {
	t.Helper()
	rds := store.AsRouteDecisionStore(s)
	if rds == nil {
		t.Skip("backend has no route-decision registry")
	}
	ctx := testCtx()
	const runID = "run-route-registry"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// staleNever: the production threshold — a claim just taken is
	// fresh. staleAlways: a future threshold, which reads every existing
	// claim as expired (the ClaimMerge testability precedent).
	staleNever := func() time.Time { return time.Now().Add(-store.RouteClaimLease) }
	staleAlways := func() time.Time { return time.Now().Add(time.Hour) }

	// Fresh claim; duplicate refused with the existing row.
	claimed, _, err := rds.ClaimRouteDecision(ctx, store.RouteDecision{RunID: runID, OutcomeSeq: 1, Decision: "merge", Reason: "r1"}, staleNever())
	if err != nil || !claimed {
		t.Fatalf("first claim = (%t, %v)", claimed, err)
	}
	claimed, existing, err := rds.ClaimRouteDecision(ctx, store.RouteDecision{RunID: runID, OutcomeSeq: 1, Decision: "merge"}, staleNever())
	if err != nil || claimed || existing == nil || existing.State != store.RouteDecisionClaimed {
		t.Fatalf("dup claim = (%t, %+v, %v), want refused with the claimed row", claimed, existing, err)
	}

	// Finish → succeeded; a succeeded episode is never reclaimable —
	// not even under a threshold that reads every claim as stale.
	if err := rds.FinishRouteDecision(ctx, runID, 1, store.RouteDecisionSucceeded, ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if claimed, _, err := rds.ClaimRouteDecision(ctx, store.RouteDecision{RunID: runID, OutcomeSeq: 1}, staleAlways()); err != nil || claimed {
		t.Fatalf("succeeded episode reclaimed = (%t, %v)", claimed, err)
	}

	// A failed episode is reclaimable, but bounded by the attempt cap.
	if claimed, _, err := rds.ClaimRouteDecision(ctx, store.RouteDecision{RunID: runID, OutcomeSeq: 2, Decision: "merge"}, staleNever()); err != nil || !claimed {
		t.Fatalf("claim ep2 = (%t, %v)", claimed, err)
	}
	for attempt := 1; ; attempt++ {
		if err := rds.FinishRouteDecision(ctx, runID, 2, store.RouteDecisionFailed, "transient"); err != nil {
			t.Fatalf("fail ep2 (attempt %d): %v", attempt, err)
		}
		claimed, _, err := rds.ClaimRouteDecision(ctx, store.RouteDecision{RunID: runID, OutcomeSeq: 2, Decision: "merge"}, staleNever())
		if err != nil {
			t.Fatalf("reclaim ep2: %v", err)
		}
		if !claimed {
			if attempt < store.MaxRouteDecisionAttempts-1 {
				t.Fatalf("failed episode refused after only %d attempts (cap %d)", attempt, store.MaxRouteDecisionAttempts)
			}
			break
		}
		if attempt > store.MaxRouteDecisionAttempts {
			t.Fatalf("failed episode reclaimable beyond the cap (%d attempts)", attempt)
		}
	}

	// The leased steal: a stale "claimed" row is re-claimable — but the
	// steal is bounded by the SAME attempt cap, or a poison episode that
	// keeps killing its claimant re-arms forever (measured 9 steals
	// against a cap of 3 before the bound).
	if claimed, _, err := rds.ClaimRouteDecision(ctx, store.RouteDecision{RunID: runID, OutcomeSeq: 3, Decision: "merge"}, staleNever()); err != nil || !claimed {
		t.Fatalf("claim ep3 = (%t, %v)", claimed, err)
	}
	if claimed, _, err := rds.ClaimRouteDecision(ctx, store.RouteDecision{RunID: runID, OutcomeSeq: 3, Decision: "merge"}, staleNever()); err != nil || claimed {
		t.Fatalf("fresh claim stolen under the production threshold = (%t, %v)", claimed, err)
	}
	for steal := 2; steal <= store.MaxRouteDecisionAttempts; steal++ {
		claimed, _, err := rds.ClaimRouteDecision(ctx, store.RouteDecision{RunID: runID, OutcomeSeq: 3, Decision: "merge"}, staleAlways())
		if err != nil || !claimed {
			t.Fatalf("steal %d of stale claim = (%t, %v)", steal, claimed, err)
		}
	}
	claimed, existing, err = rds.ClaimRouteDecision(ctx, store.RouteDecision{RunID: runID, OutcomeSeq: 3, Decision: "merge"}, staleAlways())
	if err != nil || claimed {
		t.Fatalf("steal beyond the cap = (%t, %v), want refused", claimed, err)
	}
	if existing == nil || existing.Attempts < store.MaxRouteDecisionAttempts {
		t.Fatalf("cap-refused steal must return the exhausted row, got %+v", existing)
	}

	// The audit lists newest episode first.
	ds, err := rds.ListRouteDecisions(ctx, runID)
	if err != nil || len(ds) != 3 || ds[0].OutcomeSeq != 3 || ds[1].OutcomeSeq != 2 || ds[2].OutcomeSeq != 1 {
		t.Fatalf("ListRouteDecisions = %+v (%v)", ds, err)
	}

	// requires_action is the NON-retryable terminal (a merge that hit a
	// content conflict, a decision whose execution is not wired): the
	// retry is what makes things worse, so no threshold reclaims it —
	// not even one that reads every existing claim as stale.
	if claimed, _, err := rds.ClaimRouteDecision(ctx, store.RouteDecision{RunID: runID, OutcomeSeq: 4, Decision: "merge"}, staleNever()); err != nil || !claimed {
		t.Fatalf("claim ep4 = (%t, %v)", claimed, err)
	}
	if err := rds.FinishRouteDecision(ctx, runID, 4, store.RouteDecisionRequiresAction, "merge conflict"); err != nil {
		t.Fatalf("finish ep4 requires_action: %v", err)
	}
	if claimed, ex, err := rds.ClaimRouteDecision(ctx, store.RouteDecision{RunID: runID, OutcomeSeq: 4, Decision: "merge"}, staleAlways()); err != nil || claimed {
		t.Fatalf("requires_action reclaimed = (%t, %+v, %v), want refused", claimed, ex, err)
	}

	// The activation watermark: established first-writer-wins, then
	// stable across every later call (a restart must read the original
	// activation, not its own boot). Backend round-trips may truncate
	// sub-millisecond precision — equality within a second is the claim.
	wm1, err := rds.EnsureRouterWatermark(ctx)
	if err != nil || wm1.IsZero() {
		t.Fatalf("EnsureRouterWatermark = (%v, %v)", wm1, err)
	}
	wm2, err := rds.EnsureRouterWatermark(ctx)
	if err != nil {
		t.Fatalf("EnsureRouterWatermark (second): %v", err)
	}
	if d := wm2.Sub(wm1); d < -time.Second || d > time.Second {
		t.Fatalf("watermark moved between calls: %v vs %v", wm1, wm2)
	}

	// The sweep query: only policy-carrying terminal runs, oldest first.
	pol := &store.RoutingPolicy{Version: 1, SuccessWhen: "outputs.g.ok", AllowedActions: []string{"merge"}}
	pol.Hash = pol.ComputeHash()
	mk := func(id string, terminal bool, withPolicy bool) {
		t.Helper()
		if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
		r, _ := s.LoadRun(ctx, id)
		if withPolicy {
			r.RoutingPolicy = pol
		}
		if terminal {
			r.Status = store.RunStatusFinished
		}
		if err := s.SaveRun(ctx, r); err != nil {
			t.Fatalf("SaveRun %s: %v", id, err)
		}
	}
	mk("routable-a", true, true)
	mk("not-terminal", false, true)
	mk("no-policy", true, false)
	ids, err := rds.ListRoutableRuns(ctx, time.Now().Add(-time.Hour), 50)
	if err != nil {
		t.Fatalf("ListRoutableRuns: %v", err)
	}
	found := map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if !found["routable-a"] || found["not-terminal"] || found["no-policy"] {
		t.Fatalf("sweep query = %v, want exactly the policy-carrying terminal run", ids)
	}

	// The anti-join: a run whose CURRENT episode is settled (succeeded)
	// leaves the sweep list — decided terminals must not clog the batch
	// head — while a failed-under-cap episode stays routable (its
	// bounded retry still wants the re-offer).
	ra, err := s.LoadRun(ctx, "routable-a")
	if err != nil {
		t.Fatalf("LoadRun routable-a: %v", err)
	}
	if claimed, _, err := rds.ClaimRouteDecision(ctx, store.RouteDecision{RunID: ra.ID, OutcomeSeq: ra.OutcomeSeq, Decision: "escalate"}, staleNever()); err != nil || !claimed {
		t.Fatalf("claim routable-a = (%t, %v)", claimed, err)
	}
	if err := rds.FinishRouteDecision(ctx, ra.ID, ra.OutcomeSeq, store.RouteDecisionSucceeded, ""); err != nil {
		t.Fatalf("finish routable-a: %v", err)
	}
	mk("routable-b", true, true)
	rb, err := s.LoadRun(ctx, "routable-b")
	if err != nil {
		t.Fatalf("LoadRun routable-b: %v", err)
	}
	if claimed, _, err := rds.ClaimRouteDecision(ctx, store.RouteDecision{RunID: rb.ID, OutcomeSeq: rb.OutcomeSeq, Decision: "merge"}, staleNever()); err != nil || !claimed {
		t.Fatalf("claim routable-b = (%t, %v)", claimed, err)
	}
	if err := rds.FinishRouteDecision(ctx, rb.ID, rb.OutcomeSeq, store.RouteDecisionFailed, "transient"); err != nil {
		t.Fatalf("fail routable-b: %v", err)
	}
	ids, err = rds.ListRoutableRuns(ctx, time.Now().Add(-time.Hour), 50)
	if err != nil {
		t.Fatalf("ListRoutableRuns (anti-join): %v", err)
	}
	found = map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if found["routable-a"] {
		t.Fatalf("settled (succeeded) run still swept: %v", ids)
	}
	if !found["routable-b"] {
		t.Fatalf("failed-under-cap run dropped from the sweep: %v", ids)
	}

	// Oldest first is the contract, and the limit truncates AFTER the
	// sort: with limit 1 the oldest sleeping terminal must surface —
	// not the lexically-first or insertion-first one (a directory-order
	// truncation starves exactly the run the sweep net exists for).
	time.Sleep(5 * time.Millisecond)
	mk("aaa-routable-newer", true, true)
	ids, err = rds.ListRoutableRuns(ctx, time.Now().Add(-time.Hour), 1)
	if err != nil || len(ids) != 1 || ids[0] != "routable-b" {
		t.Fatalf("ListRoutableRuns(limit=1) = (%v, %v), want the oldest routable run [routable-b]", ids, err)
	}
}
