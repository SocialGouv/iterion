package runner

import (
	"context"
	"io"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #684 — the retry arms on the earliest credential that becomes usable
// again, not on the reset of the one that happened to fail. Production
// shape, 2026-09-04: the team key was refused on its five-hour window
// (reopens 16:40Z the same day), the walk fell through to the platform
// forfait (weekly-walled until Monday 21:00Z), the run parked on the
// forfait — and the retry was armed for Monday. Fourteen reviews slept
// four days instead of three hours.
func TestUsageWindowRetryAt_ArmsOnTheEarliestReopeningCredential(t *testing.T) {
	now := time.Date(2026, 9, 4, 13, 12, 0, 0, time.UTC)
	monday := time.Date(2026, 9, 8, 21, 0, 0, 0, time.UTC)
	teamKeyReopens := time.Date(2026, 9, 4, 16, 40, 0, 0, time.UTC)
	pol := noJitter(retrypolicy.Policy{})

	at, source, ok := usageWindowRetryAt(weeklyWindowErr(monday), pol, now, teamKeyReopens)
	if !ok {
		t.Fatal("no retry armed")
	}
	if want := teamKeyReopens.Add(time.Minute); !at.Equal(want) {
		t.Fatalf("armed for %s, want %s — the skipped team key reopens four days before the failed forfait", at, want)
	}
	if source != "skipped_credential" {
		t.Fatalf("reset source = %q, want skipped_credential", source)
	}

	// The failed credential's own reset still wins when it is the earlier
	// of the two, and keeps its own source.
	soon := now.Add(2 * time.Hour)
	at, source, _ = usageWindowRetryAt(weeklyWindowErr(soon), pol, now, teamKeyReopens)
	if !at.Equal(soon.Add(time.Minute)) || source != "typed_error" {
		t.Fatalf("armed for %s via %s, want the failed credential's own reset %s via typed_error", at, source, soon.Add(time.Minute))
	}

	// A skipped credential that has ALREADY reopened means "re-resolve
	// now": the floor applies, not the failed credential's distant reset.
	at, source, _ = usageWindowRetryAt(weeklyWindowErr(monday), pol, now, now.Add(-30*time.Minute))
	if !at.Equal(now.Add(usageWindowFloor)) || source != "skipped_credential" {
		t.Fatalf("armed for %s via %s, want the floor %s via skipped_credential", at, source, now.Add(usageWindowFloor))
	}

	// Nothing skipped: unchanged behaviour.
	at, source, _ = usageWindowRetryAt(weeklyWindowErr(monday), pol, now, time.Time{})
	if !at.Equal(monday.Add(time.Minute)) || source != "typed_error" {
		t.Fatalf("with nothing skipped: armed for %s via %s", at, source)
	}
}

// The arming reads the instant from the RUN DOCUMENT the publisher stamped
// — the pure decision above is only useful if the runner feeds it.
func TestArmUsageWindowRetry_ReadsTheSkippedCredentialFromTheRun(t *testing.T) {
	monday := time.Now().UTC().Add(4 * 24 * time.Hour)
	teamKeyReopens := time.Now().UTC().Add(3 * time.Hour)
	st := &cancelAwareStore{run: &store.Run{
		ID:                   "run-684",
		Status:               store.RunStatusFailedResumable,
		RetryPolicy:          &store.RunRetryPolicy{UsageWindow: "resume", MaxAttempts: 5, MaxWait: "192h", Jitter: "0s"},
		SkippedCredReopensAt: &teamKeyReopens,
	}}
	r := &Runner{cfg: Config{Store: st}}

	got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(monday), "run-684", iterlog.New(iterlog.LevelError, io.Discard))
	if got != usageRetryArmed || !st.armed {
		t.Fatalf("outcome = %v armed=%v, want an armed retry", got, st.armed)
	}
	if want := teamKeyReopens.Add(time.Minute); !st.armedAt.Equal(want) {
		t.Fatalf("armed at %s, want the skipped credential's reopening %s (the failed forfait resets %s)", st.armedAt, want, monday)
	}
}
