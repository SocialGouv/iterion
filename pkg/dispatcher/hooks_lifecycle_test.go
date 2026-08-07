package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// These tests prove that runWorker actually FIRES the three lifecycle
// hooks — after_create, before_run, after_run — at their point in the
// cycle. Distinct from:
//   - hooks_test.go, which tests the mechanics of Hook.Run in isolation
//     (does the shell script get executed if you call .Run?); and
//   - TestCleanupWorkspace_RunsBeforeRemoveBeforeDeletingDir in
//     cleanup_workspace_test.go, which covers the FOURTH hook,
//     before_remove, at its point in the cleanup cycle.
//
// The audit that motivated this file: deleting the three
// `hooks.AfterCreate.Run(...)`, `hooks.BeforeRun.Run(...)`, and
// `hooks.AfterRun.Run(...)` calls at pkg/dispatcher/loop.go:1015-1029
// left the ENTIRE test suite green — nothing observed that the calls
// happened. The tests below anchor those firings on a single observable:
// a shared order log every phase (hook + runner) appends to. Under an
// ordinary sequential invocation of runWorker the log records the exact
// lifecycle sequence — no timestamps, no sleeps, no polling.
//
// Mutation guide:
//   - Remove any of the three `Run(...)` calls → the corresponding phase
//     name disappears from the log → the strict-equality assertion in
//     TestRunWorker_FiresLifecycleHooksInOrder fails.
//   - Remove the `if err := ...; { c.postFinished(...); return }` early
//     return after after_create or before_run → the aborted-abort tests
//     see a downstream phase in the log or the runner-append that must
//     not run.
//   - Skip after_run on the failure branch (e.g. gate it on
//     `dispatchErr == nil`) → TestRunWorker_AfterRunFiresEvenWhenRunnerFails
//     sees after_run missing.
//   - Drop the `created &&` guard on after_create → the resumed-run test
//     sees after_create firing when it must not.

