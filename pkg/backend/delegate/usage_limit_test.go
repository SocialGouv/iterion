package delegate

import (
	"testing"
	"time"
)

func TestClassifyRateLimit(t *testing.T) {
	now := time.Date(2026, 7, 17, 14, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		text      string
		wantKind  string
		wantReset time.Time
	}{
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, reset := classifyRateLimit(tt.text, now)
			if kind != tt.wantKind {
				t.Fatalf("kind = %q, want %q", kind, tt.wantKind)
			}
			if !reset.Equal(tt.wantReset) {
				t.Fatalf("reset = %v, want %v", reset, tt.wantReset)
			}
		})
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
