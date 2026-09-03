package delegate

import (
	"testing"
	"time"
)

func TestClassifyRateLimit(t *testing.T) {
	now := time.Date(2026, 7, 17, 14, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		text       string
		wantKind   string
		wantWindow string
		wantReset  time.Time
	}{
		{
			// The fair-usage account restriction, verbatim from the wire
			// (2026-09-02): it refuses the request RATE, carries no reset
			// instant, and names the frequency window so the refusal
			// reaches the meter as credential-skip evidence. Two lots and
			// two rites burned their budgets on it unclassified.
			name:       "fair-usage account refusal names the frequency window",
			text:       "API Error: Request rejected (429) · [1313][Your account's current usage pattern does not comply with the Fair Usage Policy, and your request frequency has been limited. For details, please refer to the Subscription Service Agreement. To restore access, please submit a request.][202609021420552fc3f7d9844944b6]",
			wantKind:   RateLimitKindUsageWindow,
			wantWindow: "frequency",
		},
		{
			// The account SPEND ceiling, verbatim from the wire
			// (2026-09-03, run 01a06694): three words and an apostrophe
			// between "your" and "limit" defeated the old one-word
			// qualifier, so the notice sailed through as the node's
			// answer and died as "structured output invalid: missing
			// required field …" — three branch-improve-loop runs on one
			// morning. It names its own window: a money ceiling reopens
			// when an admin raises it, never on a reset instant, so
			// filing it as a nameless window would arm a blind retry
			// against a wall.
			name:       "org spend ceiling names the spend window, no reset",
			text:       "You've hit your org's monthly spend limit · ask your admin to raise it at claude.ai/settings/usage",
			wantKind:   RateLimitKindUsageWindow,
			wantWindow: "spend",
		},
		{
			name:      "forfait limit with pm clock",
			text:      "You've hit your limit · resets 3pm",
			wantKind:  RateLimitKindUsageWindow,
			wantReset: time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC),
		},
		{
			name:      "session limit with minutes am",
			text:      "You've hit your session limit · resets 10:30am (UTC)",
			wantKind:  RateLimitKindUsageWindow,
			wantReset: time.Date(2026, 7, 18, 10, 30, 0, 0, time.UTC), // 10:30 already past 14:00 → next day
		},
		{
			name:      "weekly limit with pm clock (feed-watch runner 019f7eee)",
			text:      "You've hit your weekly limit · resets 9pm (Europe/Paris)",
			wantKind:  RateLimitKindUsageWindow,
			wantReset: time.Date(2026, 7, 17, 21, 0, 0, 0, time.UTC),
		},
		{
			name:      "zai 5h facade",
			text:      "API Error: Request rejected (429) · Usage limit reached for 5 hour. Your limit will reset later.",
			wantKind:  RateLimitKindUsageWindow,
			wantReset: now.Add(5 * time.Hour),
		},
		{
			name:     "plain throttle stays transient",
			text:     "rate limit exceeded, retry shortly",
			wantKind: RateLimitKindTransient,
		},
		{
			name:     "generic 429 without usage marker stays transient",
			text:     "Request rejected (429)",
			wantKind: RateLimitKindTransient,
		},
		{
			name:     "usage window without parseable reset",
			text:     "You've hit your limit",
			wantKind: RateLimitKindUsageWindow,
		},
		{
			// The shape a WEEKLY cap actually prints: a month name and day
			// before the clock. Seven scheduled prod runs died on this on
			// 2026-07-27 with the reset ~35h out.
			name:      "weekly limit with dated reset",
			text:      "You've hit your weekly limit · resets Jul 28, 9pm (UTC)",
			wantKind:  RateLimitKindUsageWindow,
			wantReset: time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC),
		},
		{
			// An explicit absolute instant needs no year inference, so it is
			// taken verbatim. Previously the clock pattern chewed the "20" out
			// of "2026" and produced hour 20 of TODAY.
			name:      "zai absolute reset instant",
			text:      "Usage limit reached for 5 hour. Your limit will reset at 2026-05-13 07:38:08",
			wantKind:  RateLimitKindUsageWindow,
			wantReset: time.Date(2026, 5, 13, 7, 38, 8, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, window, reset := classifyRateLimit(tt.text, now)
			if kind != tt.wantKind {
				t.Fatalf("kind = %q, want %q", kind, tt.wantKind)
			}
			if string(window) != tt.wantWindow {
				t.Fatalf("window = %q, want %q", window, tt.wantWindow)
			}
			if !reset.Equal(tt.wantReset) {
				t.Fatalf("reset = %v, want %v", reset, tt.wantReset)
			}
		})
	}
}

