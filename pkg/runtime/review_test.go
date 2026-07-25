package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/store"
)

func minimalReviewWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "review_test",
		Entry: "gate",
		Nodes: map[string]ir.Node{
			"gate": &ir.HumanNode{
				BaseNode:          ir.BaseNode{ID: "gate"},
				InteractionFields: ir.InteractionFields{Interaction: ir.InteractionReview},
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "gate", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
}

func TestBuildCompanionMessageIncludesHumanFacingContractEveryTurn(t *testing.T) {
	eng := New(minimalReviewWorkflow(), tmpStore(t), newStubExecutor())
	rs := eng.newRunState("review-message-contract", nil)
	rs.ctx = context.Background()
	hn := &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate"}}

	messages := map[string]string{
		"first": eng.buildCompanionMessage(rs, hn, nil, nil),
		"follow-up": eng.buildCompanionMessage(rs, hn, []store.InteractionTurn{
			{Role: "companion", Content: "Please test it."},
			{Role: "human", Content: "It works."},
		}, nil),
	}
	for name, message := range messages {
		t.Run(name, func(t *testing.T) {
			for _, required := range []string{
				"The first sentence must tell the operator exactly what action to take now.",
				"under 120 words and 800 characters",
				"at most three short numbered or bulleted checks",
				"Do not mention implementation jargon",
				"file paths",
				"raw diff",
			} {
				if !strings.Contains(message, required) {
					t.Errorf("companion prompt is missing %q:\n%s", required, message)
				}
			}
			if strings.Contains(message, "write precise, numbered steps") {
				t.Errorf("companion prompt retained the unbounded technical-step instruction:\n%s", message)
			}
		})
	}
}

// setupReviewRun creates a temp repo + worktree (optionally with a commit)
// and a persisted store.Run wired for a worktree-backed review gate.
func setupReviewRun(t *testing.T, withCommit bool) (*Engine, store.RunStore, *runState, string, string, string) {
	t.Helper()
	repo, originalTip := initBareishRepo(t)
	s := tmpStore(t)
	authoritySince := time.Now().UTC()
	wt := addOwnedWorktree(t, s, repo, "run-rg")

	finalSHA := originalTip
	if withCommit {
		finalSHA = addCommit(t, wt, "feature.go", "package main\n", "feat: add feature")
	}

	ctx := context.Background()
	r, err := s.CreateRun(ctx, "run-rg", "review_test", nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	r.Worktree = true
	r.WorkDir = wt
	r.RepoRoot = repo
	r.BaseCommit = originalTip
	r.WorktreeCreatedAt = authoritySince
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("save run: %v", err)
	}

	eng := New(minimalReviewWorkflow(), s, newStubExecutor(),
		WithRunName("swift-cedar-a3f2"), WithWorkDir(repo))
	rs := eng.newRunState("run-rg", nil)
	rs.ctx = ctx
	return eng, s, rs, repo, finalSHA, originalTip
}

// TestReviewGate_PerformGateMerge_Squash — the merge-during-pause squashes
// the worktree's commits into the checked-out branch and records the merge
// on run.json, and the run-end finalize is idempotent (no duplicate branch).
func TestReviewGate_PerformGateMerge_Squash(t *testing.T) {
	assumeQuiescentProcessCensus(t)
	ctx := context.Background()
	eng, s, rs, repo, finalSHA, originalTip := setupReviewRun(t, true)
	hn := &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate"}, MergeStrategy: "squash", MergeInto: "current"}

	if err := eng.performGateMerge(ctx, rs, hn, "gate", nil); err != nil {
		t.Fatalf("performGateMerge: %v", err)
	}

	// main advanced from the base, and its tree matches the worktree's commit
	// (the change landed). We don't assert the squash SHA differs from finalSHA:
	// for a single commit with identical metadata + same-second timestamp the
	// squash commit can hash-equal the original — the merge still happened.
	mainTip := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", "main")))
	if mainTip == originalTip {
		t.Fatalf("main did not advance from base %s", originalTip)
	}
	mainTree := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", "main^{tree}")))
	wtTree := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", finalSHA+"^{tree}")))
	if mainTree != wtTree {
		t.Errorf("main tree %s != worktree tree %s (change did not land)", mainTree, wtTree)
	}

	r2, _ := s.LoadRun(ctx, "run-rg")
	if r2.MergeStatus != store.MergeStatusMerged {
		t.Errorf("MergeStatus = %q, want merged", r2.MergeStatus)
	}
	if r2.FinalBranch == "" {
		t.Error("FinalBranch not recorded")
	}
	if r2.FinalCommit != finalSHA {
		t.Errorf("FinalCommit = %q, want %q", r2.FinalCommit, finalSHA)
	}
	if r2.MergedInto != "main" {
		t.Errorf("MergedInto = %q, want main", r2.MergedInto)
	}

	// Idempotency: run-end finalize must skip (final_branch set) — no second
	// (suffixed) storage branch and no re-merge.
	before := strings.TrimSpace(string(mustOutput(t, repo, "git", "branch", "--list", "iterion/run/*")))
	mainBefore := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", "main")))
	if err := s.UpdateRunStatus(ctx, "run-rg", store.RunStatusFinished, ""); err != nil {
		t.Fatalf("mark review run finished: %v", err)
	}
	r2, _ = s.LoadRun(ctx, "run-rg")
	wtCtx := eng.reconstructWorktreeContext(r2)
	eng.finalizeOnExit(
		ctx,
		"run-rg",
		wtCtx,
		newWorktreeCleanup("run-rg", wtCtx.repoRoot, wtCtx.wtPath, r2.WorktreeCreatedAt),
		nil,
	)
	after := strings.TrimSpace(string(mustOutput(t, repo, "git", "branch", "--list", "iterion/run/*")))
	mainAfter := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", "main")))
	if before != after {
		t.Errorf("finalizeOnExit created a duplicate branch: before=%q after=%q", before, after)
	}
	if mainBefore != mainAfter {
		t.Errorf("finalizeOnExit re-merged: main moved %s → %s", mainBefore, mainAfter)
	}
	if _, err := os.Stat(r2.WorkDir); !os.IsNotExist(err) {
		t.Fatalf("review-gate worktree still exists after terminal cleanup: %v", err)
	}
}

func TestReviewGate_PerformGateMergeRefusesForeignRegisteredWorktree(t *testing.T) {
	ctx := context.Background()
	repo, originalTip := initBareishRepo(t)
	s := tmpStore(t)
	const (
		runID   = "run-rg-foreign-owner"
		otherID = "run-rg-foreign-other"
	)
	victimWT := addOwnedWorktree(t, s, repo, runID)
	otherWT := addOwnedWorktree(t, s, repo, otherID)
	otherSHA := addCommit(t, otherWT, "foreign-review.go", "package foreignreview\n", "feat: foreign review output")
	r, err := s.CreateRun(ctx, runID, "review_test", nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r.Worktree = true
	r.WorkDir = otherWT // corrupt: another registered run owns this path
	r.RepoRoot = repo
	r.BaseCommit = originalTip
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	eng := New(minimalReviewWorkflow(), s, newStubExecutor(), WithRunName("foreign-review"))
	rs := eng.newRunState(runID, nil)
	rs.ctx = ctx
	hn := &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate"}, MergeStrategy: "squash", MergeInto: "current"}

	err = eng.performGateMerge(ctx, rs, hn, "gate", nil)
	if err == nil || !strings.Contains(err.Error(), "does not own recovered worktree") {
		t.Fatalf("performGateMerge error=%v, want ownership refusal", err)
	}
	if got := readHEAD(otherWT); got != otherSHA {
		t.Fatalf("foreign review worktree HEAD changed: got %q, want %q", got, otherSHA)
	}
	for _, path := range []string{victimWT, otherWT} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("review ownership refusal removed %s: %v", path, statErr)
		}
	}
	if branches := strings.TrimSpace(string(mustOutput(t, repo, "git", "branch", "--list", "iterion/run/foreign-review*"))); branches != "" {
		t.Fatalf("review ownership refusal created branch(es): %q", branches)
	}
}

