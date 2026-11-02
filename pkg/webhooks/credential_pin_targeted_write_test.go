package webhooks

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The credential delete sweep's store half: ClearCredentialPins must judge
// the row as the store holds it at write time and remove ONLY the entries
// whose value names the deleted record. A wholesale rewrite from the
// caller's listing copy would silently revert a pin edit or a second
// record's sweep landing in the list-to-write window — an explicit pin
// choice lost with a 204 and no trace — and resurrect a pin the concurrent
// sweep just cleared (#2644).
func TestClearCredentialPins_IsAValueGuardedWriteTimeClear(t *testing.T) {
	onConfigStoreTwins(t, func(t *testing.T, s ConfigStore) {
		ctx := context.Background()
		mark := time.Unix(1700000000, 0).UTC()
		if err := s.Create(ctx, Config{ID: "swept", TenantID: "tenant", TokenHash: "hash", Name: "keep-me", BotIDs: []string{"b1"}, LastUsedAt: &mark,
			KeyOverrides:    map[string]string{"anthropic": "key-dead", "openai": "key-alive"},
			SecretOverrides: map[string]string{"forge_token": "sec-dead", "other": "sec-alive"},
		}); err != nil {
			t.Fatal(err)
		}
		// before is the sweep's listing copy — everything it knows.
		before, err := s.Get(ctx, "swept")
		if err != nil {
			t.Fatal(err)
		}

		// THE FINDING'S WINDOW: an operator edit lands between the sweep's
		// list and its clear, adding a pin the listing copy cannot know.
		edited := before
		edited.SecretOverrides["added"] = "sec-added"
		if err := s.Update(ctx, edited); err != nil {
			t.Fatal(err)
		}

		cleared, err := s.ClearCredentialPins(ctx, "swept", "key-dead")
		if err != nil {
			t.Fatal(err)
		}
		if !cleared {
			t.Fatal("the clear reported nothing removed — the dead pin is still stored")
		}
		got, err := s.Get(ctx, "swept")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.KeyOverrides) != 1 || got.KeyOverrides["openai"] != "key-alive" {
			t.Fatalf("key_overrides = %v, want only the surviving pin — a stored pin naming the deleted key seals the team's default in silence", got.KeyOverrides)
		}
		// The landing edit survives: the clear judged the stored row, not
		// the listing copy. A wholesale rewrite would drop "added" here.
		if got.SecretOverrides["added"] != "sec-added" || got.SecretOverrides["other"] != "sec-alive" {
			t.Fatalf("secret_overrides = %v — the clear clobbered a pin written after the sweep's snapshot, an explicit choice lost in silence", got.SecretOverrides)
		}
		// A key sweep is a KEY sweep: the secret pins do not move.
		if got.SecretOverrides["forge_token"] != "sec-dead" {
			t.Fatalf("secret_overrides = %v — the key clear moved a pin that does not name the deleted key", got.SecretOverrides)
		}
		// A pin map is not a config: every other arm of the row survives.
		if got.Name != "keep-me" || len(got.BotIDs) != 1 || got.BotIDs[0] != "b1" {
			t.Fatalf("the clear bled into the config: name=%q bot_ids=%v", got.Name, got.BotIDs)
		}
		if got.LastUsedAt == nil || !got.LastUsedAt.Equal(mark) {
			t.Fatalf("the clear moved the independently stamped usage: %v", got.LastUsedAt)
		}

		// A row holding no such pin is a no-op that writes nothing: the
		// version must not move, or the fence would conflict the next
		// editor for no reason.
		steady, err := s.Get(ctx, "swept")
		if err != nil {
			t.Fatal(err)
		}
		cleared, err = s.ClearCredentialPins(ctx, "swept", "key-never-pinned")
		if err != nil || cleared {
			t.Fatalf("a no-op clear = (%v, %v), want (false, nil)", cleared, err)
		}
		after, err := s.Get(ctx, "swept")
		if err != nil {
			t.Fatal(err)
		}
		if after.Version != steady.Version {
			t.Fatalf("a no-op clear moved the version %d → %d — the fence must not punish an editor who did nothing", steady.Version, after.Version)
		}

		// A second record's sweep on the same row: the fence outlives it —
		// a snapshot read BEFORE the clear must conflict, so its holder
		// re-reads instead of writing the swept pin back — and the clear
		// removes only that record's entries.
		stale, err := s.Get(ctx, "swept")
		if err != nil {
			t.Fatal(err)
		}
		cleared, err = s.ClearCredentialPins(ctx, "swept", "sec-dead")
		if err != nil || !cleared {
			t.Fatalf("the second sweep = (%v, %v), want (true, nil)", cleared, err)
		}
		if err := s.Update(ctx, stale); !errors.Is(err, ErrConflict) {
			t.Fatalf("a pre-sweep snapshot updated cleanly (%v) — the sweep would be silently reverted by the next editor", err)
		}
		final, err := s.Get(ctx, "swept")
		if err != nil {
			t.Fatal(err)
		}
		if len(final.SecretOverrides) != 2 || final.SecretOverrides["forge_token"] == "sec-dead" {
			t.Fatalf("secret_overrides = %v — the conflicting write resurrected the swept pin", final.SecretOverrides)
		}

		// An unknown config is ErrNotFound, not a silent no-op.
		cleared, err = s.ClearCredentialPins(ctx, "absent", "key-dead")
		if !errors.Is(err, ErrNotFound) || cleared {
			t.Fatalf("ClearCredentialPins on a missing config = (%v, %v), want (false, ErrNotFound)", cleared, err)
		}
	})
}

