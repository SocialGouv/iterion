package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// MED-2: the nudge re-ask replays the interrupted generation's messages in a
// FRESH GenerateTextDirect — but both share the context the backend pinned
// the CommandScope on (ClawBackend.Execute), so the bound the first pass
// set still gates the second. Red when GenerateTextDirect goes back to
// pinning its own fresh scope per call.
func TestGenerateTextDirect_ReusesTheScopePinnedOnTheContext(t *testing.T) {
	t.Parallel()
	scope := &tool.CommandScope{}
	scope.Apply("audit", []string{"Read"})
	ctx := tool.WithCommandScope(context.Background(), scope)

	bash, bashCalls := charNoopTool("bash")
	read, _ := charNoopTool("read_file")
	tools := []GenerationTool{read, bash}

	// First pass: the model calls bash, which the narrowing refuses.
	res, err := GenerateTextDirect(ctx, newMockClient(
		toolUseEvents("tu_1", "bash", `{}`, 10, 5),
		textEvents("done", 10, 5),
	), GenerationOptions{
		Model:    "claude-sonnet-4-6",
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "go"}}}},
		Tools:    tools,
	})
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if *bashCalls != 0 {
		t.Fatal("the narrowed bash ran in the first pass")
	}

	// The nudge: same ctx, the generation's messages replayed, the model
	// tries bash again — the bound must still hold.
	_, err = GenerateTextDirect(ctx, newMockClient(
		toolUseEvents("tu_2", "bash", `{}`, 10, 5),
		textEvents("done", 10, 5),
	), GenerationOptions{
		Model:    "claude-sonnet-4-6",
		Messages: res.Messages,
		Tools:    tools,
	})
	if err != nil {
		t.Fatalf("nudge pass: %v", err)
	}
	if *bashCalls != 0 {
		t.Error("the nudge's fresh generation lost the narrowing")
	}

	// A caller that pinned NOTHING still gets a working, fresh scope — the
	// reuse is of a pinned scope, never of a shared global.
	if _, err := GenerateTextDirect(context.Background(), newMockClient(
		toolUseEvents("tu_3", "bash", `{}`, 10, 5),
		textEvents("done", 10, 5),
	), GenerationOptions{
		Model:    "claude-sonnet-4-6",
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "go"}}}},
		Tools:    tools,
	}); err != nil {
		t.Fatalf("unpinned pass: %v", err)
	}
	if *bashCalls != 1 {
		t.Error("an unpinned context must run unnarrowed")
	}
}

// MED-1: the pause carries the narrowing out — stashPauseConversation
// snapshots the scope onto the ErrAskUser beside the conversation, so the
// checkpoint can hold the bound the interrupted generation ran under. The
// operator approved ONE call; the restriction is not lifted by it.
func TestStashPauseConversationCarriesTheNarrowing(t *testing.T) {
	t.Parallel()
	scope := &tool.CommandScope{}
	scope.Apply("audit", []string{"Read"})
	ctx := tool.WithCommandScope(context.Background(), scope)

	askErr := &delegate.ErrAskUser{Question: "q", PendingToolUseID: "tu_1"}
	stashPauseConversation(ctx, askErr, nil, GenerationOptions{Model: "claude-sonnet-4-6"})
	if askErr.CommandNarrowing == nil || askErr.CommandNarrowing.Command != "audit" {
		t.Fatalf("the pause dropped the narrowing: %+v", askErr.CommandNarrowing)
	}

	// And it restores onto the fresh scope the resumed execution pins.
	restored := &tool.CommandScope{}
	restored.Restore(askErr.CommandNarrowing)
	if err := restored.CheckAllowed("bash"); err == nil {
		t.Error("the resumed generation must keep the bound")
	}
	if err := restored.CheckAllowed("read_file"); err != nil {
		t.Errorf("the resumed generation must keep the allowance: %v", err)
	}

	// No narrowing active: the pause carries none.
	plain := &delegate.ErrAskUser{Question: "q", PendingToolUseID: "tu_2"}
	stashPauseConversation(context.Background(), plain, nil, GenerationOptions{Model: "claude-sonnet-4-6"})
	if plain.CommandNarrowing != nil {
		t.Errorf("an unnarrowed pause carries %+v", plain.CommandNarrowing)
	}
}

