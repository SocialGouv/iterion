package usagecap

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func intp(v int) *int { return &v }

// The env fallback is the contract: a deployment that never touches the
// API must behave exactly as its env vars say.
func TestSettingsApply_NilInheritsEnvDefaults(t *testing.T) {
	def := Policy{
		FiveHour: WindowPolicy{MaxPercent: 85, Mode: ModeSoft},
		Week:     WindowPolicy{MaxPercent: 75, Mode: ModeHard},
	}
	var rec *Settings
	if got := rec.Apply(def); got != def {
		t.Fatalf("nil record must inherit env defaults, got %+v", got)
	}
	// A record with no overrides is the same as no record.
	if got := (&Settings{}).Apply(def); got != def {
		t.Fatalf("empty record must inherit env defaults, got %+v", got)
	}
}

func TestSettingsApply_DBOverridesEnv(t *testing.T) {
	def := Policy{
		FiveHour: WindowPolicy{MaxPercent: 85, Mode: ModeSoft},
		Week:     WindowPolicy{MaxPercent: 75, Mode: ModeHard},
	}
	rec := &Settings{FiveHourPct: intp(50)}
	got := rec.Apply(def)
	if got.FiveHour.MaxPercent != 50 {
		t.Fatalf("five-hour override not applied: %+v", got.FiveHour)
	}
	if got.FiveHour.Mode != ModeSoft {
		t.Fatalf("override must keep the env mode, got %q", got.FiveHour.Mode)
	}
	if got.Week != def.Week {
		t.Fatalf("unset field must inherit env, got %+v", got.Week)
	}
	// Zero is a real override: it turns the cap OFF, same as the env var.
	off := (&Settings{WeekPct: intp(0)}).Apply(def)
	if off.Week.Enabled() {
		t.Fatalf("week_pct=0 must disable the week cap, got %+v", off.Week)
	}
}

// The ITERION_USAGE_CAP=off kill switch resolves to a zero Policy (no
// modes); a DB percentage laid over it must stay inert — never silently
// replace an operator's explicit choice.
func TestSettingsApply_KillSwitchWins(t *testing.T) {
	rec := &Settings{FiveHourPct: intp(80), WeekPct: intp(80)}
	got := rec.Apply(Policy{})
	if got.Enabled() {
		t.Fatalf("DB override must not re-arm a killed policy, got %s", got.String())
	}
}

// A percentage set only in the DB, on a deployment with no env caps at
// all, must arm the cap with the window's default posture — FromEnv
// resolves modes (soft/hard) even when no percentage is set.
func TestSettingsApply_DBArmsCapOverEmptyEnv(t *testing.T) {
	t.Setenv(EnvEnabled, "")
	t.Setenv(EnvFiveHour, "")
	t.Setenv(EnvFiveMode, "")
	t.Setenv(EnvWeek, "")
	t.Setenv(EnvWeekMode, "")
	def, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	got := (&Settings{WeekPct: intp(60)}).Apply(def)
	if !got.Week.Enabled() || got.Week.Mode != ModeHard || got.Week.MaxPercent != 60 {
		t.Fatalf("DB-only week cap must arm at 60%%/hard, got %+v", got.Week)
	}
	if got.FiveHour.Enabled() {
		t.Fatalf("five-hour must stay uncapped, got %+v", got.FiveHour)
	}
}

func TestSettingsValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  Settings
		ok   bool
	}{
		{"empty", Settings{}, true},
		{"bounds", Settings{FiveHourPct: intp(0), WeekPct: intp(100)}, true},
		{"negative", Settings{FiveHourPct: intp(-1)}, false},
		{"over", Settings{WeekPct: intp(101)}, false},
	} {
		err := tc.rec.Validate()
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("%s: want validation error", tc.name)
			} else if !strings.Contains(err.Error(), "0–100") {
				t.Errorf("%s: rejection must state the accepted range, got %q", tc.name, err)
			}
		}
	}
}

// failingSettingsStore errors on read after an optional grace count.
type failingSettingsStore struct {
	calls int
	fail  bool
	rec   *Settings
}

func (f *failingSettingsStore) GetSettings(context.Context) (*Settings, error) {
	f.calls++
	if f.fail {
		return nil, errors.New("store down")
	}
	return f.rec, nil
}
func (f *failingSettingsStore) PutSettings(_ context.Context, s Settings) error {
	f.rec = &s
	return nil
}

func TestResolver_EnvOnlyFallback(t *testing.T) {
	def := Policy{FiveHour: WindowPolicy{MaxPercent: 85, Mode: ModeSoft}}
	r := NewResolver(NewMemorySettingsStore(), def)
	if got := r.Effective(context.Background()); got != def {
		t.Fatalf("no record → env defaults, got %+v", got)
	}
	_, origin := r.EffectiveOrigin(context.Background())
	if origin.String() != "env" {
		t.Fatalf("origin must read env, got %q", origin)
	}
}

