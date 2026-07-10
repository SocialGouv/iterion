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

func TestToLLMStepInfo_PropagatesThinking(t *testing.T) {
	step := StepResult{
		Number:   1,
		Thinking: "Let me reason about this.",
		Usage:    Usage{ReasoningTokens: 7, ThinkingMs: 120},
	}

	info := toLLMStepInfo(step)

	if info.Thinking != "Let me reason about this." {
		t.Errorf("Thinking = %q, want the step's thinking text", info.Thinking)
	}
	if info.ReasoningTokens != 7 {
		t.Errorf("ReasoningTokens = %d, want 7", info.ReasoningTokens)
	}
	if info.ThinkingMs != 120 {
		t.Errorf("ThinkingMs = %d, want 120", info.ThinkingMs)
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

// applyHooks must stamp the live loop iteration onto every bridged info
// struct. Before this, the EventHooks log closures read iteration from a
// context captured once at engine.Run, so every claw line was tagged #0
// and the studio's per-(node,iteration) Logs filter never matched them.
func TestApplyHooks_StampsIteration(t *testing.T) {
	const wantIter = 3
	var (
		gotReq     int
		gotStep    int
		gotTurn    int
		gotCompact int
	)
	h := EventHooks{
		OnLLMRequest:     func(_ string, i LLMRequestInfo) { gotReq = i.Iteration },
		OnLLMStepFinish:  func(_ string, i LLMStepInfo) { gotStep = i.Iteration },
		OnLLMTurnCapture: func(_ string, i LLMTurnCaptureInfo) { gotTurn = i.Iteration },
		OnLLMCompacted:   func(_ string, i LLMCompactInfo) { gotCompact = i.Iteration },
	}
	var opts GenerationOptions
	applyHooks("node1", wantIter, h, &opts)

	opts.OnRequest(RequestInfo{})
	opts.OnStepFinish(StepResult{})
	opts.OnTurnCapture(TurnCaptureInfo{})
	opts.OnCompact(CompactInfo{})

	if gotReq != wantIter {
		t.Errorf("OnLLMRequest iteration = %d, want %d", gotReq, wantIter)
	}
	if gotStep != wantIter {
		t.Errorf("OnLLMStepFinish iteration = %d, want %d", gotStep, wantIter)
	}
	if gotTurn != wantIter {
		t.Errorf("OnLLMTurnCapture iteration = %d, want %d", gotTurn, wantIter)
	}
	if gotCompact != wantIter {
		t.Errorf("OnLLMCompacted iteration = %d, want %d", gotCompact, wantIter)
	}
}
