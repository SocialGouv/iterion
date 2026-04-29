package runtime

import (
	"github.com/SocialGouv/claw-code-go/pkg/api"

	internalrt "github.com/SocialGouv/claw-code-go/internal/runtime"
)

// CompactionConfig is re-exported from internal/runtime so external
// consumers (e.g. iterion) can apply the same heuristic-based message
// reduction the in-process ConversationLoop uses.
type CompactionConfig = internalrt.CompactionConfig

// CompactionResult is the typed result of a pure compaction.
type CompactionResult = internalrt.CompactionResult

// DefaultCompactionConfig returns the same defaults as the internal
// ConversationLoop (preserve last 4 messages, compact when estimated
// tokens >= 10 000).
func DefaultCompactionConfig() CompactionConfig {
	return internalrt.DefaultCompactionConfig()
}

// CompactMessages applies the pure-function compactor to the given
// message list. Returns nil when no compaction is needed (the list is
// short enough), otherwise a CompactionResult whose CompactedMessages
// can replace the original list.
//
// This is the public façade for hosts that maintain their own session
// state outside the in-process ConversationLoop.
func CompactMessages(messages []api.Message, cfg CompactionConfig) *CompactionResult {
	return internalrt.CompactSessionPure(messages, cfg)
}
