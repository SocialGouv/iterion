package trigger

import (
	"context"
	"encoding/json"
	"testing"
)

// The memory store mutates pin maps in place under its write lock (the
// value-scoped clear), so a Subscription crossing ANY boundary shares
// nothing with the store: a reader holding a pre-clear copy keeps seeing the
// pins the store has since dropped, and a caller's payload never becomes the
// store's mutation surface (#2644 round 4). Each arm is deterministic:
// without the boundary copies the shared map shows the clear.
func TestMemorySubscriptionStoreBoundariesHandOutPrivateCopies(t *testing.T) {
	ctx := context.Background()
	pins := func() map[string]string {
		return map[string]string{"anthropic": "key-dead", "openai": "key-alive"}
	}

	// Get: the copy a reader holds keeps the pin the store has since swept —
	// a marshal of it (GET /api/v1/triggers) races the clear otherwise. Row x
	// lives in another tenant so the listing arm below is exactly one row.
	st := NewMemorySubscriptionStore()
	if err := st.Create(ctx, Subscription{ID: "x", TenantID: "t0", BotID: "b", KeyOverrides: pins()}); err != nil {
		t.Fatal(err)
	}
	held, err := st.Get(ctx, "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClearCredentialPins(ctx, "x", "key-dead"); err != nil {
		t.Fatal(err)
	}
	if held.KeyOverrides["anthropic"] != "key-dead" {
		t.Fatal("the sweep mutated the reader's Get copy — a marshal of it races the clear")
	}

	// ListByTenant: the listing copies the sweep itself walks before its
	// clear — a copy shadowing the stored map makes the sweep's own pinsName
	// judge a map the next clear deletes from.
	if err := st.Create(ctx, Subscription{ID: "y", TenantID: "t1", BotID: "b", KeyOverrides: pins()}); err != nil {
		t.Fatal(err)
	}
	listing, err := st.ListByTenant(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(listing) != 1 {
		t.Fatalf("the t1 listing holds %d rows, want 1", len(listing))
	}
	if _, err := st.ClearCredentialPins(ctx, "y", "key-dead"); err != nil {
		t.Fatal(err)
	}
	if listing[0].KeyOverrides["anthropic"] != "key-dead" {
		t.Fatal("the sweep mutated the listing copy — the sweep walks maps the clear deletes from")
	}

	// Create: the caller's payload is not the store's mutation surface — the
	// route marshals the create answer while a clear may run.
	st2 := NewMemorySubscriptionStore()
	payload := pins()
	if err := st2.Create(ctx, Subscription{ID: "c", TenantID: "t1", BotID: "b", KeyOverrides: payload}); err != nil {
		t.Fatal(err)
	}
	if _, err := st2.ClearCredentialPins(ctx, "c", "key-dead"); err != nil {
		t.Fatal(err)
	}
	if payload["anthropic"] != "key-dead" {
		t.Fatal("the clear mutated the create payload's map — the store shares the caller's header")
	}

	// Update: same for an edit payload the route marshals after storing it.
	st3 := NewMemorySubscriptionStore()
	if err := st3.Create(ctx, Subscription{ID: "u", TenantID: "t1", BotID: "b", KeyOverrides: pins()}); err != nil {
		t.Fatal(err)
	}
	edited, err := st3.Get(ctx, "u")
	if err != nil {
		t.Fatal(err)
	}
	if err := st3.Update(ctx, edited); err != nil {
		t.Fatal(err)
	}
	if _, err := st3.ClearCredentialPins(ctx, "u", "key-dead"); err != nil {
		t.Fatal(err)
	}
	if edited.KeyOverrides["anthropic"] != "key-dead" {
		t.Fatal("the clear mutated the edit payload's map after Update stored it")
	}
}

// TestMemorySubscriptionStorePinClearDoesNotRaceReaders is the crash
// witness: it must run GREEN under -race and RED without the boundary
// copies, where ranging a handed-out map while the clear deletes from the
// store's map is Go's unrecoverable "concurrent map iteration and map
// write" — the local single-host server dying on a credential delete that
// races any trigger view or launch (#2644 round 4).
func TestMemorySubscriptionStorePinClearDoesNotRaceReaders(t *testing.T) {
	ctx := context.Background()
	st := NewMemorySubscriptionStore()
	done := make(chan struct{})
	// The sweep lane: re-arm the dead pin, then clear it, 300 times — the
	// clear only deletes while a matching pin stands, so each round re-seeds
	// the map the previous round emptied.
	go func() {
		defer close(done)
		for i := 0; i < 300; i++ {
			_ = st.Create(ctx, Subscription{
				ID: "raced", TenantID: "t1", BotID: "b",
				KeyOverrides:    map[string]string{"anthropic": "key-dead", "openai": "key-alive"},
				SecretOverrides: map[string]string{"forge_token": "sec-dead"},
			})
			_, _ = st.ClearCredentialPins(ctx, "raced", "key-dead")
		}
	}()
	// The reader lane: the listing-and-marshal shape of GET /api/v1/triggers,
	// ranging the handed-out maps until the sweep lane is done.
	for {
		select {
		case <-done:
			return
		default:
		}
		subs, err := st.ListByTenant(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := json.Marshal(subs); err != nil {
			t.Fatal(err)
		}
	}
}
