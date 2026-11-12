package runtime

import (
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"testing"
)

type cancellationOwnedRun struct {
	sandbox.Run
	owned bool
}

func (r *cancellationOwnedRun) OwnsCommandCancellation() bool { return r.owned }
func TestChildDecoratorPreservesCancellationOwnership(t *testing.T) {
	for _, owned := range []bool{false, true} {
		base := &cancellationOwnedRun{owned: owned}
		wrapped := withChildSandboxPath(base, "/private/bin")
		if sandbox.OwnsCommandCancellation(wrapped) != owned {
			t.Fatalf("decorator changed ownership=%v", owned)
		}
	}
}
