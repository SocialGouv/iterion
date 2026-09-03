package delegate

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
)

// The CLI may stream SEVERAL assistant events for ONE API message (one
// per content block), each repeating that message's usage — and later
// events of the same message carry the completed output count. Summing
// naively would multiply the spend; keeping only the last message would
// forget earlier turns. The fold must therefore REPLACE within a message
// id and FOLD across ids.
func TestAccumulateAssistantUsage_DedupsByMessageID(t *testing.T) {
	var sm sessionMeta

	// Turn 1, first event: partial output count.
	cum := sm.accumulateAssistantUsage("msg_1", claudesdk.Usage{
		InputTokens: 1000, OutputTokens: 50, CacheReadInputTokens: 200,
	})
	if cum.InputTokens != 1000 || cum.OutputTokens != 50 {
		t.Fatalf("first sample: %+v", cum)
	}

	// Turn 1, second event (same id): usage UPDATED, not additive.
	cum = sm.accumulateAssistantUsage("msg_1", claudesdk.Usage{
		InputTokens: 1000, OutputTokens: 300, CacheReadInputTokens: 200,
	})
	if cum.InputTokens != 1000 || cum.OutputTokens != 300 {
		t.Fatalf("same-id event must REPLACE, not add: %+v", cum)
	}

	// Turn 2 (new id): turn 1's final sample folds in.
	cum = sm.accumulateAssistantUsage("msg_2", claudesdk.Usage{
		InputTokens: 4000, OutputTokens: 100, CacheCreationInputTokens: 500,
	})
	if cum.InputTokens != 5000 || cum.OutputTokens != 400 {
		t.Fatalf("new id must fold the previous message's final usage: %+v", cum)
	}
	if cum.CacheReadInputTokens != 200 || cum.CacheCreationInputTokens != 500 {
		t.Fatalf("cache classes must carry across turns: %+v", cum)
	}
}
