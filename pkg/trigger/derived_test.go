package trigger

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

func TestDerivedSubscriptionIDIsStableAndSeparatesEveryField(t *testing.T) {
	base := DerivedSubscriptionID("t1", "bot", bundle.InvocationKindBoard, "0", "")
	if base != DerivedSubscriptionID("t1", "bot", bundle.InvocationKindBoard, "0", "") {
		t.Fatal("the same invocation derived two ids")
	}
	if !IsDerivedID(base) || len(base) != len(DerivedIDPrefix)+32 {
		t.Fatalf("derived id %q, want %s + 32 hex", base, DerivedIDPrefix)
	}
	for name, other := range map[string]string{
		"tenant":     DerivedSubscriptionID("t2", "bot", bundle.InvocationKindBoard, "0", ""),
		"bot":        DerivedSubscriptionID("t1", "bot2", bundle.InvocationKindBoard, "0", ""),
		"kind":       DerivedSubscriptionID("t1", "bot", bundle.InvocationKindSchedule, "0", ""),
		"key":        DerivedSubscriptionID("t1", "bot", bundle.InvocationKindBoard, "1", ""),
		"connection": DerivedSubscriptionID("t1", "bot", bundle.InvocationKindBoard, "0", "conn"),
		// Concatenation alone would read these two field lists alike.
		"boundary": DerivedSubscriptionID("t1b", "ot", bundle.InvocationKindBoard, "0", ""),
	} {
		if other == base {
			t.Errorf("changing the %s kept the id", name)
		}
	}
	if IsDerivedID("8f14e45f-ceea-4f2b-9b1e-1c3e5a2e7d10") {
		t.Error("a random id read as derived")
	}
}

func TestDerivedInvocationKeysAreIntrinsic(t *testing.T) {
	invs := []bundle.Invocation{
		{Kind: bundle.InvocationKindSchedule, Schedule: &bundle.InvocationSchedule{SuggestedCron: "0  7 * * 1"}},
		{Kind: bundle.InvocationKindSchedule, Schedule: &bundle.InvocationSchedule{SuggestedCron: "0 7 * * 1"}},
		{Kind: bundle.InvocationKindSchedule, Schedule: &bundle.InvocationSchedule{SuggestedCron: "0 9 * * 5"}},
		{Kind: bundle.InvocationKindKeepalive, Keepalive: &bundle.InvocationKeepalive{Interval: "5m"}},
		{Kind: bundle.InvocationKindKeepalive, Keepalive: &bundle.InvocationKeepalive{Interval: "300s"}},
		{Kind: bundle.InvocationKindBoard, Board: &bundle.InvocationBoard{On: []string{KindCardMoved}}},
		{Kind: bundle.InvocationKindConnector, Connector: &bundle.InvocationConnector{Trigger: "slack.message.posted"}},
		{Kind: bundle.InvocationKindSchedule}, // derives nothing
	}
	if DerivedInvocationKey(invs, 0) != "0 7 * * 1" || DerivedInvocationKey(invs, 0) != DerivedInvocationKey(invs, 1) {
		t.Errorf("the same cron spelled differently derived %q and %q", DerivedInvocationKey(invs, 0), DerivedInvocationKey(invs, 1))
	}
	if DerivedInvocationKey(invs, 0) == DerivedInvocationKey(invs, 2) {
		t.Error("two different crons derived one key")
	}
	if DerivedInvocationKey(invs, 3) != DerivedInvocationKey(invs, 4) {
		t.Errorf("5m and 300s derived %q and %q", DerivedInvocationKey(invs, 3), DerivedInvocationKey(invs, 4))
	}
	if got := DerivedInvocationKey(invs, 5); got == "" || got == DerivedInvocationKey(invs, 0) {
		t.Errorf("board key = %q, want a digest apart from the crons", got)
	}
	if got := DerivedInvocationKey(invs, 6); got != "slack.message.posted" {
		t.Errorf("connector key = %q, want its trigger", got)
	}
	if got := DerivedInvocationKey(invs, 7); got != "" {
		t.Errorf("an invocation that derives nothing keyed %q", got)
	}
}