// TestReviewGate_PerformGateMerge_NoCommits — a gate over a worktree with no
// new commits records "skipped" and creates no branch.
func TestReviewGate_PerformGateMerge_NoCommits(t *testing.T) {
	ctx := context.Background()
	eng, s, rs, repo, _, _ := setupReviewRun(t, false)
	hn := &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate"}, MergeStrategy: "squash", MergeInto: "current"}

	if err := eng.performGateMerge(ctx, rs, hn, "gate", nil); err != nil {
		t.Fatalf("performGateMerge: %v", err)
	}
	r2, _ := s.LoadRun(ctx, "run-rg")
	if r2.MergeStatus != store.MergeStatusSkipped {
		t.Errorf("MergeStatus = %q, want skipped", r2.MergeStatus)
	}
	out := strings.TrimSpace(string(mustOutput(t, repo, "git", "branch", "--list", "iterion/run/*")))
	if out != "" {
		t.Errorf("no branch should be created for a no-commit gate, got %q", out)
	}
}

// TestReviewGate_PerformGateMerge_IntoNone — merge_into: none creates the
// storage branch but performs no merge (branch-only review).
func TestReviewGate_PerformGateMerge_IntoNone(t *testing.T) {
	ctx := context.Background()
	eng, s, rs, repo, finalSHA, _ := setupReviewRun(t, true)
	hn := &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate"}, MergeStrategy: "squash", MergeInto: "none"}

	if err := eng.performGateMerge(ctx, rs, hn, "gate", nil); err != nil {
		t.Fatalf("performGateMerge: %v", err)
	}
	mainTip := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", "main")))
	base := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", "HEAD")))
	if mainTip != base {
		t.Errorf("main should not move for merge_into: none")
	}
	r2, _ := s.LoadRun(ctx, "run-rg")
	if r2.MergeStatus != store.MergeStatusSkipped {
		t.Errorf("MergeStatus = %q, want skipped", r2.MergeStatus)
	}
	if r2.FinalBranch == "" || r2.FinalCommit != finalSHA {
		t.Errorf("branch-only gate should record FinalBranch + FinalCommit: %q / %q", r2.FinalBranch, r2.FinalCommit)
	}
}

