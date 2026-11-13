package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/runtime/task"
)

func newWorkflowLoop(text string) *ConversationLoop {
	return &ConversationLoop{
		Client:       &stubStreamClient{text: text},
		Session:      NewSession(),
		Config:       &Config{Model: "test-model"},
		TaskRegistry: task.NewRegistry(),
	}
}

func runWorkflow(t *testing.T, loop *ConversationLoop, input map[string]any) map[string]any {
	t.Helper()
	out, err := loop.executeWorkflow(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("non-JSON workflow result: %v\n%s", err, out)
	}
	return payload
}

func TestWorkflowPlainScript(t *testing.T) {
	payload := runWorkflow(t, newWorkflowLoop(""), map[string]any{
		"script": "log('starting'); phase('compute'); return 1 + 1;",
	})
	if payload["result"] != float64(2) {
		t.Errorf("result = %v, want 2", payload["result"])
	}
	logs, _ := payload["log"].([]any)
	if len(logs) != 2 || logs[0] != "starting" || !strings.Contains(logs[1].(string), "phase: compute") {
		t.Errorf("log capture wrong: %v", logs)
	}
}

func TestWorkflowArgsAndMetaTolerance(t *testing.T) {
	payload := runWorkflow(t, newWorkflowLoop(""), map[string]any{
		"script": "export const meta = { name: 'x' };\nreturn meta.name + ':' + args.target;",
		"args":   map[string]any{"target": "prod"},
	})
	if payload["result"] != "x:prod" {
		t.Errorf("result = %v, want x:prod", payload["result"])
	}
}

func TestWorkflowAgentAndParallel(t *testing.T) {
	loop := newWorkflowLoop("agent says hi")
	payload := runWorkflow(t, loop, map[string]any{
		"script": `
const results = await parallel([
  () => agent("first task"),
  () => agent("second task", {label: "second"}),
]);
return results.map(r => r.includes("agent says hi"));`,
	})
	got, _ := payload["result"].([]any)
	if len(got) != 2 || got[0] != true || got[1] != true {
		t.Errorf("parallel agent results wrong: %v", payload["result"])
	}
	if payload["agents"] != float64(2) {
		t.Errorf("agents = %v, want 2", payload["agents"])
	}
	// Workflow-spawned agents are registry tasks but must NOT queue
	// per-agent completion reminders.
	if n := len(loop.TaskRegistry.List(nil)); n != 2 {
		t.Errorf("expected 2 registry tasks, got %d", n)
	}
	loop.flushSystemReminders()
	if len(loop.Session.Messages) != 0 {
		t.Errorf("workflow agents must not queue completion reminders: %v", loop.Session.Messages)
	}
}

func TestWorkflowPipelineSemantics(t *testing.T) {
	payload := runWorkflow(t, newWorkflowLoop(""), map[string]any{
		"script": `
return await pipeline([1, 2, 3],
  v => { if (v === 2) throw new Error("boom"); return v * 10; },
  v => v + 1);`,
	})
	got, _ := payload["result"].([]any)
	if len(got) != 3 || got[0] != float64(11) || got[1] != nil || got[2] != float64(31) {
		t.Errorf("pipeline semantics wrong (throw must drop item to null): %v", got)
	}
}

func TestWorkflowErrors(t *testing.T) {
	loop := newWorkflowLoop("")
	if _, err := loop.executeWorkflow(context.Background(), map[string]any{}); err == nil {
		t.Error("missing script must error")
	}
	if _, err := loop.executeWorkflow(context.Background(), map[string]any{"script": "this is ( not js"}); err == nil {
		t.Error("syntax error must surface")
	}
	if _, err := loop.executeWorkflow(context.Background(), map[string]any{"script": "throw new Error('nope')"}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("script throw must surface: %v", err)
	}
	if _, err := loop.executeWorkflow(context.Background(), map[string]any{"script": "return agent('x', {subagent_type: 'ghost'})"}); err == nil {
		t.Error("unknown subagent type must reject the workflow")
	}
}

func TestWorkflowTimeoutInterruptsBusyLoop(t *testing.T) {
	loop := newWorkflowLoop("")
	_, err := loop.executeWorkflow(context.Background(), map[string]any{
		"script":          "for(;;){}",
		"timeout_seconds": float64(1),
	})
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Errorf("busy loop must hit the timeout, got %v", err)
	}
}

// A schema-bearing agent() tells its child to answer through
// structured_output and rejects when the child returned none: a typed result
// is a contract, not a transcript to parse.
func TestWorkflowAgentSchemaRejectsWithoutStructuredOutput(t *testing.T) {
	loop := newWorkflowLoop("prose only, no structured call")
	payload := runWorkflow(t, loop, map[string]any{
		"script": `
try {
  await agent("judge this", {label: "judge", schema: {type: "object", required: ["ok"], properties: {ok: {type: "boolean"}}}});
  return "resolved";
} catch (e) { return String(e); }`,
	})
	got, _ := payload["result"].(string)
	if !strings.Contains(got, "structured_output") {
		t.Fatalf("a schema agent without a structured result must reject, got %q", got)
	}
	tasks := loop.TaskRegistry.List(nil)
	if len(tasks) != 1 || !strings.Contains(tasks[0].Prompt, "structured_output") || !strings.Contains(tasks[0].Prompt, `"required"`) {
		t.Fatalf("the child's prompt must carry the structured-result instruction and the schema: %+v", tasks)
	}
}

// The typed result travels child → parent: a child that called
// structured_output leaves its payload for the parent, keyed by task.
func TestStructuredOutputPayloadReachesTheParent(t *testing.T) {
	child := &ConversationLoop{Session: NewSession(), Config: &Config{AllowImmediateStructuredOutput: true}}
	if _, err := child.executeStructuredOutput(map[string]any{"ok": true, "n": float64(2)}); err != nil {
		t.Fatal(err)
	}
	got := child.lastStructuredOutput()
	if got == nil || got["ok"] != true {
		t.Fatalf("the child must record its structured_output payload, got %v", got)
	}
	parent := &ConversationLoop{Session: NewSession()}
	parent.storeSubagentStructured("t-1", got)
	back, ok := parent.subagentStructuredOutput("t-1")
	if !ok || back["n"] != float64(2) {
		t.Fatalf("the parent must read the child's payload by task id, got %v %v", back, ok)
	}
	if _, ok := parent.subagentStructuredOutput("t-2"); ok {
		t.Fatal("an unknown task has no structured result")
	}
}

func TestValidateStructured(t *testing.T) {
	schema := map[string]interface{}{
		"type":     "object",
		"required": []interface{}{"ok", "count"},
		"properties": map[string]interface{}{
			"ok":    map[string]interface{}{"type": "boolean"},
			"count": map[string]interface{}{"type": "integer"},
			"tags":  map[string]interface{}{"type": "array"},
			"note":  map[string]interface{}{"type": "string"},
		},
	}
	cases := []struct {
		name    string
		payload map[string]any
		wantErr string
	}{
		{"valid", map[string]any{"ok": true, "count": float64(3), "tags": []interface{}{"a"}}, ""},
		{"missing required", map[string]any{"ok": true}, `missing required field "count"`},
		{"wrong type", map[string]any{"ok": "yes", "count": float64(1)}, `field "ok": want boolean`},
		{"non-integer", map[string]any{"ok": true, "count": 1.5}, `field "count": want integer`},
		{"undeclared fields pass", map[string]any{"ok": false, "count": float64(0), "extra": 1}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateStructured(tc.payload, schema)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
