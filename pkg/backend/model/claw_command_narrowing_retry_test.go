package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// narrowingFailClient scripts StreamResponse calls: each entry is either an
// event sequence or an error (the mock client's scripts cannot fail a call).
type narrowingFailClient struct {
	scripts []narrowingScript
	calls   []api.CreateMessageRequest
}

type narrowingScript struct {
	events []api.StreamEvent
	err    error
}

func (m *narrowingFailClient) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	m.calls = append(m.calls, req)
	if len(m.scripts) == 0 {
		return nil, fmt.Errorf("narrowingFailClient: no more scripts")
	}
	s := m.scripts[0]
	m.scripts = m.scripts[1:]
	if s.err != nil {
		return nil, s.err
	}
	ch := make(chan api.StreamEvent, len(s.events))
	for _, ev := range s.events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// requestText flattens every text a request's messages carry — the
// tool_result text nests one block deep.
func requestText(req api.CreateMessageRequest) string {
	var walk func(c api.ContentBlock) string
	walk = func(c api.ContentBlock) string {
		if c.Type == "text" {
			return c.Text
		}
		var s string
		for _, inner := range c.Content {
			s += walk(inner)
		}
		return s
	}
	var b strings.Builder
	for _, m := range req.Messages {
		for _, c := range m.Content {
			b.WriteString(walk(c))
		}
	}
	return b.String()
}

// #2905 LOW-2: attempt 1 invokes a command whose allowed-tools narrows the
// scope, then dies on a retryable 429; the retryLoop retries under the SAME
// pinned scope. Here NO session store is wired, so attempt 2's replay starts
// from the bare prompt — the shape in which the refusal named /audit, a
// command attempt 2's conversation never invoked. The bound must still hold
// (fail-closed) AND the retried conversation must carry the re-stated
// notice, so the model can place the command the refusal names.
//
// Mutant: drop the EnsureNarrowingNotice wiring in GenerateTextDirect and
// the notice assertion reddens while the bound still holds — the refusal
// the model cannot explain is exactly the pre-fix behaviour.
func TestRetryAfterNarrowingRestatesTheBound(t *testing.T) {
	t.Parallel()
	command := delegate.ToolDef{
		Name:        "command",
		Description: "invoke a workspace command",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
			scope := tool.CommandScopeFrom(ctx)
			scope.Apply("audit", []string{"Read"})
			return "Audit the diff.\n", nil
		},
	}
	bashRan := false
	bash := delegate.ToolDef{
		Name:        "bash",
		Description: "shell",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute:     func(context.Context, json.RawMessage) (string, error) { bashRan = true; return "RAN", nil },
	}
	read := delegate.ToolDef{
		Name:        "read_file",
		Description: "read",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute:     func(context.Context, json.RawMessage) (string, error) { return "contents", nil },
	}

	mock := &narrowingFailClient{scripts: []narrowingScript{
		// Attempt 1: invoke the narrowing command, then the next model call
		// dies retryable.
		{events: toolUseEvents("tu_1", "command", `{"name":"audit"}`, 10, 5)},
		{err: &APIError{StatusCode: 429, IsRetryable: true}},
		// Attempt 2: the model tries bash (outside the narrowing), then
		// answers.
		{events: toolUseEvents("tu_2", "bash", `{}`, 10, 5)},
		{events: textEvents("done", 10, 5)},
	}}
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
	b := NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: 2, BackoffBase: time.Millisecond})

	// NO withRuntimeContext: no store, so the retry replays the bare prompt.
	res, err := b.Execute(context.Background(), delegate.Task{
		NodeID: "n", Model: "test/test-model", UserPrompt: "go",
		HasTools: true, ToolDefs: []delegate.ToolDef{command, bash, read}, ToolMaxSteps: 5,
	})
	if err != nil {
		t.Fatalf("the retry must recover: %v", err)
	}
	if res.Output["text"] != "done" {
		t.Fatalf("output = %v", res.Output)
	}
	if len(mock.calls) != 4 {
		t.Fatalf("calls = %d, want 4", len(mock.calls))
	}

	// The bound held across the retry: bash was refused, never run.
	if bashRan {
		t.Fatal("the retry ran a tool the first attempt narrowed away")
	}
	if out := requestText(mock.calls[3]); !strings.Contains(out, `"bash" is not in allowed-tools for command "audit"`) {
		t.Errorf("attempt 2's bash was refused naming the command: %q", out)
	}

	// And the refusal is one the model can PLACE: attempt 2's first request
	// carries the re-stated bound.
	if out := requestText(mock.calls[2]); !strings.Contains(out, "narrowed your tool surface") {
		t.Errorf("attempt 2's conversation never shows the narrowing it runs under: %q", out)
	}
}
