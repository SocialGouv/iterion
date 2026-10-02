package runner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// scriptedBackend answers every call with a fixed result and error,
// optionally emitting an in-process claw loop's own llm_request /
// llm_step_finished through the run's hooks.
type scriptedBackend struct {
	mu    sync.Mutex
	res   delegate.Result
	err   error
	hooks *model.EventHooks
	steps [][2]int
}

func (s *scriptedBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hooks != nil {
		for i, st := range s.steps {
			s.hooks.OnLLMRequest(task.NodeID, model.LLMRequestInfo{Model: task.Model, WireModel: "wire", Timestamp: time.Now()})
			s.hooks.OnLLMStepFinish(task.NodeID, model.LLMStepInfo{Number: i + 1, InputTokens: st[0], OutputTokens: st[1]})
		}
	}
	r := s.res
	if r.Output == nil {
		r.Output = map[string]any{}
	}
	return r, s.err
}

type eventLog struct {
	mu     sync.Mutex
	inner  model.EventEmitter
	events []store.Event
}

func (r *eventLog) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	r.mu.Lock()
	r.events = append(r.events, evt)
	r.mu.Unlock()
	return r.inner.AppendEvent(ctx, runID, evt)
}

func usageWindowErr() error {
	return &delegate.ErrRateLimited{Provider: "x", Kind: delegate.RateLimitKindUsageWindow, ResetAt: time.Now().Add(time.Hour)}
}

// runChain executes node through the real executor, its store hooks and the
// meter, with one call per element.
func runChain(t *testing.T, node *ir.AgentNode, backends map[string]*scriptedBackend) (*metricsEmitter, *eventLog) {
	t.Helper()
	regd := map[string]delegate.Backend{}
	for name, b := range backends {
		regd[name] = b
	}
	return runChainWith(t, node, regd, backends, model.RetryPolicy{MaxAttempts: 1, MaxAttemptsTransient: 1})
}

// runChainWith is runChain with any backends and a retry policy.
func runChainWith(t *testing.T, node *ir.AgentNode, regd map[string]delegate.Backend, scripted map[string]*scriptedBackend, rp model.RetryPolicy) (*metricsEmitter, *eventLog) {
	t.Helper()
	usage := newMetricsEmitter(discardEmitter{}, metrics.New())
	log := &eventLog{inner: usage}
	hooks := model.NewStoreEventHooks(context.Background(), log, "run", iterlog.Nop(), nil)
	reg := delegate.NewRegistry()
	for _, b := range scripted {
		if b.steps != nil {
			b.hooks = &hooks
		}
	}
	for name, b := range regd {
		reg.Register(name, b)
	}
	exec := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{Prompts: map[string]*ir.Prompt{"ask": {Body: "go"}}, Schemas: map[string]*ir.Schema{}},
		model.WithBackendRegistry(reg), model.WithEventHooks(hooks), model.WithLogger(iterlog.Nop()),
		model.WithDefaultBackend(node.Backend), model.WithRetryPolicy(rp))
	_, _ = exec.Execute(model.WithRunID(context.Background(), "run"), node, map[string]any{})
	return usage, log
}

func agentNode(backend, modelSpec string, fallbacks ...ir.Fallback) *ir.AgentNode {
	node := &ir.AgentNode{}
	node.ID = "impl"
	node.Backend = backend
	node.Model = modelSpec
	node.UserPrompt = "ask"
	node.Fallbacks = fallbacks
	return node
}

var giveUp = ir.Fallback{Name: "give_up", Action: ir.FallbackActionSkip, On: []string{"usage_window"}}

// booked returns the meter's per-route totals with the slot each is charged
// to, and their sum.
func booked(usage *metricsEmitter, creds secrets.Credentials) (map[string]int64, int64) {
	bySlot := map[string]int64{}
	var total int64
	for k, v := range usage.RouteTotals() {
		bySlot[routeSlot(creds, k)] += v.tokens()
		total += v.tokens()
	}
	return bySlot, total
}

