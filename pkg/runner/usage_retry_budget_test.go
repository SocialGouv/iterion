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

// #922 — the two clocks (a skipped credential's speculative reopening and the
// failed credential's authoritative reset) share ONE attempt budget, because
// every arming increments store.RunRetryState.Attempts. The invariant the
// arming must hold: the budget must not expire before the authoritative reset
// is reachable.
//
// Production shape, 2026-09-07: Anthropic's seven-day window shut on the
// credential serving the reviewer, while the credential the resolution skipped
// reopens on a five-hour cycle. Five attempts five hours apart cover 25h of a
// seven-day wall, so the budget retires the run days before the wall it waits
// on ever falls.
func TestUsageWindowRetryAt_BudgetOutlastsTheAuthoritativeWall(t *testing.T) {
	now := time.Date(2026, 9, 7, 20, 48, 0, 0, time.UTC)
	// The failed credential's own reset: a seven-day window, well inside the
	// 8-day MaxWait default, so the ceiling is never the binding constraint.
	wall := now.Add(7 * 24 * time.Hour)
	pol := noJitter(retrypolicy.Policy{})
	if pol.MaxAttempts != retrypolicy.DefaultMaxAttempts {
		t.Fatalf("fixture drift: max_attempts = %d, want the package default %d", pol.MaxAttempts, retrypolicy.DefaultMaxAttempts)
	}

	// Walk the whole budget the way production does: each wake republishes the
	// run, which re-resolves the credential chain and re-stamps
	// SkippedCredReopensAt (cloudpublisher.SetRunCredStamp), so the skipped
	// credential has cycled to its next five-hour reopening.
	clock := now
	armed := make([]time.Time, 0, pol.MaxAttempts)
	sources := make([]string, 0, pol.MaxAttempts)
	for spent := 0; spent < pol.MaxAttempts; spent++ {
		skipped := clock.Add(5 * time.Hour)
		at, source, ok := usageWindowRetryAt(weeklyWindowErr(wall), pol, clock, skipped, spent)
		if !ok {
			t.Fatalf("attempt %d: no retry armed", spent+1)
		}
		if !at.After(clock) {
			t.Fatalf("attempt %d armed for %s, not after the wake that armed it (%s)", spent+1, at, clock)
		}
		armed = append(armed, at)
		sources = append(sources, source)
		clock = at
	}

	last := armed[len(armed)-1]
	if last.Before(wall) {
		t.Fatalf("the attempt budget is spent %s before the wall falls.\n"+
			"  authoritative reset : %s\n"+
			"  attempts armed for  : %v\n"+
			"  reset sources       : %v\n"+
			"the last of %d attempts must land at or after the authoritative reset — "+
			"a speculative wake on another credential must not retire the run before the wall it waits on",
			wall.Sub(last).Round(time.Minute), wall, armed, sources, pol.MaxAttempts)
	}
	if got := sources[len(sources)-1]; got != "typed_error+last_attempt_pinned" {
		t.Errorf("last reset source = %q, want %q — the decision to decline the speculative wake must be visible on the event",
			got, "typed_error+last_attempt_pinned")
	}
}

// The recovery #684 shipped must survive the fix: while the budget still has
// an attempt to spare, the speculative wake is taken exactly as before. This
// is the half that recovered fifteen parked runs on 2026-09-07.
func TestUsageWindowRetryAt_SpeculativeWakeSurvivesWhileTheBudgetCanSpareIt(t *testing.T) {
	now := time.Date(2026, 9, 7, 20, 48, 0, 0, time.UTC)
	wall := now.Add(7 * 24 * time.Hour)
	reopens := now.Add(3 * time.Hour)
	pol := noJitter(retrypolicy.Policy{}) // 5 attempts

	for spent := 0; spent < pol.MaxAttempts-1; spent++ {
		at, source, ok := usageWindowRetryAt(weeklyWindowErr(wall), pol, now, reopens, spent)
		if !ok {
			t.Fatalf("attempts spent %d: no retry armed", spent)
		}
		if want := reopens.Add(time.Minute); !at.Equal(want) {
			t.Fatalf("attempts spent %d: armed for %s, want the skipped credential's reopening %s", spent, at, want)
		}
		if source != "skipped_credential" {
			t.Fatalf("attempts spent %d: reset source = %q, want skipped_credential", spent, source)
		}
	}

	// Only the last attempt is reserved.
	at, source, ok := usageWindowRetryAt(weeklyWindowErr(wall), pol, now, reopens, pol.MaxAttempts-1)
	if !ok {
		t.Fatal("last attempt: no retry armed")
	}
	if want := wall.Add(time.Minute); !at.Equal(want) {
		t.Fatalf("last attempt armed for %s, want the authoritative reset %s", at, want)
	}
	if source != "typed_error+last_attempt_pinned" {
		t.Fatalf("last attempt reset source = %q, want typed_error+last_attempt_pinned", source)
	}
}

// The reservation is conditional on the wall being REACHABLE. When the
// authoritative reset lies beyond the policy's own horizon, the ceiling clamp
// would pull the attempt back short of it anyway — so declining the
// speculative wake would buy nothing and lose the only chance left.
func TestUsageWindowRetryAt_UnreachableWallKeepsTheSpeculativeWake(t *testing.T) {
	now := time.Date(2026, 9, 7, 20, 48, 0, 0, time.UTC)
	pol := noJitter(retrypolicy.Policy{MaxWait: "36h"})
	wall := now.Add(7 * 24 * time.Hour) // far beyond max_wait
	reopens := now.Add(3 * time.Hour)

	at, source, ok := usageWindowRetryAt(weeklyWindowErr(wall), pol, now, reopens, pol.MaxAttempts-1)
	if !ok {
		t.Fatal("no retry armed")
	}
	if want := reopens.Add(time.Minute); !at.Equal(want) {
		t.Fatalf("armed for %s, want the skipped credential's reopening %s — an unreachable wall reserves nothing", at, want)
	}
	if source != "skipped_credential" {
		t.Fatalf("reset source = %q, want skipped_credential", source)
	}
}

