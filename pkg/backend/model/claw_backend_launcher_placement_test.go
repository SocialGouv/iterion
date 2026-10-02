package model

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// placementTask advertises every name with a recording host closure, the way
// buildTask advertises a sandboxed node's resolved tools to the launcher.
func placementTask(names []string) (delegate.Task, map[string]bool) {
	ran := make(map[string]bool, len(names))
	defs := make([]delegate.ToolDef, 0, len(names))
	for _, name := range names {
		defs = append(defs, delegate.ToolDef{
			Name: name,
			Execute: func(context.Context, json.RawMessage) (string, error) {
				ran[name] = true
				return "host:" + name, nil
			},
		})
	}
	return delegate.Task{NodeID: "boundary", ToolDefs: defs}, ran
}

// The launcher holds the sandbox boundary: a tool_call envelope for a tool
// that belongs in the container, or that no side may serve, is refused and its
// host closure never runs — whichever runner version sent it, and whoever in
// the container wrote it on the runner's stdout. The names come from the
// classification itself, so a tool added to it is covered here too.
func TestMultiplexerHandler_RefusesEveryToolNotPlacedOnTheLauncher(t *testing.T) {
	contained := append(tool.SandboxPlacedTools(tool.PlacementSandbox), tool.SandboxPlacedTools(tool.PlacementRefused)...)
	contained = append(contained, "unclassified_future_tool")
	task, ran := placementTask(contained)
	handler := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{}).multiplexerHandler(context.Background(), task)

	for _, name := range contained {
		out, err := handler.OnToolCall(context.Background(), name, json.RawMessage(`{}`))
		if err == nil {
			t.Errorf("OnToolCall(%q) = %q, nil; want a refusal: the launcher executed a container-side tool on the host", name, out)
			continue
		}
		if !strings.Contains(err.Error(), "refusing to execute") {
			t.Errorf("OnToolCall(%q) error = %v; want the placement refusal", name, err)
		}
		if ran[name] {
			t.Errorf("OnToolCall(%q): the host closure ran", name)
		}
	}
}

// The permission gate the runner applies before forwarding is applied again
// on the launcher: a forwarded call for a launcher-placed tool the policy
// denies — an older runner's, or one written on the runner's stdout by
// anything else in the container — is refused, and its host closure never
// runs. Allowed, the same call goes through.
//
// Reddens on the mutation that deletes the launcher's permission check.
func TestMultiplexerHandler_EnforcesThePermissionGateOnForwardedCalls(t *testing.T) {
	const name = "mcp_github_create_issue"
	for _, tc := range []struct {
		allow    []string
		wantDeny bool
	}{
		{allow: []string{"read_file"}, wantDeny: true},
		{allow: []string{"read_file", name}, wantDeny: false},
	} {
		pol, err := permission.NewPolicy(permission.ModeDeny, tc.allow, nil, nil)
		if err != nil {
			t.Fatalf("NewPolicy: %v", err)
		}
		task, ran := placementTask([]string{name})
		task.Permission = pol
		handler := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{}).multiplexerHandler(context.Background(), task)

		_, err = handler.OnToolCall(context.Background(), name, json.RawMessage(`{"title":"x"}`))
		if tc.wantDeny {
			if err == nil || !strings.Contains(err.Error(), "Permission denied") {
				t.Errorf("allow=%v: OnToolCall = %v; want the permission refusal", tc.allow, err)
			}
			if ran[name] {
				t.Errorf("allow=%v: the host closure of a denied tool ran", tc.allow)
			}
			continue
		}
		if err != nil || !ran[name] {
			t.Errorf("allow=%v: OnToolCall = %v, ran=%v; want the allowed call executed", tc.allow, err, ran[name])
		}
	}
}

// identityTask advertises one MCP tool under its sanitized name, with its
// registry identity, and records whether its host closure ran.
func identityTask(pol *permission.Policy) (delegate.Task, *bool) {
	ran := false
	return delegate.Task{
		NodeID:     "gated",
		Permission: pol,
		ToolDefs: []delegate.ToolDef{{
			Name:          "mcp_github_delete_repo",
			QualifiedName: "mcp.github.delete_repo",
			Execute: func(context.Context, json.RawMessage) (string, error) {
				ran = true
				return "done", nil
			},
		}},
	}, &ran
}

