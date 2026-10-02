package model

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// A failed delegation names the last element that executed — the failure is
// its — whatever earlier element spent, and never an empty route. Red when
// the error outcomes stop carrying the route, or take another element's.
func TestDelegateError_NamesTheLastElementThatExecuted(t *testing.T) {
	twoRoutes := []chainElement{{Label: "primary"}, {Label: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5.5"}}
	cases := []struct {
		name             string
		chain            []chainElement
		headTok, tailTok int
		want             string
	}{
		{"single element", []chainElement{{Label: "primary"}}, 10, 0, "anthropic/claude-opus-5"},
		{"two elements, both spent", twoRoutes, 10, 30, "openai/gpt-5.5"},
		{"two elements, only the first spent", twoRoutes, 10, 0, "openai/gpt-5.5"},
		{"two elements, nothing spent", twoRoutes, 0, 0, "openai/gpt-5.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := delegate.NewRegistry()
			reg.Register(delegate.BackendClaudeCode, &backendScriptedBackend{name: delegate.BackendClaudeCode, fail: &delegate.ErrTransient{Reason: "boom"}, tokens: tc.headTok})
			reg.Register(delegate.BackendClaw, &backendScriptedBackend{name: delegate.BackendClaw, fail: &delegate.ErrTransient{Reason: "boom2"}, tokens: tc.tailTok})
			var failed []DelegateInfo
			e := newFallbackExecutor(reg, EventHooks{OnDelegateError: func(_ string, di DelegateInfo) { failed = append(failed, di) }})
			build := e.newElementBuilder("review", delegate.BackendClaudeCode, nil,
				func(_ context.Context, _ string) (*delegate.Task, error) {
					return &delegate.Task{NodeID: "review", Model: "anthropic/claude-opus-5"}, nil
				})
			if _, err := e.dispatchWithObservability(context.Background(), "review", delegate.BackendClaudeCode, "model: node", tc.chain, "anthropic/claude-opus-5", build, nil); err == nil {
				t.Fatal("want an error")
			}
			if len(failed) != 1 || failed[0].RouteModel != tc.want {
				t.Fatalf("delegate_error = %+v, want one naming route %q", failed, tc.want)
			}
		})
	}
}

// A cancelled dispatch names the route it abandoned.
func TestDelegateError_ACancelledDispatchNamesItsRoute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaw, &cancellingBackend{cancel: cancel})
	var failed []DelegateInfo
	e := newFallbackExecutor(reg, EventHooks{OnDelegateError: func(_ string, di DelegateInfo) { failed = append(failed, di) }})
	build := e.newElementBuilder("review", delegate.BackendClaw, nil,
		func(_ context.Context, _ string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "review", Model: "openai/gpt-5.5"}, nil
		})
	chain := []chainElement{{Label: "primary"}, {Label: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5.4"}}
	if _, err := e.dispatchWithObservability(ctx, "review", delegate.BackendClaw, "model: node", chain, "openai/gpt-5.5", build, nil); err == nil {
		t.Fatal("want an error")
	}
	if len(failed) != 1 || failed[0].RouteModel != "openai/gpt-5.5" {
		t.Fatalf("delegate_error = %+v, want one naming route openai/gpt-5.5", failed)
	}
}

// cancellingBackend cancels the dispatch while its call runs, then fails.
type cancellingBackend struct{ cancel context.CancelFunc }

func (b *cancellingBackend) Execute(context.Context, delegate.Task) (delegate.Result, error) {
	b.cancel()
	return delegate.Result{BackendName: delegate.BackendClaw, Tokens: 5}, context.Canceled
}

// scriptedCall is one answer of a sequencedBackend.
type scriptedCall struct {
	res delegate.Result
	err error
}

// sequencedBackend answers call i with seq[i] (the last one repeats); a task
// carrying a session id answers withSession instead.
type sequencedBackend struct {
	mu          sync.Mutex
	withSession *scriptedCall
	seq         []scriptedCall
	n           int
}

func (b *sequencedBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var c scriptedCall
	if task.SessionID != "" && b.withSession != nil {
		c = *b.withSession
	} else {
		c = b.seq[min(b.n, len(b.seq)-1)]
		b.n++
	}
	r := c.res
	out := map[string]any{}
	for k, v := range r.Output {
		out[k] = v
	}
	r.Output = out
	return r, c.err
}

// A session-degraded attempt that spent, then a fresh retry and a fallback
// both refused at once, then skip: the skip names the degraded element — its
// backend, route and fingerprint. Red when the degrade path does not mark its
// attempt as the spender.
func TestSkip_ADegradedAttemptIsTheSpender(t *testing.T) {
	wall := func() error {
		return &delegate.ErrRateLimited{Provider: "x", Kind: delegate.RateLimitKindUsageWindow, ResetAt: time.Now().Add(time.Hour)}
	}
	cc := &sequencedBackend{
		withSession: &scriptedCall{res: delegate.Result{BackendName: delegate.BackendClaudeCode, Tokens: 4000, SessionFingerprint: "anthropic-oauth", EffectiveModel: "claude-opus-4-5",
			Output: map[string]any{"_tokens": 4000, "_cost_usd": 0.4}}, err: errors.New("error_during_execution: session not found")},
		seq: []scriptedCall{{res: delegate.Result{BackendName: delegate.BackendClaudeCode}, err: wall()}},
	}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, cc)
	reg.Register(delegate.BackendClaw, &sequencedBackend{seq: []scriptedCall{{res: delegate.Result{BackendName: delegate.BackendClaw}, err: wall()}}})
	var finished []DelegateInfo
	e := newFallbackExecutor(reg, EventHooks{OnDelegateFinished: func(_ string, di DelegateInfo) { finished = append(finished, di) }})
	build := e.newElementBuilder("impl", delegate.BackendClaudeCode, nil,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			if bn == delegate.BackendClaudeCode {
				return &delegate.Task{NodeID: "impl", Model: "claude-opus-4-5", SessionID: "s-up", SessionOptional: true}, nil
			}
			return &delegate.Task{NodeID: "impl", Model: "openai/gpt-5.5"}, nil
		})
	uw := []delegate.FallbackCategory{delegate.FallbackUsageWindow}
	chain := []chainElement{{Label: "primary"}, {Label: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5.5", On: uw}, {Label: "give_up", Skip: true, On: uw}}
	out, err := e.dispatchWithObservability(context.Background(), "impl", delegate.BackendClaudeCode, "model: node", chain, "claude-opus-4-5", build, nil)
	if err != nil || !out.Skipped || len(finished) != 1 {
		t.Fatalf("err=%v skipped=%v finished=%d, want one skip outcome", err, out.Skipped, len(finished))
	}
	if di := finished[0]; di.BackendName != delegate.BackendClaudeCode || di.RouteModel != "claude-opus-4-5" || di.Fingerprint != "anthropic-oauth" || di.Tokens != 4000 {
		t.Errorf("skip labelled %s/%s fp=%q tokens=%d, want claude_code/claude-opus-4-5 fp=anthropic-oauth tokens=4000", di.BackendName, di.RouteModel, di.Fingerprint, di.Tokens)
	}
}
