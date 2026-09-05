package boardmongo

import (
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// TestUnleasedClaimHorizon_FromEnv: the horizon is an operator dial with
// a floor, not a constant — a deployment whose rolling window is minutes
// should not wait a day for a stripped claim, and one that sets it below
// a lease must be refused rather than silently clamped.
func TestUnleasedClaimHorizon_FromEnv(t *testing.T) {
	prev := UnleasedClaimHorizon()
	t.Cleanup(func() { unleasedClaimHorizon = prev })

	t.Setenv(UnleasedClaimHorizonEnv, "")
	if d, err := ConfigureUnleasedClaimHorizonFromEnv(); err != nil || d != DefaultUnleasedClaimHorizon {
		t.Fatalf("unset: d=%s err=%v, want the %s default", d, err, DefaultUnleasedClaimHorizon)
	}

	t.Setenv(UnleasedClaimHorizonEnv, "2h")
	d, err := ConfigureUnleasedClaimHorizonFromEnv()
	if err != nil || d != 2*time.Hour || UnleasedClaimHorizon() != 2*time.Hour {
		t.Fatalf("2h: d=%s err=%v in force=%s", d, err, UnleasedClaimHorizon())
	}
	// The arm reads the value in force: a claim untouched for 3h is now a
	// candidate, one untouched for 1h is not.
	cutoff := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	arm := UnleasedArm(cutoff)
	bound := arm["issue.updatedat"].(bson.M)["$lt"].(time.Time)
	if !bound.Equal(cutoff.Add(-2 * time.Hour)) {
		t.Fatalf("UnleasedArm bound = %s, want cutoff-2h", bound)
	}

	for _, bad := range []string{"abc", "0", "-1h", (native.ClaimLeaseDuration - time.Second).String()} {
		t.Setenv(UnleasedClaimHorizonEnv, bad)
		if _, err := ConfigureUnleasedClaimHorizonFromEnv(); err == nil {
			t.Fatalf("%q must be refused, not clamped", bad)
		} else if !strings.Contains(err.Error(), UnleasedClaimHorizonEnv) {
			t.Fatalf("the error must name the variable: %v", err)
		}
		if UnleasedClaimHorizon() != 2*time.Hour {
			t.Fatalf("a refused value must not change the horizon in force: %s", UnleasedClaimHorizon())
		}
	}
}
