package retrypolicy

import (
	"testing"
	"time"
)

func TestNormalizeIsIdempotentAndFillsDefaults(t *testing.T) {
	once := Normalize(Policy{})
	if once.UsageWindow != UsageWindowResume {
		t.Errorf("usage_window = %q, want %q", once.UsageWindow, UsageWindowResume)
	}
	if once.MaxAttempts != DefaultMaxAttempts {
		t.Errorf("max_attempts = %d, want %d", once.MaxAttempts, DefaultMaxAttempts)
	}
	if once.MaxWaitDuration() != DefaultMaxWait {
		t.Errorf("max_wait = %v, want %v", once.MaxWaitDuration(), DefaultMaxWait)
	}
	if once.JitterDuration() != DefaultJitter {
		t.Errorf("jitter = %v, want %v", once.JitterDuration(), DefaultJitter)
	}
	if twice := Normalize(once); twice != once {
		t.Errorf("Normalize not idempotent: %+v vs %+v", twice, once)
	}
}

func TestNormalizePreservesExplicitValues(t *testing.T) {
	p := Normalize(Policy{UsageWindow: UsageWindowOff, MaxAttempts: 2, MaxWait: "3h", Jitter: "0s"})
	if p.UsageWindow != UsageWindowOff || p.MaxAttempts != 2 {
		t.Errorf("explicit fields overwritten: %+v", p)
	}
	if p.MaxWaitDuration() != 3*time.Hour {
		t.Errorf("max_wait = %v, want 3h", p.MaxWaitDuration())
	}
	// An explicit zero jitter is meaningful (never spread) and must survive.
	if p.JitterDuration() != 0 {
		t.Errorf("jitter = %v, want 0", p.JitterDuration())
	}
}

