package delegate

import (
	"regexp"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// zaiCapture is a refusal the z.ai facade returned in production, with the
// instant the run recorded it at. The request id that closes the text starts
// with the facade's own clock (yyyymmddhhmmss), which is how the zone of the
// reset instant beside it is known: both are written on the same clock.
type zaiCapture struct {
	name       string
	text       string
	receivedAt time.Time
	wantReset  time.Time
	wantWindow usagecap.Window
	wantDated  bool
}

var zaiCaptures = []zaiCapture{
	{
		// A bko Vetty run, 2026-11-03: a 5-hour refusal mid-window.
		// Read as UTC its instant would sit 8h18m after the refusal — longer
		// than the 5-hour window it names.
		name:       "5-hour window refused mid-window",
		text:       "API Error: Request rejected (429) · [1308][Usage limit reached for 5 hour. Your limit will reset at 2026-11-04 03:36:15][202611040318268475472f054a4764]",
		receivedAt: time.Date(2026, 11, 3, 19, 18, 26, 0, time.UTC),
		wantReset:  time.Date(2026, 11, 3, 19, 36, 15, 0, time.UTC),
		wantWindow: "",
		wantDated:  true,
	},
	{
		// Run 01a1a626 (bko Revi #62), 2026-11-04: the weekly wall.
		name:       "weekly/monthly wall",
		text:       "API Error: Request rejected (429) · [1310][Weekly/Monthly Limit Exhausted. Your limit will reset at 2026-11-08 01:20:00][2026110418514406fe99ff3c144072]",
		receivedAt: time.Date(2026, 11, 4, 10, 51, 44, 0, time.UTC),
		wantReset:  time.Date(2026, 11, 7, 17, 20, 0, 0, time.UTC),
		wantWindow: usagecap.WindowSevenDay,
		wantDated:  true,
	},
}

var zaiRequestClockRe = regexp.MustCompile(`\]\[(\d{14})[0-9a-f]+\]$`)

// The captures are evidence only while their request ids still say what
// the zone rests on: the facade's clock reads UTC+8 at the moment the run
// received the refusal.
func TestZaiCapturesCarryTheFacadeClock(t *testing.T) {
	for _, c := range zaiCaptures {
		m := zaiRequestClockRe.FindStringSubmatch(c.text)
		if m == nil {
			t.Fatalf("%s: no request id clock in %q", c.name, c.text)
		}
		clock, err := time.ParseInLocation("20060102150405", m[1], zaiResetZone)
		if err != nil {
			t.Fatalf("%s: request id clock %q: %v", c.name, m[1], err)
		}
		if !clock.Equal(c.receivedAt) {
			t.Errorf("%s: the request id reads %s in UTC+8, the run received the refusal at %s — the fixture no longer shows the facade's zone",
				c.name, clock.UTC().Format(time.RFC3339), c.receivedAt.Format(time.RFC3339))
		}
	}
}

func TestZaiResetInstantIsReadInTheFacadeZone(t *testing.T) {
	for _, c := range zaiCaptures {
		t.Run(c.name, func(t *testing.T) {
			kind, window, reset := classifyRateLimit(c.text, c.receivedAt)
			if kind != RateLimitKindUsageWindow {
				t.Fatalf("kind = %q, want %q", kind, RateLimitKindUsageWindow)
			}
			if window != c.wantWindow {
				t.Fatalf("window = %q, want %q", window, c.wantWindow)
			}
			if !reset.Equal(c.wantReset) {
				t.Fatalf("reset = %s, want %s — the facade writes its instant in UTC+8; read as UTC the wall stands 8h past the real reset",
					reset.Format(time.RFC3339), c.wantReset.Format(time.RFC3339))
			}
			at, dated, ok := ParseResetHintDated(c.text, c.receivedAt)
			if !ok || !at.Equal(c.wantReset) || dated != c.wantDated {
				t.Fatalf("ParseResetHintDated = (%s, dated=%v, ok=%v), want (%s, dated=%v, ok=true)",
					at.Format(time.RFC3339), dated, ok, c.wantReset.Format(time.RFC3339), c.wantDated)
			}
		})
	}
}

// A 5-hour refusal at the very start of the window prints now+5h on the
// facade's clock. Only read in that zone does it match its own duration.
func TestZaiRenderingAtTheWindowStartIsTheDurationNotADate(t *testing.T) {
	now := time.Date(2026, 11, 3, 14, 36, 15, 0, time.UTC)
	text := "API Error: Request rejected (429) · [1308][Usage limit reached for 5 hour. Your limit will reset at 2026-11-04 03:36:15][202611032236158475472f054a4764]"
	at, dated, ok := ParseResetHintDated(text, now)
	if !ok {
		t.Fatal("no hint parsed")
	}
	if want := now.Add(5 * time.Hour); !at.Equal(want) {
		t.Fatalf("at = %s, want %s", at.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if dated {
		t.Fatal("dated = true: the instant is the 5-hour duration rendered on the facade's clock, not a wall of its own")
	}
}

// The zone the text writes or names wins over every default; a zone-less
// absolute outside the facade's sentence keeps UTC.
func TestResetInstantHonoursTheZoneTheTextGives(t *testing.T) {
	summer := time.Date(2026, 7, 17, 14, 0, 0, 0, time.UTC)
	winter := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		text   string
		now    time.Time
		want   time.Time
		wantOK bool
	}{
		{
			name:   "an RFC 3339 Z instant is UTC",
			text:   "monthly_run_quota — monthly run quota (5) exhausted (resets at 2026-10-01T00:00:00Z)",
			now:    time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC),
			want:   time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "a numeric offset on the instant is honoured",
			text:   "quota exhausted, resets at 2026-10-01T02:00:00+02:00",
			now:    time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC),
			want:   time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "a written offset beats the facade sentence",
			text:   "Usage limit reached for 5 hour. Your limit will reset at 2026-11-04 03:36:15+00:00",
			now:    time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC),
			want:   time.Date(2026, 11, 4, 3, 36, 15, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "a zone named after the facade sentence beats its default",
			text:   "Usage limit reached for 5 hour. Your limit will reset at 2026-11-04 03:36:15 (UTC)",
			now:    time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC),
			want:   time.Date(2026, 11, 4, 3, 36, 15, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "a zone-less absolute outside the facade sentence stays UTC",
			text:   "limit hit, resets at 2026-10-01 00:00",
			now:    time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC),
			want:   time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "a named zone on a bare clock, summer time",
			text:   "You've hit your weekly limit · resets 9pm (Europe/Paris)",
			now:    summer,
			want:   time.Date(2026, 7, 17, 19, 0, 0, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "a named zone on a bare clock, winter time",
			text:   "You've hit your weekly limit · resets 9pm (Europe/Paris)",
			now:    winter,
			want:   time.Date(2026, 11, 5, 20, 0, 0, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "a named-zone bare clock already past rolls to the next local day",
			text:   "You've hit your limit · resets 8am (Europe/Paris)",
			now:    winter, // 09:00 in Paris
			want:   time.Date(2026, 11, 6, 7, 0, 0, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "a named zone on a dated reset across the spring change",
			text:   "You've hit your weekly limit · resets Mar 29, 9pm (Europe/Paris)",
			now:    time.Date(2026, 3, 27, 12, 0, 0, 0, time.UTC),
			want:   time.Date(2026, 3, 29, 19, 0, 0, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "a named UTC stays UTC",
			text:   "You've hit your session limit · resets 10:30am (UTC)",
			now:    summer,
			want:   time.Date(2026, 7, 18, 10, 30, 0, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "a zone this build cannot load is no hint, not an instant hours off",
			text:   "You've hit your weekly limit · resets 9pm (Mars/Olympus_Mons)",
			now:    summer,
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at, ok := ParseResetHint(tt.text, tt.now)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v (at %s), want %v", ok, at.Format(time.RFC3339), tt.wantOK)
			}
			if ok && !at.Equal(tt.want) {
				t.Fatalf("at = %s, want %s", at.Format(time.RFC3339), tt.want.Format(time.RFC3339))
			}
			if ok && at.Location() != time.UTC {
				t.Fatalf("at is in %s: the parser answers in UTC", at.Location())
			}
		})
	}
}
