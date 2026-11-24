package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// mcpGateTools registers an MCP filesystem server the way a run does
// (Registry.RegisterMCP) and carries it through the same two conversions the
// claw backend applies to a task's ToolDefs (ToDelegateDef, then
// toolDefsToGeneration), so the advertised name is the sanitized
// `mcp_fs_read_file` a model sees on claw.
func mcpGateTools(t *testing.T, exec func(context.Context, json.RawMessage) (string, error)) map[string]*GenerationTool {
	t.Helper()
	reg := tool.NewRegistry()
	if err := reg.RegisterMCP("fs", "read_file", "read a file", json.RawMessage(`{"type":"object"}`), exec); err != nil {
		t.Fatalf("RegisterMCP(read_file): %v", err)
	}
	if err := reg.RegisterMCP("fs", "delete_file", "delete a file", json.RawMessage(`{"type":"object"}`), exec); err != nil {
		t.Fatalf("RegisterMCP(delete_file): %v", err)
	}
	var defs []delegate.ToolDef
	for _, ref := range []string{"mcp.fs.read_file", "mcp.fs.delete_file"} {
		td, err := reg.Resolve(ref)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", ref, err)
		}
		defs = append(defs, td.ToDelegateDef())
	}
	return buildToolMap(toolDefsToGeneration(defs))
}

// The gate must identify an MCP call the way dispatch resolves it: claw
// advertises `mcp_<server>_<tool>`, a model may emit the documented FQN
// `mcp__<server>__<tool>`, and dispatch lands both on the same tool — so a
// rule in either spelling, and a call in either spelling, must reach one
// verdict. The rows are the holes a spelling-only matcher leaves:
//
//   - a documented secrets deny degrades to an approval pause in ask mode
//     when the call arrives under the advertised spelling;
//   - a documented allow never opens the tool in deny mode for the same
//     reason;
//   - a claw-spelled deny under allow `*` is dodged by an FQN-emitted call.
func TestThePermissionGateJudgesAnMCPCallOnOneKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("secret deny refuses without pausing in ask mode", func(t *testing.T) {
		t.Parallel()
		ran := false
		tools := mcpGateTools(t, func(context.Context, json.RawMessage) (string, error) {
			ran = true
			return "{}", nil
		})
		policy, err := permission.NewPolicy(permission.ModeAsk, nil, nil,
			[]string{`mcp__fs__*(/run/iterion/secrets/**)`})
		if err != nil {
			t.Fatal(err)
		}
		results, abort := executeToolsDirect(ctx,
			[]toolUseBlock{{ID: "tu_secret", Name: "mcp_fs_read_file", PartialJSON: `{"path":"/run/iterion/secrets/token"}`}},
			tools, nil, nil, nil, nil, policy)
		if errors.As(abort, new(*delegate.ErrAskUser)) {
			t.Fatal("a secrets deny paused the run for approval; the refusal must be a tool_result the model can adapt to")
		}
		if abort != nil {
			t.Fatalf("unexpected abort: %v", abort)
		}
		if len(results) != 1 || !results[0].IsError {
			t.Fatalf("the secret read was not refused: results=%v abort=%v", results, abort)
		}
		if !strings.Contains(charToolResultText(t, results[0]), "deny rule: mcp__fs__*(/run/iterion/secrets/**)") {
			t.Errorf("refusal does not name the deny rule: %q", charToolResultText(t, results[0]))
		}
		if ran {
			t.Error("the tool executed past a deny rule")
		}
	})

	t.Run("fqn allow opens the ordinary call in deny mode", func(t *testing.T) {
		t.Parallel()
		ran := false
		tools := mcpGateTools(t, func(context.Context, json.RawMessage) (string, error) {
			ran = true
			return `{"content":"notes"}`, nil
		})
		policy, err := permission.NewPolicy(permission.ModeDeny, []string{"mcp__fs__*"}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		results, abort := executeToolsDirect(ctx,
			[]toolUseBlock{{ID: "tu_open", Name: "mcp_fs_read_file", PartialJSON: `{"path":"/work/notes.md"}`}},
			tools, nil, nil, nil, nil, policy)
		if abort != nil {
			t.Fatalf("the allow rule did not open the tool: %v", abort)
		}
		if len(results) != 1 || results[0].IsError {
			t.Fatalf("the ordinary read was refused: %v", charToolResultText(t, results[0]))
		}
		if !ran {
			t.Error("the allowed tool never executed")
		}
	})

	t.Run("a claw-spelled deny is not dodged by an fqn-emitted call", func(t *testing.T) {
		t.Parallel()
		ran := false
		tools := mcpGateTools(t, func(context.Context, json.RawMessage) (string, error) {
			ran = true
			return "{}", nil
		})
		policy, err := permission.NewPolicy(permission.ModeAsk, []string{"*"}, nil, []string{"mcp_fs_delete_file"})
		if err != nil {
			t.Fatal(err)
		}
		results, abort := executeToolsDirect(ctx,
			[]toolUseBlock{{ID: "tu_delete", Name: "mcp__fs__delete_file", PartialJSON: `{"path":"/work/notes.md"}`}},
			tools, nil, nil, nil, nil, policy)
		if abort != nil {
			t.Fatalf("the explicit deny paused instead of refusing: %v", abort)
		}
		if len(results) != 1 || !results[0].IsError {
			t.Fatalf("the deny rule did not bite on the FQN spelling: results=%v", results)
		}
		if !strings.Contains(charToolResultText(t, results[0]), "deny rule: mcp_fs_delete_file") {
			t.Errorf("refusal does not name the deny rule: %q", charToolResultText(t, results[0]))
		}
		if ran {
			t.Error("the catch-all allow opened a call an explicit deny refuses")
		}
	})
}
