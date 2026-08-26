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

// TestChainCooldownSkipsRefusedRouteAcrossNodes is the regression for #468:
// once one node learns that a subscription window is shut, later nodes on
// the same executor must enter the chain at the accepting fallback without
// spawning the refused primary again.
func TestChainCooldownSkipsRefusedRouteAcrossNodes(t *testing.T) {
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	head := &backendScriptedBackend{
		name: delegate.BackendClaudeCode,
		fail: &delegate.ErrRateLimited{
			Provider: delegate.BackendClaudeCode,
			Kind:     delegate.RateLimitKindUsageWindow,
			Detail:   "You've hit your session limit",
			ResetAt:  reset,
		},
	}
	tail := &backendScriptedBackend{name: delegate.BackendCodex}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, head)
	reg.Register(delegate.BackendCodex, tail)
	rec := &fallbackRecorder{}
	e := newFallbackExecutor(reg, rec.hook())
	e.now = func() time.Time { return now }
	chain := []chainElement{
		{Label: "primary"},
		{Label: "codex", Backend: delegate.BackendCodex},
	}

	dispatch := func(nodeID string) chainOutcome {
		t.Helper()
		build := e.newElementBuilder(nodeID, delegate.BackendClaudeCode, nil,
			func(_ context.Context, _ string) (*delegate.Task, error) {
				return &delegate.Task{NodeID: nodeID, Model: "claude-opus-5"}, nil
			})
		out, err := e.dispatchChain(context.Background(), nodeID, chain, "claude-opus-5", build)
		if err != nil {
			t.Fatalf("dispatch %s: %v", nodeID, err)
		}
		return out
	}

	dispatch("first")
	out := dispatch("second")
	if got := len(head.tasks); got != 1 {
		t.Errorf("primary spawned %d times, want 1: the second node must use the cooldown", got)
	}
	if got := len(tail.tasks); got != 2 {
		t.Errorf("fallback spawned %d times, want once per node", got)
	}
	if !out.FellThrough || out.ServedBy != "codex" {
		t.Errorf("second outcome = fell_through:%v served_by:%q, want cooldown route to codex", out.FellThrough, out.ServedBy)
	}
	if len(rec.events) != 2 {
		t.Fatalf("fallback events = %d, want refusal then cooldown skip", len(rec.events))
	}
	skip := rec.events[1]
	if !skip.Cooldown || skip.Attempts != 0 || !skip.CooldownUntil.Equal(reset) {
		t.Errorf("cooldown event = %+v, want attempts=0 and reset %s", skip, reset)
	}
	if skip.Err != nil || skip.Reason != string(delegate.FallbackUsageWindow) {
		t.Errorf("cooldown event error/reason = %v/%q", skip.Err, skip.Reason)
	}

	// Expiry is checked at dispatch; no sweeper is required. The primary is
	// attempted again as soon as the reset instant has passed.
	now = reset
	dispatch("third")
	if got := len(head.tasks); got != 2 {
		t.Errorf("primary spawned %d times after reset, want 2", got)
	}
}

func TestChainCooldownFailsOpenWithoutResetOrAcceptingFallback(t *testing.T) {
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		chain []chainElement
		reset time.Time
		want  int
	}{
		{
			name: "provider omitted reset",
			chain: []chainElement{
				{Label: "primary"},
				{Label: "codex", Backend: delegate.BackendCodex},
			},
			want: 2,
		},
		{
			name:  "no fallback configured",
			chain: []chainElement{{Label: "primary"}},
			reset: now.Add(time.Hour),
			want:  4,
		},
		{
			name: "fallback rejects usage window",
			chain: []chainElement{
				{Label: "primary"},
				{Label: "codex", Backend: delegate.BackendCodex, On: []delegate.FallbackCategory{delegate.FallbackUnavailable}},
			},
			reset: now.Add(time.Hour),
			want:  4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			head := &backendScriptedBackend{
				name: delegate.BackendClaudeCode,
				fail: &delegate.ErrRateLimited{
					Provider: delegate.BackendClaudeCode,
					Kind:     delegate.RateLimitKindUsageWindow,
					ResetAt:  tt.reset,
				},
			}
			tail := &backendScriptedBackend{name: delegate.BackendCodex}
			reg := delegate.NewRegistry()
			reg.Register(delegate.BackendClaudeCode, head)
			reg.Register(delegate.BackendCodex, tail)
			e := newFallbackExecutor(reg, EventHooks{})
			e.now = func() time.Time { return now }

			for n := 0; n < 2; n++ {
				build := e.newElementBuilder("node", delegate.BackendClaudeCode, nil,
					func(_ context.Context, _ string) (*delegate.Task, error) {
						return &delegate.Task{NodeID: "node"}, nil
					})
				_, _ = e.dispatchChain(context.Background(), "node", tt.chain, "", build)
			}
			if got := len(head.tasks); got != tt.want {
				t.Errorf("primary attempts = %d, want %d", got, tt.want)
			}
		})
	}
}

