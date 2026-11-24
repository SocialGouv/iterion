package delegate

import (
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// The liveness family reads planted-as-unset: the knobs stay bot variables
// (pkg/platformcfg's whyLiveness) and the operator's export answers, but a
// value a project `.env` planted reads as nothing said — the H-10 review
// measured the plant reaching this raw read and parking the anti-deadlock
// tier at `=0`. envtrust.Inherited is the shared chokepoint; every
// duration reader in this package and its siblings goes through it.
func TestTheLivenessFamilyReadsPlantedAsUnset(t *testing.T) {
	// The marker travels in the environment once MarkPlanted exports it;
	// t.Setenv pins and restores it, and the second phase clears it by hand
	// — ResetForTest re-reads the marker, so leaving it set would keep the
	// value planted.
	envtrust.ResetForTest()
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv("ITERION_CLAUDE_CODE_STREAM_IDLE_TIMEOUT", "0")
	envtrust.MarkPlanted("ITERION_CLAUDE_CODE_STREAM_IDLE_TIMEOUT")
	if got := envDurationOr("ITERION_CLAUDE_CODE_STREAM_IDLE_TIMEOUT", defaultStreamHotTimeout); got != defaultStreamHotTimeout {
		t.Errorf("a planted =0 disabled the tier (%v): the plant reached the raw read", got)
	}
	// The same read honours the operator's own export the moment it is not
	// planted: the belt removes the file's authority, not the operator's.
	os.Unsetenv(envtrust.EnvPlantedNames)
	envtrust.ResetForTest()
	if got := envDurationOr("ITERION_CLAUDE_CODE_STREAM_IDLE_TIMEOUT", defaultStreamHotTimeout); got != 0 {
		t.Errorf("the operator's own =0 stopped answering (%v): the belt stripped the operator too", got)
	}
	// The pi twin in this package, same rule.
	t.Setenv("ITERION_PI_SETTLE_GRACE", "0")
	envtrust.MarkPlanted("ITERION_PI_SETTLE_GRACE")
	if got := envDuration("ITERION_PI_SETTLE_GRACE", 30*time.Second); got != 30*time.Second {
		t.Errorf("a planted pi grace read through (%v)", got)
	}
}
