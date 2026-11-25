package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// regexEmptinessRefusal is PLIAGE's answer when vLLM's grammar engine cannot
// compile a tool schema under a forced tool_choice (qwen3.6, a hostname
// pattern with length bounds).
func regexEmptinessRefusal() error {
	return &api.APIError{Provider: "openai", StatusCode: 500, Message: "litellm.APIConnectionError: APIConnectionError: OpenAIException - Grammar error: Unable to determine if regex is empty:  (And (SearchRegex /^x$/) (Regex /(?s:.{1,253})/))"}
}

// forcedChoiceGateway refuses every request that forces a tool choice with
// refusal, and serves the others from scripts in order.
type forcedChoiceGateway struct {
	mu      sync.Mutex
	refusal error
	scripts [][]api.StreamEvent
	calls   []api.CreateMessageRequest
}

func (g *forcedChoiceGateway) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, req)
	if req.ToolChoice != nil {
		return nil, g.refusal
	}
	if len(g.scripts) == 0 {
		return nil, errors.New("forcedChoiceGateway: no more scripts")
	}
	script := g.scripts[0]
	g.scripts = g.scripts[1:]
	ch := make(chan api.StreamEvent, len(script))
	for _, ev := range script {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func (g *forcedChoiceGateway) requests() []api.CreateMessageRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]api.CreateMessageRequest(nil), g.calls...)
}

func gatewayGeneration(prompt []api.Message, executed *int, refusals *[]error) GenerationOptions {
	return GenerationOptions{
		Model: "openai_compatible/scaleway/qwen3.6-35b-a3b", MaxSteps: 4, ForceInitialToolUse: true,
		Messages: prompt,
		Tools: []GenerationTool{{Name: "read_image", InputSchema: json.RawMessage(`{"type":"object"}`), Execute: func(context.Context, json.RawMessage) (string, error) {
			*executed++
			return "schema.png", nil
		}}},
		OnForcedToolChoiceRefused: func(err error) { *refusals = append(*refusals, err) },
	}
}

func TestGrammarRefusedForcedChoiceRetriesTheTurnByInstruction(t *testing.T) {
	t.Parallel()
	gw := &forcedChoiceGateway{refusal: regexEmptinessRefusal(), scripts: [][]api.StreamEvent{
		toolUseEvents("c1", "read_image", `{}`, 11, 7),
		textEvents("PORTAIL-7 -> BASE-QX", 11, 7),
	}}
	prompt := []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Open schema.png"}}}}
	executed := 0
	var refusals []error
	out, err := GenerateTextDirect(context.Background(), gw, gatewayGeneration(prompt, &executed, &refusals))
	if err != nil {
		t.Fatalf("generation: %v", err)
	}
	if out.Text != "PORTAIL-7 -> BASE-QX" || executed != 1 {
		t.Fatalf("text=%q executed=%d, want the answer after one tool run", out.Text, executed)
	}
	calls := gw.requests()
	if len(calls) != 3 {
		t.Fatalf("requests = %d, want the refused one, the retried turn and the turn after the tool", len(calls))
	}
	if calls[0].ToolChoice == nil || calls[0].ToolChoice.Type != "any" {
		t.Fatalf("first request choice = %+v, want the forced choice", calls[0].ToolChoice)
	}
	for i, c := range calls[1:] {
		if c.ToolChoice != nil {
			t.Errorf("request %d after the refusal forces %+v again", i+1, c.ToolChoice)
		}
	}
	retried := calls[1].Messages
	if len(retried) != 1 || retried[0].Role != "user" {
		t.Fatalf("retried turn = %+v, want the one user message", retried)
	}
	if c := retried[0].Content; len(c) != 2 || c[0].Text != "Open schema.png" || c[1].Text != toolUseInstruction {
		t.Errorf("retried user content = %+v, want the prompt then the tool-use instruction", c)
	}
	if len(refusals) != 1 || !isGrammarRefusal(refusals[0]) {
		t.Errorf("refusals reported = %v, want the one grammar refusal", refusals)
	}
	if len(prompt[0].Content) != 1 {
		t.Errorf("the caller's prompt was extended in place: %+v", prompt[0].Content)
	}
}

