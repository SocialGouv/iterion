package delegate

import (
	"fmt"
	"os"
	"strings"
)

// AsyncQuestionBackend is the optional capability for backends that carry
// ask_user_async and await_answers. An out-of-tree backend opts in through
// this interface; the executor never infers support from its registered name.
type AsyncQuestionBackend interface {
	SupportsAsyncQuestions() bool
}

// ErrCapabilityUnsupported refuses a declared capability before dispatch.
// It is deterministic: retrying the same backend cannot add the missing tools.
type ErrCapabilityUnsupported struct {
	NodeID, Backend, Capability string
}

func (e *ErrCapabilityUnsupported) Error() string {
	return fmt.Sprintf("node %q: backend %q cannot serve %s — choose a backend and transport that support this capability", e.NodeID, e.Backend, e.Capability)
}

func (*ClaudeCodeBackend) SupportsAsyncQuestions() bool { return true }
func (*PiRPCBackend) SupportsAsyncQuestions() bool      { return true }

func (b *PiBackend) SupportsAsyncQuestions() bool {
	return b.rpc != nil && !strings.EqualFold(strings.TrimSpace(os.Getenv("ITERION_PI_MODE")), "print")
}
