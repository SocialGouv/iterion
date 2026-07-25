package github

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// Both the admin client AND the App client (the production path) must satisfy
// the merge-gate commit-status capability.
var _ forge.CommitStatusClient = (*AdminClient)(nil)
var _ forge.CommitStatusClient = (*AppClient)(nil)

func TestSetCommitStatusInterface(t *testing.T) {
	// Compile-time assertion above is the real check; this keeps the file a
	// valid test unit.
}
