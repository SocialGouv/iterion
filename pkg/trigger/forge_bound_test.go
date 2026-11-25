package trigger

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// forgeRow is a subscription bound to an integration the way its projector
// writes it: the binding and the origin of one integration, a matcher over
// the labels a forge event added.
func forgeRow(id, tenantID, botID, integrationID string, labels ...string) Subscription {
	return Subscription{
		ID: id, TenantID: tenantID, BotID: botID, Invocation: bundle.InvocationKindForge, Mode: bundle.ExecutionDirect,
		Enabled:      true,
		ForgeBinding: ForgeBindingForIntegration(integrationID),
		Origin:       ForgeOrigin(integrationID),
		Match: Matcher{
			Sources: []Source{SourceForge}, Kinds: []string{bundle.ForgeEventIssueLabeled}, Actions: []string{ForgeIssueAction},
			AddedLabels: labels,
		},
	}
}

// labelEvent is the event a forge router emits when labels are added to an
// issue.
func labelEvent(tenantID string, added ...string) Event {
	return Event{
		ID: "forge:github:integration:int-1:delivery-1", Source: SourceForge, Kind: bundle.ForgeEventIssueLabeled, Action: ForgeIssueAction,
		TenantID: tenantID, Repo: "acme/api", Subject: Subject{Type: "issue", ID: "42", Title: "Add export"},
		AddedLabels: added, Payload: map[string]any{PayloadForgeKind: "issues"},
	}
}

// handedStore answers fixed rows whatever the event or the binding asks, so
// a test reads the prelude's own verdict and never a store filter standing
// in front of it. Its rows are answered in the order given.
type handedStore struct {
	*MemorySubscriptionStore
	candidates []Subscription
	bound      []Subscription
	err        error
}

func (h handedStore) ListCandidates(context.Context, Event) ([]Subscription, error) {
	return h.candidates, h.err
}

func (h handedStore) ListByForgeBinding(context.Context, string, string) ([]Subscription, error) {
	return h.bound, h.err
}

func subscriptionIDs(subs []Subscription) []string {
	ids := []string{}
	for _, s := range subs {
		ids = append(ids, s.ID)
	}
	return ids
}