// An update becomes effective WITHOUT restart, within one TTL window —
// the propagation bound the docs promise.
func TestResolver_UpdatePropagatesWithinTTL(t *testing.T) {
	def := Policy{
		FiveHour: WindowPolicy{MaxPercent: 85, Mode: ModeSoft},
		Week:     WindowPolicy{MaxPercent: 75, Mode: ModeHard},
	}
	store := NewMemorySettingsStore()
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	r := NewResolver(store, def, WithClock(clock))

	if got := r.Effective(context.Background()); got.FiveHour.MaxPercent != 85 {
		t.Fatalf("pre-update: want env 85, got %v", got.FiveHour.MaxPercent)
	}

	if err := store.PutSettings(context.Background(), Settings{FiveHourPct: intp(40)}); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Inside the TTL the cached value is served — that staleness IS the
	// documented propagation bound.
	if got := r.Effective(context.Background()); got.FiveHour.MaxPercent != 85 {
		t.Fatalf("within TTL: want cached 85, got %v", got.FiveHour.MaxPercent)
	}

	// One TTL later the new value is live. No restart happened.
	now = now.Add(DefaultSettingsTTL)
	got, origin := r.EffectiveOrigin(context.Background())
	if got.FiveHour.MaxPercent != 40 {
		t.Fatalf("past TTL: want db 40, got %v", got.FiveHour.MaxPercent)
	}
	if !origin.FiveHourDB || origin.WeekDB {
		t.Fatalf("origin must read five-hour from db only, got %+v", origin)
	}
	if origin.String() != "db+env" {
		t.Fatalf("mixed origin renders db+env, got %q", origin)
	}
}

// Invalidate makes the change visible immediately on the pod that served
// the update.
func TestResolver_InvalidateBypassesTTL(t *testing.T) {
	def := Policy{Week: WindowPolicy{MaxPercent: 75, Mode: ModeHard}}
	store := NewMemorySettingsStore()
	r := NewResolver(store, def)
	if got := r.Effective(context.Background()); got.Week.MaxPercent != 75 {
		t.Fatalf("want env 75, got %v", got.Week.MaxPercent)
	}
	if err := store.PutSettings(context.Background(), Settings{WeekPct: intp(10)}); err != nil {
		t.Fatalf("put: %v", err)
	}
	r.Invalidate()
	if got := r.Effective(context.Background()); got.Week.MaxPercent != 10 {
		t.Fatalf("after Invalidate: want db 10, got %v", got.Week.MaxPercent)
	}
}

// A store failure serves the last-known value (env defaults before any
// success) and re-arms the TTL — one retry per window, not a hammer.
func TestResolver_StoreFailureFailsToLastKnown(t *testing.T) {
	def := Policy{FiveHour: WindowPolicy{MaxPercent: 85, Mode: ModeSoft}}
	store := &failingSettingsStore{fail: true}
	now := time.Unix(1_700_000_000, 0)
	r := NewResolver(store, def, WithClock(func() time.Time { return now }))

	if got := r.Effective(context.Background()); got != def {
		t.Fatalf("failure before first success must serve env, got %+v", got)
	}
	// Within the TTL the failure is not retried.
	r.Effective(context.Background())
	if store.calls != 1 {
		t.Fatalf("wedged store must be consulted once per TTL, got %d calls", store.calls)
	}

	// Store heals with a record; next window picks it up.
	store.fail = false
	store.rec = &Settings{FiveHourPct: intp(30)}
	now = now.Add(DefaultSettingsTTL)
	if got := r.Effective(context.Background()); got.FiveHour.MaxPercent != 30 {
		t.Fatalf("healed store: want 30, got %v", got.FiveHour.MaxPercent)
	}

	// Store fails again: the LAST-KNOWN db value keeps serving.
	store.fail = true
	now = now.Add(DefaultSettingsTTL)
	if got := r.Effective(context.Background()); got.FiveHour.MaxPercent != 30 {
		t.Fatalf("failure after success must serve last-known 30, got %v", got.FiveHour.MaxPercent)
	}
}

// The guard consults its source per evaluation: a cap changed under a
// LIVE guard starts biting without the run restarting.
func TestGuard_LivePolicySource(t *testing.T) {
	def := Policy{FiveHour: WindowPolicy{MaxPercent: 90, Mode: ModeSoft}}
	store := NewMemorySettingsStore()
	now := time.Unix(1_700_000_000, 0)
	r := NewResolver(store, def, WithClock(func() time.Time { return now }))
	g := NewGuardWithSource(r, nil)

	reading := Reading{Window: WindowFiveHour, Utilization: 0.50, Status: StatusAllowed, ObservedAt: now}
	if d := g.Observe(reading); d.Blocked {
		t.Fatalf("50%% under a 90%% cap must pass, got %+v", d)
	}
	if err := store.PutSettings(context.Background(), Settings{FiveHourPct: intp(40)}); err != nil {
		t.Fatalf("put: %v", err)
	}
	now = now.Add(DefaultSettingsTTL)
	if d := g.Observe(reading); !d.Blocked || d.Cap != 40 {
		t.Fatalf("the tightened cap must bite the live guard, got %+v", d)
	}
}
