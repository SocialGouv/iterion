package webhooks

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The credential delete sweep's store half: SetCredentialPins must replace
// the two pin maps and NOTHING else. The sweep calls it for a config it
// only listed, so a whole-config replacement here would race an operator's
// concurrent edit (losing or resurrecting settings arms), and a write that
// skips the version bump would let a pre-sweep snapshot pass its fence and
// write the swept pin back (#2644).
func TestSetCredentialPins_ReplacesOnlyThePins(t *testing.T) {
	onConfigStoreTwins(t, func(t *testing.T, s ConfigStore) {
		ctx := context.Background()
		mark := time.Unix(1700000000, 0).UTC()
		cfg := Config{ID: "swept", TenantID: "tenant", TokenHash: "hash", Name: "keep-me", BotIDs: []string{"b1"}, LastUsedAt: &mark,
			KeyOverrides:    map[string]string{"anthropic": "key-dead", "openai": "key-alive"},
			SecretOverrides: map[string]string{"forge_token": "sec-dead", "other": "sec-alive"},
		}
		if err := s.Create(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		before, err := s.Get(ctx, "swept")
		if err != nil {
			t.Fatal(err)
		}

		// The dead key's pin goes, wholesale: the surviving map holds only
		// the entries the sweep kept — never a merge with the stored one.
		if err := s.SetCredentialPins(ctx, "swept", map[string]string{"openai": "key-alive"}, map[string]string{"other": "sec-alive"}); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(ctx, "swept")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.KeyOverrides) != 1 || got.KeyOverrides["openai"] != "key-alive" {
			t.Fatalf("key_overrides = %v, want only the surviving pin — a merged map would read as pins the sweep dropped", got.KeyOverrides)
		}
		if len(got.SecretOverrides) != 1 || got.SecretOverrides["other"] != "sec-alive" {
			t.Fatalf("secret_overrides = %v, want only the surviving pin", got.SecretOverrides)
		}
		// A pin map is not a config: every other arm of the row survives.
		if got.Name != "keep-me" || len(got.BotIDs) != 1 || got.BotIDs[0] != "b1" {
			t.Fatalf("the targeted write bled into the config: name=%q bot_ids=%v", got.Name, got.BotIDs)
		}
		if got.LastUsedAt == nil || !got.LastUsedAt.Equal(mark) {
			t.Fatalf("the targeted write moved the independently stamped usage: %v", got.LastUsedAt)
		}

		// An empty map is the clear: the last pin of a kind leaves no empty
		// husk behind.
		if err := s.SetCredentialPins(ctx, "swept", nil, nil); err != nil {
			t.Fatal(err)
		}
		got, err = s.Get(ctx, "swept")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.KeyOverrides) != 0 || len(got.SecretOverrides) != 0 {
			t.Fatalf("the clear left pins behind: %v / %v", got.KeyOverrides, got.SecretOverrides)
		}

		// The fence outlives the sweep: a snapshot read BEFORE the targeted
		// writes must conflict, so its holder re-reads instead of writing
		// the dead pin back over the sweep.
		if err := s.Update(ctx, before); !errors.Is(err, ErrConflict) {
			t.Fatalf("a pre-sweep snapshot updated cleanly (%v) — the sweep would be silently reverted by the next editor", err)
		}
		final, _ := s.Get(ctx, "swept")
		if len(final.KeyOverrides) != 0 || len(final.SecretOverrides) != 0 {
			t.Fatalf("the conflicting write resurrected pins: %v / %v", final.KeyOverrides, final.SecretOverrides)
		}

		// An unknown config is ErrNotFound, not a silent no-op.
		if err := s.SetCredentialPins(ctx, "absent", nil, nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("SetCredentialPins on a missing config = %v, want ErrNotFound", err)
		}
	})
}
