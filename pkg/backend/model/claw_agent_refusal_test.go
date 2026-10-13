package model

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestTheAgentRefusalReachesTheModelAsAToolError exercises the refusal from
// the caller that actually runs it: the claw tool loop. executeOneTool is what
// every tool_use block goes through, and toolDefsToGeneration is the same
// four-field conversion claw_backend applies to the task's ToolDefs.
//
// Two things have to hold at once, and only this level shows both: the model
// reads a tool error naming what cannot be served, and the loop CONTINUES —
// the node does not die on a tool the engine granted it under ultracode.
func TestTheAgentRefusalReachesTheModelAsAToolError(t *testing.T) {
	reg := tool.NewRegistry()
	if err := tool.RegisterClawSubagents(reg, nil); err != nil {
		t.Fatalf("RegisterClawSubagents: %v", err)
	}
	td, err := reg.Resolve("agent")
	if err != nil {
		t.Fatalf("resolve agent: %v", err)
	}
	gt := GenerationTool{
		Name:        td.QualifiedName,
		Description: td.Description,
		InputSchema: td.InputSchema,
		Execute:     td.Execute,
	}

	var reported error
	block, abort := executeOneTool(context.Background(),
		toolUseBlock{ID: "tu-agent", Name: "agent", PartialJSON: `{"description":"delegate","prompt":"do the thing"}`},
		map[string]*GenerationTool{"agent": &gt},
		nil,
		func(info ToolCallInfo) { reported = info.Error },
		hooks.NewRunner(), nil, nil)

	if abort != nil {
		t.Fatalf("executeOneTool abort = %v; a capability refusal is a tool error the model can act on, not a loop abort", abort)
	}
	if !block.IsError {
		t.Error("the tool_result is not marked as an error, so the model reads the refusal as a successful delegation")
	}
	got := charToolResultText(t, block)
	for _, want := range []string{"tool error:", "claw", "agent", "claude_code"} {
		if !strings.Contains(got, want) {
			t.Errorf("tool_result = %q, missing %q", got, want)
		}
	}
	// agent_id is the stub's fingerprint: a started subagent always carries
	// one, and a refusal never can.
	if strings.Contains(got, "agent_id") || strings.Contains(got, "\"status\"") {
		t.Errorf("tool_result = %q; the model still reads a started subagent", got)
	}
	if reported == nil {
		t.Error("the tool-call hook carries no error, so the run log records a successful agent call")
	}
}

// TestAPaddedToolEntryNeverReachesTheRefusal pins what the production registry
// does with the author's entry as written, because two diagnostics reason from
// it: C159 quotes the TRIMMED tool name for a padded entry (the catalog
// predicates trim), and C135 stays silent on one.
//
// Registry.Resolve matches exactly — iterion hands `tools:` entries over
// verbatim, the one field it never expands — so an entry carrying whitespace
// resolves to nothing and resolveToolsForNode fails the node at dispatch
// (executor_build_task turns that error into `model: node %q: ...`, before the
// first token). The refusal the padded `agent` entry seems to ask for is never
// reached, and a padded `read_file` dies identically: the class belongs to
// C135's reduction, not to the refusal.
//
// Reddens on the mutation that trims the name inside
// resolveSingleToolForNode — the engine silently repairing the author's list.
func TestAPaddedToolEntryNeverReachesTheRefusal(t *testing.T) {
	reg := tool.NewRegistry()
	if err := tool.RegisterClawAll(reg, tool.ClawDefaults{}); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}
	for _, name := range []string{"agent", "read_file"} {
		if _, err := reg.Resolve(name); err != nil {
			t.Fatalf("premise broken: %q must resolve on the production registry: %v", name, err)
		}
		if _, err := reg.Resolve(" " + name + " "); err == nil {
			t.Errorf("the registry resolved a padded %q, so C159's trimmed quote describes a call that happens", name)
		}
	}

	e := &ClawExecutor{logger: iterlog.Nop(), toolRegistry: reg}
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "x"}}
	for _, name := range []string{"agent", "read_file"} {
		defs, _, err := e.resolveToolsForNode(context.Background(), node, []string{" " + name + " "})
		if err == nil {
			t.Errorf("a padded %q entry resolved to %d tool(s): the node would start and the padding would cost nothing",
				name, len(defs))
			continue
		}
		if !strings.Contains(err.Error(), " "+name+" ") {
			t.Errorf("the dispatch error does not quote the entry as written, so nothing names the padding: %v", err)
		}
	}
}