// Once refused, the forced choice stays off for the whole generation: a turn
// the operator's late message adds before any tool ran asks again by
// instruction, not by a choice the gateway already refused.
func TestGrammarRefusedForcedChoiceStaysOffForLaterTurns(t *testing.T) {
	t.Parallel()
	gw := &forcedChoiceGateway{refusal: regexEmptinessRefusal(), scripts: [][]api.StreamEvent{
		textEvents("let me think", 11, 7),
		toolUseEvents("c1", "read_image", `{}`, 11, 7),
		textEvents("done", 11, 7),
	}}
	executed := 0
	var refusals []error
	opts := gatewayGeneration([]api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Open schema.png"}}}}, &executed, &refusals)
	opts.Inbox = &charStubInbox{queued: []string{"look at the arrow too"}}
	if _, err := GenerateTextDirect(context.Background(), gw, opts); err != nil {
		t.Fatalf("generation: %v", err)
	}
	calls := gw.requests()
	if len(calls) != 4 {
		t.Fatalf("requests = %d, want refused, retried, operator turn, after tool", len(calls))
	}
	for i, c := range calls[1:] {
		if c.ToolChoice != nil {
			t.Errorf("request %d forces %+v after the refusal", i+1, c.ToolChoice)
		}
	}
	if len(refusals) != 1 || executed != 1 {
		t.Errorf("refusals=%d executed=%d, want one refusal and one tool run", len(refusals), executed)
	}
}

// The instruction replaces the forced choice, not the contract: an answer
// with no tool run is refused, as the forced choice would have prevented it.
func TestGrammarRefusedForcedChoiceStillRequiresAnExecutedTool(t *testing.T) {
	t.Parallel()
	gw := &forcedChoiceGateway{refusal: regexEmptinessRefusal(), scripts: [][]api.StreamEvent{textEvents("from priors", 11, 7)}}
	executed := 0
	var refusals []error
	_, err := GenerateTextDirect(context.Background(), gw, gatewayGeneration(
		[]api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Open schema.png"}}}}, &executed, &refusals))
	if err == nil || !strings.Contains(err.Error(), "before executing a required initial tool") {
		t.Fatalf("err = %v, want the executed-tool refusal", err)
	}
}

// Only a grammar refusal of a forced choice changes the request: any other
// failure surfaces as it came, after one request.
func TestForcedChoiceRetryIsForGrammarRefusalsOnly(t *testing.T) {
	t.Parallel()
	prompt := func() []api.Message {
		return []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Open schema.png"}}}}
	}
	t.Run("another gateway error", func(t *testing.T) {
		t.Parallel()
		upstream := &api.APIError{Provider: "openai", StatusCode: 500, Message: "upstream connect error", Retryable: true}
		gw := &forcedChoiceGateway{refusal: upstream}
		executed := 0
		var refusals []error
		_, err := GenerateTextDirect(context.Background(), gw, gatewayGeneration(prompt(), &executed, &refusals))
		if !errors.Is(err, upstream) || len(gw.requests()) != 1 || len(refusals) != 0 {
			t.Fatalf("err=%v requests=%d refusals=%d, want the upstream error after one request", err, len(gw.requests()), len(refusals))
		}
	})
	t.Run("no forced choice", func(t *testing.T) {
		t.Parallel()
		gw := &unconditionalRefusal{err: regexEmptinessRefusal()}
		executed := 0
		var refusals []error
		opts := gatewayGeneration(prompt(), &executed, &refusals)
		opts.ForceInitialToolUse = false
		_, err := GenerateTextDirect(context.Background(), gw, opts)
		if !isGrammarRefusal(err) || gw.n != 1 || len(refusals) != 0 {
			t.Fatalf("err=%v requests=%d refusals=%d, want the refusal after one request", err, gw.n, len(refusals))
		}
	})
}

type unconditionalRefusal struct {
	mu  sync.Mutex
	n   int
	err error
}

func (u *unconditionalRefusal) StreamResponse(context.Context, api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.n++
	return nil, u.err
}

func TestWithToolUseInstructionKeepsRolesAlternating(t *testing.T) {
	t.Parallel()
	// Spare capacity behind the trailing content: an in-place append would
	// write into the caller's array without changing the caller's length.
	backing := make([]api.ContentBlock, 1, 4)
	backing[0] = api.ContentBlock{Type: "tool_result", ToolUseID: "c1"}
	toolTurn := []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "go"}}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: "c1", Name: "read"}}},
		{Role: "user", Content: backing},
	}
	got := withToolUseInstruction(toolTurn)
	if len(got) != 3 || len(got[2].Content) != 2 || got[2].Content[1].Text != toolUseInstruction {
		t.Fatalf("got %+v, want the instruction inside the trailing user message", got)
	}
	if len(toolTurn[2].Content) != 1 {
		t.Error("the input's trailing message was replaced")
	}
	if backing[:2][1].Text != "" {
		t.Error("the instruction was written into the caller's content array")
	}
	afterAssistant := []api.Message{{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}}
	if got := withToolUseInstruction(afterAssistant); len(got) != 2 || got[1].Role != "user" {
		t.Fatalf("got %+v, want a user message after the assistant", got)
	}
}

func TestApplyHooksReportsAForcedChoiceRefusalAsAnLLMRetry(t *testing.T) {
	t.Parallel()
	var got []RetryInfo
	opts := GenerationOptions{}
	applyHooks("voir", 0, EventHooks{OnLLMRetry: func(_ string, info RetryInfo) { got = append(got, info) }}, &opts)
	if opts.OnForcedToolChoiceRefused == nil {
		t.Fatal("OnForcedToolChoiceRefused not wired to OnLLMRetry")
	}
	refusal := regexEmptinessRefusal()
	opts.OnForcedToolChoiceRefused(refusal)
	if len(got) != 1 || got[0].StatusCode != 500 || !errors.Is(got[0].Error, refusal) {
		t.Fatalf("llm_retry = %+v, want the refusal with its status", got)
	}
	if !strings.Contains(got[0].Error.Error(), "tool_choice auto") {
		t.Errorf("llm_retry error %q does not name the switch", got[0].Error)
	}
}
