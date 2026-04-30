package runtime

import (
	"github.com/SocialGouv/claw-code-go/internal/apikit"
	internalrt "github.com/SocialGouv/claw-code-go/internal/runtime"
	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// CompactionConfig is re-exported from internal/runtime so external
// consumers (e.g. iterion) can apply the same heuristic-based message
// reduction the in-process ConversationLoop uses.
type CompactionConfig = internalrt.CompactionConfig

// CompactionResult is the typed result of a pure compaction.
type CompactionResult = internalrt.CompactionResult

// DefaultCompactionThresholdRatio is the default fraction of a model's
// context window at which compaction is triggered. Matches the in-process
// ConversationLoop's stateful behavior and biases towards "compact only
// near the real limit, never on message-count alone".
const DefaultCompactionThresholdRatio = 0.85

// DefaultCompactionPreserveRecent is the default count of recent messages
// kept verbatim by the pure compactor.
const DefaultCompactionPreserveRecent = 4

// DefaultCompactionConfig returns the legacy defaults (preserve last 4
// messages, compact when estimated tokens >= 10 000). Kept for backwards
// compatibility with consumers that don't pass a model name. New callers
// should use DefaultCompactionConfigForModel instead.
func DefaultCompactionConfig() CompactionConfig {
	return internalrt.DefaultCompactionConfig()
}

// DefaultCompactionConfigForModel returns a CompactionConfig sized to the
// given model's context window. The estimated-token threshold is set to
// `ratio × ContextWindowTokens` (default 0.85 when ratio <= 0). preserveRecent
// of 0 falls back to 4. For models unknown to the registry, the legacy
// 10 000-token threshold is used so that callers always get safe behavior.
//
// Pass ratio = 0 and preserveRecent = 0 to use the built-in defaults.
func DefaultCompactionConfigForModel(model string, ratio float64, preserveRecent int) CompactionConfig {
	if ratio <= 0 {
		ratio = DefaultCompactionThresholdRatio
	}
	if preserveRecent <= 0 {
		preserveRecent = DefaultCompactionPreserveRecent
	}
	cfg := CompactionConfig{
		PreserveRecentMessages: preserveRecent,
		MaxEstimatedTokens:     internalrt.DefaultCompactionConfig().MaxEstimatedTokens,
	}
	if model != "" {
		if limit := apikit.ModelTokenLimitForModel(model); limit != nil && limit.ContextWindowTokens > 0 {
			cfg.MaxEstimatedTokens = int(float64(limit.ContextWindowTokens) * ratio)
		}
	}
	return cfg
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
