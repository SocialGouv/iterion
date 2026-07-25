package forgejo

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// The admin client must satisfy the merge-gate commit-status capability.
var _ forge.CommitStatusClient = (*AdminClient)(nil)

func TestSetCommitStatusInterface(t *testing.T) {
	// Compile-time assertion above is the real check; this keeps the file a
	// valid test unit.
}