// Jitter is applied BEFORE the max_wait clamp, so a reservation whose spread
// would be clamped back short of the wall is no reservation at all. The
// horizon check counts the spread for that reason.
func TestUsageWindowRetryAt_ReservationCountsTheJitterAgainstTheHorizon(t *testing.T) {
	now := time.Date(2026, 9, 7, 20, 48, 0, 0, time.UTC)
	pol := retrypolicy.Normalize(retrypolicy.Policy{MaxWait: "10h", Jitter: "30m"})
	// The wall sits inside max_wait, but not by enough to absorb the spread.
	wall := now.Add(9*time.Hour + 50*time.Minute)
	reopens := now.Add(2 * time.Hour)

	at, source, ok := usageWindowRetryAt(weeklyWindowErr(wall), pol, now, reopens, pol.MaxAttempts-1)
	if !ok {
		t.Fatal("no retry armed")
	}
	if source != "skipped_credential" {
		t.Fatalf("reset source = %q, want skipped_credential — a wall the spread would clamp past is not reachable", source)
	}
	if at.Before(reopens.Add(time.Minute)) || !at.Before(reopens.Add(time.Minute+30*time.Minute)) {
		t.Fatalf("armed for %s, want within the jitter band after %s", at, reopens.Add(time.Minute))
	}
}

// A blind wait is a guess, not a wall: with no reset instant to reserve for,
// the speculative wake stays the better use of the last attempt.
func TestUsageWindowRetryAt_BlindWaitReservesNothing(t *testing.T) {
	now := time.Date(2026, 9, 7, 20, 48, 0, 0, time.UTC)
	pol := noJitter(retrypolicy.Policy{})
	reopens := now.Add(20 * time.Minute)

	at, source, ok := usageWindowRetryAt(blindUsageWindowErr(), pol, now, reopens, pol.MaxAttempts-1)
	if !ok {
		t.Fatal("no retry armed")
	}
	if want := reopens.Add(time.Minute); !at.Equal(want) {
		t.Fatalf("armed for %s, want the skipped credential's reopening %s", at, want)
	}
	if source != "skipped_credential" {
		t.Fatalf("reset source = %q, want skipped_credential", source)
	}
}

// A single-attempt budget makes the FIRST arming the last one: the invariant
// then spends it on the wall rather than the gamble.
func TestUsageWindowRetryAt_SingleAttemptBudgetGoesStraightToTheWall(t *testing.T) {
	now := time.Date(2026, 9, 7, 20, 48, 0, 0, time.UTC)
	pol := noJitter(retrypolicy.Policy{MaxAttempts: 1})
	wall := now.Add(7 * 24 * time.Hour)

	at, source, ok := usageWindowRetryAt(weeklyWindowErr(wall), pol, now, now.Add(3*time.Hour), 0)
	if !ok {
		t.Fatal("no retry armed")
	}
	if want := wall.Add(time.Minute); !at.Equal(want) {
		t.Fatalf("armed for %s, want the authoritative reset %s", at, want)
	}
	if source != "typed_error+last_attempt_pinned" {
		t.Fatalf("reset source = %q, want typed_error+last_attempt_pinned", source)
	}
}

// The arming reads the spent-attempt count from the RUN DOCUMENT — the pure
// decision above is only useful if the runner feeds it what the store will
// charge the arming against.
func TestArmUsageWindowRetry_ReadsTheSpentAttemptsFromTheRun(t *testing.T) {
	wall := time.Now().UTC().Add(7 * 24 * time.Hour)
	reopens := time.Now().UTC().Add(3 * time.Hour)
	newStore := func(attempts int) *cancelAwareStore {
		return &cancelAwareStore{run: &store.Run{
			ID:                   "run-922",
			Status:               store.RunStatusFailedResumable,
			RetryPolicy:          &store.RunRetryPolicy{UsageWindow: "resume", MaxAttempts: 5, MaxWait: "192h", Jitter: "0s"},
			SkippedCredReopensAt: &reopens,
			RetryState:           &store.RunRetryState{Attempts: attempts},
		}}
	}
	logger := iterlog.New(iterlog.LevelError, io.Discard)

	// Budget to spare: the speculative wake still wins.
	st := newStore(3)
	r := &Runner{cfg: Config{Store: st}}
	if got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(wall), "run-922", logger); got != usageRetryArmed || !st.armed {
		t.Fatalf("outcome = %v armed = %v, want an armed retry", got, st.armed)
	}
	if want := reopens.Add(time.Minute); !st.armedAt.Equal(want) {
		t.Fatalf("with 3 of 5 attempts spent: armed at %s, want the skipped credential's reopening %s", st.armedAt, want)
	}

	// Last attempt: the run gets its one try at the wall.
	st = newStore(4)
	r = &Runner{cfg: Config{Store: st}}
	if got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(wall), "run-922", logger); got != usageRetryArmed || !st.armed {
		t.Fatalf("outcome = %v armed = %v, want an armed retry", got, st.armed)
	}
	if want := wall.Add(time.Minute); !st.armedAt.Equal(want) {
		t.Fatalf("with 4 of 5 attempts spent: armed at %s, want the authoritative reset %s", st.armedAt, want)
	}
}
