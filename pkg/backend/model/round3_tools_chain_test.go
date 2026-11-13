package model

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A claw AGENT (tools + schema) whose loop ends on prose and whose
// structured recovery pass cannot parse: does the declared rescue serve,
// as it does for a tool-less claw node?
func TestR3_ClawToolsParseFallbackReachesRescue(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	var scripts [][]api.StreamEvent
	for i := 0; i < 6; i++ {
		scripts = append(scripts,
			textEvents("Verdict: approve. Reason: the change is fine.", 200, 20),
			toolUseEvents("x", "structured_output", `{"verdict":tru`, 200, 20),
			toolUseEvents("y", "structured_output", `{"verdict":tru`, 200, 20))
	}
	primary := newMockClient(scripts...)
	rescue := newMockClient(
		textEvents(`{"verdict":true,"reason":"rescued"}`, 30, 10),
		toolUseEvents("ok", "structured_output", `{"verdict":true,"reason":"rescued"}`, 30, 10),
		toolUseEvents("ok2", "structured_output", `{"verdict":true,"reason":"rescued"}`, 30, 10))
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return primary, nil })
	reg.Register("alt", func(string) (api.APIClient, error) { return rescue, nil })
	tr := tool.NewRegistry()
	for _, name := range []string{"read_file", "todo_write"} {
		if err := tr.RegisterBuiltin(name, name, json.RawMessage(`{"type":"object"}`), nil); err != nil {
			t.Fatal(err)
		}
	}
	wf := verdictWorkflow()
	exec := newTestClawExecutor(reg, wf, WithToolRegistry(tr), WithWorkDir(t.TempDir()), WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}))
	node := &ir.AgentNode{
		BaseNode:     ir.BaseNode{ID: "agent"},
		LLMFields:    ir.LLMFields{Backend: "claw", Model: "test/test-model", UserPrompt: "ask"},
		SchemaFields: ir.SchemaFields{OutputSchema: "verdict_schema"},
	}
	node.Tools = []string{"read_file"}
	node.Fallbacks = []ir.Fallback{{Name: "rescue", Backend: "claw", Model: "alt/alt-model"}}
	out, err := exec.Execute(WithRunID(context.Background(), "run-tools"), node, map[string]any{})
	if err != nil || out["_served_by"] != "rescue" || len(rescue.getCalls()) == 0 {
		t.Fatalf("claw tools: out=%v err=%v primary=%d rescue=%d", out, err, len(primary.getCalls()), len(rescue.getCalls()))
	}
}
