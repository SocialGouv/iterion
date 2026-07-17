package schedgate

import (
	"strings"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	got := Normalize(Policy{})
	if got.Overlap != OverlapSkip {
		t.Fatalf("Overlap = %q, want %q", got.Overlap, OverlapSkip)
	}
	if got.GuardTimeout != DefaultGuardTimeout.String() {
		t.Fatalf("GuardTimeout = %q, want %q", got.GuardTimeout, DefaultGuardTimeout.String())
	}
	if got.GuardVar != DefaultGuardVar {
		t.Fatalf("GuardVar = %q, want %q", got.GuardVar, DefaultGuardVar)
	}

	explicit := Policy{Overlap: OverlapAllow, MaxConcurrent: 3, Guard: "true", GuardTimeout: "5s", GuardVar: "ctx"}
	if got := Normalize(explicit); got != explicit {
		t.Fatalf("Normalize mutated explicit policy: %+v", got)
	}
	if got := Normalize(Normalize(Policy{})); got != Normalize(Policy{}) {
		t.Fatalf("Normalize not idempotent")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		p       Policy
		wantErr string
	}{
		{"zero value", Policy{}, ""},
		{"skip explicit", Policy{Overlap: OverlapSkip}, ""},
		{"allow bounded", Policy{Overlap: OverlapAllow, MaxConcurrent: 1}, ""},
		{"allow unlimited", Policy{Overlap: OverlapAllow}, ""},
		{"bad overlap", Policy{Overlap: "queue"}, "invalid overlap"},
		{"max with skip", Policy{Overlap: OverlapSkip, MaxConcurrent: 2}, "only valid with overlap=allow"},
		{"max with implicit skip", Policy{MaxConcurrent: 2}, "only valid with overlap=allow"},
		{"negative max", Policy{Overlap: OverlapAllow, MaxConcurrent: -1}, "must be >= 1"},
		{"bad timeout", Policy{GuardTimeout: "5x"}, "invalid guard_timeout"},
		{"good timeout", Policy{GuardTimeout: "90s"}, ""},
		{"whitespace guard_var", Policy{GuardVar: "a b"}, "must not contain whitespace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.p)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate(%+v) = %v, want nil", tt.p, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate(%+v) = %v, want error containing %q", tt.p, err, tt.wantErr)
			}
		})
	}
}

func TestGuardTimeoutDuration(t *testing.T) {
	if d := (Policy{}).GuardTimeoutDuration(); d != DefaultGuardTimeout {
		t.Fatalf("zero policy timeout = %s, want default", d)
	}
	if d := (Policy{GuardTimeout: "2s"}).GuardTimeoutDuration(); d != 2*time.Second {
		t.Fatalf("timeout = %s, want 2s", d)
	}
	if d := (Policy{GuardTimeout: "-1s"}).GuardTimeoutDuration(); d != DefaultGuardTimeout {
		t.Fatalf("negative timeout = %s, want default", d)
	}
}

func TestEvaluateOverlap(t *testing.T) {
	tests := []struct {
		name         string
		live         []string
		p            Policy
		want         Decision
		wantBlocking string
	}{
		{"skip no live", nil, Policy{}, DecisionFire, ""},
		{"skip one live", []string{"r1"}, Policy{}, DecisionSkipOverlap, "r1"},
		{"skip picks oldest", []string{"r1", "r2"}, Policy{Overlap: OverlapSkip}, DecisionSkipOverlap, "r1"},
		{"allow under cap", []string{"r1", "r2"}, Policy{Overlap: OverlapAllow, MaxConcurrent: 3}, DecisionFire, ""},
		{"allow at cap", []string{"r1", "r2", "r3"}, Policy{Overlap: OverlapAllow, MaxConcurrent: 3}, DecisionSkipOverlap, "r1"},
		{"allow unlimited", make([]string, 100), Policy{Overlap: OverlapAllow}, DecisionFire, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, blocking := EvaluateOverlap(tt.live, tt.p)
			if got != tt.want || blocking != tt.wantBlocking {
				t.Fatalf("EvaluateOverlap = (%v, %q), want (%v, %q)", got, blocking, tt.want, tt.wantBlocking)
			}
		})
	}
}