// What a provider refused is a property of the ROUTE, not of the node that
// happened to hit it first. A node with no fallback — or with one whose `on:`
// refuses the category — cannot USE the cooldown, but it must still ARM it,
// or the next node with a richer chain pays for the same refused spawn.
func TestChainCooldownLearnsFromANodeThatCannotUseIt(t *testing.T) {
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	tests := []struct {
		name    string
		teacher []chainElement
	}{
		{
			name:    "no fallback configured",
			teacher: []chainElement{{Label: "primary"}},
		},
		{
			name: "fallback refuses the category",
			teacher: []chainElement{
				{Label: "primary"},
				{Label: "codex", Backend: delegate.BackendCodex, On: []delegate.FallbackCategory{delegate.FallbackUnavailable}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			head := &backendScriptedBackend{
				name: delegate.BackendClaudeCode,
				fail: &delegate.ErrRateLimited{
					Provider: delegate.BackendClaudeCode,
					Kind:     delegate.RateLimitKindUsageWindow,
					ResetAt:  reset,
				},
			}
			tail := &backendScriptedBackend{name: delegate.BackendCodex}
			reg := delegate.NewRegistry()
			reg.Register(delegate.BackendClaudeCode, head)
			reg.Register(delegate.BackendCodex, tail)
			e := newFallbackExecutor(reg, EventHooks{})
			e.now = func() time.Time { return now }
			dispatch := func(nodeID string, chain []chainElement) {
				build := e.newElementBuilder(nodeID, delegate.BackendClaudeCode, nil,
					func(_ context.Context, _ string) (*delegate.Task, error) {
						return &delegate.Task{NodeID: nodeID, Model: "claude-opus-5"}, nil
					})
				_, _ = e.dispatchChain(context.Background(), nodeID, chain, "claude-opus-5", build)
			}

			dispatch("teacher", tt.teacher)
			spentByTeacher := len(head.tasks)
			if spentByTeacher == 0 {
				t.Fatal("precondition: the teaching node never reached the primary")
			}
			dispatch("learner", []chainElement{
				{Label: "primary"},
				{Label: "codex", Backend: delegate.BackendCodex},
			})
			if got := len(head.tasks); got != spentByTeacher {
				t.Errorf("primary spawned %d times, want %d: the learner must reuse the teacher's cooldown",
					got, spentByTeacher)
			}
			if len(tail.tasks) != 1 {
				t.Errorf("fallback served %d times, want 1", len(tail.tasks))
			}
		})
	}
}

func TestChainCooldownSupportsResettableUnavailable(t *testing.T) {
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	head := &backendScriptedBackend{
		name: delegate.BackendClaudeCode,
		fail: &delegate.ErrModelUnavailable{
			Provider: delegate.BackendClaudeCode,
			Model:    "claude-opus-5",
			ResetAt:  now.Add(30 * time.Minute),
		},
	}
	tail := &backendScriptedBackend{name: delegate.BackendCodex}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, head)
	reg.Register(delegate.BackendCodex, tail)
	e := newFallbackExecutor(reg, EventHooks{})
	e.now = func() time.Time { return now }
	chain := []chainElement{{Label: "primary"}, {Label: "codex", Backend: delegate.BackendCodex}}

	for n := 0; n < 2; n++ {
		build := e.newElementBuilder("node", delegate.BackendClaudeCode, nil,
			func(_ context.Context, _ string) (*delegate.Task, error) { return &delegate.Task{}, nil })
		if _, err := e.dispatchChain(context.Background(), "node", chain, "", build); err != nil {
			t.Fatalf("dispatch %d: %v", n, err)
		}
	}
	if got := len(head.tasks); got != 1 {
		t.Errorf("temporarily unavailable primary spawned %d times, want 1", got)
	}
}