// A chain ending on `action: skip` books what its failed routes burned on the
// route that spent, never on a later route refused before it spent anything
// (TestRouteModel_ASkipBooksTheRouteThatBurned holds the other order). Red
// when the skip is labelled with the last route that executed.
func TestSkip_BooksTheSpendOnTheRouteThatSpent(t *testing.T) {
	forfait := secrets.Credentials{
		APIKeys:              map[secrets.Provider]string{secrets.ProviderOpenAI: "sk-openai"},
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): "/forfait"},
	}
	t.Run("the first route spent, the fallback was refused at once", func(t *testing.T) {
		usage, _ := runChain(t, agentNode(delegate.BackendClaudeCode, "claude-opus-4-5",
			ir.Fallback{Name: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5.5", On: []string{"usage_window"}}, giveUp),
			map[string]*scriptedBackend{
				delegate.BackendClaudeCode: {res: delegate.Result{BackendName: delegate.BackendClaudeCode, Tokens: 5000, SessionFingerprint: "anthropic-oauth"}, err: usageWindowErr()},
				delegate.BackendClaw:       {err: usageWindowErr()},
			})
		if bySlot, total := booked(usage, forfait); total != 5000 || bySlot[string(secrets.OAuthKindClaudeCode)] != 5000 {
			t.Errorf("booked %v (total %d), want the 5000 the forfait burned on its slot", bySlot, total)
		}
		for k := range usage.RouteTotals() {
			if k.backend != delegate.BackendClaudeCode || k.model != "claude-opus-4-5" {
				t.Errorf("spend booked on route %+v, want claude_code's claude-opus-4-5", k)
			}
		}
	})
}

// A skip after a claude_code session spent keeps that session's credential
// fingerprint: the ledger books it on the forfait that paid, as it would had
// the session served. Red when the skip outcome drops the fingerprint.
func TestSkip_KeepsTheSpendersCredentialFingerprint(t *testing.T) {
	creds := secrets.Credentials{
		APIKeys:              map[secrets.Provider]string{secrets.ProviderZAI: "zai-key"},
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): "/forfait"},
	}
	for name, err := range map[string]error{"skipped": usageWindowErr(), "served": nil} {
		t.Run(name, func(t *testing.T) {
			usage, _ := runChain(t, agentNode(delegate.BackendClaudeCode, "claude-opus-4-5", giveUp),
				map[string]*scriptedBackend{delegate.BackendClaudeCode: {res: delegate.Result{BackendName: delegate.BackendClaudeCode, Tokens: 5000, SessionFingerprint: "anthropic-oauth"}, err: err}})
			if bySlot, total := booked(usage, creds); total != 5000 || bySlot[string(secrets.OAuthKindClaudeCode)] != 5000 {
				t.Errorf("booked %v (total %d), want 5000 on the forfait", bySlot, total)
			}
		})
	}
}

// An in-process claw route whose steps the meter already counted, then a
// fallback refused at once, then skip: the claw spend is booked once. Red
// when the skip is labelled with the refused fallback's backend, which the
// claw double-count exclusion does not recognise.
func TestSkip_AnObservedClawRouteIsBookedOnce(t *testing.T) {
	usage, _ := runChain(t, agentNode(delegate.BackendClaw, "openai/gpt-5.5",
		ir.Fallback{Name: "cc", Backend: delegate.BackendClaudeCode, Model: "claude-opus-4-5", On: []string{"usage_window"}}, giveUp),
		map[string]*scriptedBackend{
			delegate.BackendClaw:       {res: delegate.Result{BackendName: delegate.BackendClaw, Tokens: 5000}, err: usageWindowErr(), steps: [][2]int{{4000, 1000}}},
			delegate.BackendClaudeCode: {res: delegate.Result{BackendName: delegate.BackendClaudeCode, SessionFingerprint: "anthropic-oauth"}, err: usageWindowErr()},
		})
	if _, total := booked(usage, secrets.Credentials{}); total != 5000 {
		t.Errorf("booked %d tokens, want the 5000 the claw route burned, once", total)
	}
}

