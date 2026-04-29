package model

import "testing"

func TestToLLMStepInfo_PropagatesCacheTokens(t *testing.T) {
	step := StepResult{
		Number: 2,
		Text:   "hello",
		Usage: Usage{
			InputTokens:      100,
			OutputTokens:     50,
			CacheReadTokens:  300,
			CacheWriteTokens: 80,
		},
		FinishReason: FinishStop,
	}

	info := toLLMStepInfo(step)

	if info.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", info.InputTokens)
	}
	if info.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", info.OutputTokens)
	}
	if info.CacheReadTokens != 300 {
		t.Errorf("CacheReadTokens = %d, want 300", info.CacheReadTokens)
	}
	if info.CacheWriteTokens != 80 {
		t.Errorf("CacheWriteTokens = %d, want 80", info.CacheWriteTokens)
	}
	if info.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", info.FinishReason, "stop")
	}
}

func TestToLLMResponseInfo_PropagatesCacheTokens(t *testing.T) {
	resp := ResponseInfo{
		Usage: Usage{
			InputTokens:      200,
			OutputTokens:     40,
			CacheReadTokens:  600,
			CacheWriteTokens: 120,
		},
		FinishReason: FinishStop,
		StatusCode:   200,
	}

	info := toLLMResponseInfo(resp)

	if info.CacheReadTokens != 600 {
		t.Errorf("CacheReadTokens = %d, want 600", info.CacheReadTokens)
	}
	if info.CacheWriteTokens != 120 {
		t.Errorf("CacheWriteTokens = %d, want 120", info.CacheWriteTokens)
	}
}