// A cooldown avoids the repeated provider call, not the provider condition.
// If the serving fallback then fails, downstream run-level recovery must still
// see the remembered usage-window cause and park until its reset instant.
func TestChainCooldownPreservesRememberedCauseWhenFallbackFails(t *testing.T) {
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	head := &backendScriptedBackend{
		name: delegate.BackendClaudeCode,
		fail: &delegate.ErrRateLimited{
			Provider: delegate.BackendClaudeCode,
			Kind:     delegate.RateLimitKindUsageWindow,
			ResetAt:  reset,
		},
	}
	tail := &backendScriptedBackend{name: delegate.BackendClaw}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, head)
	reg.Register(delegate.BackendClaw, tail)
	e := newFallbackExecutor(reg, EventHooks{})
	e.now = func() time.Time { return now }
	chain := []chainElement{
		{Label: "primary"},
		{Label: "api", Backend: delegate.BackendClaw},
	}
	dispatch := func(nodeID string) error {
		t.Helper()
		build := e.newElementBuilder(nodeID, delegate.BackendClaudeCode, nil,
			func(_ context.Context, _ string) (*delegate.Task, error) {
				return &delegate.Task{NodeID: nodeID, Model: "claude-opus-5"}, nil
			})
		_, err := e.dispatchChain(context.Background(), nodeID, chain, "claude-opus-5", build)
		return err
	}

	if err := dispatch("learn-window"); err != nil {
		t.Fatalf("learning dispatch: %v", err)
	}
	tail.fail = &delegate.ErrAuthFailed{Provider: "claw", Detail: "no API key"}
	err := dispatch("cooled-primary")
	if err == nil {
		t.Fatal("expected the fallback failure to exhaust the chain")
	}
	if got := len(head.tasks); got != 1 {
		t.Fatalf("primary spawned %d times, want 1: second dispatch should use cooldown", got)
	}
	var rl *delegate.ErrRateLimited
	if !errors.As(err, &rl) || rl.Kind != delegate.RateLimitKindUsageWindow || !rl.ResetAt.Equal(reset) {
		t.Errorf("remembered usage-window cause lost from terminal error: %v", err)
	}
	var auth *delegate.ErrAuthFailed
	if !errors.As(err, &auth) {
		t.Errorf("fallback auth cause lost from terminal error: %v", err)
	}
	var exhausted *ErrChainExhausted
	if !errors.As(err, &exhausted) || len(exhausted.Errs) != 2 {
		t.Errorf("terminal error = %#v, want two-cause ErrChainExhausted", err)
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

// TestChainStopsWhenNoRemainingRouteAccepts: `on:` is a per-route
// filter. When the only remaining route refuses the category, the walk
// ends rather than burning a route that cannot help.
func TestChainStopsWhenNoRemainingRouteAccepts(t *testing.T) {
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
		t.Fatal("expected the chain to stop when no remaining route accepts the category")
	}
	if len(tail.tasks) != 0 {
		t.Errorf("the claw element ran %d times; it only accepts usage_window and the failure was auth", len(tail.tasks))
	}
}

