package model

import (
	"fmt"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	clawrt "github.com/SocialGouv/claw-code-go/pkg/runtime"
)

// toolPairSanitizeStats records structural repairs without retaining any
// model- or tool-produced content. It is safe to expose through logs.
type toolPairSanitizeStats struct {
	ToolUsesRemoved       int
	ToolResultsRemoved    int
	MessagesRemoved       int
	ProviderBlocksRemoved int
}

func (s toolPairSanitizeStats) removedBlocks() int {
	return s.ToolUsesRemoved + s.ToolResultsRemoved + s.ProviderBlocksRemoved
}

type toolBlockRef struct {
	message int
	block   int
}

// sanitizeToolPairs returns a provider-neutral conversation in which every
// tool_result has exactly one earlier tool_use and every tool_use has exactly
// one later tool_result. allowedPending names the only tool_use IDs that may
// intentionally remain unanswered (the ask_user pause seam).
//
// Pairing is block-based rather than message-based: providers may group all
// results in one user message or project them as one role=tool message per
// result. A message whose every block is removed disappears as well.
func sanitizeToolPairs(messages []api.Message, allowedPending map[string]struct{}, stripProviderBlocks bool) ([]api.Message, toolPairSanitizeStats) {
	if len(messages) == 0 {
		return messages, toolPairSanitizeStats{}
	}

	keep := make([][]bool, len(messages))
	pending := make(map[string]toolBlockRef)
	stats := toolPairSanitizeStats{}
	for mi, message := range messages {
		keep[mi] = make([]bool, len(message.Content))
		for bi, block := range message.Content {
			keep[mi][bi] = true
			ref := toolBlockRef{message: mi, block: bi}
			switch block.Type {
			case "tool_use":
				if block.ID == "" {
					keep[mi][bi] = false
					stats.ToolUsesRemoved++
					continue
				}
				if previous, exists := pending[block.ID]; exists {
					keep[previous.message][previous.block] = false
					stats.ToolUsesRemoved++
				}
				pending[block.ID] = ref
			case "tool_result":
				if block.ToolUseID == "" {
					keep[mi][bi] = false
					stats.ToolResultsRemoved++
					continue
				}
				if _, exists := pending[block.ToolUseID]; !exists {
					keep[mi][bi] = false
					stats.ToolResultsRemoved++
					continue
				}
				delete(pending, block.ToolUseID)
			case "thinking", "redacted_thinking":
				if stripProviderBlocks {
					keep[mi][bi] = false
					stats.ProviderBlocksRemoved++
				}
			}
		}
	}

	for id, ref := range pending {
		if _, allowed := allowedPending[id]; allowed {
			continue
		}
		keep[ref.message][ref.block] = false
		stats.ToolUsesRemoved++
	}

	if stats.removedBlocks() == 0 {
		return messages, stats
	}
	out := make([]api.Message, 0, len(messages))
	for mi, message := range messages {
		blocks := make([]api.ContentBlock, 0, len(message.Content))
		for bi, block := range message.Content {
			if keep[mi][bi] {
				blocks = append(blocks, block)
			}
		}
		if len(blocks) == 0 {
			stats.MessagesRemoved++
			continue
		}
		message.Content = blocks
		out = append(out, message)
	}
	return out, stats
}

// validateToolPairs is the final request-boundary invariant. Repair belongs
// earlier in the pipeline so persisted state is healed too; this guard makes a
// future call site fail locally instead of sending malformed history upstream.
func validateToolPairs(messages []api.Message) error {
	pending := make(map[string]struct{})
	for _, message := range messages {
		for _, block := range message.Content {
			switch block.Type {
			case "tool_use":
				if block.ID == "" {
					return fmt.Errorf("tool transcript invariant: tool_use has an empty id")
				}
				if _, duplicate := pending[block.ID]; duplicate {
					return fmt.Errorf("tool transcript invariant: duplicate pending tool_use id %q", block.ID)
				}
				pending[block.ID] = struct{}{}
			case "tool_result":
				if block.ToolUseID == "" {
					return fmt.Errorf("tool transcript invariant: tool_result has an empty tool_use_id")
				}
				if _, exists := pending[block.ToolUseID]; !exists {
					return fmt.Errorf("tool transcript invariant: tool_result %q has no earlier tool_use", block.ToolUseID)
				}
				delete(pending, block.ToolUseID)
			}
		}
	}
	for id := range pending {
		return fmt.Errorf("tool transcript invariant: tool_use %q has no later tool_result", id)
	}
	return nil
}

// compactMessagesToolSafe is Iterion's single boundary around claw's pure
// compactor. claw deliberately slices by raw message count; the sanitizer
// restores the provider protocol invariant without growing the retained
// window, so forced compaction still guarantees shrink under a tight budget.
func compactMessagesToolSafe(messages []api.Message, cfg clawrt.CompactionConfig, allowedPending map[string]struct{}) *clawrt.CompactionResult {
	res := clawrt.CompactMessages(messages, cfg)
	if res == nil {
		return nil
	}
	res.CompactedMessages, _ = sanitizeToolPairs(res.CompactedMessages, allowedPending, false)
	return res
}