// newHookLifecycleDispatcher builds a minimal, un-started dispatcher with
// the given hooks and runner. The actor loop is NOT running, so tests
// drive runWorker directly on the calling goroutine; postFinished lands
// on the buffered c.cmds channel and can be popped for assertions.
func newHookLifecycleDispatcher(t *testing.T, wsRoot string, hooks Hooks, runner Runner) (*Dispatcher, *Workspaces) {
	t.Helper()
	cfg := &Config{
		Name:      "test",
		Workflow:  t.TempDir() + "/fake.bot",
		Tracker:   TrackerConfig{Kind: "fake"},
		Polling:   PollingConfig{IntervalMS: 50},
		Agent:     AgentConfig{MaxConcurrent: 4, MaxRetryBackoffMS: 1000, RunningState: "in_progress"},
		Workspace: WorkspaceConfig{Root: wsRoot, Persist: WorkspacePersistCleanupOnDone},
		Stall:     StallConfig{TimeoutMS: 0},
		Hooks:     hooks,
	}
	cfg.applyDefaults()
	ws, err := NewWorkspaces(wsRoot)
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	c, err := New(Options{
		Config:     cfg,
		Tracker:    newFakeTracker(),
		Runner:     runner,
		Workspaces: ws,
		Logger:     iterlog.New(iterlog.LevelError, &bytes.Buffer{}),
		HostMarker: "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, ws
}

// popRunFinished pops the cmdRunFinished the runWorker posts on exit.
// The actor is not running in these tests, so the command sits in c.cmds;
// draining it here lets the assertion read the error runWorker reported.
func popRunFinished(t *testing.T, c *Dispatcher) cmdRunFinished {
	t.Helper()
	select {
	case m := <-c.cmds:
		got, ok := m.(cmdRunFinished)
		if !ok {
			t.Fatalf("wanted cmdRunFinished on c.cmds, got %T", m)
		}
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("runWorker never posted cmdRunFinished — the finish signal is the actor's release trigger, missing it wedges dispatch forever")
		return cmdRunFinished{}
	}
}

// runnerAppending returns a StubRunner that appends "runner" to path when
// invoked, then returns dispatchErr. The append proves the runner ran.
func runnerAppending(t *testing.T, path string, dispatchErr error) *StubRunner {
	t.Helper()
	return &StubRunner{Handler: func(context.Context, DispatchSpec) error {
		if err := appendLifecycleLine(path, "runner"); err != nil {
			t.Errorf("runner append: %v", err)
		}
		return dispatchErr
	}}
}

// appendLifecycleLine appends one line to path, creating it if missing.
// Used by both the runner (Go) and hook scripts (sh printf >>), which
// interleave through the same file in the order runWorker calls them.
func appendLifecycleLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString(line + "\n")
	return err
}

// readLifecycleLog returns the non-empty lines of path. A missing file is
// an empty slice — the file only exists once at least one phase ran.
func readLifecycleLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s: %v", path, err)
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// scriptAppend returns a Hook whose script appends `name` to path, exit 0.
// printf is POSIX-strict; echo behaves differently across sh variants.
func scriptAppend(name, path string) *Hook {
	return &Hook{Script: fmt.Sprintf(`printf '%s\n' >> %q`, name, path)}
}

// scriptAppendThenFail returns a Hook whose script appends `name` to
// path, then exits non-zero. Hook.Run returns an error — the exact
// failing-hook shape runWorker must react to (fail-fast on
// after_create / before_run, best-effort log on after_run).
func scriptAppendThenFail(name, path string) *Hook {
	return &Hook{Script: fmt.Sprintf(`printf '%s\n' >> %q; exit 3`, name, path)}
}

// equalLifecycleOrder compares two string slices for exact equality
// (order matters, length matters). Kept local to avoid the slices import
// churn and to give the test a name that reads at the callsite.
func equalLifecycleOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// hookLifecycleEntry builds a runningEntry consistent with what
// launchDispatchSetup would hand to runWorker: cleanup policy on, the
// workspace generation set to the run id, and the workspace path
// already materialised by CreateForRun.
func hookLifecycleEntry(issueID, runID, wsPath string) *runningEntry {
	return &runningEntry{
		IssueID:                   issueID,
		Identifier:                "fake#" + issueID,
		RunID:                     runID,
		WorkspaceGeneration:       runID,
		WorkflowState:             "in_progress",
		CleanupWorkspaceOnSuccess: true,
		WorkspacePath:             wsPath,
	}
}

// TestRunWorker_FiresLifecycleHooksInOrder is the core regression guard.
// A clean run must fire after_create → before_run → runner → after_run →
// before_remove, in that exact order. before_remove closes the sequence
// because cleanupWorkspace runs iff dispatchErr == nil.
//
// Mutation: remove any of `hooks.AfterCreate.Run(...)`,
// `hooks.BeforeRun.Run(...)`, or `hooks.AfterRun.Run(...)` from
// runWorker (pkg/dispatcher/loop.go) — the corresponding phase name
// vanishes from the log and this assertion fails. The isolated
// Hook.Run tests keep passing (they don't observe the calls).
func TestRunWorker_FiresLifecycleHooksInOrder(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	orderLog := filepath.Join(dir, "order.log")

	hooks := Hooks{
		AfterCreate:  scriptAppend("after_create", orderLog),
		BeforeRun:    scriptAppend("before_run", orderLog),
		AfterRun:     scriptAppend("after_run", orderLog),
		BeforeRemove: scriptAppend("before_remove", orderLog),
	}
	runner := runnerAppending(t, orderLog, nil)

	c, ws := newHookLifecycleDispatcher(t, wsRoot, hooks, runner)

	const issueID, runID = "fake:hooks-order", "run-hooks-order"
	wsPath, _, err := ws.CreateForRun(issueID, runID)
	if err != nil {
		t.Fatalf("ws.CreateForRun: %v", err)
	}
	entry := hookLifecycleEntry(issueID, runID, wsPath)
	spec := DispatchSpec{RunID: runID, WorkspacePath: wsPath}

	c.runWorker(context.Background(), entry, true /*created*/, spec)

	got := readLifecycleLog(t, orderLog)
	want := []string{"after_create", "before_run", "runner", "after_run", "before_remove"}
	if !equalLifecycleOrder(got, want) {
		t.Fatalf("lifecycle order = %v, want %v", got, want)
	}

	// before_remove ran inside cleanupWorkspace, which then removed the
	// workspace directory. Anchoring this here keeps the "before_remove
	// fires WHILE the workspace exists" contract wired to the runWorker
	// path (cleanup_workspace_test.go covers it on the direct call).
	if _, statErr := os.Stat(wsPath); !os.IsNotExist(statErr) {
		t.Fatalf("workspace %q not removed after a fully successful run (stat err=%v)", wsPath, statErr)
	}

	cmd := popRunFinished(t, c)
	if cmd.issueID != issueID {
		t.Errorf("cmdRunFinished.issueID = %q, want %q", cmd.issueID, issueID)
	}
	if cmd.err != nil {
		t.Errorf("cmdRunFinished.err = %v, want nil on a clean run", cmd.err)
	}
}

// TestRunWorker_AfterCreateFailureAbortsBeforeRunner locks in the
// fail-fast contract on after_create: a non-zero exit MUST prevent
// before_run, the runner, after_run, and cleanup from running. The
// workspace must survive (dispatchErr != nil skips cleanup).
//
// Mutation: delete the `if err := hooks.AfterCreate.Run(...); return`
// early return — before_run and the runner run when they must not, and
// the strict-equality order check fails.
func TestRunWorker_AfterCreateFailureAbortsBeforeRunner(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	orderLog := filepath.Join(dir, "order.log")

	hooks := Hooks{
		AfterCreate:  scriptAppendThenFail("after_create", orderLog),
		BeforeRun:    scriptAppend("before_run", orderLog),
		AfterRun:     scriptAppend("after_run", orderLog),
		BeforeRemove: scriptAppend("before_remove", orderLog),
	}
	runner := runnerAppending(t, orderLog, nil)

	c, ws := newHookLifecycleDispatcher(t, wsRoot, hooks, runner)

	const issueID, runID = "fake:hooks-ac-fail", "run-hooks-ac-fail"
	wsPath, _, err := ws.CreateForRun(issueID, runID)
	if err != nil {
		t.Fatalf("ws.CreateForRun: %v", err)
	}
	entry := hookLifecycleEntry(issueID, runID, wsPath)
	spec := DispatchSpec{RunID: runID, WorkspacePath: wsPath}

	c.runWorker(context.Background(), entry, true, spec)

	got := readLifecycleLog(t, orderLog)
	// after_create appended its name BEFORE exiting 3, so it is present.
	// Nothing downstream is allowed to have run.
	want := []string{"after_create"}
	if !equalLifecycleOrder(got, want) {
		t.Fatalf("after_create failure did not abort: order = %v, want %v", got, want)
	}
	if _, statErr := os.Stat(wsPath); statErr != nil {
		t.Fatalf("workspace destroyed after an aborted run: stat err=%v (cleanupWorkspace must be skipped when dispatchErr != nil)", statErr)
	}

	cmd := popRunFinished(t, c)
	if cmd.err == nil || !strings.Contains(cmd.err.Error(), "after_create") {
		t.Fatalf("cmdRunFinished.err = %v, want an error naming after_create", cmd.err)
	}
}

// TestRunWorker_BeforeRunFailureAbortsBeforeRunner: after_create OK,
// before_run fails; the runner MUST NOT run and after_run MUST NOT run.
//
// Mutation: delete the `if err := hooks.BeforeRun.Run(...); return`
// early return — the runner appends to the log when it must not.
func TestRunWorker_BeforeRunFailureAbortsBeforeRunner(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	orderLog := filepath.Join(dir, "order.log")

	hooks := Hooks{
		AfterCreate:  scriptAppend("after_create", orderLog),
		BeforeRun:    scriptAppendThenFail("before_run", orderLog),
		AfterRun:     scriptAppend("after_run", orderLog),
		BeforeRemove: scriptAppend("before_remove", orderLog),
	}
	runner := runnerAppending(t, orderLog, nil)

	c, ws := newHookLifecycleDispatcher(t, wsRoot, hooks, runner)

	const issueID, runID = "fake:hooks-br-fail", "run-hooks-br-fail"
	wsPath, _, err := ws.CreateForRun(issueID, runID)
	if err != nil {
		t.Fatalf("ws.CreateForRun: %v", err)
	}
	entry := hookLifecycleEntry(issueID, runID, wsPath)
	spec := DispatchSpec{RunID: runID, WorkspacePath: wsPath}

	c.runWorker(context.Background(), entry, true, spec)

	got := readLifecycleLog(t, orderLog)
	want := []string{"after_create", "before_run"} // stops before runner
	if !equalLifecycleOrder(got, want) {
		t.Fatalf("before_run failure did not abort: order = %v, want %v", got, want)
	}
	if _, statErr := os.Stat(wsPath); statErr != nil {
		t.Fatalf("workspace destroyed after an aborted run: stat err=%v", statErr)
	}

	cmd := popRunFinished(t, c)
	if cmd.err == nil || !strings.Contains(cmd.err.Error(), "before_run") {
		t.Fatalf("cmdRunFinished.err = %v, want an error naming before_run", cmd.err)
	}
}

// TestRunWorker_AfterRunFiresEvenWhenRunnerFails locks in the
// best-effort contract on after_run: a runner failure MUST NOT skip it.
// after_run is the operator's post-hoc audit surface — skipping it on the
// failure branch would silently drop the signal exactly when it matters.
// cleanupWorkspace still stays skipped when dispatchErr != nil, so the
// workspace and its ownership marker survive for inspection.
//
// Mutation: gate `hooks.AfterRun.Run(...)` behind `if dispatchErr == nil`
// — after_run disappears from the log when the runner fails.
func TestRunWorker_AfterRunFiresEvenWhenRunnerFails(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	orderLog := filepath.Join(dir, "order.log")

	hooks := Hooks{
		AfterCreate:  scriptAppend("after_create", orderLog),
		BeforeRun:    scriptAppend("before_run", orderLog),
		AfterRun:     scriptAppend("after_run", orderLog),
		BeforeRemove: scriptAppend("before_remove", orderLog),
	}
	runner := runnerAppending(t, orderLog, errors.New("dispatch boom"))

	c, ws := newHookLifecycleDispatcher(t, wsRoot, hooks, runner)

	const issueID, runID = "fake:hooks-ar-boom", "run-hooks-ar-boom"
	wsPath, _, err := ws.CreateForRun(issueID, runID)
	if err != nil {
		t.Fatalf("ws.CreateForRun: %v", err)
	}
	entry := hookLifecycleEntry(issueID, runID, wsPath)
	spec := DispatchSpec{RunID: runID, WorkspacePath: wsPath}

	c.runWorker(context.Background(), entry, true, spec)

	got := readLifecycleLog(t, orderLog)
	// No before_remove: cleanupWorkspace is gated on dispatchErr == nil.
	want := []string{"after_create", "before_run", "runner", "after_run"}
	if !equalLifecycleOrder(got, want) {
		t.Fatalf("after_run must fire even after a runner failure: order = %v, want %v", got, want)
	}
	if _, statErr := os.Stat(wsPath); statErr != nil {
		t.Fatalf("workspace destroyed after a runner failure: stat err=%v (cleanupWorkspace must be skipped when dispatchErr != nil)", statErr)
	}

	cmd := popRunFinished(t, c)
	if cmd.err == nil || !strings.Contains(cmd.err.Error(), "dispatch boom") {
		t.Fatalf("cmdRunFinished.err = %v, want the dispatch error verbatim", cmd.err)
	}
}

// TestRunWorker_SkipsAfterCreateOnResumedRun locks in the created=false
// branch: a run that adopts an already-existing workspace (the resume
// / retry path) MUST NOT re-fire after_create — the caller has captured
// created once at CreateForRun time and passes it in. before_run,
// runner, after_run, and before_remove still all fire.
//
// Mutation: drop the `created && ...` guard on after_create — the hook
// re-fires on every resume and this test sees after_create in the log.
func TestRunWorker_SkipsAfterCreateOnResumedRun(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	orderLog := filepath.Join(dir, "order.log")

	hooks := Hooks{
		AfterCreate:  scriptAppend("after_create", orderLog),
		BeforeRun:    scriptAppend("before_run", orderLog),
		AfterRun:     scriptAppend("after_run", orderLog),
		BeforeRemove: scriptAppend("before_remove", orderLog),
	}
	runner := runnerAppending(t, orderLog, nil)

	c, ws := newHookLifecycleDispatcher(t, wsRoot, hooks, runner)

	const issueID, runID = "fake:hooks-resume", "run-hooks-resume"
	wsPath, _, err := ws.CreateForRun(issueID, runID)
	if err != nil {
		t.Fatalf("ws.CreateForRun: %v", err)
	}
	entry := hookLifecycleEntry(issueID, runID, wsPath)
	spec := DispatchSpec{RunID: runID, WorkspacePath: wsPath}

	// created=false — the dispatch is reusing an existing workspace.
	c.runWorker(context.Background(), entry, false, spec)

	got := readLifecycleLog(t, orderLog)
	want := []string{"before_run", "runner", "after_run", "before_remove"}
	if !equalLifecycleOrder(got, want) {
		t.Fatalf("resumed run must not re-fire after_create: order = %v, want %v", got, want)
	}
	_ = popRunFinished(t, c)
}
