package claudesdk

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// The close grace reads planted-as-unset, like the rest of the liveness
// family (delegate.envDurationOr's rule): the operator's export answers, a
// project `.env`'s plant does not.
func TestTheCloseGraceReadsPlantedAsUnset(t *testing.T) {
	// The marker travels in the environment once MarkPlanted exports it;
	// clear it by hand before the operator phase — ResetForTest re-reads
	// the marker, so leaving it set would keep the value planted.
	envtrust.ResetForTest()
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv("ITERION_CLAUDE_CODE_CLOSE_GRACE", "0")
	envtrust.MarkPlanted("ITERION_CLAUDE_CODE_CLOSE_GRACE")
	if got := resolveDuration("ITERION_CLAUDE_CODE_CLOSE_GRACE", defaultCloseGraceTimeout); got != defaultCloseGraceTimeout {
		t.Errorf("a planted close grace read through (%v)", got)
	}
	os.Unsetenv(envtrust.EnvPlantedNames)
	envtrust.ResetForTest()
	if got := resolveDuration("ITERION_CLAUDE_CODE_CLOSE_GRACE", defaultCloseGraceTimeout); got != 0 {
		t.Errorf("the operator's own 0 stopped answering (%v)", got)
	}
}
