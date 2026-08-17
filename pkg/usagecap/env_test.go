package usagecap

import (
	"strings"
	"testing"
	"time"
)

func TestFromEnv(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Policy
	}{
		{
			name: "unset is inert",
			env:  nil,
			want: Policy{
				FiveHour: WindowPolicy{Mode: DefaultFiveHourMode},
				Week:     WindowPolicy{Mode: DefaultWeekMode},
			},
		},
		{
			// The shipped recommendation: two variables, and each window
			// gets the posture it deserves without saying so.
			name: "percentages alone pick soft 5h / hard week",
			env:  map[string]string{EnvFiveHour: "85", EnvWeek: "75"},
			want: Policy{
				FiveHour: WindowPolicy{MaxPercent: 85, Mode: ModeSoft},
				Week:     WindowPolicy{MaxPercent: 75, Mode: ModeHard},
			},
		},
		{
			name: "modes are overridable per window",
			env: map[string]string{
				EnvFiveHour: "90", EnvFiveMode: "hard",
				EnvWeek: "70", EnvWeekMode: "soft",
			},
			want: Policy{
				FiveHour: WindowPolicy{MaxPercent: 90, Mode: ModeHard},
				Week:     WindowPolicy{MaxPercent: 70, Mode: ModeSoft},
			},
		},
		{
			name: "a percent sign is tolerated",
			env:  map[string]string{EnvWeek: "75%"},
			want: Policy{
				FiveHour: WindowPolicy{Mode: DefaultFiveHourMode},
				Week:     WindowPolicy{MaxPercent: 75, Mode: DefaultWeekMode},
			},
		},
		{
			// The kill switch: one variable disarms both caps without
			// having to remember what the percentages were.
			name: "the master switch wins over configured caps",
			env:  map[string]string{EnvEnabled: "OFF", EnvFiveHour: "85", EnvWeek: "75"},
			want: Policy{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			got, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if got != tt.want {
				t.Errorf("FromEnv() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// A typo must not read as "no cap": every wrong answer here fails open, and
// a guard silently disabled is the failure this package exists to prevent.
func TestFromEnv_MalformedIsAnError(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"not a number", map[string]string{EnvWeek: "seventy-five"}, EnvWeek},
		{"above 100", map[string]string{EnvWeek: "175"}, EnvWeek},
		{"negative", map[string]string{EnvFiveHour: "-5"}, EnvFiveHour},
		{"unknown mode", map[string]string{EnvWeek: "75", EnvWeekMode: "hrad"}, EnvWeekMode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			got, err := FromEnv()
			if err == nil {
				t.Fatalf("FromEnv() = %+v, nil — a malformed cap must be refused, not ignored", got)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name the offending variable %q", err, tt.want)
			}
			if got.Enabled() {
				t.Error("a refused policy must not come back armed")
			}
		})
	}
}

func TestParseMode(t *testing.T) {
	for _, in := range []string{"soft", "SOFT", " soft "} {
		if got, err := ParseMode(in, ModeHard); err != nil || got != ModeSoft {
			t.Errorf("ParseMode(%q) = %q, %v", in, got, err)
		}
	}
	if got, _ := ParseMode("", ModeHard); got != ModeHard {
		t.Errorf("empty should fall back to the supplied default, got %q", got)
	}
	if _, err := ParseMode("nope", ModeHard); err == nil {
		t.Error("want an error on an unknown mode")
	}
}

func TestMemStore(t *testing.T) {
	ctx := t.Context()
	s := NewMemStore()
	key := Key("claude_code", ScopePlatform)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	if got, err := s.Latest(ctx, "unknown"); err != nil || len(got) != 0 {
		t.Fatalf("Latest(unknown) = %v, %v — an unknown key means 'nothing learned', not an error", got, err)
	}
	if err := s.Record(ctx, key, Reading{Window: WindowSevenDay, Utilization: 0.5, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	// Several pods observe the same credential at once and the network does
	// not preserve order: a late-arriving OLD reading must not undo a newer
	// one, or a fleet could talk itself back under the cap.
	if err := s.Record(ctx, key, Reading{Window: WindowSevenDay, Utilization: 0.1, ObservedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Latest(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Utilization != 0.5 {
		t.Fatalf("Latest = %+v, want the newer 0.5 reading to survive", got)
	}
	if err := s.Record(ctx, key, Reading{Window: WindowFiveHour, Utilization: 0.2, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Latest(ctx, key); len(got) != 2 {
		t.Fatalf("Latest = %+v, want one reading per window", got)
	}
}

func TestKeyScoping(t *testing.T) {
	// A tenant's own subscription is a different meter from the
	// deployment's: merging them would let one tenant's spend park
	// another's runs.
	if Key("claude_code", TenantScope("t1")) == Key("claude_code", ScopePlatform) {
		t.Fatal("tenant and platform credentials must not share a key")
	}
	if Key("claude_code", TenantScope("")) != Key("claude_code", ScopePlatform) {
		t.Fatal("a run with no tenant falls back to the platform meter")
	}
}