// Whatever a store answers, the evaluator fires no subscription bound to a
// forge binding from the bus — on the synchronous path or through the effect
// outbox, launched or promoted. The store's own exclusion is taken out of
// the way: the candidates are handed over as they are.
func TestEvaluator_NeverFiresForgeBoundSubscription(t *testing.T) {
	ev := labelEvent("", "nestor")
	for _, mode := range []bundle.ExecutionMode{bundle.ExecutionDirect, bundle.ExecutionBoard} {
		t.Run(string(mode), func(t *testing.T) {
			bound := forgeRow("forge", "", "nestor", "int-1", "nestor")
			bound.Mode = mode
			if !bound.Enabled || !bound.Match.Match(ev) {
				t.Fatal("the fixture row must be one the event would fire were it not bound")
			}
			st := unscopedCandidates{seed(t, bound)}
			if got := candidateIDs(t, st, ev); !reflect.DeepEqual(got, []string{"forge"}) {
				t.Fatalf("the fake store hands %v to the evaluator, want the bound row", got)
			}
			fl, fb := &fakeLauncher{}, &fakeBoard{}
			if err := NewEvaluator(st, WithLauncher(fl), WithBoardEffect(fb)).Handle(context.Background(), ev); err != nil {
				t.Fatal(err)
			}
			if len(fl.plans) != 0 || len(fb.plans) != 0 {
				t.Fatalf("a bus event fired a forge-bound subscription: launched %+v, promoted %+v", fl.plans, fb.plans)
			}
			rows, err := MaterializeEffects(context.Background(), st, nil, ev, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 0 {
				t.Fatalf("the outbox owes a forge-bound subscription %d effect(s): %+v", len(rows), rows)
			}

			// The control: the same row with no binding fires, so the silence
			// above is the binding's.
			free := bound
			free.ID, free.ForgeBinding, free.Origin = "free", "", ""
			st = unscopedCandidates{seed(t, free)}
			fl, fb = &fakeLauncher{}, &fakeBoard{}
			if err := NewEvaluator(st, WithLauncher(fl), WithBoardEffect(fb)).Handle(context.Background(), ev); err != nil {
				t.Fatal(err)
			}
			if len(fl.plans)+len(fb.plans) != 1 {
				t.Fatalf("the unbound control fired %d time(s), want 1", len(fl.plans)+len(fb.plans))
			}
		})
	}
}

// The bus path, the outbox materialization and the forge router decline the
// same events at one shared prelude: an event one of them does not evaluate
// is evaluated by none. Each caller is handed a row the event would fire.
func TestMatchForgeBound_SharesTheAdmissionPrelude(t *testing.T) {
	if !tracker.IsMachineReason(tracker.ReasonWatchdog) {
		t.Fatal("the fixture needs a machine reason")
	}
	with := func(ev Event, key string, value any) Event {
		ev.Payload = map[string]any{PayloadForgeKind: "issues", key: value}
		return ev
	}
	connector := labelEvent("", "nestor")
	connector.Source = SourceConnector
	for _, tc := range []struct {
		name     string
		ev       Event
		admitted bool
	}{
		{name: "an ordinary event", ev: labelEvent("", "nestor"), admitted: true},
		{name: "a machine-caused event", ev: with(labelEvent("", "nestor"), "reason", tracker.ReasonWatchdog)},
		{name: "an event an authoritative path already launched", ev: with(labelEvent("", "nestor"), PayloadLaunchedRunID, "run-1")},
		{name: "an observational event", ev: with(labelEvent("", "nestor"), PayloadObservational, true)},
		{name: "a connector event", ev: connector},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Both rows match on the added label alone, so no dimension of
			// their own stands between the event and the prelude.
			bus := Subscription{ID: "bus", BotID: "bus-bot", Mode: bundle.ExecutionDirect, Enabled: true, Match: Matcher{AddedLabels: []string{"nestor"}}}
			bound := forgeRow("bound", "", "nestor", "int-1", "nestor")
			bound.Match = Matcher{AddedLabels: []string{"nestor"}}
			st := handedStore{MemorySubscriptionStore: seed(t, bus, bound), candidates: []Subscription{bus}, bound: []Subscription{bound}}
			want := 0
			if tc.admitted {
				want = 1
			}

			fl := &fakeLauncher{}
			if err := NewEvaluator(st, WithLauncher(fl)).Handle(context.Background(), tc.ev); err != nil {
				t.Fatal(err)
			}
			if len(fl.plans) != want {
				t.Errorf("the bus path launched %d run(s), want %d", len(fl.plans), want)
			}

			rows, err := MaterializeEffects(context.Background(), st, nil, tc.ev, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != want {
				t.Errorf("the outbox owes %d effect(s), want %d", len(rows), want)
			}

			fm, err := MatchForgeBound(context.Background(), st, "", ForgeBindingForIntegration("int-1"), tc.ev)
			if err != nil {
				t.Fatal(err)
			}
			if fm.Admitted != tc.admitted {
				t.Errorf("the forge router admitted = %t, want %t", fm.Admitted, tc.admitted)
			}
			if len(fm.Matched) != want {
				t.Errorf("the forge router matched %v, want %d subscription(s)", subscriptionIDs(fm.Matched), want)
			}
			if len(fm.Verdicts) != want {
				t.Errorf("the forge router judged %d subscription(s), want %d: an event that is not admitted is evaluated against none", len(fm.Verdicts), want)
			}
			if fm.Bound != 1 {
				t.Errorf("Bound = %d, want 1: what the binding holds does not depend on the event", fm.Bound)
			}
		})
	}
}

