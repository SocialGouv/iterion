package trigger

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// stubBindings answers the ONE question the projection arm asks of the forge
// layer: does this tenant have a board binding?
type stubBindings struct {
	bound map[string]bool
	err   error
}

func (s *stubBindings) HasBoardBinding(_ context.Context, tenantID string) (bool, error) {
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

// TestMaterializeEffects_MachineCausedOwesNoProjection (#798): a move iterion
// made on its own authority — a watchdog park, a column rename, a card given
// back after a failed launch — is not a move the external roadmap follows,
// so no projection row is owed for it. Declined at materialization for the
// reason launches are: a schema migration emits one event per card, and a
// column of rows that each retire with nothing to write sits FIFO ahead of
// the next genuine reflect. The launch rows stay declined too.
func TestMaterializeEffects_MachineCausedOwesNoProjection(t *testing.T) {
	for _, reason := range []string{tracker.ReasonWatchdog, tracker.ReasonStateRename, tracker.ReasonUnlaunchable} {
		t.Run(reason, func(t *testing.T) {
			ev := movedEvent()
			ev.Payload = map[string]any{"reason": reason}
			if !machineCaused(ev) {
				t.Fatalf("fixture is not machine-caused — reason %q", reason)
			}
			rows, err := MaterializeEffects(context.Background(), projectionSubs(t),
				&stubBindings{bound: map[string]bool{"t1": true}}, ev, time.Now().UTC())
			if err != nil {
				t.Fatalf("materialize: %v", err)
			}
			if len(rows) != 0 {
				t.Fatalf("machine-caused move materialized %+v, want no row at all — not a launch, not a projection", rows)
			}
		})
	}
	// A DESCRIPTIVE reason is a gesture (or a run's verdict) and still owes
	// its projection: the watchdog filing a finished run's `done` is what
	// the roadmap must show.
	ev := movedEvent()
	ev.Payload = map[string]any{"reason": tracker.ReasonRunFinished}
	rows, err := MaterializeEffects(context.Background(), projectionSubs(t),
		&stubBindings{bound: map[string]bool{"t1": true}}, ev, time.Now().UTC())
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	var proj int
	for _, r := range rows {
		if r.IsProjection() {
			proj++
		}
	}
	if proj != 1 {
		t.Fatalf("a run-verdict move materialized %d projection row(s), want 1 (%+v)", proj, rows)
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

// stubProjection records the reflects the worker asked for.
type stubProjection struct {
	cards []string
	errs  []error // popped per call; empty → nil
}

func (s *stubProjection) ReflectCard(_ context.Context, ev Event) error {
	s.cards = append(s.cards, ev.Subject.ID)
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.errs = s.errs[1:]
		return err
	}
	return nil
}

// noSubsStore fails any subscription read. A projection row is owed to the
// tenant's BINDING; consulting the subscription registry for one would be the
// pseudo-subscription shape coming back in through the worker.
type noSubsStore struct{ SubscriptionStore }

func (noSubsStore) Get(context.Context, string) (Subscription, error) {
	return Subscription{}, errors.New("the projection arm must not read the subscription store")
}

// projectionWorld drives one projection row end to end through the real
// outbox + worker + evaluator.
func projectionWorld(t *testing.T, ev Event, proj ProjectionEffect) (*EffectWorker, *MemoryEffectOutbox) {
	t.Helper()
	ctx := context.Background()
	subs := NewMemorySubscriptionStore()
	eval := NewEvaluator(subs, WithProjectionEffect(proj))
	out := NewMemoryEffectOutbox()
	rows, err := MaterializeEffects(ctx, subs, &stubBindings{bound: map[string]bool{"t1": true}}, ev, time.Now().UTC())
	if err != nil || len(rows) != 1 || !rows[0].IsProjection() {
		t.Fatalf("materialize: rows=%+v err=%v", rows, err)
	}
	if err := out.UpsertPending(ctx, rows); err != nil {
		t.Fatal(err)
	}
	return &EffectWorker{Outbox: out, Subs: noSubsStore{subs}, Evaluator: eval}, out
}

// TestEffectWorker_ProjectionReflectsAndCompletes: the row executes through
// the projection arm, reaches the reflect with its event, and retires — with
// the subscription store never consulted.
func TestEffectWorker_ProjectionReflectsAndCompletes(t *testing.T) {
	ev := movedEvent()
	proj := &stubProjection{}
	w, out := projectionWorld(t, ev, proj)
	if n := w.Tick(context.Background(), 10); n != 1 {
		t.Fatalf("tick acted on %d rows, want 1", n)
	}
	if len(proj.cards) != 1 || proj.cards[0] != "card1" {
		t.Fatalf("reflected %v, want [card1]", proj.cards)
	}
	row, _ := out.Row(ProjectionEffectID(ev.ID))
	if row.State != EffectDone {
		t.Fatalf("projection row state = %q, want %q", row.State, EffectDone)
	}
}

// TestEffectWorker_ProjectionArmStillExecutesAQueuedRow: the projection arm
// does not re-derive the machine decline from the event — a row that IS in
// the outbox reaches the reflect, which judges the CARD's persisted
// provenance (the one authority both callers share). A row queued by an
// older binary for a machine move therefore retires through the reflect's
// own refusal, never as a dead-letter.
func TestEffectWorker_ProjectionArmStillExecutesAQueuedRow(t *testing.T) {
	ev := movedEvent()
	proj := &stubProjection{}
	w, out := projectionWorld(t, ev, proj)
	// Stamp the provenance AFTER materialization: the row exists; only its
	// execution is under test.
	row, _ := out.Row(ProjectionEffectID(ev.ID))
	row.Event.Payload = map[string]any{"reason": tracker.ReasonWatchdog}
	if err := out.UpsertPending(context.Background(), []EffectRow{row}); err != nil {
		t.Fatal(err)
	}
	w.Tick(context.Background(), 10)
	if len(proj.cards) != 1 {
		t.Fatalf("a queued projection row reached the reflect %d times, want 1 — the arm hands every row to the reflect, which decides on the card", len(proj.cards))
	}
	if row, _ := out.Row(ProjectionEffectID(ev.ID)); row.State != EffectDone {
		t.Fatalf("projection row state = %q, want %q", row.State, EffectDone)
	}
}

// TestEffectWorker_ProjectionRetriesThenDeadLetters: the reflect inherits the
// outbox's bounded retry and visible dead-letter for free — a forge 403 must
// not vanish, and must not retry forever either.
func TestEffectWorker_ProjectionRetriesThenDeadLetters(t *testing.T) {
	ev := movedEvent()
	boom := errors.New("forge refused the status write")
	errs := make([]error, MaxEffectAttempts)
	for i := range errs {
		errs[i] = boom
	}
	proj := &stubProjection{errs: errs}
	w, out := projectionWorld(t, ev, proj)
	now := time.Now().UTC()
	w.Now = func() time.Time { return now }
	for i := 0; i < MaxEffectAttempts; i++ {
		w.Tick(context.Background(), 10)
		now = now.Add(EffectBackoff(MaxEffectAttempts) + time.Second)
	}
	row, _ := out.Row(ProjectionEffectID(ev.ID))
	if row.State != EffectFailed {
		t.Fatalf("after %d failing attempts the row is %q, want %q (a visible dead-letter)", MaxEffectAttempts, row.State, EffectFailed)
	}
	if row.LastError == "" {
		t.Fatal("dead-lettered projection row carries no last_error — the operator has nothing to read")
	}
}

// TestEffectWorker_DeadLetteredProjectionNamesItself: the worker's warnings
// are the ONLY readout of a parked row — nothing lists the outbox. A line
// naming a blank subscription is what an operator would get for every
// projection, so the row describes itself by kind instead.
func TestEffectWorker_DeadLetteredProjectionNamesItself(t *testing.T) {
	ev := movedEvent()
	errs := make([]error, MaxEffectAttempts)
	for i := range errs {
		errs[i] = errors.New("forge refused the status write")
	}
	var logs bytes.Buffer
	w, _ := projectionWorld(t, ev, &stubProjection{errs: errs})
	w.Logger = iterlog.New(iterlog.LevelWarn, &logs)
	now := time.Now().UTC()
	w.Now = func() time.Time { return now }
	for i := 0; i < MaxEffectAttempts; i++ {
		w.Tick(context.Background(), 10)
		now = now.Add(EffectBackoff(MaxEffectAttempts) + time.Second)
	}
	out := logs.String()
	if !strings.Contains(out, EffectKindProjection) {
		t.Fatalf("the dead-letter warning never names the projection:\n%s", out)
	}
	if strings.Contains(out, "sub )") || strings.Contains(out, "sub , ") {
		t.Fatalf("the warning names a blank subscription for a row that has none:\n%s", out)
	}
}

// TestEffectWorker_LaunchWarningStillNamesItsSubscription: the same lines must
// keep naming the subscription for a launch row — that id is how an operator
// finds the trigger behind a parked effect.
func TestEffectWorker_LaunchWarningStillNamesItsSubscription(t *testing.T) {
	var logs bytes.Buffer
	l := &stubEffectLauncher{errs: make([]error, MaxEffectAttempts)}
	for i := range l.errs {
		l.errs[i] = errors.New("launch refused")
	}
	w, _, _ := effectWorld(t, directConsumeSub(), &stubConsumingBoard{consumeLeft: MaxEffectAttempts}, l)
	w.Logger = iterlog.New(iterlog.LevelWarn, &logs)
	now := time.Now().UTC()
	w.Now = func() time.Time { return now }
	for i := 0; i < MaxEffectAttempts; i++ {
		w.Tick(context.Background(), 10)
		now = now.Add(EffectBackoff(MaxEffectAttempts) + time.Second)
	}
	if out := logs.String(); !strings.Contains(out, "sub s1") {
		t.Fatalf("a launch row's warning no longer names its subscription:\n%s", out)
	}
}

// TestEffectWorker_ProjectionWithNoEffectWiredIsExplicit: a projection row can
// only exist where a binding does, so an unwired effect is a WIRING bug. It
// must say so, not retire quietly leaving the board silently unreflected.
func TestEffectWorker_ProjectionWithNoEffectWiredIsExplicit(t *testing.T) {
	ev := movedEvent()
	w, out := projectionWorld(t, ev, nil)
	w.Tick(context.Background(), 10)
	row, _ := out.Row(ProjectionEffectID(ev.ID))
	if row.State == EffectDone {
		t.Fatal("a projection row with no projection effect wired was retired as done — the reflect is silently lost")
	}
	if row.LastError == "" {
		t.Fatal("no last_error recorded for an unwired projection effect")
	}
}