// TestChainSkipsNonAcceptingRouteAndTriesLater: `on:` is a per-route
// filter, not a chain terminator (Re50c7d). The shipped example shape
// (api with default [usage_window, unavailable], then gpt that also
// accepts transient_exhausted) must reach gpt when the primary dies of
// a transient that exhausted its budget — not stop at api.
func TestChainSkipsNonAcceptingRouteAndTriesLater(t *testing.T) {
	head := &backendScriptedBackend{
		name: delegate.BackendClaudeCode,
		// ClassifyFallback maps *ErrTransient with exhausted retries via
		// the isDelegateRetryable path inside dispatchChain; force the
		// category by using a plain error that the walker classifies
		// after the retry loop reports exhaustion. Use ErrTransient so
		// the retry loop runs, then fails as transient_exhausted.
		fail:   &delegate.ErrTransient{Reason: "boom"},
		tokens: 100, costUSD: 0.05,
	}
	// Middle route: default-style filter, refuses transient_exhausted.
	middle := &backendScriptedBackend{name: "middle-unused", tokens: 1}
	// Later route: explicitly accepts transient_exhausted (the gpt route).
	// Register under claw so newElementBuilder can resolve it.
	tail := &backendScriptedBackend{name: delegate.BackendClaw, tokens: 30, costUSD: 0.01}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, head)
	reg.Register("middle-unused", middle)
	reg.Register(delegate.BackendClaw, tail)
	e := newFallbackExecutor(reg, EventHooks{})

	build := e.newElementBuilder("implement", delegate.BackendClaudeCode, nil,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "implement"}, nil
		})
	out, err := e.dispatchChain(context.Background(), "implement", []chainElement{
		{Label: "primary"},
		{Label: "api", Backend: "middle-unused", Model: "anthropic/claude-opus-5",
			On: []delegate.FallbackCategory{delegate.FallbackUsageWindow, delegate.FallbackUnavailable}},
		{Label: "gpt", Backend: delegate.BackendClaw, Model: "openai/gpt-5.5",
			On: []delegate.FallbackCategory{
				delegate.FallbackUsageWindow, delegate.FallbackUnavailable, delegate.FallbackTransientExhausted,
			}},
	}, "claude-opus-5", build)
	if err != nil {
		t.Fatalf("expected gpt to serve after api was skipped: %v", err)
	}
	if len(middle.tasks) != 0 {
		t.Errorf("api route ran %d times; it refuses transient_exhausted and must be skipped", len(middle.tasks))
	}
	if len(tail.tasks) != 1 {
		t.Errorf("gpt route ran %d times, want 1", len(tail.tasks))
	}
	if out.ServedBy != "gpt" {
		t.Errorf("ServedBy = %q, want gpt", out.ServedBy)
	}
	// Head's spend is folded into the winner (R5180a7 still holds).
	if got, want := out.Result.Tokens, 130; got != want {
		t.Errorf("tokens = %d, want %d (head 100 + gpt 30, api never ran)", got, want)
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

// A remembered fall-through is still a route change. A prior failed attempt
// of this node may have left provider-specific messages in the session store,
// even though the cooled primary is not called during this dispatch.
func TestChainEvictsNodeSessionOnCooldownFallThrough(t *testing.T) {
	const runID, nodeID = "run-1", "review"
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	head := &backendScriptedBackend{
		name: delegate.BackendClaudeCode,
		fail: &delegate.ErrRateLimited{
			Provider: delegate.BackendClaudeCode,
			Kind:     delegate.RateLimitKindUsageWindow,
			ResetAt:  now.Add(time.Hour),
		},
	}
	tail := &backendScriptedBackend{name: delegate.BackendClaw}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, head)
	reg.Register(delegate.BackendClaw, tail)
	e := newFallbackExecutor(reg, EventHooks{})
	e.now = func() time.Time { return now }
	chain := []chainElement{
		{Label: "primary"},
		{Label: "api", Backend: delegate.BackendClaw},
	}
	build := func(id string) elementBuilder {
		return e.newElementBuilder(id, delegate.BackendClaudeCode, nil,
			func(_ context.Context, _ string) (*delegate.Task, error) {
				return &delegate.Task{NodeID: id, Model: "claude-opus-5"}, nil
			})
	}

	if _, err := e.dispatchChain(context.Background(), "learn-window", chain,
		"claude-opus-5", build("learn-window")); err != nil {
		t.Fatalf("learning dispatch: %v", err)
	}

	sessions := newNodeSessionStore()
	sessions.SaveSnapshot(runID, nodeID, []byte(`[{"role":"assistant"}]`))
	if sessions.LoadSnapshot(runID, nodeID) == nil {
		t.Fatal("precondition: snapshot not stored")
	}
	ctx := withRuntimeContext(context.Background(), runID, sessions)
	if _, err := e.dispatchChain(ctx, nodeID, chain, "claude-opus-5", build(nodeID)); err != nil {
		t.Fatalf("cooled dispatch: %v", err)
	}
	if got := len(head.tasks); got != 1 {
		t.Fatalf("primary spawned %d times, want 1: second dispatch should use cooldown", got)
	}
	if snap := sessions.LoadSnapshot(runID, nodeID); snap != nil {
		t.Errorf("the prior attempt's conversation survived the cooldown route change: %s", snap)
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

// TestUsageWindow_KeepsRetryBudgetWhenNextRouteRejectsIt: the carve-out
// must consult the next route's `on:` filter, not merely its existence.
// A node whose only route declares `on: [unavailable]` would otherwise
// lose its in-place budget on a usage window AND then stop at the
// filter — strictly worse off than before the chain existed.
func TestUsageWindow_KeepsRetryBudgetWhenNextRouteRejectsIt(t *testing.T) {
	head := &backendScriptedBackend{name: delegate.BackendClaudeCode, fail: usageWindowErr()}
	tail := &backendScriptedBackend{name: delegate.BackendClaw}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, head)
	reg.Register(delegate.BackendClaw, tail)
	e := newFallbackExecutor(reg, EventHooks{})

	build := e.newElementBuilder("review", delegate.BackendClaudeCode, nil,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "review"}, nil
		})
	_, _ = e.dispatchChain(context.Background(), "review", []chainElement{
		{Label: "primary"},
		{Label: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5.5",
			On: []delegate.FallbackCategory{delegate.FallbackUnavailable}},
	}, "claude-opus-5", build)

	if len(head.tasks) != 2 {
		t.Errorf("head attempted %d times, want the full budget (2) — the route ahead does not accept usage_window, so skipping the budget buys nothing", len(head.tasks))
	}
	if len(tail.tasks) != 0 {
		t.Errorf("the filtered route ran %d times, want 0", len(tail.tasks))
	}
}