// TestReviewGate_ResumeApproveMerge_FullCycle — the full resume dispatch:
// a run paused at a review gate, resumed with __review_action=approve_merge,
// squash-merges the worktree and finishes. Exercises Resume → resumeFromPause
// → resumeReviewGate → performGateMerge → gateSelectEdge → execLoop → done.
func TestReviewGate_ResumeApproveMerge_FullCycle(t *testing.T) {
	assumeQuiescentProcessCensus(t)
	ctx := context.Background()

	const src = `
schema v:
  decision: string

human gate:
  interaction: review
  model: "test-model"
  output: v

workflow wf:
  entry: gate
  worktree: auto
  gate -> done when "decision == 'approved'"
  gate -> fail
`
	cr := ir.Compile(parser.Parse("t.bot", src).File)
	if cr.Workflow == nil {
		t.Fatalf("compile failed: %+v", cr.Diagnostics)
	}

	repo, originalTip := initBareishRepo(t)
	s := tmpStore(t)
	wt := addOwnedWorktree(t, s, repo, "run-rg")
	addCommit(t, wt, "feature.go", "package main\n", "feat: add feature")

	r, err := s.CreateRun(ctx, "run-rg", "wf", nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	r.Worktree = true
	r.WorkDir = wt
	r.RepoRoot = repo
	r.BaseCommit = originalTip
	r.WorktreeCreatedAt = testWorktreeAuthority()
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("save run: %v", err)
	}
	// Park the run paused at the gate with a checkpoint, as the engine would.
	if err := s.PauseRun(ctx, "run-rg", &store.Checkpoint{
		NodeID:        "gate",
		InteractionID: "run-rg_gate",
		Outputs:       map[string]map[string]any{},
	}); err != nil {
		t.Fatalf("pause run: %v", err)
	}

	eng := New(cr.Workflow, s, newStubExecutor(),
		WithRunName("swift-cedar-a3f2"), WithWorkDir(repo))

	if err := eng.Resume(ctx, "run-rg", map[string]any{
		reviewActionKey: "approve_merge",
	}); err != nil {
		t.Fatalf("Resume(approve_merge): %v", err)
	}

	r2, _ := s.LoadRun(ctx, "run-rg")
	if r2.Status != store.RunStatusFinished {
		t.Errorf("status = %q, want finished", r2.Status)
	}
	if r2.MergeStatus != store.MergeStatusMerged {
		t.Errorf("MergeStatus = %q, want merged", r2.MergeStatus)
	}
	mainTip := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", "main")))
	if mainTip == originalTip {
		t.Errorf("main did not advance — merge did not land")
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("resumed review worktree still exists after finalization: %v", err)
	}
}

