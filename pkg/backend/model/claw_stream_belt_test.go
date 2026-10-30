package model

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// The claw stream timeouts read planted-as-unset, like the rest of the
// liveness family (delegate.envDurationOr's rule).
func TestTheClawStreamTimeoutsReadPlantedAsUnset(t *testing.T) {
	// The marker travels in the environment once MarkPlanted exports it;
	// clear it by hand before the operator phase — ResetForTest re-reads
	// the marker, so leaving it set would keep the value planted.
	envtrust.ResetForTest()
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv("ITERION_CLAW_STREAM_IDLE_TIMEOUT", "0")
	envtrust.MarkPlanted("ITERION_CLAW_STREAM_IDLE_TIMEOUT")
	if got := resolveClawStreamTimeout("ITERION_CLAW_STREAM_IDLE_TIMEOUT", defaultClawStreamIdleTimeout); got != defaultClawStreamIdleTimeout {
		t.Errorf("a planted 0 read through (%v): the idle watchdog could be disarmed by a file", got)
	}
	os.Unsetenv(envtrust.EnvPlantedNames)
	envtrust.ResetForTest()
	if got := resolveClawStreamTimeout("ITERION_CLAW_STREAM_IDLE_TIMEOUT", defaultClawStreamIdleTimeout); got != 0 {
		t.Errorf("the operator's own 0 stopped answering (%v)", got)
	}
}
