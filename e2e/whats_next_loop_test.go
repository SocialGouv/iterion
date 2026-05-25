// E2E smoke loop for the operator's whats-next → board → dispatcher →
// bot → memory cycle. No LLM calls; the runtime side is exercised via
// stubs (StubRunner + a static IR check on the emit_action prompt), the
// board + dispatcher are the real native store + actor.
//
// Regression guards bundled into the two tests:
//   - commit 89249f02 — emit_action's user prompt MUST reference
//     {{input.selected_titles}} so the LLM-side filter actually fires.
//   - commit 45eafe28 — dispatcher MUST auto-transition in_progress →
//     review on a clean run finish (otherwise the issue stays eligible
//     and gets re-dispatched on the next tick).
//   - commit 567ef0c3 — findings written under
//     ${PROJECT_MEMORY_DIR}/findings/ MUST survive across a run's
//     lifecycle (the inbox is the shared cross-bot memory channel).

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/memory"
)

// TestWhatsNext_EmitAction_UserPromptReferencesSelectedTitles guards
// commit 89249f02. The fix surfaced `{{input.selected_titles}}` in the
// emit_action_user prompt so the LLM applies the operator's per-item
// selection BEFORE materialising roadmap items as kanban issues.
// Without that reference the filter silently degrades to "create every
// item" — the failure mode that prompted the 7-vs-5 issue-count
// mismatch caught in the 2026-05-24 dogfood.
func TestWhatsNext_EmitAction_UserPromptReferencesSelectedTitles(t *testing.T) {
	wf := compileFixture(t, "whats-next/main.bot")
	p, ok := wf.Prompts["emit_action_user"]
	if !ok {
		t.Fatal("emit_action_user prompt missing from whats-next/main.bot")
	}
	if !strings.Contains(p.Body, "selected_titles") {
		t.Fatalf("emit_action_user prompt no longer references selected_titles — regression of 89249f02\nprompt body:\n%s", p.Body)
	}
}

// newSmokeDispatcherFixture wires a dispatcher + native store +
// StubRunner the same way newDispatcherFixture does, but routes the
// Config through ApplyDefaults() so CompletedState defaults to "review"
// (matches the production path post-45eafe28). Kept as a sibling helper
// rather than modifying newDispatcherFixture to avoid changing the
// semantics other dispatcher_test.go cases rely on.
func newSmokeDispatcherFixture(t *testing.T, polling time.Duration) (
	*dispatcher.Dispatcher,
	*native.Store,
	*dispatcher.StubRunner,
	func(),
) {
	t.Helper()
	dir := t.TempDir()

	ns, err := native.NewStore(dir + "/dispatcher")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ws, err := dispatcher.NewWorkspaces(dir + "/dispatcher/workspaces")
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}

	cfg := &dispatcher.Config{
		Name:      "e2e-smoke-loop",
		Workflow:  dir + "/dummy.iter",
		Tracker:   dispatcher.TrackerConfig{Kind: "native"},
		Polling:   dispatcher.PollingConfig{IntervalMS: int(polling.Milliseconds())},
		Agent:     dispatcher.AgentConfig{MaxConcurrent: 2, MaxRetryBackoffMS: 500},
		Workspace: dispatcher.WorkspaceConfig{Root: dir + "/dispatcher/workspaces"},
	}
	cfg.ApplyDefaults()

	logger := iterlog.New(iterlog.LevelError, &bytes.Buffer{})
	runner := &dispatcher.StubRunner{}
	c, err := dispatcher.New(dispatcher.Options{
		Config:     cfg,
		Tracker:    native.NewAdapter(ns),
		Runner:     runner,
		Workspaces: ws,
		Logger:     logger,
		StoreDir:   dir,
		HostMarker: "e2e-smoke",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)
	return c, ns, runner, func() { cancel(); c.Stop() }
}