// TestFilterStopDoesNotDoubleCountSpend: when the next route's `on:`
// filter refuses the failure category, the walk stops with the failed
// element as the terminal result. That element must NOT also sit in
// `spent` — applyTo would then count its tokens twice (R5180a7).
func TestFilterStopDoesNotDoubleCountSpend(t *testing.T) {
	head := &backendScriptedBackend{
		name: delegate.BackendClaudeCode, fail: &delegate.ErrTransient{Reason: "boom"},
		tokens: 1000, costUSD: 0.40,
	}
	// Tail is never reached: its On filter rejects transient/unclassified
	// failures (only accepts usage_window).
	tail := &backendScriptedBackend{
		name:   delegate.BackendClaw,
		tokens: 500, costUSD: 0.20,
	}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, head)
	reg.Register(delegate.BackendClaw, tail)
	e := newFallbackExecutor(reg, EventHooks{})

	build := e.newElementBuilder("review", delegate.BackendClaudeCode, nil,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "review"}, nil
		})
	out, err := e.dispatchChain(context.Background(), "review", []chainElement{
		{Label: "primary"},
		{Label: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5.5",
			On: []delegate.FallbackCategory{delegate.FallbackUsageWindow}},
	}, "claude-opus-5", build)
	if err == nil {
		t.Fatal("expected the chain to stop on the filter")
	}
	if len(tail.tasks) != 0 {
		t.Fatalf("tail ran %d times, want 0 (filter refused)", len(tail.tasks))
	}
	// Head burned 1000 tokens; it is the terminal result and must appear
	// exactly once — not once in spent and once in result.
	if got, want := out.Result.Tokens, 1000; got != want {
		t.Errorf("tokens = %d, want %d (filter-stop must not double-count the last route)", got, want)
	}
	if got, _ := out.Result.Output["_cost_usd"].(float64); got != 0.40 {
		t.Errorf("_cost_usd = %v, want 0.40", got)
	}
}

// TestExhaustedChainCarriesEverySpend: an exhausted chain still burned
// what its routes burned. Dropping it under-reports whole agentic
// sessions to max_cost_usd, the org monthly cap and a donor's ledger.
func TestExhaustedChainCarriesEverySpend(t *testing.T) {
	head := &backendScriptedBackend{
		name: delegate.BackendClaudeCode, fail: &delegate.ErrTransient{Reason: "boom"},
		tokens: 1000, costUSD: 0.40,
	}
	tail := &backendScriptedBackend{
		name: delegate.BackendClaw, fail: &delegate.ErrTransient{Reason: "boom"},
		tokens: 500, costUSD: 0.20,
	}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, head)
	reg.Register(delegate.BackendClaw, tail)
	e := newFallbackExecutor(reg, EventHooks{})

	build := e.newElementBuilder("review", delegate.BackendClaudeCode, nil,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "review"}, nil
		})
	out, err := e.dispatchChain(context.Background(), "review", []chainElement{
		{Label: "primary"},
		{Label: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5.5"},
	}, "claude-opus-5", build)
	if err == nil {
		t.Fatal("expected the chain to fail")
	}
	// Each route reports its last attempt's usage (the retry loop does
	// not accumulate across attempts), so the node's total is the head's
	// 1000 plus the tail's 500 — asserted as an EXACT sum, because a
	// loose lower bound is exactly what let a double-count of the last
	// route pass unnoticed.
	if got, want := out.Result.Tokens, 1500; got != want {
		t.Errorf("tokens = %d, want %d (each route folded in exactly once)", got, want)
	}
}

// TestCollapseHintOnlyChain_TrimsPrefixNotWholeChain: a node carrying
// both `provider: "a,b"` and a `fallbacks:` route must still collapse
// its inert hint prefix on a hint-ignoring backend. Refusing to collapse
// merely because an unrelated route exists re-issues an identical call
// with a second full retry budget — the waste the guard exists for.
func TestCollapseHintOnlyChain_TrimsPrefixNotWholeChain(t *testing.T) {
	chain := []chainElement{
		{Provider: "zai"},
		{Provider: "anthropic"},
		{Label: "api", Backend: delegate.BackendClaudeCode, Model: "claude-opus-5"},
	}
	got := collapseHintOnlyChain(chain, delegate.BackendClaw)
	if len(got) != 2 {
		t.Fatalf("got %d elements, want the hint prefix collapsed to its head plus the named route: %+v", len(got), got)
	}
	if got[0].Provider != "zai" || got[1].Label != "api" {
		t.Errorf("collapse kept the wrong elements: %+v", got)
	}
}