// MED-3: a narrowed parent narrows its child — `allowed-tools: Task` folds
// to `agent`, and the delegation must not hand the child the node's full
// set. The filter intersects with the parent's CommandScope, one more
// narrowing on top of the agent file's own `tools:`.
func TestANarrowedParentNarrowsItsSubagent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	run, client := parentLoopRunner(t, "")

	scope := &tool.CommandScope{}
	scope.Apply("audit", []string{"Read", "Task"})
	ctx := tool.WithCommandScope(WithParentLoop(context.Background(), ParentLoop{
		Model:   "test/parent-model",
		Tools:   []GenerationTool{parentTool("read_file"), parentTool("bash")},
		WorkDir: dir,
	}), scope)
	if _, err := run(ctx, map[string]any{"description": "probe", "prompt": "go", "subagent_type": "general-purpose"}); err != nil {
		t.Fatalf("subagent: %v", err)
	}
	// The node holds read_file and bash; the narrowing holds read_file alone
	// (plus the trio, which the child never carries wholesale).
	offered := client.offered(t, 1)
	if len(offered) != 1 || offered[0] != "read_file" {
		t.Errorf("the child was offered %v, want the narrowed [read_file]", offered)
	}

	// Unnarrowed control: the child gets the node's set.
	run2, client2 := parentLoopRunner(t, "")
	ctx2 := WithParentLoop(context.Background(), ParentLoop{
		Model:   "test/parent-model",
		Tools:   []GenerationTool{parentTool("read_file"), parentTool("bash")},
		WorkDir: dir,
	})
	if _, err := run2(ctx2, map[string]any{"description": "probe", "prompt": "go", "subagent_type": "general-purpose"}); err != nil {
		t.Fatalf("subagent (control): %v", err)
	}
	if offered := client2.offered(t, 1); strings.Join(offered, ",") != "read_file,bash" && strings.Join(offered, ",") != "bash,read_file" {
		t.Errorf("the unnarrowed child was offered %v, want the node's set", offered)
	}
}

// dispatch is refused — the narrowing never rewrites what the node holds,
// and never widens: a tool the map cannot resolve stays unknown.
func TestExecuteToolsDirect_CommandNarrowingRefusesAtDispatch(t *testing.T) {
	t.Parallel()
	executed := 0
	toolMap := map[string]*GenerationTool{
		"read_file": {Name: "read_file", Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			executed++
			return "contents", nil
		}},
		"bash": {Name: "bash", Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			executed++
			return "shell-out", nil
		}},
	}

	scope := &tool.CommandScope{}
	scope.Apply("audit", []string{"Read"})
	ctx := tool.WithCommandScope(context.Background(), scope)
	uses := []toolUseBlock{
		{ID: "tu_1", Name: "read_file", PartialJSON: `{}`},
		{ID: "tu_2", Name: "bash", PartialJSON: `{}`},
		{ID: "tu_3", Name: "todo_write", PartialJSON: `{}`}, // always allowed — but not the node's
		{ID: "tu_4", Name: "ghost", PartialJSON: `{}`},      // never widens past the map
	}

	var calls []ToolCallInfo
	results, err := executeToolsDirect(ctx, uses, toolMap, nil,
		func(i ToolCallInfo) { calls = append(calls, i) }, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("results = %d, want 4", len(results))
	}
	if results[0].IsError || charToolResultText(t, results[0]) != "contents" {
		t.Errorf("read_file must run: %+v", results[0])
	}
	if !results[1].IsError {
		t.Fatalf("bash must be refused: %+v", results[1])
	}
	refusal := charToolResultText(t, results[1])
	if !strings.Contains(refusal, `"bash" is not in allowed-tools for command "audit"`) {
		t.Errorf("the refusal names the tool AND the command that narrowed: %q", refusal)
	}
	if executed != 1 {
		t.Errorf("executed = %d, want 1 — only the allowed tool ran", executed)
	}
	// The refusal reaches onToolCall, like the permission gate's: a reader of
	// tool_called events sees the bound held, not a silent skip.
	var sawRefusal bool
	for _, c := range calls {
		if c.ToolName == "bash" && c.Error != nil && strings.Contains(c.Error.Error(), "allowed-tools") {
			sawRefusal = true
		}
	}
	if !sawRefusal {
		t.Errorf("onToolCall never reported the narrowing refusal: %+v", calls)
	}
	// Always-allowed does not conjure the tool: todo_write passes the
	// narrowing gate but is not the node's, so it stays "unknown tool".
	if got := charToolResultText(t, results[2]); !strings.Contains(got, "unknown tool") {
		t.Errorf("todo_write undeclared: %q, want the unknown-tool refusal", got)
	}
	if got := charToolResultText(t, results[3]); !strings.Contains(got, "unknown tool") {
		t.Errorf("a name outside the narrowing stays unknown, never granted: %q", got)
	}

	// No scope on the context — the shape every direct-Execute caller has —
	// narrows nothing.
	results, err = executeToolsDirect(context.Background(), uses[:2], toolMap, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error without scope: %v", err)
	}
	if results[1].IsError {
		t.Errorf("without a scope bash must run: %+v", results[1])
	}
}
