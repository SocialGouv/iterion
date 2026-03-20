package goai

import (
	"slices"

	"github.com/zendev-sh/goai/provider"
)

// applyCaching applies cache control markers to system messages only.
// Per arxiv 2601.06007v2: caching system prompts (not conversation messages or
// tool results) saves 41-80% cost and 13-31% latency for agentic workloads.
func applyCaching(msgs []provider.Message) []provider.Message {
	result := slices.Clone(msgs)
	for i := range result {
		if result[i].Role != provider.RoleSystem {
			continue
		}
		if len(result[i].Content) == 0 {
			continue
		}
		// Copy content slice to avoid mutating original.
		content := slices.Clone(result[i].Content)
		lastIdx := len(content) - 1
		content[lastIdx].CacheControl = "ephemeral"
		result[i].Content = content
	}
	return result
}