// The per-row half of the prelude is shared like the admission half: a row
// that is disabled, or whose matcher refuses the event, is claimed on no
// path. The candidates are handed over as they are, so the store's own
// enabled filter does not answer for the prelude.
func TestMatchForgeBound_SharesThePerRowPredicate(t *testing.T) {
	ev := labelEvent("", "nestor")
	for _, tc := range []struct {
		name   string
		edit   func(*Subscription)
		failed string
		claims bool
	}{
		{name: "an enabled row its matcher accepts", edit: func(*Subscription) {}, claims: true},
		{name: "a disabled row", edit: func(s *Subscription) { s.Enabled = false }, failed: "disabled"},
		{name: "a row whose matcher refuses the event", edit: func(s *Subscription) { s.Match = Matcher{AddedLabels: []string{"saboteur"}} },
			failed: "added_labels: wants [saboteur] got [nestor]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bus := Subscription{ID: "bus", BotID: "bus-bot", Mode: bundle.ExecutionDirect, Enabled: true, Match: Matcher{AddedLabels: []string{"nestor"}}}
			bound := forgeRow("bound", "", "nestor", "int-1", "nestor")
			tc.edit(&bus)
			tc.edit(&bound)
			st := handedStore{MemorySubscriptionStore: seed(t, bus, bound), candidates: []Subscription{bus}, bound: []Subscription{bound}}
			want := 0
			if tc.claims {
				want = 1
			}

			fl := &fakeLauncher{}
			if err := NewEvaluator(st, WithLauncher(fl)).Handle(context.Background(), ev); err != nil {
				t.Fatal(err)
			}
			if len(fl.plans) != want {
				t.Errorf("the bus path launched %d run(s), want %d", len(fl.plans), want)
			}
			rows, err := MaterializeEffects(context.Background(), st, nil, ev, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != want {
				t.Errorf("the outbox owes %d effect(s), want %d", len(rows), want)
			}
			fm, err := MatchForgeBound(context.Background(), st, "", ForgeBindingForIntegration("int-1"), ev)
			if err != nil {
				t.Fatal(err)
			}
			if len(fm.Matched) != want {
				t.Errorf("the forge router matched %v, want %d subscription(s)", subscriptionIDs(fm.Matched), want)
			}
			if len(fm.Verdicts) != 1 || fm.Verdicts[0].Failed != tc.failed {
				t.Errorf("the forge router's verdict = %+v, want one row refused by %q", fm.Verdicts, tc.failed)
			}
		})
	}
}