// The launcher gates the tool's IDENTITY, not the one spelling it was
// forwarded under: rules are commonly written in the claude_code FQN form
// (`mcp__<server>__*`) while the runner forwards the sanitized name.
//
//   - an FQN deny still denies the forwarded sanitized name (a forged call
//     cannot pick the spelling no deny names);
//   - an FQN allow still allows it (an honest call the runner allowed is not
//     refused here);
//   - a spelling the tool was not advertised under is refused outright.
//
// Reddens on the mutation that evaluates the forwarded spelling alone, on the
// one that lets an allow outrank an explicit deny, and on the one that maps
// an unadvertised spelling back onto the tool.
func TestMultiplexerHandler_PermissionGateTakesTheToolsIdentity(t *testing.T) {
	if err, ran := gateForwardedCall(t, []string{"*"}, []string{"mcp__github__delete_*"}, "mcp_github_delete_repo", `{}`); err == nil || ran {
		t.Errorf("an FQN deny rule did not deny the sanitized forwarded name: err=%v ran=%v", err, ran)
	}
	if err, ran := gateForwardedCall(t, []string{"mcp__github__*"}, nil, "mcp_github_delete_repo", `{}`); err != nil || !ran {
		t.Errorf("an FQN allow rule did not allow the sanitized forwarded name: err=%v ran=%v", err, ran)
	}
	for _, forged := range []string{"mcp__github__delete_repo", "mcp__github_delete_repo"} {
		if err, ran := gateForwardedCall(t, []string{"*"}, nil, forged, `{}`); err == nil || ran || !strings.Contains(err.Error(), "was not advertised") {
			t.Errorf("an unadvertised spelling %q was executed: err=%v ran=%v", forged, err, ran)
		}
	}
}

// gateForwardedCall forwards one call for the identityTask tool under a
// deny-mode policy built from allow/deny, and reports the handler's error and
// whether the host closure ran.
func gateForwardedCall(t *testing.T, allow, deny []string, name, input string) (error, bool) {
	t.Helper()
	pol, err := permission.NewPolicy(permission.ModeDeny, allow, nil, deny)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	task, ran := identityTask(pol)
	handler := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{}).multiplexerHandler(context.Background(), task)
	_, callErr := handler.OnToolCall(context.Background(), name, json.RawMessage(input))
	return callErr, *ran
}

// Rules written in the ADVERTISED form bind too: a sanitized deny beats an
// allow-all, and a sanitized allow is honoured — the advertised spelling's own
// verdict is not lost to the FQN spelling's mode default.
//
// Reddens on the mutation that skips the first spelling's explicit deny, and
// on the one that lets a default deny from the FQN spelling refuse.
func TestMultiplexerHandler_PermissionGateHonoursAdvertisedFormRules(t *testing.T) {
	for _, tc := range []struct {
		allow, deny []string
		wantRun     bool
	}{
		{allow: []string{"*"}, deny: []string{"mcp_github_delete_*"}, wantRun: false},
		{allow: []string{"*"}, deny: []string{"mcp_github_delete_repo"}, wantRun: false},
		{allow: []string{"mcp_github_*"}, wantRun: true},
		{allow: []string{"mcp_github_delete_repo"}, wantRun: true},
	} {
		err, ran := gateForwardedCall(t, tc.allow, tc.deny, "mcp_github_delete_repo", `{}`)
		if ran != tc.wantRun || (err == nil) != tc.wantRun {
			t.Errorf("allow=%v deny=%v: err=%v ran=%v; want ran=%v", tc.allow, tc.deny, err, ran, tc.wantRun)
		}
	}
}

// A forwarded input that is not a JSON object cannot be matched against
// argument-scoped rules, so under an enabled policy it is refused — never
// gated on an empty argument set.
//
// Reddens on the mutation that carries on with nil arguments.
func TestMultiplexerHandler_RefusesAnUndecodableInputUnderAPolicy(t *testing.T) {
	if err, ran := gateForwardedCall(t, []string{"*"}, nil, "mcp_github_delete_repo", `[]`); err == nil || ran {
		t.Errorf("a non-object input was gated as if it had no arguments: err=%v ran=%v", err, ran)
	}
}