// TestIsRateLimitMessage_RelayedAPIErrorLength pins the two sides of the
// prefix-anchored acceptance: the real fair-usage relay (~330 chars, over
// the one-liner cap) must be seen, and an agent essay that merely embeds
// or quotes the same words must not — the length cap is what keeps a
// PARKING path from firing on prose.
func TestIsRateLimitMessage_RelayedAPIErrorLength(t *testing.T) {
	relay := "API Error: Request rejected (429) · [1313][Your account's current usage pattern does not comply with the Fair Usage Policy, and your request frequency has been limited. For details, please refer to the Subscription Service Agreement. To restore access, please submit a request.][202609021420552fc3f7d9844944b6]"
	if len(relay) <= 200 {
		t.Fatalf("fixture lost its point: the relay must exceed the one-liner cap, len=%d", len(relay))
	}
	if !isRateLimitMessage(relay) {
		t.Fatal("the verbatim fair-usage relay must be recognised — this exact miss let the refusal become node output")
	}
	prose := "While auditing the client I read their Fair Usage Policy and noticed the request frequency has been limited in their nginx config; here is a long analysis of why that matters for the migration plan, with several paragraphs of detail that keep going well past any plausible relayed error length. " + relay
	if isRateLimitMessage(prose) {
		t.Fatal("an agent essay embedding the relay mid-text must not classify — it does not START with the relay prefix")
	}
	tooLong := relay + " ... and then the agent keeps narrating for hundreds of characters about what it plans to do next, which is no longer the provider talking but the model writing an essay around a quote, so the acceptance must cut off. Padding padding padding padding padding padding padding padding padding padding padding padding."
	if len(tooLong) <= relayedAPIErrorMaxLen {
		t.Fatalf("fixture lost its point: len=%d must exceed relayedAPIErrorMaxLen", len(tooLong))
	}
	if isRateLimitMessage(tooLong) {
		t.Fatal("a prefix-opening text past the relay length bound must not classify")
	}
}

func TestParseResetHint_ClockRollsToNextDay(t *testing.T) {
	// "resets 3pm" seen at 16:00 → tomorrow 15:00, never the past.
	now := time.Date(2026, 7, 17, 16, 0, 0, 0, time.UTC)
	got := parseResetHint("resets 3pm", now)
	want := time.Date(2026, 7, 18, 15, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestParseResetHint_DatedShape covers the month-name shape and its year
// inference. A reset is always within days, so a candidate that lands
// implausibly far from now means the text was not really a reset hint —
// returning zero (caller falls back to a bounded wait) beats returning a
// confidently wrong instant.
func TestParseResetHint_DatedShape(t *testing.T) {
	tests := []struct {
		name string
		text string
		now  time.Time
		want time.Time
	}{
		{
			name: "same year, days ahead",
			text: "resets jul 28, 9pm (utc)",
			now:  time.Date(2026, 7, 27, 6, 0, 0, 0, time.UTC),
			want: time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC),
		},
		{
			name: "december to january rolls the year forward",
			text: "resets jan 2, 10am (utc)",
			now:  time.Date(2026, 12, 30, 23, 0, 0, 0, time.UTC),
			want: time.Date(2027, 1, 2, 10, 0, 0, 0, time.UTC),
		},
		{
			name: "january seeing december rolls the year back",
			text: "resets dec 30, 11pm (utc)",
			now:  time.Date(2027, 1, 2, 1, 0, 0, 0, time.UTC),
			want: time.Date(2026, 12, 30, 23, 0, 0, 0, time.UTC),
		},
		{
			name: "full month name",
			text: "resets december 30, 11pm",
			now:  time.Date(2026, 12, 29, 1, 0, 0, 0, time.UTC),
			want: time.Date(2026, 12, 30, 23, 0, 0, 0, time.UTC),
		},
		{
			name: "implausibly distant candidate is not trusted",
			text: "resets may 12, 9pm (utc)",
			now:  time.Date(2026, 7, 17, 14, 0, 0, 0, time.UTC),
			want: time.Time{},
		},
		{
			name: "unknown month falls through to the bare clock",
			text: "resets 9pm",
			now:  time.Date(2026, 7, 17, 14, 0, 0, 0, time.UTC),
			want: time.Date(2026, 7, 17, 21, 0, 0, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseResetHint(tt.text, tt.now); !got.Equal(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestParseResetHint_Exported asserts the exported wrapper reports
// parse success separately, so a caller can distinguish "no hint" from
// "hint that happens to be the zero time" and log the degraded path.
func TestParseResetHint_Exported(t *testing.T) {
	now := time.Date(2026, 7, 27, 6, 0, 0, 0, time.UTC)
	got, ok := ParseResetHint("You've hit your weekly limit · resets Jul 28, 9pm (UTC)", now)
	if !ok {
		t.Fatal("ok = false, want true for a dated weekly notice")
	}
	if want := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if _, ok := ParseResetHint("node \"synthesize\" execution failed: something else", now); ok {
		t.Fatal("ok = true for a message with no reset hint, want false")
	}
}