func TestEnabled(t *testing.T) {
	tests := []struct {
		name string
		p    Policy
		want bool
	}{
		{"zero value retries", Policy{}, true},
		{"explicit resume", Policy{UsageWindow: UsageWindowResume}, true},
		{"off disables", Policy{UsageWindow: UsageWindowOff}, false},
		{"off wins over an attempt count", Policy{UsageWindow: UsageWindowOff, MaxAttempts: 9}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.Enabled(); got != tt.want {
				t.Errorf("Enabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		p       Policy
		wantErr bool
	}{
		{"zero value is legal", Policy{}, false},
		{"resume with bounds", Policy{UsageWindow: UsageWindowResume, MaxAttempts: 3, MaxWait: "36h", Jitter: "1m"}, false},
		{"off is legal", Policy{UsageWindow: UsageWindowOff}, false},
		{"zero jitter is legal", Policy{Jitter: "0s"}, false},
		{"unknown usage_window", Policy{UsageWindow: "retry"}, true},
		{"negative attempts", Policy{MaxAttempts: -1}, true},
		{"unparseable max_wait", Policy{MaxWait: "soon"}, true},
		{"zero max_wait", Policy{MaxWait: "0s"}, true},
		{"unparseable jitter", Policy{Jitter: "a bit"}, true},
		{"negative jitter", Policy{Jitter: "-1m"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.p)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate(%+v) error = %v, wantErr %v", tt.p, err, tt.wantErr)
			}
		})
	}
}

// TestAccessorsAreTotal pins that a value Validate would have rejected still
// yields a usable duration at runtime, so a corrupted row can never panic or
// produce a zero wait that hot-loops the sweeper.
func TestAccessorsAreTotal(t *testing.T) {
	bad := Policy{MaxWait: "not-a-duration", Jitter: "also-not"}
	if got := bad.MaxWaitDuration(); got != DefaultMaxWait {
		t.Errorf("MaxWaitDuration() = %v, want the default %v", got, DefaultMaxWait)
	}
	if got := bad.JitterDuration(); got != DefaultJitter {
		t.Errorf("JitterDuration() = %v, want the default %v", got, DefaultJitter)
	}
}

// TestResolveIsPerField is the core of the configurability contract: each
// layer wins only the fields it actually sets, so a schedule pinning one
// field cannot silently discard the bot's choice on another.
func TestResolveIsPerField(t *testing.T) {
	got, src := Resolve(
		Layer{Source: SourceRunOverride, Policy: Policy{MaxAttempts: 1}},
		Layer{Source: SourceSchedule, Policy: Policy{MaxWait: "36h"}},
		Layer{Source: SourceBot, Policy: Policy{UsageWindow: UsageWindowOff, MaxAttempts: 7, MaxWait: "200h"}},
		Layer{Source: SourceEnv, Policy: Policy{Jitter: "2m"}},
	)

	if got.MaxAttempts != 1 {
		t.Errorf("max_attempts = %d, want 1 (run override wins)", got.MaxAttempts)
	}
	if got.MaxWaitDuration() != 36*time.Hour {
		t.Errorf("max_wait = %v, want 36h (schedule wins over bot)", got.MaxWaitDuration())
	}
	if got.UsageWindow != UsageWindowOff {
		t.Errorf("usage_window = %q, want %q (only the bot set it)", got.UsageWindow, UsageWindowOff)
	}
	if got.JitterDuration() != 2*time.Minute {
		t.Errorf("jitter = %v, want 2m (only env set it)", got.JitterDuration())
	}

	want := map[string]string{
		"max_attempts": SourceRunOverride,
		"max_wait":     SourceSchedule,
		"usage_window": SourceBot,
		"jitter":       SourceEnv,
	}
	for field, wantSrc := range want {
		if src[field] != wantSrc {
			t.Errorf("provenance[%s] = %q, want %q", field, src[field], wantSrc)
		}
	}
}

func TestResolveEmptyLayersDoNotOverwrite(t *testing.T) {
	got, src := Resolve(
		Layer{Source: SourceRunOverride, Policy: Policy{}},
		Layer{Source: SourceSchedule, Policy: Policy{}},
		Layer{Source: SourceBot, Policy: Policy{MaxAttempts: 4}},
	)
	if got.MaxAttempts != 4 {
		t.Errorf("max_attempts = %d, want 4 (empty higher layers must not win)", got.MaxAttempts)
	}
	if src["max_attempts"] != SourceBot {
		t.Errorf("provenance = %q, want %q", src["max_attempts"], SourceBot)
	}
	// Fields nobody set are attributed to the default, not to the last layer.
	if src["usage_window"] != SourceDefault {
		t.Errorf("provenance[usage_window] = %q, want %q", src["usage_window"], SourceDefault)
	}
}

func TestResolveNoLayersYieldsDefaults(t *testing.T) {
	got, src := Resolve()
	if got != Normalize(Policy{}) {
		t.Errorf("Resolve() = %+v, want the normalized zero value", got)
	}
	for _, f := range []string{"usage_window", "max_attempts", "max_wait", "jitter"} {
		if src[f] != SourceDefault {
			t.Errorf("provenance[%s] = %q, want %q", f, src[f], SourceDefault)
		}
	}
}

// TestClampOnlyLowers is the multi-tenant safeguard: the platform ceiling
// must be able to cut a tenant's request down, and must never raise it.
func TestClampOnlyLowers(t *testing.T) {
	ceiling := Ceiling{MaxAttempts: 3, MaxWait: 24 * time.Hour}

	src := map[string]string{"max_attempts": SourceSchedule, "max_wait": SourceSchedule}
	lowered := Clamp(Policy{MaxAttempts: 50, MaxWait: "720h"}, ceiling, src)
	if lowered.MaxAttempts != 3 {
		t.Errorf("max_attempts = %d, want 3 (clamped)", lowered.MaxAttempts)
	}
	if lowered.MaxWaitDuration() != 24*time.Hour {
		t.Errorf("max_wait = %v, want 24h (clamped)", lowered.MaxWaitDuration())
	}
	if src["max_attempts"] != SourceCeiling || src["max_wait"] != SourceCeiling {
		t.Errorf("clamped fields must be attributed to the ceiling, got %v", src)
	}

	// Below the ceiling: untouched, and provenance is preserved.
	src2 := map[string]string{"max_attempts": SourceBot, "max_wait": SourceBot}
	kept := Clamp(Policy{MaxAttempts: 2, MaxWait: "2h"}, ceiling, src2)
	if kept.MaxAttempts != 2 || kept.MaxWaitDuration() != 2*time.Hour {
		t.Errorf("a policy under the ceiling must be untouched, got %+v", kept)
	}
	if src2["max_attempts"] != SourceBot || src2["max_wait"] != SourceBot {
		t.Errorf("provenance must survive a no-op clamp, got %v", src2)
	}
}

func TestClampNeverEnablesADisabledPolicy(t *testing.T) {
	got := Clamp(Policy{UsageWindow: UsageWindowOff}, Ceiling{MaxAttempts: 9, MaxWait: time.Hour}, nil)
	if got.Enabled() {
		t.Error("a ceiling must not re-enable a policy the owner turned off")
	}
}

func TestClampWithZeroCeilingIsANoOp(t *testing.T) {
	p := Normalize(Policy{MaxAttempts: 50, MaxWait: "720h"})
	if got := Clamp(p, Ceiling{}, nil); got != p {
		t.Errorf("Clamp with a zero ceiling changed the policy: %+v vs %+v", got, p)
	}
	if !(Ceiling{}).IsZero() {
		t.Error("zero Ceiling should report IsZero")
	}
}

func TestClampAcceptsNilProvenance(t *testing.T) {
	// The sweeper clamps without tracking provenance; a nil map must not panic.
	got := Clamp(Policy{MaxAttempts: 50}, Ceiling{MaxAttempts: 2}, nil)
	if got.MaxAttempts != 2 {
		t.Errorf("max_attempts = %d, want 2", got.MaxAttempts)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv(EnvUsageWindow, "off")
	t.Setenv(EnvMaxAttempts, "2")
	t.Setenv(EnvMaxWait, "12h")
	t.Setenv(EnvJitter, "30s")

	p := FromEnv()
	if p.UsageWindow != UsageWindowOff || p.MaxAttempts != 2 || p.MaxWait != "12h" || p.Jitter != "30s" {
		t.Errorf("FromEnv() = %+v, want the env values", p)
	}
	if err := Validate(p); err != nil {
		t.Errorf("Validate(FromEnv()) = %v, want nil", err)
	}
}

// TestFromEnvIgnoresGarbage pins that a typo in an env var falls through to
// the package defaults rather than disabling retries or poisoning Validate
// far from the variable's source.
func TestFromEnvIgnoresGarbage(t *testing.T) {
	t.Setenv(EnvMaxAttempts, "lots")
	t.Setenv(EnvMaxWait, "whenever")
	t.Setenv(EnvJitter, "-5m")

	p := FromEnv()
	if p.MaxAttempts != 0 || p.MaxWait != "" || p.Jitter != "" {
		t.Errorf("FromEnv() = %+v, want unparseable values dropped to empty", p)
	}
	if got := Normalize(p).MaxAttempts; got != DefaultMaxAttempts {
		t.Errorf("normalized max_attempts = %d, want the default %d", got, DefaultMaxAttempts)
	}
}

func TestCeilingFromEnv(t *testing.T) {
	t.Setenv(EnvCeilingMaxAttempts, "4")
	t.Setenv(EnvCeilingMaxWait, "48h")
	c := CeilingFromEnv()
	if c.MaxAttempts != 4 || c.MaxWait != 48*time.Hour {
		t.Errorf("CeilingFromEnv() = %+v, want {4 48h}", c)
	}

	t.Setenv(EnvCeilingMaxAttempts, "0")
	t.Setenv(EnvCeilingMaxWait, "")
	if c := CeilingFromEnv(); !c.IsZero() {
		t.Errorf("CeilingFromEnv() = %+v, want a zero ceiling", c)
	}
}
