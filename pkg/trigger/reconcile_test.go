package trigger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

func reconcileBot(name string, invs ...bundle.Invocation) ManifestBot {
	return ManifestBot{Name: name, Invocations: invs}
}

func scheduleInv(cron string) bundle.Invocation {
	return bundle.Invocation{Kind: bundle.InvocationKindSchedule, Schedule: &bundle.InvocationSchedule{SuggestedCron: cron}}
}

// reconcile runs on tenant t1: the Mongo twin serves real tenants only (an
// empty tenant_id is an absent field there).
func reconcile(t *testing.T, st SubscriptionStore, bots ...ManifestBot) ReconcileReport {
	t.Helper()
	rep, err := ReconcileManifests(context.Background(), st, "t1", bots, time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func scheduleID(bot string, cron string) string {
	return DerivedSubscriptionID("t1", bot, bundle.InvocationKindSchedule, cron, "")
}

// Two invocations of one kind derive two rows, under ids a second
// reconciliation finds again without a write.
func TestReconcileDerivesStableRows(t *testing.T) {
	onSubscriptionTwins(t, func(t *testing.T, st SubscriptionStore) {
		bot := reconcileBot("reporter", scheduleInv("0 7 * * 1"), scheduleInv("0 9 * * 5"))
		if rep := reconcile(t, st, bot); rep.Created != 2 {
			t.Fatalf("first reconciliation = %+v, want 2 created", rep)
		}
		if rep := reconcile(t, st, bot); rep != (ReconcileReport{}) {
			t.Fatalf("an unchanged manifest reconciled to %+v, want no write", rep)
		}
		for _, cron := range []string{"0 7 * * 1", "0 9 * * 5"} {
			got, err := st.Get(context.Background(), scheduleID("reporter", cron))
			if err != nil || got.Cron != cron || got.Origin != ManifestOriginPrefix+"reporter" {
				t.Fatalf("schedule %q = %+v, %v", cron, got, err)
			}
		}
	})
}

// A manifest change that keeps the invocation's identity (its cron) reaches
// the row; the operator's choices on it stay. A changed cron is another
// invocation: its row is retired and the new one derives enabled, without
// the old row's choices.
func TestReconcileRefreshesTheManifestAndKeepsTheOperator(t *testing.T) {
	onSubscriptionTwins(t, func(t *testing.T, st SubscriptionStore) {
		ctx := context.Background()
		reconcile(t, st, reconcileBot("reporter", scheduleInv("0 7 * * 1")))
		id := scheduleID("reporter", "0 7 * * 1")
		cur, err := st.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		cur.Enabled = false
		cur.KeyOverrides = map[string]string{"anthropic": "key-1"}
		cur.Guard = "test -f ready"
		if err := st.Update(ctx, cur); err != nil {
			t.Fatal(err)
		}
		changed := scheduleInv("0 7 * * 1")
		changed.Schedule.DefaultVars = map[string]string{"window": "7 days"}
		if rep := reconcile(t, st, reconcileBot("reporter", changed)); rep.Refreshed != 1 {
			t.Fatalf("a changed cadence reconciled to %+v, want 1 refreshed", rep)
		}
		got, _ := st.Get(ctx, id)
		if len(got.Vars) != 1 || got.Vars["window"] != "7 days" {
			t.Errorf("vars = %v, want the manifest's", got.Vars)
		}
		if got.Enabled || got.KeyOverrides["anthropic"] != "key-1" || got.Guard != "test -f ready" {
			t.Errorf("the refresh lost the operator's choices: enabled=%v pins=%v guard=%q", got.Enabled, got.KeyOverrides, got.Guard)
		}

		// A changed cron is another invocation: retired, the new one derives
		// enabled, nothing carried over.
		if rep := reconcile(t, st, reconcileBot("reporter", scheduleInv("30 6 * * 1"))); rep.Created != 1 || rep.Removed != 1 {
			t.Fatalf("a changed cron reconciled to %+v, want one created and one removed", rep)
		}
		if _, err := st.Get(ctx, id); !errors.Is(err, ErrSubscriptionNotFound) {
			t.Fatalf("the old cron's row stayed: %v", err)
		}
		next, _ := st.Get(ctx, scheduleID("reporter", "30 6 * * 1"))
		if !next.Enabled || next.KeyOverrides != nil {
			t.Fatalf("the new invocation inherited %+v", next)
		}
	})
}

// An operator's delete holds; an invocation that left its manifest takes
// its row with it, leaving no tombstone behind; a bot discovery did not
// return keeps its rows.
func TestReconcileHonoursDeletesAndDepartures(t *testing.T) {
	onSubscriptionTwins(t, func(t *testing.T, st SubscriptionStore) {
		ctx := context.Background()
		two := reconcileBot("reporter", scheduleInv("0 7 * * 1"), scheduleInv("0 9 * * 5"))
		other := reconcileBot("watcher", scheduleInv("*/5 * * * *"))
		// The operator's own row for a discovered bot is never the
		// manifest's to remove.
		if err := st.Create(ctx, Subscription{ID: "op-1", TenantID: "t1", BotID: "reporter", Enabled: true}); err != nil {
			t.Fatal(err)
		}
		reconcile(t, st, two, other)

		deleted := scheduleID("reporter", "0 7 * * 1")
		if err := st.Tombstone(ctx, Tombstone{ID: deleted, TenantID: "t1", DeletedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if err := st.Delete(ctx, deleted); err != nil {
			t.Fatal(err)
		}
		reconcile(t, st, two, other)
		if _, err := st.Get(ctx, deleted); !errors.Is(err, ErrSubscriptionNotFound) {
			t.Fatalf("a reconciliation re-created a row the operator deleted: %v", err)
		}

		// reporter's second schedule leaves its manifest; watcher is not
		// discovered at all this time.
		one := reconcileBot("reporter", scheduleInv("0 7 * * 1"))
		if rep := reconcile(t, st, one); rep.Removed != 1 {
			t.Fatalf("a departed invocation reconciled to %+v, want 1 removed", rep)
		}
		if _, err := st.Get(ctx, scheduleID("reporter", "0 9 * * 5")); !errors.Is(err, ErrSubscriptionNotFound) {
			t.Fatalf("the departed invocation's row stayed: %v", err)
		}
		if _, err := st.Get(ctx, scheduleID("watcher", "*/5 * * * *")); err != nil {
			t.Fatalf("an undiscovered bot lost its row: %v", err)
		}
		if _, err := st.Get(ctx, "op-1"); err != nil {
			t.Fatalf("a reconciliation removed the operator's own row: %v", err)
		}
		if rep := reconcile(t, st, two); rep.Created != 1 {
			t.Fatalf("the invocation coming back reconciled to %+v, want it created again", rep)
		}
	})
}

// No connector subscription is derived, refreshed or removed by a
// reconciliation, and a mode no effect honours derives nothing.
func TestReconcileLeavesConnectorsAndRefusedModesAlone(t *testing.T) {
	onSubscriptionTwins(t, func(t *testing.T, st SubscriptionStore) {
		ctx := context.Background()
		conn := bundle.Invocation{Kind: bundle.InvocationKindConnector, Connector: &bundle.InvocationConnector{Trigger: "slack.message.posted"}}
		bound := DerivedSubscriptionID("t1", "chatter", bundle.InvocationKindConnector, "slack.message.posted", "conn-a")
		if err := st.Create(ctx, Subscription{ID: bound, TenantID: "t1", BotID: "chatter", Invocation: bundle.InvocationKindConnector,
			ConnectionID: "conn-a", TriggerID: "slack.message.posted", Enabled: true}); err != nil {
			t.Fatal(err)
		}
		deliver := scheduleInv("0 7 * * 1")
		deliver.Mode = bundle.ExecutionDeliver
		if rep := reconcile(t, st, reconcileBot("chatter", conn, deliver)); rep != (ReconcileReport{}) {
			t.Fatalf("reconciled to %+v, want nothing written", rep)
		}
		if _, err := st.Get(ctx, bound); err != nil {
			t.Fatalf("a reconciliation removed a bound connector subscription: %v", err)
		}
		if rows, _ := st.ListByTenant(ctx, "t1"); len(rows) != 1 {
			t.Fatalf("a reconciliation derived %d row(s), want the bound connector's alone", len(rows))
		}
	})
}

// A reordered manifest keeps the operator's gestures: the ids are intrinsic
// to what each invocation says, so an insertion moves none of them — the
// paused row stays paused, the deleted one stays dead.
func TestAReorderedManifestKeepsTheOperatorGestures(t *testing.T) {
	onSubscriptionTwins(t, func(t *testing.T, st SubscriptionStore) {
		ctx := context.Background()
		a, b, c := "0 7 * * 1", "0 9 * * 5", "0 6 * * 2"
		reconcile(t, st, reconcileBot("reporter", scheduleInv(a), scheduleInv(b)))
		idB := scheduleID("reporter", b)
		if err := st.Tombstone(ctx, Tombstone{ID: idB, TenantID: "t1", DeletedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if err := st.Delete(ctx, idB); err != nil {
			t.Fatal(err)
		}
		cur, err := st.Get(ctx, scheduleID("reporter", a))
		if err != nil {
			t.Fatal(err)
		}
		cur.Enabled = false
		if err := st.Update(ctx, cur); err != nil {
			t.Fatal(err)
		}

		// The author inserts C before the two; nothing of the operator moves.
		reconcile(t, st, reconcileBot("reporter", scheduleInv(c), scheduleInv(a), scheduleInv(b)))
		gotA, err := st.Get(ctx, scheduleID("reporter", a))
		if err != nil || gotA.Enabled || gotA.Cron != a {
			t.Fatalf("A after the insertion = %+v, %v; want still paused on its own cron", gotA, err)
		}
		if _, err := st.Get(ctx, idB); !errors.Is(err, ErrSubscriptionNotFound) {
			t.Fatalf("the deleted invocation came back: %v", err)
		}
		if gotC, err := st.Get(ctx, scheduleID("reporter", c)); err != nil || !gotC.Enabled {
			t.Fatalf("C = %+v, %v; want created enabled", gotC, err)
		}
	})
}

// The cron of a row the operator enabled is the operator's own: the
// manifest's suggestion never rewrites it, however the manifest changes.
func TestTheCronOverrideSurvivesTheReconciliation(t *testing.T) {
	onSubscriptionTwins(t, func(t *testing.T, st SubscriptionStore) {
		ctx := context.Background()
		reconcile(t, st, reconcileBot("reporter", scheduleInv("0 7 * * 1")))
		cur, err := st.Get(ctx, scheduleID("reporter", "0 7 * * 1"))
		if err != nil {
			t.Fatal(err)
		}
		cur.Origin = BotHomeOrigin
		cur.Cron = "0 8 * * 1"
		if err := st.Update(ctx, cur); err != nil {
			t.Fatal(err)
		}
		reconcile(t, st, reconcileBot("reporter", scheduleInv("0 7 * * 1")))
		got, _ := st.Get(ctx, scheduleID("reporter", "0 7 * * 1"))
		if got.Cron != "0 8 * * 1" {
			t.Fatalf("cron = %q after the reconciliation, want the operator's override", got.Cron)
		}
	})
}