// TestReviewGate_ResumeRequestChanges_LoopsBack — request_changes records the
// verdict and routes the gate's changes_requested edge (no merge).
func TestReviewGate_ResumeRequestChanges(t *testing.T) {
	ctx := context.Background()
	const src = `
schema v:
  decision: string

agent impl:
  model: "test-model"
  output: v

human gate:
  interaction: review
  model: "test-model"
  output: v

workflow wf:
  entry: impl
  worktree: auto
  impl -> gate
  gate -> done when "decision == 'approved'"
  gate -> impl when "decision == 'changes_requested'" as fix_loop(3)
  gate -> fail
`
	cr := ir.Compile(parser.Parse("t.bot", src).File)
	if cr.Workflow == nil {
		t.Fatalf("compile failed: %+v", cr.Diagnostics)
	}

	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })

	s := tmpStore(t)
	r, _ := s.CreateRun(ctx, "run-rc", "wf", nil)
	r.Worktree = true
	r.WorkDir = wt
	r.RepoRoot = repo
	r.BaseCommit = originalTip
	_ = s.SaveRun(ctx, r)
	_ = s.PauseRun(ctx, "run-rc", &store.Checkpoint{
		NodeID:        "gate",
		InteractionID: "run-rc_gate",
		Outputs:       map[string]map[string]any{},
	})

	// The implementer stub re-pauses the dialogue indirectly; here we just
	// assert the gate routes back to impl (the run re-pauses at the gate on
	// the next loop or fails the loop — either way it must NOT merge).
	eng := New(cr.Workflow, s, newStubExecutor(), WithRunName("rc-run"), WithWorkDir(repo))
	_ = eng.Resume(ctx, "run-rc", map[string]any{reviewActionKey: "request_changes"})

	r2, _ := s.LoadRun(ctx, "run-rc")
	if r2.MergeStatus == store.MergeStatusMerged {
		t.Errorf("request_changes must not merge, got merge_status=merged")
	}
	mainTip := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", "main")))
	if mainTip != originalTip {
		t.Errorf("main moved on request_changes — should not merge")
	}
}

// TestReviewGate_MessageOverride — the studio form's squash-message override
// (in answers) is used as the squash commit message.
func TestReviewGate_PerformGateMerge_MessageOverride(t *testing.T) {
	ctx := context.Background()
	eng, _, rs, repo, _, _ := setupReviewRun(t, true)
	hn := &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate"}, MergeStrategy: "squash", MergeInto: "current"}
	answers := map[string]any{reviewMessageKey: "custom squash subject\n\nbody"}

	if err := eng.performGateMerge(ctx, rs, hn, "gate", answers); err != nil {
		t.Fatalf("performGateMerge: %v", err)
	}
	subject := strings.TrimSpace(string(mustOutput(t, repo, "git", "log", "-1", "--format=%s", "main")))
	if subject != "custom squash subject" {
		t.Errorf("squash subject = %q, want custom override", subject)
	}
}
