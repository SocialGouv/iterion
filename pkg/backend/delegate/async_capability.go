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
	// Remedy, when set, replaces the generic closing advice with what to do
	// about THIS capability. A node with no fallback shows this text and
	// nothing else, so a refusal whose actionable half lived somewhere the
	// operator never sees ("route it to claude_code, or run unsandboxed")
	// reads as a dead end.
	Remedy string
}

func (e *ErrCapabilityUnsupported) Error() string {
	remedy := "choose a backend and transport that support this capability"
	if e.Remedy != "" {
		remedy = e.Remedy
	}
	return fmt.Sprintf("node %q: backend %q cannot serve %s — %s", e.NodeID, e.Backend, e.Capability, remedy)
}

func (*ClaudeCodeBackend) SupportsAsyncQuestions() bool { return true }
func (*PiRPCBackend) SupportsAsyncQuestions() bool      { return true }

func (b *PiBackend) SupportsAsyncQuestions() bool {
	return b.rpc != nil && !strings.EqualFold(strings.TrimSpace(os.Getenv("ITERION_PI_MODE")), "print")
}
