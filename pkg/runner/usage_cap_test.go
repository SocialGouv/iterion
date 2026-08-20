package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// capStatusStore records the status flip the pre-flight must perform: the
// durable retry conditions on failed_resumable, so a run stopped before its
// first node has to be MARKED before the park path can arm anything.
type capStatusStore struct {
	store.RunStore
	gotStatus store.RunStatus
	gotErr    string
	gotFrom   []store.RunStatus
	calls     int
}

func (s *capStatusStore) UpdateRunStatusIf(_ context.Context, _ string, status store.RunStatus, runErr string, from []store.RunStatus) (bool, error) {
	s.calls++
	s.gotStatus = status
	s.gotErr = runErr
	s.gotFrom = from
	return true, nil
}

type failingCapStore struct{ usagecap.Store }

func (failingCapStore) Latest(context.Context, string) ([]usagecap.Reading, error) {
	return nil, errors.New("store unavailable")
}

func capTestPolicy() usagecap.Policy {
	return usagecap.Policy{
		FiveHour: usagecap.WindowPolicy{MaxPercent: 85, Mode: usagecap.ModeSoft},
		Week:     usagecap.WindowPolicy{MaxPercent: 75, Mode: usagecap.ModeHard},
	}
}

func capRunner(pol usagecap.Policy, caps usagecap.Store, rs store.RunStore) *Runner {
	return &Runner{cfg: Config{
		Logger:         iterlog.Nop(),
		UsageCapPolicy: pol,
		UsageCaps:      caps,
		Store:          rs,
	}}
}

// capLLMWorkflow is the shape every pre-flight test assumed before the
// predicate existed: a workflow with an agent node, i.e. one that can
// actually draw on the subscription the cap protects.
func capLLMWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "capped",
		Entry: "think",
		Nodes: map[string]ir.Node{
			"think": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "think"}},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "think", To: "done"}},
	}
}

func TestUsageCapPreflight_BlocksBeforeSpendingAnything(t *testing.T) {
	ctx := t.Context()
	resets := time.Now().UTC().Add(30 * time.Hour)
	caps := usagecap.NewMemStore()
	key := usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform)
	if err := caps.Record(ctx, key, usagecap.Reading{
		Window:      usagecap.WindowSevenDay,
		Utilization: 0.92,
		Status:      usagecap.StatusWarning,
		ResetsAt:    resets,
		ObservedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	rs := &capStatusStore{}
	r := capRunner(capTestPolicy(), caps, rs)

	err := r.usageCapPreflight(ctx, capLLMWorkflow(), &queue.RunMessage{RunID: "run-1"}, iterlog.Nop())
	if err == nil {
		t.Fatal("want the run refused: the weekly window is at 92% against a 75% cap")
	}
	var rl *delegate.ErrRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("got %T, want *delegate.ErrRateLimited so the park path recognises it", err)
	}
	// Kind is what routes it to the usage-window park (and away from the
	// DLQ); SelfImposed keeps it out of the in-place retry budget.
	if rl.Kind != delegate.RateLimitKindUsageWindow || !rl.SelfImposed {
		t.Errorf("kind=%q self-imposed=%v", rl.Kind, rl.SelfImposed)
	}
	if !rl.ResetAt.Equal(resets) {
		t.Errorf("ResetAt = %v, want the window's own reopening %v", rl.ResetAt, resets)
	}
	if rs.calls != 1 || rs.gotStatus != store.RunStatusFailedResumable {
		t.Fatalf("status flip: calls=%d status=%q — without failed_resumable the retry cannot arm", rs.calls, rs.gotStatus)
	}
	if rs.gotErr == "" {
		t.Error("the run must say why it did not start")
	}
	// Only a run this pod has just claimed may be flipped: an operator who
	// paused or cancelled it in the meantime must win.
	if len(rs.gotFrom) == 0 {
		t.Error("the flip must be conditional on the run still being claimed")
	}
}

