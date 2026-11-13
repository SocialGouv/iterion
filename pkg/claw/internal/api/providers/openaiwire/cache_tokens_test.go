package openaiwire

import (
	"encoding/json"
	"strings"
	"testing"
)

// LiteLLM and the OpenAI-compatible hosts report cache-read tokens inside
// the usage block as prompt_tokens_details.cached_tokens (chat-completions
// shape). The tracker only ever learns a cache read when the wire decodes
// it: a run against deepseek-v4.1-flash through LiteLLM serves most of its
// prompt from cache, and iterion's meter credits that cache only when this
// package carries the count into the UsageDelta.
func TestUsageDecodesPromptTokensDetailsCachedTokens(t *testing.T) {
	var u Usage
	if err := json.Unmarshal([]byte(`{
		"prompt_tokens": 100,
		"completion_tokens": 5,
		"total_tokens": 105,
		"prompt_tokens_details": {"cached_tokens": 80}
	}`), &u); err != nil {
		t.Fatalf("unmarshal usage: %v", err)
	}
	if u.PromptTokens != 100 || u.CompletionTokens != 5 {
		t.Fatalf("counters = %d/%d, want 100/5", u.PromptTokens, u.CompletionTokens)
	}
	if u.PromptTokensDetails == nil || u.PromptTokensDetails.CachedTokens != 80 {
		t.Fatalf("cached tokens = %+v, want 80", u.PromptTokensDetails)
	}
	if !u.Counted() {
		t.Fatal("a usage block with a cache detail but no counter would report nothing; counters are present here, so Counted must be true")
	}
}

// A host that omits the details object entirely decodes to no cache
// account — distinct from an explicit zero, which is a real account of no
// cache read.
func TestUsageWithoutPromptTokensDetailsHasNoCacheAccount(t *testing.T) {
	var u Usage
	if err := json.Unmarshal([]byte(`{"prompt_tokens": 10, "completion_tokens": 2}`), &u); err != nil {
		t.Fatalf("unmarshal usage: %v", err)
	}
	if u.PromptTokensDetails != nil {
		t.Fatalf("details = %+v, want nil when the host omits the block", u.PromptTokensDetails)
	}
}

// The terminal usage chunk is the only frame the chat-completions endpoint
// reports prompt counts on, so it is also the only place a cache read can
// ride: the delta the stream emits must carry the cached count through.
func TestStreamEvents_CarryCachedTokensIntoTheUsageDelta(t *testing.T) {
	const cachedUsageFrame = `data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,"prompt_tokens_details":{"cached_tokens":80}}}` + "\n\n"
	events := streamOver(stringsReader(contentFrame + finishFrame + cachedUsageFrame))
	u := finalUsage(t, events)
	if u.InputTokens != 100 || u.OutputTokens != 5 {
		t.Fatalf("counters = %d/%d, want 100/5", u.InputTokens, u.OutputTokens)
	}
	if u.InputTokensDetails.CachedTokens != 80 {
		t.Fatalf("cached tokens = %d, want 80 carried into the delta", u.InputTokensDetails.CachedTokens)
	}
	if !u.Reported {
		t.Fatal("delta must stay reported when the usage chunk named counters")
	}
}

// A usage chunk with no details keeps the delta at a zero cache count, so
// consumers can take the field unconditionally.
func TestStreamEvents_UsageWithoutDetailsLeavesCacheAtZero(t *testing.T) {
	events := streamOver(stringsReader(contentFrame + finishFrame + usageFrame))
	u := finalUsage(t, events)
	if u.InputTokensDetails.CachedTokens != 0 {
		t.Fatalf("cached tokens = %d, want 0 when the host omits the details", u.InputTokensDetails.CachedTokens)
	}
}

func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }
