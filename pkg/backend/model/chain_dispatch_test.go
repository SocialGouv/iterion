package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// backendScriptedBackend records the tasks it was handed, keyed by the
// backend it was registered under. Unlike providerScriptedBackend (which
// keys on the provider hint alone) it can prove that a CROSS-BACKEND
// chain hands each element a task shaped for its own backend.
type backendScriptedBackend struct {
	name  string
	fail  error
	tasks []delegate.Task
	// result is returned on success; Output is copied per call so a test
	// can mutate it without cross-talk.
	tokens  int
	costUSD float64
}

func (b *backendScriptedBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.tasks = append(b.tasks, task)
	res := delegate.Result{
		BackendName: b.name,
		Tokens:      b.tokens,
		Output:      map[string]any{"served_by": b.name},
	}
	if b.costUSD > 0 {
		res.Output["_cost_usd"] = b.costUSD
	}
	res.Duration = time.Millisecond
	if b.fail != nil {
		// A failing element still reports what it burned — that is
		// exactly the spend a chain must not lose.
		return res, b.fail
	}
	return res, nil
}

// crossBackendChain builds an executor with two registered backends and
// a chain that falls through from the first to the second.
func crossBackendChain(t *testing.T, headErr error) (*ClawExecutor, *backendScriptedBackend, *backendScriptedBackend, []chainElement) {
	t.Helper()
	head := &backendScriptedBackend{name: delegate.BackendClaudeCode, fail: headErr, tokens: 1000, costUSD: 0.40}
	tail := &backendScriptedBackend{name: delegate.BackendClaw, tokens: 30, costUSD: 0.01}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, head)
	reg.Register(delegate.BackendClaw, tail)
	e := newFallbackExecutor(reg, EventHooks{})
	chain := []chainElement{
		{Label: "primary"},
		{Label: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5.5"},
	}
	return e, head, tail, chain
}

// TestChainRebuildsTaskPerBackend is the correctness core of ADR-087.
// Reusing one task across a backend boundary is not a fidelity loss but a
// silent capability wipe: ToolDefs is populated only for claw and
// AllowedTools only reaches CLI backends, so a claude_code-shaped task
// executed on claw yields an agent with ZERO tools that still carries an
// output schema — a schema-valid verdict it never verified.
func TestChainRebuildsTaskPerBackend(t *testing.T) {
	e, head, tail, chain := crossBackendChain(t, &delegate.ErrTransient{Reason: "boom"})

	assembled := map[string]int{}
	build := e.newElementBuilder("review", delegate.BackendClaudeCode, nil,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			assembled[bn]++
			task := &delegate.Task{
				NodeID:           "review",
				Model:            "claude-opus-5",
				SystemPromptMode: delegate.SystemPromptModeForBackend(bn),
			}
			if bn == delegate.BackendClaw {
				task.ToolDefs = []delegate.ToolDef{{Name: "bash"}}
			} else {
				task.AllowedTools = []string{"Bash"}
			}
			return task, nil
		})

	out, err := e.dispatchChain(context.Background(), "review", chain, "claude-opus-5", build)
	if err != nil {
		t.Fatalf("chain should have succeeded on the claw element: %v", err)
	}
	if out.BackendName != delegate.BackendClaw {
		t.Fatalf("served by %q, want claw", out.BackendName)
	}
	if assembled[delegate.BackendClaudeCode] != 1 || assembled[delegate.BackendClaw] != 1 {
		t.Errorf("assembled = %v, want exactly one task per backend", assembled)
	}
	if len(head.tasks) == 0 || len(tail.tasks) != 1 {
		t.Fatalf("head got %d calls, claw got %d", len(head.tasks), len(tail.tasks))
	}
	clawTask := tail.tasks[0]
	if len(clawTask.ToolDefs) == 0 {
		t.Error("the claw element ran with no ToolDefs — a task built for claude_code was reused, which is the tool-less-agent failure ADR-087 exists to prevent")
	}
	if clawTask.SystemPromptMode != delegate.SystemPromptModeForBackend(delegate.BackendClaw) {
		t.Errorf("claw element ran with SystemPromptMode %v, want claw's — an appended-to-native prompt on claw means NO operating posture at all", clawTask.SystemPromptMode)
	}
	if clawTask.Model != "openai/gpt-5.5" {
		t.Errorf("claw element model = %q, want the element's own pin", clawTask.Model)
	}
}

// TestChainReusesTaskWithinOneBackend proves the rebuild is narrow: a
// legacy `provider:` chain (every element on the node's backend) must
// still assemble exactly once, so adding the chain machinery costs
// existing bots nothing.
func TestChainReusesTaskWithinOneBackend(t *testing.T) {
	head := &backendScriptedBackend{name: delegate.BackendClaudeCode, fail: &delegate.ErrTransient{Reason: "boom"}}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, head)
	e := newFallbackExecutor(reg, EventHooks{})

	assembled := 0
	build := e.newElementBuilder("review", delegate.BackendClaudeCode, head,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			assembled++
			return &delegate.Task{NodeID: "review", Model: "claude-opus-5"}, nil
		})
	_, _ = e.dispatchChain(context.Background(), "review",
		[]chainElement{{Provider: "zai"}, {Provider: "anthropic"}}, "claude-opus-5", build)

	if assembled != 1 {
		t.Errorf("assembled %d times for a hint-only chain, want 1 — rebuilding when the backend cannot change is pure waste", assembled)
	}
}

// TestChainAccumulatesFailedElementSpend: a discarded element's tokens
// and cost are real money. With a same-model credential swap the loss
// was bounded; with a cross-backend element it can be a whole agentic
// session, invisible to max_cost_usd, the org monthly cap and a lending
// donor's ledger.
func TestChainAccumulatesFailedElementSpend(t *testing.T) {
	e, _, _, chain := crossBackendChain(t, &delegate.ErrTransient{Reason: "boom"})
	build := e.newElementBuilder("review", delegate.BackendClaudeCode, nil,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "review", Model: "claude-opus-5"}, nil
		})

	out, err := e.dispatchChain(context.Background(), "review", chain, "claude-opus-5", build)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// head burned 1000 tokens across its retry budget (2 attempts), tail 30.
	if out.Result.Tokens <= 30 {
		t.Errorf("tokens = %d, want the failed element's spend folded in (>30)", out.Result.Tokens)
	}
	got, _ := out.Result.Output["_cost_usd"].(float64)
	if got <= 0.01 {
		t.Errorf("_cost_usd = %v, want the failed element's cost folded in (>0.01)", got)
	}
}

// TestErrChainExhaustedPreservesEveryCause is the decision that keeps the
// surrounding machinery alive. The run-level usage-window retry and the
// credential-pool donor cooldown both errors.As on a node's terminal
// error; a chain that surfaced only its LAST failure would silently
// disarm both whenever the forfait wall was followed by anything else.
func TestErrChainExhaustedPreservesEveryCause(t *testing.T) {
	head := &backendScriptedBackend{name: delegate.BackendClaudeCode, fail: usageWindowErr()}
	tail := &backendScriptedBackend{
		name: delegate.BackendClaw,
		fail: &delegate.ErrAuthFailed{Provider: "claw", Detail: "no OPENAI_API_KEY"},
	}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, head)
	reg.Register(delegate.BackendClaw, tail)
	e := newFallbackExecutor(reg, EventHooks{})

	build := e.newElementBuilder("review", delegate.BackendClaudeCode, nil,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "review"}, nil
		})
	_, err := e.dispatchChain(context.Background(), "review",
		[]chainElement{{Label: "primary"}, {Label: "api", Backend: delegate.BackendClaw}},
		"claude-opus-5", build)
	if err == nil {
		t.Fatal("expected the exhausted chain to fail")
	}

	var rl *delegate.ErrRateLimited
	if !errors.As(err, &rl) || rl.Kind != delegate.RateLimitKindUsageWindow {
		t.Errorf("usage-window cause lost behind the last element's error: %v", err)
	}
	var auth *delegate.ErrAuthFailed
	if !errors.As(err, &auth) {
		t.Errorf("last element's auth cause not reachable: %v", err)
	}
	var exhausted *ErrChainExhausted
	if !errors.As(err, &exhausted) {
		t.Fatalf("not an *ErrChainExhausted: %v", err)
	}
	if len(exhausted.Errs) != 2 {
		t.Errorf("carried %d causes, want one per element", len(exhausted.Errs))
	}
}

// TestChainStopsWhenNextElementRejectsCategory: `on:` is a closed
// positive list. A budget cap or a schema-shape failure re-fails
// identically on every element, so an element that does not accept the
// condition must end the walk rather than burn another route.
func TestChainStopsWhenNextElementRejectsCategory(t *testing.T) {
	head := &backendScriptedBackend{
		name: delegate.BackendClaudeCode,
		fail: &delegate.ErrAuthFailed{Provider: "claude_code", Detail: "invalid bearer token"},
	}
	tail := &backendScriptedBackend{name: delegate.BackendClaw}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, head)
	reg.Register(delegate.BackendClaw, tail)
	e := newFallbackExecutor(reg, EventHooks{})

	build := e.newElementBuilder("review", delegate.BackendClaudeCode, nil,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "review"}, nil
		})
	_, err := e.dispatchChain(context.Background(), "review", []chainElement{
		{Label: "primary"},
		{Label: "api", Backend: delegate.BackendClaw, On: []delegate.FallbackCategory{delegate.FallbackUsageWindow}},
	}, "claude-opus-5", build)

	if err == nil {
		t.Fatal("expected the chain to stop on a condition its next element does not accept")
	}
	if len(tail.tasks) != 0 {
		t.Errorf("the claw element ran %d times; it only accepts usage_window and the failure was auth", len(tail.tasks))
	}
}

// TestChainAdvancesOnUnclassifiedDespiteFilter: refusing an
// unclassifiable failure would strand a run on exactly the errors
// iterion failed to describe — sandboxed claw flattens every error to a
// string at the IPC boundary, and kimi/grok have no error channel at
// all. A missing classifier must degrade to a fall-through, never to a
// dead end.
func TestChainAdvancesOnUnclassifiedDespiteFilter(t *testing.T) {
	head := &backendScriptedBackend{
		name: delegate.BackendClaudeCode,
		fail: errors.New("claw backend: runner: something opaque"),
	}
	tail := &backendScriptedBackend{name: delegate.BackendClaw}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, head)
	reg.Register(delegate.BackendClaw, tail)
	e := newFallbackExecutor(reg, EventHooks{})

	build := e.newElementBuilder("review", delegate.BackendClaudeCode, nil,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "review"}, nil
		})
	if _, err := e.dispatchChain(context.Background(), "review", []chainElement{
		{Label: "primary"},
		{Label: "api", Backend: delegate.BackendClaw, On: []delegate.FallbackCategory{delegate.FallbackUsageWindow}},
	}, "claude-opus-5", build); err != nil {
		t.Fatalf("unclassified failure should still route: %v", err)
	}
	if len(tail.tasks) != 1 {
		t.Errorf("the claw element ran %d times, want 1", len(tail.tasks))
	}
}

// TestChainEvictsNodeSessionOnFallThrough: the in-process claw session
// store is keyed (runID, nodeID) with NO provider fingerprint and
// captures a FAILED attempt's messages, so carrying it across a
// fall-through replays one provider's signed thinking blocks into
// another — a 400 at best, a mangled conversation at worst.
func TestChainEvictsNodeSessionOnFallThrough(t *testing.T) {
	const runID, nodeID = "run-1", "review"
	sessions := newNodeSessionStore()
	sessions.SaveSnapshot(runID, nodeID, []byte(`[{"role":"assistant"}]`))
	if sessions.LoadSnapshot(runID, nodeID) == nil {
		t.Fatal("precondition: snapshot not stored")
	}
	ctx := withRuntimeContext(context.Background(), runID, sessions)

	e, _, _, chain := crossBackendChain(t, &delegate.ErrTransient{Reason: "boom"})
	build := e.newElementBuilder(nodeID, delegate.BackendClaudeCode, nil,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: nodeID}, nil
		})
	if _, err := e.dispatchChain(ctx, nodeID, chain, "claude-opus-5", build); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap := sessions.LoadSnapshot(runID, nodeID); snap != nil {
		t.Errorf("the failed element's conversation survived the fall-through: %s", snap)
	}
}

// TestChainDropsResumeContinuityOnFallThrough: the resume continuity
// applied at build time belongs to the element that paused. Replaying it
// into another backend re-sends one provider's turn to a provider that
// never issued it.
func TestChainDropsResumeContinuityOnFallThrough(t *testing.T) {
	e, _, tail, chain := crossBackendChain(t, &delegate.ErrTransient{Reason: "boom"})
	build := e.newElementBuilder("review", delegate.BackendClaudeCode, nil,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			return &delegate.Task{
				NodeID:                 "review",
				ResumeConversation:     []byte(`[{"role":"assistant"}]`),
				ResumePendingToolUseID: "toolu_abc",
				ResumeAnswer:           "yes",
			}, nil
		})
	if _, err := e.dispatchChain(context.Background(), "review", chain, "claude-opus-5", build); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tail.tasks) != 1 {
		t.Fatalf("claw element ran %d times, want 1", len(tail.tasks))
	}
	got := tail.tasks[0]
	if len(got.ResumeConversation) != 0 || got.ResumePendingToolUseID != "" || got.ResumeAnswer != "" {
		t.Errorf("fall-through inherited resume continuity: conv=%s pending=%q answer=%q",
			got.ResumeConversation, got.ResumePendingToolUseID, got.ResumeAnswer)
	}
}

// TestBuildFailureDoesNotVetoLaterElements: an unresolvable backend or
// an uncredentialed element is that element's failure, not the node's.
func TestBuildFailureDoesNotVetoLaterElements(t *testing.T) {
	tail := &backendScriptedBackend{name: delegate.BackendClaw}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaw, tail)
	e := newFallbackExecutor(reg, EventHooks{})

	build := e.newElementBuilder("review", "nonexistent_backend", nil,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "review"}, nil
		})
	out, err := e.dispatchChain(context.Background(), "review",
		[]chainElement{{Label: "primary"}, {Label: "api", Backend: delegate.BackendClaw}},
		"", build)
	if err != nil {
		t.Fatalf("a head element that cannot even be built must not veto the rest: %v", err)
	}
	if out.BackendName != delegate.BackendClaw {
		t.Errorf("served by %q, want claw", out.BackendName)
	}
}