// TestWhatsNext_Loop_DispatchAutoTransitionsNoReloop drives the
// production board → dispatcher loop end-to-end with stubs:
//
//  1. Pin ITERION_HOME to a tempdir and seed a finding under
//     ${PROJECT_MEMORY_DIR}/findings/. Asserts later that the file
//     survives the loop (guard 567ef0c3).
//  2. Boot a dispatcher with ApplyDefaults() so CompletedState=review
//     mirrors production.
//  3. Create two ready issues via boardops (matches the production
//     path emit_action takes — boardops.Call create_issue per surviving
//     roadmap item).
//  4. StubRunner clean-finishes each dispatch; the actor must
//     auto-transition the issue in_progress → review (guard 45eafe28).
//  5. Wait several polling intervals and assert the dispatch counter
//     stays at 2 — without 45eafe28 the issues would remain in
//     in_progress + eligible and the actor would re-dispatch them.
//
// Wall-clock budget: dispatch loop finishes in ~3× polling (claim →
// finish → transition), then 5× polling for the no-reloop watch.
// At 50ms polling that's ~400ms; the deadline is 3s for slow CI.
func TestWhatsNext_Loop_DispatchAutoTransitionsNoReloop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)

	workDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	findingsDir := filepath.Join(memory.WorkspaceMemoryDir(workDir), "findings")
	if err := os.MkdirAll(findingsDir, 0o755); err != nil {
		t.Fatalf("mkdir findings: %v", err)
	}
	findingPath := filepath.Join(findingsDir, "2026-05-25-smoke-loop-seed.md")
	const findingBody = `---
title: "smoke loop seed"
description: "seeded by e2e — must survive the dispatch loop"
kind: "improvement"
source_bot: "e2e-test"
tags: ["area:test"]
---

# body

Sentinel content for the findings-inbox guard.
`
	if err := os.WriteFile(findingPath, []byte(findingBody), 0o644); err != nil {
		t.Fatalf("write finding: %v", err)
	}

	const polling = 50 * time.Millisecond
	c, ns, runner, cleanup := newSmokeDispatcherFixture(t, polling)
	defer cleanup()

	var dispatchCount atomic.Int32
	runner.Handler = func(_ context.Context, _ dispatcher.DispatchSpec) error {
		dispatchCount.Add(1)
		return nil
	}

	caps := boardops.NewCapabilities("board.create,board.read,board.move,board.assign")
	mkIssue := func(title string) native.Issue {
		raw, err := boardops.Call(ns, caps, "create_issue", json.RawMessage(`{"title":"`+title+`","state":"ready","assignee":"feature_dev"}`))
		if err != nil {
			t.Fatalf("create_issue %q: %v", title, err)
		}
		var iss native.Issue
		if err := json.Unmarshal(raw, &iss); err != nil {
			t.Fatalf("unmarshal %q: %v", title, err)
		}
		return iss
	}
	issX := mkIssue("Refactor X")
	issY := mkIssue("Implement Y")

	// Wait for both issues to reach review state.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		xs, _ := ns.Get(issX.ID)
		ys, _ := ns.Get(issY.ID)
		if xs != nil && ys != nil && xs.State == native.StateReview && ys.State == native.StateReview {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	finalX, _ := ns.Get(issX.ID)
	finalY, _ := ns.Get(issY.ID)
	if finalX == nil || finalX.State != native.StateReview {
		t.Fatalf("issue %q never reached review — got state=%v (guard 45eafe28)", issX.Title, stateOf(finalX))
	}
	if finalY == nil || finalY.State != native.StateReview {
		t.Fatalf("issue %q never reached review — got state=%v (guard 45eafe28)", issY.Title, stateOf(finalY))
	}
	if got := dispatchCount.Load(); got != 2 {
		t.Fatalf("expected exactly 2 dispatches before transition, got %d", got)
	}

	// No-reloop guard: dispatcher must not re-dispatch issues sitting
	// in review. Wait several polling intervals; counter must stay at 2.
	time.Sleep(5 * polling)
	if got := dispatchCount.Load(); got != 2 {
		t.Fatalf("re-dispatch detected after review transition: dispatchCount=%d (expected 2) — regression of 45eafe28", got)
	}
	if running := len(c.Snapshot().Running); running != 0 {
		t.Fatalf("running set not drained after clean finish: %d still running", running)
	}

	// Findings inbox guard (567ef0c3): the seeded file must still be
	// readable AND content-intact after the loop ran to completion.
	body, err := os.ReadFile(findingPath)
	if err != nil {
		t.Fatalf("seeded finding missing after loop: %v", err)
	}
	if !strings.Contains(string(body), "smoke loop seed") {
		t.Fatalf("seeded finding content corrupted: %q", string(body))
	}
}

func stateOf(iss *native.Issue) string {
	if iss == nil {
		return "<nil>"
	}
	return iss.State
}