// Failing open is the deliberate posture: the mid-run guard still stands
// behind the pre-flight, so an unavailable ledger costs one call — while
// failing closed would strand a whole fleet on a bookkeeping outage.
func TestUsageCapPreflight_FailsOpen(t *testing.T) {
	fresh := func() usagecap.Reading {
		return usagecap.Reading{
			Window:      usagecap.WindowSevenDay,
			Utilization: 0.99,
			ObservedAt:  time.Now().UTC(),
			ResetsAt:    time.Now().UTC().Add(time.Hour),
		}
	}
	stale := func() usagecap.Reading {
		r := fresh()
		// Window already rolled over: the number describes a window that no
		// longer exists.
		r.ResetsAt = time.Now().UTC().Add(-time.Minute)
		r.ObservedAt = time.Now().UTC().Add(-2 * time.Hour)
		return r
	}
	tests := []struct {
		name  string
		pol   usagecap.Policy
		caps  func(*testing.T) usagecap.Store
		store store.RunStore
	}{
		{
			name: "no cap configured",
			pol:  usagecap.Policy{},
			caps: func(t *testing.T) usagecap.Store {
				s := usagecap.NewMemStore()
				_ = s.Record(t.Context(), usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform), fresh())
				return s
			},
		},
		{
			name: "no shared store",
			pol:  capTestPolicy(),
			caps: func(*testing.T) usagecap.Store { return nil },
		},
		{
			name: "store unreadable",
			pol:  capTestPolicy(),
			caps: func(*testing.T) usagecap.Store { return failingCapStore{} },
		},
		{
			name: "nothing measured yet",
			pol:  capTestPolicy(),
			caps: func(*testing.T) usagecap.Store { return usagecap.NewMemStore() },
		},
		{
			name: "the measured window has rolled over",
			pol:  capTestPolicy(),
			caps: func(t *testing.T) usagecap.Store {
				s := usagecap.NewMemStore()
				_ = s.Record(t.Context(), usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform), stale())
				return s
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs := &capStatusStore{}
			r := capRunner(tt.pol, tt.caps(t), rs)
			if err := r.usageCapPreflight(t.Context(), capLLMWorkflow(), &queue.RunMessage{RunID: "run-1"}, iterlog.Nop()); err != nil {
				t.Fatalf("blocked the run: %v", err)
			}
			if rs.calls != 0 {
				t.Errorf("flipped the run's status %d times without blocking it", rs.calls)
			}
		})
	}
}

// One tenant's own subscription is a different meter from the deployment's:
// merging them would let one tenant's spend park another's runs, and would
// consult the wrong ledger for both.
func TestUsageCapKey_SeparatesTenantCredentialsFromThePlatform(t *testing.T) {
	msg := &queue.RunMessage{TenantID: "team-7"}

	if got := usageCapKey(context.Background(), msg); got != usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform) {
		t.Errorf("no credentials in ctx = %q, want the platform meter", got)
	}

	own := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-tenant"},
	})
	if got := usageCapKey(own, msg); got != usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope("team-7")) {
		t.Errorf("tenant BYOK = %q, want the tenant's own meter", got)
	}

	// A bundle carrying only unrelated secrets still runs on the
	// deployment's subscription.
	unrelated := secrets.WithCredentials(context.Background(), secrets.Credentials{
		Generic: map[string]string{"forge_token": "x"},
	})
	if got := usageCapKey(unrelated, msg); got != usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform) {
		t.Errorf("unrelated secrets = %q, want the platform meter", got)
	}

	// A credential the publisher filled from the DB-backed platform tier
	// rides the bundle exactly like a tenant credential — but it IS the
	// deployment's single meter. Classifying it per tenant would fragment
	// the shared window into as many meters as there are teams.
	platformFilled := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:              map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-platform"},
		OAuthCredentialFiles: map[string]string{delegate.BackendClaudeCode: "/tmp/oauth"},
		PlatformSourced: map[string]bool{
			string(secrets.ProviderAnthropic): true,
			delegate.BackendClaudeCode:        true,
		},
	})
	if got := usageCapKey(platformFilled, msg); got != usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform) {
		t.Errorf("platform-sourced bundle = %q, want the platform meter", got)
	}

	// Mixed bundle: the tenant's own anthropic credential next to a
	// platform-filled slot of another family still meters on the tenant.
	mixed := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys: map[secrets.Provider]string{
			secrets.ProviderAnthropic: "sk-tenant",
			secrets.ProviderOpenAI:    "sk-platform",
		},
		PlatformSourced: map[string]bool{string(secrets.ProviderOpenAI): true},
	})
	if got := usageCapKey(mixed, msg); got != usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope("team-7")) {
		t.Errorf("tenant anthropic + platform openai = %q, want the tenant meter", got)
	}
}

