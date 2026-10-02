package delegate

import (
	"testing"
	"time"
)

// The facade's multi-day wall must classify as a usage WINDOW, not as a
// throttle. Two things ride on the distinction and neither is visible from
// the error text: a named window is what makes a meter reading get
// recorded at all, and FallbackCategory usage_window is what lets a
// declared fallback route be tried — defaultFallbackTriggers refuses
// transient_exhausted.
func TestFacadeMultiDayWallIsAWindowWithItsReopenInstant(t *testing.T) {
	const refusal = "API Error: Request rejected (429) · [1310][Weekly/Monthly Limit Exhausted. " +
		"Your limit will reset at 2026-09-24 18:14:59][20260922194425aaaaaaaaaaaaaaaa]"

	if !isRateLimitMessage(refusal) {
		t.Fatal("the refusal is not even recognised as a provider rate limit")
	}

	kind, window, resetAt := classifyRateLimit(refusal, time.Now())
	if kind != RateLimitKindUsageWindow {
		t.Errorf("kind = %q, want %q — a dated wall is not a throttle", kind, RateLimitKindUsageWindow)
	}
	if window == "" {
		t.Error("window is unnamed: both reading sites are gated on the name, so the wall stays invisible to the resolution walk")
	}
	want := time.Date(2026, 9, 24, 18, 14, 59, 0, time.UTC)
	if !resetAt.Equal(want) {
		t.Errorf("resetAt = %v, want %v — the refusal carries its own reopen instant", resetAt, want)
	}

	if got := ClassifyFallback(&ErrRateLimited{Kind: kind}, true); got != FallbackUsageWindow {
		t.Errorf("category = %q, want %q — anything else is screened out of the default fallback triggers", got, FallbackUsageWindow)
	}
}

// The other face: widening the window signals must not swallow an ordinary
// throttle, which IS worth retrying in place. Without this the bench above
// would pass on a predicate that says yes to everything.
func TestAnOrdinaryThrottleStaysTransient(t *testing.T) {
	for _, text := range []string{
		"API Error: Request rejected (429) · Rate limit exceeded, please slow down",
		"API Error: Request rejected (429) · quota exceeded for this minute",
	} {
		kind, window, resetAt := classifyRateLimit(text, time.Now())
		if kind != RateLimitKindTransient {
			t.Errorf("%q: kind = %q, want %q", text, kind, RateLimitKindTransient)
		}
		if window != "" || !resetAt.IsZero() {
			t.Errorf("%q: a throttle records no window reading (window=%q resetAt=%v)", text, window, resetAt)
		}
	}
}

// The facade's 5h shape and the forfait's own wording keep classifying as
// they did: the new signal is an addition, not a replacement.
func TestTheKnownWindowShapesStillClassify(t *testing.T) {
	cases := []struct {
		text       string
		wantWindow bool
	}{
		{"API Error: Request rejected (429) · Usage limit reached for 5 hour. Your limit will reset at 2026-09-22 14:00:00", true},
		{"You've hit your weekly limit · resets 9pm (Europe/Paris)", true},
		{"You've hit your limit · resets 10:30am (UTC)", true},
	}
	for _, c := range cases {
		kind, _, _ := classifyRateLimit(c.text, time.Now())
		if got := kind == RateLimitKindUsageWindow; got != c.wantWindow {
			t.Errorf("%q: usage window = %v, want %v (kind=%q)", c.text, got, c.wantWindow, kind)
		}
	}
}
