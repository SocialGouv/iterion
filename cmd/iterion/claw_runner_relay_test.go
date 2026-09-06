package main

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
)

// TestClawRunner_RelayHooksCarryTheContainersToolAndLoopEvents (#811):
// the in-container backend executes its builtins, its retries and its
// compaction itself, so the hooks the runner installs must carry them —
// otherwise the launcher's timeline shows LLM steps and nothing else,
// and `iterion fork --turn` has no anchor for the node.
func TestClawRunner_RelayHooksCarryTheContainersToolAndLoopEvents(t *testing.T) {
	runnerStdinR, _ := io.Pipe()
	runnerStdoutR, runnerStdoutW := io.Pipe()
	defer runnerStdinR.Close()

	var stderr strings.Builder
	hooks := relayEventHooks(newProxyDispatcher(runnerStdinR, runnerStdoutW), &stderr)
	for name, wired := range map[string]bool{
		"tool_started":     hooks.OnToolStarted != nil,
		"tool_called":      hooks.OnToolCall != nil,
		"llm_retry":        hooks.OnLLMRetry != nil,
		"llm_compacted":    hooks.OnLLMCompacted != nil,
		"llm_turn_capture": hooks.OnLLMTurnCapture != nil,
	} {
		if !wired {
			t.Fatalf("the runner's claw backend has no %s hook: that observation dies in the container", name)
		}
	}

	go func() {
		hooks.OnToolStarted("campaign", model.LLMToolStartedInfo{
			ToolName: "bash", ToolUseID: "tu_1", InputSize: 12, Input: json.RawMessage(`{"command":"ls"}`),
		})
		hooks.OnToolCall("campaign", model.LLMToolCallInfo{
			ToolName: "bash", ToolUseID: "tu_1", Output: "main.go\n",
		})
		hooks.OnLLMRetry("campaign", model.RetryInfo{Attempt: 1, Error: errors.New("429"), StatusCode: 429})
		hooks.OnLLMCompacted("campaign", model.LLMCompactInfo{BeforeMessages: 40, AfterMessages: 8})
		hooks.OnLLMTurnCapture("campaign", model.LLMTurnCaptureInfo{Step: 1, FinishReason: "tool_use"})
		_ = runnerStdoutW.Close()
	}()

	reader := delegate.NewEnvelopeReader(runnerStdoutR)
	var got []string
	for {
		env, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read envelope: %v", err)
		}
		if env.Type != delegate.EnvelopeEvent {
			t.Fatalf("envelope type = %q, want %q", env.Type, delegate.EnvelopeEvent)
		}
		var ed delegate.EventData
		if err := json.Unmarshal(env.Data, &ed); err != nil {
			t.Fatalf("decode event data: %v", err)
		}
		got = append(got, ed.Type)
	}
	want := []string{"tool_started", "tool_called", "llm_retry", "llm_compacted", "llm_turn_capture"}
	if len(got) != len(want) {
		t.Fatalf("relayed events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("relayed events = %v, want %v", got, want)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing on a healthy channel", stderr.String())
	}
}