// The drop is unconditional across BOTH maps: the contract removes every
// entry whose value names recordID, so a config whose key pins AND secret
// pins name the same id must leave neither standing — a short-circuit on
// the first map's drop leaves the second map's dead pin stored under a
// "cleared" answer, and the memory twin then diverges from its Mongo twin,
// which scans both.
func TestClearCredentialPins_SweepsBothMapsUnconditionally(t *testing.T) {
	onConfigStoreTwins(t, func(t *testing.T, s ConfigStore) {
		ctx := context.Background()
		if err := s.Create(ctx, Config{ID: "both-maps", TenantID: "tenant", TokenHash: "hash",
			KeyOverrides:    map[string]string{"anthropic": "dead-everywhere", "openai": "key-alive"},
			SecretOverrides: map[string]string{"forge_token": "dead-everywhere"},
		}); err != nil {
			t.Fatal(err)
		}
		cleared, err := s.ClearCredentialPins(ctx, "both-maps", "dead-everywhere")
		if err != nil || !cleared {
			t.Fatalf("the both-maps clear = (%v, %v), want (true, nil)", cleared, err)
		}
		got, err := s.Get(ctx, "both-maps")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.KeyOverrides) != 1 || got.KeyOverrides["openai"] != "key-alive" {
			t.Fatalf("key_overrides = %v, want only the surviving pin", got.KeyOverrides)
		}
		if _, stands := got.SecretOverrides["forge_token"]; stands {
			t.Fatalf("secret_overrides = %v — the secret map was never scanned once the key map dropped something; the contract removes every entry naming the record", got.SecretOverrides)
		}
	})
}

// Two records dying together sweep the same rows: each clear must keep the
// other's target. A wholesale rewrite from a stale snapshot would resurrect
// the pin the concurrent sweep just cleared — and which order lands last
// decides who wins, so the witness runs in BOTH orders.
func TestClearCredentialPins_ConcurrentSweepsComposeInBothOrders(t *testing.T) {
	onConfigStoreTwins(t, func(t *testing.T, s ConfigStore) {
		ctx := context.Background()
		for _, order := range [][]string{
			{"key-a", "key-b"},
			{"key-b", "key-a"},
		} {
			if err := s.Create(ctx, Config{ID: "pair-" + order[0], TenantID: "tenant", TokenHash: "hash-pair-" + order[0],
				KeyOverrides: map[string]string{"anthropic": "key-a", "openai": "key-b"},
			}); err != nil {
				t.Fatal(err)
			}
			for _, dead := range order {
				cleared, err := s.ClearCredentialPins(ctx, "pair-"+order[0], dead)
				if err != nil || !cleared {
					t.Fatalf("clear %s = (%v, %v), want (true, nil)", dead, cleared, err)
				}
			}
			got, err := s.Get(ctx, "pair-"+order[0])
			if err != nil {
				t.Fatal(err)
			}
			if len(got.KeyOverrides) != 0 {
				t.Fatalf("order %v left key_overrides = %v — the later sweep resurrected the pin the earlier one cleared", order, got.KeyOverrides)
			}
		}
	})
}
