package retrycoord

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestKeyPrefersWorkflowRevision(t *testing.T) {
	got := Key(&store.Run{WorkflowHash: "abc", WorkflowName: "demo"})
	if got != "workflow:abc" {
		t.Fatalf("key = %q", got)
	}
	if got := Key(&store.Run{WorkflowName: "demo"}); got != "workflow-name:demo" {
		t.Fatalf("legacy key = %q", got)
	}
}

func TestCircuitConfigFromEnv(t *testing.T) {
	t.Setenv(EnvThreshold, "4")
	t.Setenv(EnvCooldown, "2m")
	c := FromEnv()
	if c.Threshold != 4 || c.Cooldown != 2*time.Minute {
		t.Fatalf("config = %+v", c)
	}
}

// TestCircuitConfigRejectsBadEnvLoudly: a value the parser cannot use keeps
// the default AND is reported. Silently running the default on a knob meant
// to be tuned against a live provider is how a deployment believes itself
// configured for hours — `ITERION_RETRY_CIRCUIT_COOLDOWN=15` (no unit) is
// the shape an operator actually types.
func TestCircuitConfigRejectsBadEnvLoudly(t *testing.T) {
	defaults := Config{Threshold: DefaultThreshold, Cooldown: DefaultCooldown}
	tests := []struct {
		name      string
		env       map[string]string
		want      Config
		wantNotes int
	}{
		{"cooldown without a unit", map[string]string{EnvCooldown: "15"}, defaults, 1},
		{"threshold is not a number", map[string]string{EnvThreshold: "three"}, defaults, 1},
		// Neither knob is an off switch, so a non-positive value is a
		// mistake to report, not an instruction to obey.
		{"non-positive values", map[string]string{EnvThreshold: "0", EnvCooldown: "0s"}, defaults, 2},
		{"unset is not a problem", map[string]string{}, defaults, 0},
		{"usable values are taken silently", map[string]string{EnvThreshold: "7", EnvCooldown: "90s"},
			Config{Threshold: 7, Cooldown: 90 * time.Second}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, notes := configFromEnv(func(k string) string { return tc.env[k] })
			if got != tc.want {
				t.Errorf("config = %+v, want %+v", got, tc.want)
			}
			if len(notes) != tc.wantNotes {
				t.Errorf("%d rejection notes, want %d: %v", len(notes), tc.wantNotes, notes)
			}
		})
	}
}