// The identity the launcher gates on is the one buildTask produces — the
// registry's qualified name carried through ToDelegateDef, and through the
// tool_policy guard that wraps it — not a hand-built double.
//
// Reddens on the mutation that drops QualifiedName from ToDelegateDef.
func TestMultiplexerHandler_GatesTheIdentityBuildTaskProduces(t *testing.T) {
	for _, withToolPolicy := range []bool{false, true} {
		reg := tool.NewRegistry()
		ran := false
		if err := reg.RegisterMCP("github", "delete_repo", "", nil, func(context.Context, json.RawMessage) (string, error) {
			ran = true
			return "done", nil
		}); err != nil {
			t.Fatalf("RegisterMCP: %v", err)
		}
		if err := reg.RegisterBuiltin("todo_write", "todo_write", nil, func(context.Context, json.RawMessage) (string, error) { return "", nil }); err != nil {
			t.Fatalf("RegisterBuiltin: %v", err)
		}
		e := &ClawExecutor{logger: iterlog.Nop(), toolRegistry: reg, sandbox: fakeSandboxRun{}}
		if withToolPolicy {
			e.toolPolicy = tool.NewPolicy("*")
		}
		node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gated"}}
		f := backendFields{id: "gated", model: "openai/gpt-5.6-sol", tools: []string{"mcp.github.delete_repo"}}
		task, err := e.buildTask(context.Background(), node, f, map[string]any{}, delegate.BackendClaw, nil)
		if err != nil {
			t.Fatalf("buildTask (tool_policy=%v): %v", withToolPolicy, err)
		}
		pol, err := permission.NewPolicy(permission.ModeDeny, []string{"*"}, nil, []string{"mcp__github__delete_*"})
		if err != nil {
			t.Fatalf("NewPolicy: %v", err)
		}
		task.Permission = pol
		handler := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{}).multiplexerHandler(context.Background(), task)
		if _, err := handler.OnToolCall(context.Background(), "mcp_github_delete_repo", json.RawMessage(`{}`)); err == nil || ran {
			t.Errorf("tool_policy=%v: an FQN deny did not bind the tool buildTask advertised: err=%v ran=%v", withToolPolicy, err, ran)
		}
	}
}

// A refusal is recorded on the launcher's side — logged and emitted as a
// failed tool call — rather than left to the runner's event relay, which the
// author of a forged call also controls.
//
// Reddens on the mutation that drops the hook from refuseForwardedCall.
func TestMultiplexerHandler_RecordsARefusedForwardedCall(t *testing.T) {
	var logged bytes.Buffer
	var recorded []LLMToolCallInfo
	hooks := EventHooks{OnToolCall: func(_ string, info LLMToolCallInfo) { recorded = append(recorded, info) }}
	b := NewClawBackend(NewRegistry(), hooks, RetryPolicy{}, WithClawLogger(iterlog.New(iterlog.LevelInfo, &logged)))
	task, _ := placementTask([]string{"bash"})

	if _, err := b.multiplexerHandler(context.Background(), task).OnToolCall(context.Background(), "bash", json.RawMessage(`{}`)); err == nil {
		t.Fatal("a forwarded bash call was not refused")
	}
	if len(recorded) != 1 || recorded[0].ToolName != "bash" || recorded[0].Error == nil {
		t.Errorf("the refusal was not emitted as a failed tool call: %+v", recorded)
	}
	if !strings.Contains(logged.String(), "refusing to execute tool") {
		t.Errorf("the refusal was not logged by the launcher:\n%s", logged.String())
	}
}

// The refusal must spare what the runner is SUPPOSED to proxy: every
// launcher-placed built-in and MCP tool, under the name it was advertised by —
// the one a runner's proxy closure forwards.
func TestMultiplexerHandler_ExecutesLauncherPlacedTools(t *testing.T) {
	names := append(tool.SandboxPlacedTools(tool.PlacementLauncher), "mcp_github_create_issue")
	task, ran := placementTask(names)
	handler := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{}).multiplexerHandler(context.Background(), task)

	for _, name := range names {
		if _, err := handler.OnToolCall(context.Background(), name, json.RawMessage(`{}`)); err != nil {
			t.Errorf("OnToolCall(%q): %v; want the launcher to execute it", name, err)
		}
	}
	for _, name := range names {
		if !ran[name] {
			t.Errorf("the host closure of launcher-placed %q never ran", name)
		}
	}
}