// A row standing beside its tombstone is retired by the next EnsureDerived:
// an operator's delete may have died between its tombstone and the row's
// delete, and the Mongo withdraw may have failed once.
func TestATombstoneRetiresAStandingRow(t *testing.T) {
	onSubscriptionTwins(t, func(t *testing.T, st SubscriptionStore) {
		ctx := context.Background()
		id := DerivedSubscriptionID("t1", "bot", bundle.InvocationKindBoard, "0", "")
		if err := st.Create(ctx, Subscription{ID: id, TenantID: "t1", BotID: "bot", Enabled: true}); err != nil {
			t.Fatal(err)
		}
		if err := st.Tombstone(ctx, Tombstone{ID: id, TenantID: "t1", DeletedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if created, err := st.EnsureDerived(ctx, Subscription{ID: id, TenantID: "t1", BotID: "bot"}); err != nil || created {
			t.Fatalf("EnsureDerived beside a tombstone = %v, %v; want no create", created, err)
		}
		if _, err := st.Get(ctx, id); !errors.Is(err, ErrSubscriptionNotFound) {
			t.Fatalf("the row the tombstone outlives is still stored: %v", err)
		}
	})
}

// EnsureDerived creates once and never over a row or a tombstone; a
// tombstone holds until its tenant lifts it.
func TestDerivedRowsHonourTombstones(t *testing.T) {
	onSubscriptionTwins(t, func(t *testing.T, st SubscriptionStore) {
		ctx := context.Background()
		id := DerivedSubscriptionID("t1", "bot", bundle.InvocationKindBoard, "0", "")
		sub := Subscription{ID: id, TenantID: "t1", BotID: "bot", Invocation: bundle.InvocationKindBoard, Enabled: true}
		if created, err := st.EnsureDerived(ctx, sub); err != nil || !created {
			t.Fatalf("first EnsureDerived = %v, %v; want created", created, err)
		}
		changed := sub
		changed.BotID = "other"
		if created, err := st.EnsureDerived(ctx, changed); err != nil || created {
			t.Fatalf("EnsureDerived over a row = %v, %v; want no write", created, err)
		}
		if got, _ := st.Get(ctx, id); got.BotID != "bot" {
			t.Fatalf("EnsureDerived over a row rewrote it: bot = %q", got.BotID)
		}

		if err := st.Tombstone(ctx, Tombstone{ID: id, TenantID: "t1", DeletedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if err := st.Delete(ctx, id); err != nil {
			t.Fatal(err)
		}
		if created, err := st.EnsureDerived(ctx, sub); err != nil || created {
			t.Fatalf("EnsureDerived over a tombstone = %v, %v; want no write", created, err)
		}
		if _, err := st.Get(ctx, id); !errors.Is(err, ErrSubscriptionNotFound) {
			t.Fatalf("a tombstoned row came back: %v", err)
		}
		if err := st.ClearTombstone(ctx, "t2", id); err != nil {
			t.Fatal(err)
		}
		if created, _ := st.EnsureDerived(ctx, sub); created {
			t.Fatal("another tenant lifted the tombstone")
		}
		if err := st.ClearTombstone(ctx, "t1", id); err != nil {
			t.Fatal(err)
		}
		if created, err := st.EnsureDerived(ctx, sub); err != nil || !created {
			t.Fatalf("EnsureDerived after the tombstone lifted = %v, %v; want created", created, err)
		}

		random := Subscription{ID: "8f14e45f-ceea-4f2b-9b1e-1c3e5a2e7d10", TenantID: "t1", BotID: "bot"}
		if _, err := st.EnsureDerived(ctx, random); err == nil {
			t.Error("EnsureDerived accepted a random id")
		}
		if err := st.Tombstone(ctx, Tombstone{ID: random.ID, TenantID: "t1"}); err == nil {
			t.Error("Tombstone accepted a random id")
		}
	})
}

// The journal is the local store's whole truth: a second process opening
// it reads every field — the hidden version, the pins, the health stamps,
// the tombstones — and a stale copy still conflicts there.
func TestTheSubscriptionJournalKeepsEveryFieldAcrossProcesses(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "trigger-subscriptions.json")
	a := NewFileSubscriptionStore(path)
	sub := Subscription{
		ID: "s1", BotID: "bot", Enabled: true, Cron: "0 7 * * 1",
		KeyOverrides: map[string]string{"anthropic": "key-1"},
		Match:        Matcher{Sources: []Source{SourceSchedule}},
	}
	if err := a.Create(ctx, sub); err != nil {
		t.Fatal(err)
	}
	read, err := a.Get(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	stale := read
	read.Cron = "0 8 * * 1"
	if err := a.Update(ctx, read); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	if err := a.MarkRunOutcome(ctx, "s1", "run-1", "failed", "boom", "CODE", at); err != nil {
		t.Fatal(err)
	}
	tomb := DerivedSubscriptionID("", "bot", bundle.InvocationKindBoard, "0", "")
	if err := a.Tombstone(ctx, Tombstone{ID: tomb, DeletedAt: at}); err != nil {
		t.Fatal(err)
	}

	b := NewFileSubscriptionStore(path)
	got, err := b.Get(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || got.Cron != "0 8 * * 1" || got.KeyOverrides["anthropic"] != "key-1" || got.LastRunError != "boom" {
		t.Fatalf("a second process read %+v", got)
	}
	if err := b.Update(ctx, stale); !errors.Is(err, ErrSubscriptionConflict) {
		t.Fatalf("a stale copy written through a second process = %v, want a conflict", err)
	}
	if created, _ := b.EnsureDerived(ctx, Subscription{ID: tomb, BotID: "bot"}); created {
		t.Fatal("a second process did not read the tombstone")
	}
}

// An operator's delete that lands between a reconciliation's tombstone read
// and its insert still wins: the insert reads the tombstone again and
// withdraws its own row.
func TestADeleteRacingADerivedInsertWins(t *testing.T) {
	onSubscriptionTwins(t, func(t *testing.T, st SubscriptionStore) {
		mst, ok := st.(*MongoSubscriptionStore)
		if !ok {
			t.Skip("the race window is the Mongo twin's: the others decide under one lock")
		}
		ctx := context.Background()
		id := DerivedSubscriptionID("t1", "bot", bundle.InvocationKindBoard, "0", "")
		mst.afterDerivedInsert = func() {
			if err := mst.Tombstone(ctx, Tombstone{ID: id, TenantID: "t1", DeletedAt: time.Now().UTC()}); err != nil {
				t.Error(err)
			}
		}
		created, err := mst.EnsureDerived(ctx, Subscription{ID: id, TenantID: "t1", BotID: "bot"})
		if err != nil || created {
			t.Fatalf("EnsureDerived racing a delete = %v, %v; want withdrawn", created, err)
		}
		if _, err := mst.Get(ctx, id); !errors.Is(err, ErrSubscriptionNotFound) {
			t.Fatalf("the row a delete raced is still stored: %v", err)
		}
	})
}

// A tombstoned id is never inserted, not even for the moment a second read
// would take to withdraw it: an event matched in that moment would fire the
// subscription an operator deleted.
func TestATombstonedIDIsNeverInserted(t *testing.T) {
	onSubscriptionTwins(t, func(t *testing.T, st SubscriptionStore) {
		mst, ok := st.(*MongoSubscriptionStore)
		if !ok {
			t.Skip("the insert-then-read window is the Mongo twin's")
		}
		ctx := context.Background()
		id := DerivedSubscriptionID("t1", "bot", bundle.InvocationKindBoard, "0", "")
		if err := mst.Tombstone(ctx, Tombstone{ID: id, TenantID: "t1", DeletedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		inserted := false
		mst.afterDerivedInsert = func() { inserted = true }
		if created, err := mst.EnsureDerived(ctx, Subscription{ID: id, TenantID: "t1", BotID: "bot"}); err != nil || created {
			t.Fatalf("EnsureDerived over a tombstone = %v, %v", created, err)
		}
		if inserted {
			t.Fatal("a tombstoned id was inserted before being withdrawn")
		}
	})
}