// The router's answer names every subscription of the binding and why it
// did or did not claim the event; it reads the binding's rows only — not
// another integration's, not the tenant's unbound ones, not another
// tenant's.
func TestMatchForgeBound_ExplainsEveryBoundRow(t *testing.T) {
	ctx := context.Background()
	nestor := forgeRow("drv-2", "t1", "nestor", "int-1", "nestor")
	saboteur := forgeRow("drv-1", "t1", "saboteur", "int-1", "saboteur")
	parked := forgeRow("drv-3", "t1", "feature-dev", "int-1")
	parked.Enabled = false
	parked.DisabledReason = "declares issue_labeled with no label while 3 bots share acme/api"
	otherIntegration := forgeRow("drv-4", "t1", "nestor", "int-2", "nestor")
	otherTenant := forgeRow("drv-5", "t2", "nestor", "int-1", "nestor")
	unbound := Subscription{ID: "bus", TenantID: "t1", BotID: "bus-bot", Enabled: true}
	st := NewMemorySubscriptionStore()
	for _, sub := range []Subscription{nestor, saboteur, parked, otherIntegration, otherTenant, unbound} {
		if err := st.Create(ctx, sub); err != nil {
			t.Fatal(err)
		}
	}

	fm, err := MatchForgeBound(ctx, st, "t1", ForgeBindingForIntegration("int-1"), labelEvent("t1", "nestor"))
	if err != nil {
		t.Fatal(err)
	}
	if !fm.Admitted || fm.Bound != 3 {
		t.Fatalf("admitted = %t, bound = %d, want an admitted event against 3 bound rows", fm.Admitted, fm.Bound)
	}
	if got := subscriptionIDs(fm.Matched); !reflect.DeepEqual(got, []string{"drv-2"}) {
		t.Fatalf("matched %v, want nestor's row only", got)
	}
	want := []CandidateVerdict{
		{SubscriptionID: "drv-3", BotID: "feature-dev", Enabled: false, DisabledReason: parked.DisabledReason, Failed: "disabled"},
		{SubscriptionID: "drv-2", BotID: "nestor", Enabled: true},
		{SubscriptionID: "drv-1", BotID: "saboteur", Enabled: true, Failed: "added_labels: wants [saboteur] got [nestor]"},
	}
	if !reflect.DeepEqual(fm.Verdicts, want) {
		t.Fatalf("verdicts =\n%+v\nwant\n%+v", fm.Verdicts, want)
	}

	// A label nobody claims: every row is judged, none matches.
	fm, err = MatchForgeBound(ctx, st, "t1", ForgeBindingForIntegration("int-1"), labelEvent("t1", "bug"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fm.Matched) != 0 || len(fm.Verdicts) != 3 || fm.Bound != 3 {
		t.Fatalf("an unclaimed label matched %v with %d verdicts over %d bound rows", subscriptionIDs(fm.Matched), len(fm.Verdicts), fm.Bound)
	}

	// A binding nothing was projected onto holds nothing, whatever the
	// tenant's other subscriptions would claim.
	fm, err = MatchForgeBound(ctx, st, "t1", ForgeBindingForIntegration("int-9"), labelEvent("t1", "nestor"))
	if err != nil {
		t.Fatal(err)
	}
	if !fm.Admitted || fm.Bound != 0 || len(fm.Matched) != 0 {
		t.Fatalf("an unprojected binding answered %+v, want an admitted event against no row", fm)
	}

	// An empty binding names no integration: refused, never read as the
	// tenant's unbound subscriptions.
	fm, err = MatchForgeBound(ctx, st, "t1", "", labelEvent("t1", "nestor"))
	if !errors.Is(err, ErrEmptyForgeBinding) {
		t.Fatalf("an empty binding answered %+v, %v, want ErrEmptyForgeBinding", fm, err)
	}
	if fm.Bound != 0 || len(fm.Matched) != 0 {
		t.Fatalf("an empty binding selected rows: %+v", fm)
	}
}

// The matched subscriptions and their verdicts are ordered by bot, then by
// id, whatever order the store lists the binding's rows in.
func TestMatchForgeBound_OrdersByBotThenID_InBothOrders(t *testing.T) {
	// The id order (s1, s2, s3) is not the bot order, so neither listing
	// order below is already the answer.
	s1 := forgeRow("s1", "", "saboteur", "int-1", "chaos")
	s2 := forgeRow("s2", "", "nestor", "int-1", "chaos")
	s3 := forgeRow("s3", "", "nestor", "int-1", "*")
	want := []string{"s2", "s3", "s1"}
	for name, listed := range map[string][]Subscription{
		"listed by id":          {s1, s2, s3},
		"listed in the reverse": {s3, s2, s1},
	} {
		t.Run(name, func(t *testing.T) {
			handed := append([]Subscription(nil), listed...)
			st := handedStore{MemorySubscriptionStore: NewMemorySubscriptionStore(), bound: handed}
			fm, err := MatchForgeBound(context.Background(), st, "", ForgeBindingForIntegration("int-1"), labelEvent("", "chaos"))
			if err != nil {
				t.Fatal(err)
			}
			if got := subscriptionIDs(fm.Matched); !reflect.DeepEqual(got, want) {
				t.Errorf("matched %v, want %v", got, want)
			}
			var judged []string
			for _, v := range fm.Verdicts {
				judged = append(judged, v.SubscriptionID)
			}
			if !reflect.DeepEqual(judged, want) {
				t.Errorf("verdicts in order %v, want %v", judged, want)
			}
			if !reflect.DeepEqual(subscriptionIDs(handed), subscriptionIDs(listed)) {
				t.Errorf("the store's own listing was reordered to %v", subscriptionIDs(handed))
			}
		})
	}
}

// A store that cannot list the binding's rows fails the match: the router
// answers for an unreadable lane, never for an empty one.
func TestMatchForgeBound_ReturnsTheStoreError(t *testing.T) {
	boom := errors.New("store unavailable")
	st := handedStore{MemorySubscriptionStore: NewMemorySubscriptionStore(), err: boom}
	fm, err := MatchForgeBound(context.Background(), st, "", ForgeBindingForIntegration("int-1"), labelEvent("", "nestor"))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the store's", err)
	}
	if fm.Admitted || fm.Bound != 0 || fm.Matched != nil || fm.Verdicts != nil {
		t.Fatalf("a failed read answered %+v, want the zero match", fm)
	}
}
