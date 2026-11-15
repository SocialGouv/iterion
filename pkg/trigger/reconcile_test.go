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

func scheduleID(bot string, ordinal string) string {
	return DerivedSubscriptionID("t1", bot, bundle.InvocationKindSchedule, ordinal, "")
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
		for ordinal, cron := range map[string]string{"0": "0 7 * * 1", "1": "0 9 * * 5"} {
			got, err := st.Get(context.Background(), scheduleID("reporter", ordinal))
			if err != nil || got.Cron != cron || got.Origin != ManifestOriginPrefix+"reporter" {
				t.Fatalf("schedule %s = %+v, %v; want cron %q", ordinal, got, err, cron)
			}
		}
	})
}

// A manifest change reaches its row; the operator's choices on it stay.
func TestReconcileRefreshesTheManifestAndKeepsTheOperator(t *testing.T) {
	onSubscriptionTwins(t, func(t *testing.T, st SubscriptionStore) {
		ctx := context.Background()
		reconcile(t, st, reconcileBot("reporter", scheduleInv("0 7 * * 1")))
		id := scheduleID("reporter", "0")
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
		if rep := reconcile(t, st, reconcileBot("reporter", scheduleInv("30 6 * * 1"))); rep.Refreshed != 1 {
			t.Fatalf("a changed cron reconciled to %+v, want 1 refreshed", rep)
		}
		got, _ := st.Get(ctx, id)
		if got.Cron != "30 6 * * 1" {
			t.Errorf("the row kept cron %q, want the manifest's", got.Cron)
		}
		if got.Enabled || got.KeyOverrides["anthropic"] != "key-1" || got.Guard != "test -f ready" {
			t.Errorf("the refresh lost the operator's choices: enabled=%v pins=%v guard=%q", got.Enabled, got.KeyOverrides, got.Guard)
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

		deleted := scheduleID("reporter", "0")
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
		if _, err := st.Get(ctx, scheduleID("reporter", "1")); !errors.Is(err, ErrSubscriptionNotFound) {
			t.Fatalf("the departed invocation's row stayed: %v", err)
		}
		if _, err := st.Get(ctx, scheduleID("watcher", "0")); err != nil {
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