// An exhausted chain's delegate_error names one element in all its fields:
// the last one that ran, whose failure it reports — also when that element's
// result names no backend of its own. Red when the outcome pairs one
// element's backend with another's route.
func TestExhausted_NamesTheLastElementThatRan(t *testing.T) {
	for name, clawRes := range map[string]delegate.Result{
		"the last element names its backend": {BackendName: delegate.BackendClaw},
		"the last element names none":        {},
	} {
		t.Run(name, func(t *testing.T) {
			_, log := runChain(t, agentNode(delegate.BackendClaudeCode, "claude-opus-4-5",
				ir.Fallback{Name: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5.5", On: []string{"usage_window"}}),
				map[string]*scriptedBackend{
					delegate.BackendClaudeCode: {res: delegate.Result{BackendName: delegate.BackendClaudeCode, Tokens: 5000}, err: usageWindowErr()},
					delegate.BackendClaw:       {res: clawRes, err: usageWindowErr()},
				})
			var seen int
			for _, e := range log.events {
				if e.Type != store.EventDelegateError {
					continue
				}
				seen++
				if e.Data["backend"] != delegate.BackendClaw || e.Data["route_model"] != "openai/gpt-5.5" {
					t.Errorf("delegate_error = %v, want claw on openai/gpt-5.5", e.Data)
				}
			}
			if seen != 1 {
				t.Fatalf("%d delegate_error events, want one", seen)
			}
		})
	}
}

// A trailing element that never built leaves the error on the route that
// executed.
func TestExhausted_ATrailingBuildFailureKeepsTheExecutedRoute(t *testing.T) {
	_, log := runChain(t, agentNode(delegate.BackendClaudeCode, "claude-opus-4-5",
		ir.Fallback{Name: "ghost", Backend: "ghost", Model: "openai/gpt-5.5", On: []string{"usage_window"}}),
		map[string]*scriptedBackend{delegate.BackendClaudeCode: {res: delegate.Result{BackendName: delegate.BackendClaudeCode, Tokens: 5000}, err: usageWindowErr()}})
	for _, e := range log.events {
		if e.Type == store.EventDelegateError && (e.Data["route_model"] != "claude-opus-4-5" || e.Data["backend"] != delegate.BackendClaudeCode) {
			t.Errorf("delegate_error = %v, want claude_code on claude-opus-4-5", e.Data)
		}
	}
}

// attemptsBackend answers call i with steps[i] (the last one repeats).
type attemptsBackend struct {
	mu    sync.Mutex
	calls int
	steps []struct {
		res delegate.Result
		err error
	}
}

func (b *attemptsBackend) Execute(_ context.Context, _ delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.steps[min(b.calls, len(b.steps)-1)]
	b.calls++
	r := s.res
	out := map[string]any{}
	for k, v := range r.Output {
		out[k] = v
	}
	r.Output = out
	return r, s.err
}

// A forfait session that spent, was retried in place, and whose retry was
// refused before it spent anything, then skip: the spend keeps the session's
// fingerprint and is booked on the forfait — not on a metered key the wire
// precedence would name. Red when the in-place fold takes the identity of
// the attempt that spent nothing.
func TestSkip_KeepsTheFingerprintAcrossInPlaceRetries(t *testing.T) {
	creds := secrets.Credentials{
		APIKeys:              map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant-metered"},
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): "/forfait"},
	}
	cc := &attemptsBackend{steps: []struct {
		res delegate.Result
		err error
	}{
		{delegate.Result{BackendName: delegate.BackendClaudeCode, Tokens: 5000, SessionFingerprint: "anthropic-oauth", Output: map[string]any{"_tokens": 5000, "_cost_usd": 0.5}}, &delegate.ErrTransient{Reason: "overloaded"}},
		{delegate.Result{BackendName: delegate.BackendClaudeCode, ExitCode: -1}, usageWindowErr()},
	}}
	usage, _ := runChainWith(t, agentNode(delegate.BackendClaudeCode, "claude-opus-4-5", giveUp),
		map[string]delegate.Backend{delegate.BackendClaudeCode: cc}, nil,
		model.RetryPolicy{MaxAttempts: 2, MaxAttemptsTransient: 2, BackoffBase: time.Millisecond})
	if cc.calls != 2 {
		t.Fatalf("%d calls, want the in-place retry", cc.calls)
	}
	if bySlot, total := booked(usage, creds); total != 5000 || bySlot[string(secrets.OAuthKindClaudeCode)] != 5000 {
		t.Errorf("booked %v (total %d), want 5000 on the forfait", bySlot, total)
	}
}
