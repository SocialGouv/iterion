package trigger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// stubBindings answers the ONE question the projection arm asks of the forge
// layer: does this tenant have a board binding?
type stubBindings struct {
	bound map[string]bool
	err   error
	calls int
}

func (s *stubBindings) HasBoardBinding(_ context.Context, tenantID string) (bool, error) {
	s.calls++
	if s.err != nil {
		return false, s.err
	}
	return s.bound[tenantID], nil
}

func movedEvent() Event {
	return Event{
		ID: "board:b:card1:7", Source: SourceBoard, Kind: KindCardMoved,
		TenantID: "t1", Subject: Subject{Type: "card", ID: "card1", State: "in_progress"},
		Labels: []string{"triage:auto"}, Payload: map[string]any{"from_state": "ready"},
	}
}

func projectionSubs(t *testing.T) SubscriptionStore {
	t.Helper()
	subs := NewMemorySubscriptionStore()
	if err := subs.Create(context.Background(), directConsumeSub()); err != nil {
		t.Fatal(err)
	}
	return subs
}

// TestMaterializeEffects_ProjectionForABoundTenant: a bound tenant's card move
// owes a projection row ON TOP OF whatever subscriptions matched — the two are
// independent, and the projection names no subscription.
func TestMaterializeEffects_ProjectionForABoundTenant(t *testing.T) {
	subs := projectionSubs(t)
	binds := &stubBindings{bound: map[string]bool{"t1": true}}
	ev := movedEvent()
	rows, err := MaterializeEffects(context.Background(), subs, binds, ev, time.Now().UTC())
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	var proj, launch int
	for _, r := range rows {
		if r.IsProjection() {
			proj++
			if r.SubID != "" {
				t.Fatalf("projection row carries sub_id %q", r.SubID)
			}
			if r.ID != ProjectionEffectID(ev.ID) {
				t.Fatalf("projection row id = %q, want %q", r.ID, ProjectionEffectID(ev.ID))
			}
			if r.TenantID != "t1" || r.Event.Subject.ID != "card1" {
				t.Fatalf("projection row lost its tenant/card: %+v", r)
			}
			continue
		}
		launch++
	}
	if proj != 1 || launch != 1 {
		t.Fatalf("rows: projection=%d launch=%d, want 1 and 1 (%+v)", proj, launch, rows)
	}
}

// TestMaterializeEffects_ProjectionOnlyWhereItIsOwED covers the three
// declines, each for its own reason.
func TestMaterializeEffects_ProjectionOnlyWhereItIsOwed(t *testing.T) {
	unmoved := movedEvent()
	unmoved.Kind = KindCardUpdated
	noCard := movedEvent()
	noCard.Subject.ID = ""
	noTenant := movedEvent()
	noTenant.TenantID = ""

	for _, tc := range []struct {
		name  string
		binds ProjectionBindings
		ev    Event
	}{
		{"tenant with no board binding", &stubBindings{bound: map[string]bool{}}, movedEvent()},
		{"no bindings oracle wired (local, unbound)", nil, movedEvent()},
		{"not a state transition", &stubBindings{bound: map[string]bool{"t1": true}}, unmoved},
		{"no card to reflect", &stubBindings{bound: map[string]bool{"t1": true}}, noCard},
		{"no tenant to resolve a binding for", &stubBindings{bound: map[string]bool{"": true}}, noTenant},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := MaterializeEffects(context.Background(), projectionSubs(t), tc.binds, tc.ev, time.Now().UTC())
			if err != nil {
				t.Fatalf("materialize: %v", err)
			}
			for _, r := range rows {
				if r.IsProjection() {
					t.Fatalf("a projection row was materialized anyway: %+v", r)
				}
			}
		})
	}
}

// TestMaterializeEffects_ProjectionExemptFromTheMachineDecline is the point of
// the whole exercise: the machine-caused decline guards LAUNCH admission (a
// column rename must not mass-launch), and a watchdog filing a card in
// `blocked` is EXACTLY what the external roadmap has to show. The launch rows
// stay declined; the projection row is owed.
func TestMaterializeEffects_ProjectionExemptFromTheMachineDecline(t *testing.T) {
	ev := movedEvent()
	ev.Payload = map[string]any{"reason": tracker.ReasonWatchdog}
	if !machineCaused(ev) {
		t.Fatalf("fixture is not machine-caused — reason %q", ev.Payload["reason"])
	}
	rows, err := MaterializeEffects(context.Background(), projectionSubs(t),
		&stubBindings{bound: map[string]bool{"t1": true}}, ev, time.Now().UTC())
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(rows) != 1 || !rows[0].IsProjection() {
		t.Fatalf("machine-caused move materialized %+v, want exactly one projection row and no launch", rows)
	}
}

// TestMaterializeEffects_BindingLookupFailureAbortsTheBatch: the cloud tail
// materializes BEFORE advancing its cursor, so a swallowed store error would
// silently drop the projection for an event nothing replays. It must surface.
func TestMaterializeEffects_BindingLookupFailureAbortsTheBatch(t *testing.T) {
	boom := errors.New("mongo unreachable")
	rows, err := MaterializeEffects(context.Background(), projectionSubs(t),
		&stubBindings{err: boom}, movedEvent(), time.Now().UTC())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the store failure wrapped — a silent skip loses the reflect for good", err)
	}
	if rows != nil {
		t.Fatalf("rows returned alongside the error: %+v", rows)
	}
}

// TestMaterializeEffects_ProjectionIsNotASubscription pins the shape ADR-097
// §10 rejected: the projection must reach the outbox WITHOUT existing in the
// subscription registry, so no /api/v1/triggers listing shows it and no
// operator DELETE can silently kill the reflect.
func TestMaterializeEffects_ProjectionIsNotASubscription(t *testing.T) {
	ctx := context.Background()
	subs := NewMemorySubscriptionStore() // deliberately EMPTY
	rows, err := MaterializeEffects(ctx, subs, &stubBindings{bound: map[string]bool{"t1": true}}, movedEvent(), time.Now().UTC())
	if err != nil || len(rows) != 1 || !rows[0].IsProjection() {
		t.Fatalf("materialize with no subscriptions: rows=%+v err=%v, want one projection row", rows, err)
	}
	listed, err := subs.ListByTenant(ctx, "t1")
	if err != nil {
		t.Fatalf("list subscriptions: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("materializing a projection created %d subscription(s) — an operator could delete the reflect", len(listed))
	}
	if rows[0].Kind != EffectKindProjection || rows[0].State != EffectPending {
		t.Fatalf("projection row = %+v, want a pending projection", rows[0])
	}
}

