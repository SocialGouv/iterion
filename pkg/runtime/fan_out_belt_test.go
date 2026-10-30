package runtime

import (
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// The branch cancel grace reads planted-as-unset, like the rest of the
// liveness family (delegate.envDurationOr's rule): a project `.env`'s
// plant does not move the collector's bound, the operator's export does.
func TestTheBranchCancelGraceReadsPlantedAsUnset(t *testing.T) {
	// The marker travels in the environment once MarkPlanted exports it;
	// clear it by hand before the operator phase — ResetForTest re-reads
	// the marker, so leaving it set would keep the value planted.
	envtrust.ResetForTest()
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv("ITERION_BRANCH_CANCEL_GRACE", "9m")
	envtrust.MarkPlanted("ITERION_BRANCH_CANCEL_GRACE")
	if got := defaultBranchCancelGracePeriod(); got != 5*time.Second {
		t.Errorf("a planted grace moved the bound (%v)", got)
	}
	os.Unsetenv(envtrust.EnvPlantedNames)
	envtrust.ResetForTest()
	if got := defaultBranchCancelGracePeriod(); got != 9*time.Minute {
		t.Errorf("the operator's own grace stopped answering (%v)", got)
	}
}
