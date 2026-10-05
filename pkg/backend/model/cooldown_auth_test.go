package model

import (
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// The ledger's policy extension (ADR-121 §2, slice 4, the F5 arbitration):
// AUTH alone arms a run-long cooldown — a refused credential stays refused,
// and the ledger is what stops every LATER node from paying the same doomed
// spawn. transient_exhausted arms NOTHING (its own definition says a later
// attempt might work; a run-long entry would darken healthy routes), and
// auth outside the resolved triggers stays today's fail-open no-record.
func TestAuthCooldown(t *testing.T) {
	now := time.Now()
	err := errors.New("invalid api key")
	triggers := []string{"usage_window", "auth"}

	cd, armed := authCooldown(delegate.FallbackAuth, triggers, err, now)
	if !armed {
		t.Fatal("auth + triggers[auth] = not armed — the selection's own scenario would pay the doomed spawn per node")
	}
	if cd.Category != delegate.FallbackAuth || !cd.Until.After(now.Add(6*24*time.Hour)) {
		t.Fatalf("until = %v, want run-long (inside the 8-day guard)", cd.Until)
	}
	if cd.Cause == nil {
		t.Fatal("the typed cause must ride the entry (the retry classifiers read it)")
	}

	// Without auth in the triggers: today's fail-open no-record.
	if _, armed := authCooldown(delegate.FallbackAuth, []string{"usage_window"}, err, now); armed {
		t.Fatal("auth armed without the trigger — a level that must not switch must not darken routes")
	}
	// transient_exhausted arms nothing, ever (the F5 arbitration).
	if _, armed := authCooldown(delegate.FallbackTransientExhausted, triggers, err, now); armed {
		t.Fatal("transient_exhausted armed — a later attempt might work; a run-long entry would darken healthy routes")
	}
	// usage_window keeps its authoritative record-or-not exactly.
	if _, armed := authCooldown(delegate.FallbackUsageWindow, triggers, err, now); armed {
		t.Fatal("usage_window must stay on cooldownForFailure's authoritative path (the provider's reset)")
	}
}