func TestUsageGuardFor_NilWithoutAPolicy(t *testing.T) {
	r := capRunner(usagecap.Policy{}, usagecap.NewMemStore(), nil)
	if g := r.usageGuardFor(context.Background(), &queue.RunMessage{}, iterlog.Nop()); g != nil {
		t.Fatal("a deployment that configured no cap must not carry a guard")
	}
}

// What a pod measures has to reach the ledger, or every other pod keeps
// rediscovering the ceiling by spending against it.
func TestUsageGuardFor_PublishesUnderTheRunsCredentialKey(t *testing.T) {
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-tenant"},
	})
	caps := usagecap.NewMemStore()
	r := capRunner(capTestPolicy(), caps, nil)

	g := r.usageGuardFor(ctx, &queue.RunMessage{TenantID: "team-7"}, iterlog.Nop())
	if g == nil {
		t.Fatal("want a guard")
	}
	g.Observe(usagecap.Reading{Window: usagecap.WindowFiveHour, Utilization: 0.5, ObservedAt: time.Now().UTC()})

	got, err := caps.Latest(ctx, usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope("team-7")))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Utilization != 0.5 {
		t.Fatalf("ledger = %+v, want the reading under the tenant's key", got)
	}
}

// capToolOnlyWorkflow is the Vigie `collect` shape: a graph of tool nodes
// that never reaches a model. Documented zero-LLM, and refused for five days
// by a cap on a subscription it could not have spent.
func capToolOnlyWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "collect",
		Entry: "fetch",
		Nodes: map[string]ir.Node{
			"fetch": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "fetch"}, Command: "true"},
			"dedup": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "dedup"}, Command: "true"},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "fetch", To: "dedup"}, {From: "dedup", To: "done"}},
	}
}

// TestUsageCapPreflight_SparesAWorkflowThatCannotCallAModel is the fix's
// reason for existing, in the shape that produced it.
//
// The cap protects an LLM subscription. A workflow with no model call cannot
// draw on it, so refusing that run protects nothing — and costs whatever the
// run was there to do. For a feed collector the loss is not recoverable by
// retrying later: a feed serves a short window and does not remember what
// nobody fetched, so five days of refusals are five days of material gone.
//
// The ledger below is the one that blocked Vigie: the seven-day window at
// 92% against a 75% cap. The LLM-shaped run must still be refused (asserted
// in the same test, so a fix that simply disabled the cap fails here).
func TestUsageCapPreflight_SparesAWorkflowThatCannotCallAModel(t *testing.T) {
	ctx := t.Context()
	caps := usagecap.NewMemStore()
	if err := caps.Record(ctx, usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform),
		usagecap.Reading{
			Window:      usagecap.WindowSevenDay,
			Utilization: 0.92,
			Status:      usagecap.StatusWarning,
			ResetsAt:    time.Now().UTC().Add(30 * time.Hour),
			ObservedAt:  time.Now().UTC(),
		}); err != nil {
		t.Fatal(err)
	}
	rs := &capStatusStore{}
	r := capRunner(capTestPolicy(), caps, rs)

	if err := r.usageCapPreflight(ctx, capToolOnlyWorkflow(),
		&queue.RunMessage{RunID: "collect-1"}, iterlog.Nop()); err != nil {
		t.Fatalf("a zero-LLM run was refused by an LLM cap: %v", err)
	}
	// And it must not be marked failed_resumable either: a run that was
	// never blocked has no reason to carry the status of a blocked one.
	if rs.calls != 0 {
		t.Errorf("flipped the run's status %d times without blocking it", rs.calls)
	}

	// Same ledger, same runner: a workflow that CAN spend is still refused.
	if err := r.usageCapPreflight(ctx, capLLMWorkflow(),
		&queue.RunMessage{RunID: "think-1"}, iterlog.Nop()); err == nil {
		t.Error("the cap must still refuse a model-calling run")
	}
}
