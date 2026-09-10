package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/eventbus"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// POST /api/v1/triggers/emit publishes ONE event that fans out to however many
// subscriptions match it — zero, one or ten. Charging the request a monthly run
// slot therefore priced the wrong thing on both sides: an event nobody listens
// to consumed a slot, and ten launches consumed one. The request keeps the
// admission as a PRE-CHECK (a suspended org still gets its 403 synchronously)
// but hands its slot back at once; the spine meters each launch it performs.

func emitRequest(t *testing.T, s *Server, ctx context.Context, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/triggers/emit", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleEmitTrigger(rec, req)
	return rec
}

func TestEmitTrigger_FanOutToNothingMetersNothing(t *testing.T) {
	pub := &spinePublisher{}
	s := newGatedSpineServer(t, gateSpec{id: "t1", orgRunQuota: 5}, pub)
	counter := orgusage.NewMemoryCounter()
	s.orgUsage = counter
	s.cfg.TriggerStore = trigger.NewMemorySubscriptionStore()
	s.cfg.EventsBus = eventbus.NewInProcBus(s.logger)
	ctx := gatedTeamCtx("t1")

	rec := emitRequest(t, s, ctx, `{"kind":"probe"}`)
	if rec.Code != 202 {
		t.Fatalf("emit = %d %s, want 202", rec.Code, rec.Body.String())
	}
	u, _ := counter.Usage(context.Background(), "t1", time.Now().UTC())
	if u.Runs != 0 {
		t.Errorf("monthly runs = %d after an emit that launched nothing, want 0", u.Runs)
	}
}

// A suspended org still gets a synchronous refusal: the pre-check stays, only
// its metered slot is released.
func TestEmitTrigger_DeniedOrgIsRefusedSynchronously(t *testing.T) {
	pub := &spinePublisher{}
	s := newGatedSpineServer(t, gateSpec{id: "t1", maxConcurrentRuns: 1}, pub)
	s.cfg.Store = fakeActiveStore{active: 1}
	s.cfg.TriggerStore = trigger.NewMemorySubscriptionStore()
	s.cfg.EventsBus = eventbus.NewInProcBus(s.logger)
	ctx := gatedTeamCtx("t1")

	rec := emitRequest(t, s, ctx, `{"kind":"probe"}`)
	if rec.Code != 429 || !strings.Contains(rec.Body.String(), denyConcurrencyCap) {
		t.Fatalf("emit = %d %s, want 429 %s", rec.Code, rec.Body.String(), denyConcurrencyCap)
	}
}

// N matching subscriptions = N launches = N metered slots. Driven through the
// evaluator directly (the bus worker is asynchronous; the metering claim is
// about the fan-out, not about delivery timing).
func TestEmitFanOut_MetersOneSlotPerLaunch(t *testing.T) {
	pub := &spinePublisher{}
	s := newGatedSpineServer(t, gateSpec{id: "t1", orgRunQuota: 5}, pub)
	counter := orgusage.NewMemoryCounter()
	s.orgUsage = counter
	subs := trigger.NewMemorySubscriptionStore()
	for _, id := range []string{"sub-a", "sub-b"} {
		if err := subs.Create(context.Background(), trigger.Subscription{
			ID: id, TenantID: "t1", BotID: "probe",
			Invocation: bundle.InvocationKindBoard, Mode: bundle.ExecutionDirect,
			Match:   trigger.Matcher{Sources: []trigger.Source{trigger.SourceCustom}},
			Enabled: true, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	eval := trigger.NewEvaluator(subs, trigger.WithLauncher(s.triggerLauncher()))

	ev := trigger.Event{ID: "custom:probe:1", TenantID: "t1", Source: trigger.SourceCustom, Kind: "probe", OccurredAt: time.Now().UTC()}
	if err := eval.Handle(spineCtx(t), ev); err != nil {
		t.Fatalf("Handle = %v, want nil", err)
	}
	if got := pub.count(); got != 2 {
		t.Fatalf("the fan-out launched %d run(s), want 2", got)
	}
	u, _ := counter.Usage(context.Background(), "t1", time.Now().UTC())
	if u.Runs != 2 {
		t.Errorf("monthly runs = %d for a fan-out of 2 launches, want 2", u.Runs)
	}
}

// gatedTeamCtx is the request context of a signed-in member of the seeded
// team — what the gate reads to find the org and its caps.
func gatedTeamCtx(id string) context.Context {
	return auth.WithIdentity(context.Background(), auth.Identity{UserID: "u1", TeamID: id, OrgID: id})
}
