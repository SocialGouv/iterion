package model

// #2812: the ask channel pauses the RUN, and a child conversation is not the
// run's node — its ErrAskUser surfaced from inside the parent's tool call,
// attributed to nobody, with no conversation persisted for the resume to
// return to. filterSubagentTools withholds the family from every set a child
// draws from; these witnesses redden the moment a child's ask_user reaches
// the operator unlabelled again.

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

var theAskFamily = []string{"ask_user", delegate.AskUserAsyncToolName, delegate.AwaitAnswersToolName}

func offeredNamesMustHoldNoAskTool(t *testing.T, offered []string) {
	t.Helper()
	for _, ask := range theAskFamily {
		if slices.Contains(offered, ask) {
			t.Errorf("the child was offered %q (%v): a question asked from inside a tool call pauses the run unattributed, and nothing persists the child's conversation for the resume", ask, offered)
		}
	}
}

// A node whose interaction opt-in put the ask channel on its OWN list hands
// none of it to a child: the channel is the node's, and the child reports a
// question back to its parent, whose ask is intact.
func TestAChildCannotPauseTheRunFromInsideAToolCall(t *testing.T) {
	t.Parallel()
	run, client := parentLoopRunner(t, "")
	tools := []GenerationTool{parentTool("read_file")}
	for _, ask := range theAskFamily {
		tools = append(tools, parentTool(ask))
	}
	ctx := WithParentLoop(context.Background(), ParentLoop{
		Model: "test/parent-model",
		Tools: tools,
	})
	if _, err := run(ctx, map[string]any{"description": "probe", "prompt": "go"}); err != nil {
		t.Fatalf("subagent: %v", err)
	}
	offered := client.offered(t, 1)
	offeredNamesMustHoldNoAskTool(t, offered)
	if !slices.Contains(offered, "read_file") {
		t.Fatalf("the child lost the node's own reader along with the ask channel: %v", offered)
	}
}

// The withholding is not the parent-loop path's alone: a stand-alone host's
// child selects from the process registry (buildSubagentTools), which is
// where the ask tools are registered — a pause raised there has no run
// behind it at all.
func TestAStandAloneHostsChildGetsNoAskChannel(t *testing.T) {
	t.Parallel()
	client := &parentLoopClient{}
	modelReg := NewRegistry()
	modelReg.Register("test", func(string) (api.APIClient, error) { return client, nil })

	toolReg := tool.NewRegistry()
	for _, name := range append([]string{"bash", "read_file"}, theAskFamily...) {
		if err := toolReg.RegisterBuiltin(name, "test "+name, json.RawMessage(`{"type":"object"}`),
			func(context.Context, json.RawMessage) (string, error) { return "ran", nil }); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	run := NewSubagentRunner(modelReg, toolReg, EventHooks{}, nil, "test/host-model")
	if _, err := run(context.Background(), map[string]any{"description": "probe", "prompt": "go"}); err != nil {
		t.Fatalf("subagent: %v", err)
	}
	offered := client.offered(t, 1)
	offeredNamesMustHoldNoAskTool(t, offered)
	for _, want := range []string{"bash", "read_file"} {
		if !slices.Contains(offered, want) {
			t.Errorf("the stand-alone child lost %q along with the ask channel: %v", want, offered)
		}
	}
}
