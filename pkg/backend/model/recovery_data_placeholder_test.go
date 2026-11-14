package model

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// echoingRepairClient is a repair model doing what a repair model does: it
// keeps the failed command, fixes its failing tail, and copies what the
// command printed into a file of its own — every placeholder the prompt
// showed it goes back into the command the agent sink runs.
type echoingRepairClient struct {
	mu      sync.Mutex
	prompts []string
}

func (c *echoingRepairClient) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	text := req.Messages[0].Content[0].Text
	c.mu.Lock()
	c.prompts = append(c.prompts, text)
	c.mu.Unlock()
	failed := between(text, "FAILED COMMAND:\n", "\n\nSTDOUT:")
	stdout := between(text, "STDOUT:\n", "\n\nSTDERR:")
	corrected := strings.Replace(failed, "; exit 1", "; printf '%s' '"+stdout+"' > echoed.txt; touch repaired.txt", 1)
	b, err := json.Marshal(map[string]any{"corrected_command": corrected})
	if err != nil {
		return nil, err
	}
	evs := toolUseEvents("t1", "structured_output", string(b), 10, 10)
	ch := make(chan api.StreamEvent, len(evs))
	for _, ev := range evs {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func (c *echoingRepairClient) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.prompts...)
}

func between(s, from, to string) string {
	i := strings.Index(s, from)
	if i < 0 {
		return ""
	}
	s = s[i+len(from):]
	if j := strings.Index(s, to); j >= 0 {
		return s[:j]
	}
	return s
}

// The self-repair rung shows a model the failed recipe and runs what it
// answers through the agent sink. A placeholder that arrived through data is
// text in the recipe (ADR-146, amendment of 2026-11-14), so the repair model
// must not be able to turn it into the value by echoing it: the failed
// command it reads carries such a placeholder defused, and so does the
// output the recipe printed. The author's own reference stays live — the
// repaired command still reaches the value the recipe reached.
func TestASelfRepairCannotMaterialiseADataPlaceholder(t *testing.T) {
	t.Parallel()
	g := secretguard.New([]secretguard.Secret{{Name: "pw", Value: bodyTestValue}}, secretguard.DefaultConfig())
	ph := secretguard.PlaceholderForName("pw")
	defused := strings.TrimSuffix(ph, "__") + "_redacted__"
	dir := t.TempDir()
	reg := NewRegistry()
	client := &echoingRepairClient{}
	reg.Register("anthropic", func(string) (api.APIClient, error) { return client, nil })
	exec := newTestClawExecutor(reg, &ir.Workflow{}, WithWorkDir(dir), WithSecretGuard(g))
	body := "printf '%s' {{secrets.pw}} > own.txt; printf '%s' {{input.answer}} > data.txt; printf '%s' {{input.answer}}; exit 1"
	refs, err := ir.ParseRefs(body)
	if err != nil {
		t.Fatal(err)
	}
	node := &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "publish"},
		Command:       body,
		CommandRefs:   refs,
		Goal:          "publish the answer",
		Postcondition: "test -f repaired.txt",
		Policy:        ir.PolicyRecover,
		// A recovery model named, so the rung does not ask the default's
		// funding of the environment: the registry is a mock.
		Recovery: &ir.RecoverySpec{MaxRepairAttempts: 1, Model: "anthropic/claude-test"},
	}
	answer := "the key is " + ph
	if _, err := exec.Execute(context.Background(), node, map[string]any{"answer": answer}); err != nil {
		t.Fatalf("Execute: %v — the repaired command did not run", err)
	}
	prompts := client.seen()
	if len(prompts) != 1 {
		t.Fatalf("the repair model was asked %d times, want once", len(prompts))
	}
	if strings.Contains(prompts[0], bodyTestValue) {
		t.Errorf("the repair prompt carries the value")
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("the repaired command did not write %s: %v", name, err)
		}
		return string(b)
	}
	// The failed command the model read: the data's placeholder defused.
	if got := read("data.txt"); got != "the key is "+defused {
		t.Errorf("data.txt = %q after the repair, want the data's placeholder as the defused text %q", got, "the key is "+defused)
	}
	// The output the model read: the placeholder the recipe printed, defused.
	if got := read("echoed.txt"); got != "the key is "+defused {
		t.Errorf("echoed.txt = %q after the repair, want the printed placeholder as the defused text %q", got, "the key is "+defused)
	}
	// The author's own reference, shown live, still resolves.
	if got := read("own.txt"); got != bodyTestValue {
		t.Errorf("own.txt = %q after the repair, want the value the author's reference names", got)
	}
}

// The recovery agent (rung 4) is a model too, whose tool calls are the agent
// sink: what it is handed of the node — the input the recipe rendered, the
// recipe's own text — carries every placeholder that is no reference
// defused, so echoing one back resolves nothing.
func TestTheRecoveryAgentReceivesDataPlaceholdersDefused(t *testing.T) {
	t.Parallel()
	g := secretguard.New([]secretguard.Secret{{Name: "pw", Value: bodyTestValue}}, secretguard.DefaultConfig())
	ph := secretguard.PlaceholderForName("pw")
	defused := strings.TrimSuffix(ph, "__") + "_redacted__"
	reg := NewRegistry()
	mock := newMockClient(textEvents("done", 10, 5))
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	exec := newTestClawExecutor(reg, &ir.Workflow{}, WithWorkDir(t.TempDir()), WithSecretGuard(g))
	body := "printf '%s' {{input.answer}} > /dev/null; echo typed " + ph + " >/dev/null; exit 1"
	refs, err := ir.ParseRefs(body)
	if err != nil {
		t.Fatal(err)
	}
	node := &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "publish"},
		Command:       body,
		CommandRefs:   refs,
		Goal:          "publish the answer",
		Postcondition: "test -f never.txt",
		Policy:        ir.PolicyRecover,
		Recovery:      &ir.RecoverySpec{MaxAgentAttempts: 1, Model: "anthropic/claude-test"},
	}
	input := map[string]any{"answer": "the key is " + ph, "nested": map[string]any{"list": []any{"again " + ph}}}
	if _, err := exec.Execute(context.Background(), node, input); err == nil {
		t.Fatal("the postcondition cannot hold: the node must fail after its recovery agent")
	}
	if input["answer"] != "the key is "+ph {
		t.Errorf("the node's own input was rewritten: %q", input["answer"])
	}
	calls := mock.getCalls()
	if len(calls) == 0 {
		t.Fatal("the recovery agent was never dispatched")
	}
	var seen strings.Builder
	for _, call := range calls {
		for _, msg := range call.Messages {
			for _, block := range msg.Content {
				seen.WriteString(block.Text)
				seen.WriteByte('\n')
			}
		}
	}
	got := seen.String()
	if strings.Contains(got, ph) {
		t.Errorf("the recovery agent read a live placeholder that is no reference:\n%s", got)
	}
	for _, want := range []string{"the key is " + defused, "again " + defused, "typed " + defused} {
		if !strings.Contains(got, want) {
			t.Errorf("the recovery agent's message lacks %q:\n%s", want, got)
		}
	}
}
