package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/scripthost"
)

func discoveryTools() []GenerationTool {
	echo := func(context.Context, json.RawMessage) (string, error) { return "echoed", nil }
	return []GenerationTool{
		{Name: "read_file", Description: "Read a file from the workspace.", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`), Execute: echo},
		{Name: "list_dir", Description: "List a directory.", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`), Execute: echo},
		{Name: "ask_user", Description: "Ask the operator.", Execute: echo},
		{Name: "agent", Description: "Start a sub-agent.", Execute: echo},
		{Name: "mcp_iterion_board_list", Description: "List the board.", Execute: echo},
	}
}

func installedCodemode(t *testing.T, tools []GenerationTool) GenerationTool {
	t.Helper()
	opts := GenerationOptions{Tools: append([]GenerationTool(nil), tools...)}
	if err := installCodemode(&opts, nil, nil, nil); err != nil {
		t.Fatalf("installCodemode: %v", err)
	}
	return opts.Tools[len(opts.Tools)-1]
}

// The codemode tool's description sits in the cached prompt prefix: two
// installs of one node's tools, registered in any order, must read the same
// bytes — or every turn re-bills the prefix.
func TestTheCodemodeDescriptionIsByteStable(t *testing.T) {
	t.Parallel()

	tools := discoveryTools()
	reversed := make([]GenerationTool, len(tools))
	for i, tl := range tools {
		reversed[len(tools)-1-i] = tl
	}
	a, b := installedCodemode(t, tools), installedCodemode(t, reversed)
	if a.Description != b.Description {
		t.Fatalf("the description depends on registration order:\n%s\n---\n%s", a.Description, b.Description)
	}
	if !strings.HasPrefix(a.Description, codemodeDescription+"\n\nTools a program can call:\n") {
		t.Fatalf("the description does not start with the constant preamble:\n%s", a.Description)
	}
}

// The index lists exactly the tools the door serves a program: the node's
// tools minus the withheld class — the predicate the door narrows by, not a
// second list.
func TestTheIndexListsWhatTheDoorServesAProgram(t *testing.T) {
	t.Parallel()

	desc := installedCodemode(t, discoveryTools()).Description
	index := desc[strings.Index(desc, "Tools a program can call:\n"):]
	for _, tl := range discoveryTools() {
		_, withheld := scriptWithheld(tl.Name, ScriptHostToolName)
		listed := strings.Contains(index, "\n- "+tl.Name+" ") || strings.Contains(index, "\n- "+tl.Name+":")
		if listed == withheld {
			t.Errorf("%s: listed=%v withheld=%v — the index and the door disagree", tl.Name, listed, withheld)
		}
	}
	if strings.Contains(index, "- "+ScriptHostToolName) {
		t.Errorf("the index lists codemode itself")
	}
}

// Through the real child and the node's real door, installed the way the
// claw backend installs it: a program finds a tool by what it does, reads its
// declaration and calls it, and a withheld tool is not discoverable either.
func TestAProgramDiscoversTheNodesToolsThroughTheDoor(t *testing.T) {
	t.Parallel()

	opts := GenerationOptions{Tools: discoveryTools()}
	if err := installCodemodeOn(&opts, childHost(), &scripthost.Limits{Memory: 1 << 30}); err != nil {
		t.Fatalf("installCodemodeOn: %v", err)
	}
	cm := opts.Tools[len(opts.Tools)-1]
	_, ctx := newToolDispatcher(context.Background(), buildToolMap(opts.Tools), nil, nil, nil, nil, nil)
	in, err := json.Marshal(map[string]string{"source": `
		const hit = call.search("read a file")[0];
		let askable = "found";
		try { call.describe("ask_user"); } catch (e) { askable = "withheld"; }
		[hit.name, call(hit.name, { path: "/w/a" }).text, askable, call.search("operator sub-agent board").map(h => h.name)]
	`})
	if err != nil {
		t.Fatal(err)
	}
	out, err := cm.Execute(ctx, in)
	if err != nil {
		t.Fatalf("the program failed: %v", err)
	}
	if out != `["read_file","echoed","withheld",[]]` {
		t.Fatalf("the program returned %s", out)
	}
}
