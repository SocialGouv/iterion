package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Almost every MCP tool is registered per server (`mcp.<server>.<tool>`), so
// the node's `mcp:` declaration scopes it by construction. Three do not:
// list_mcp_resources, read_mcp_resource and mcp_auth take the server's NAME
// as an argument the MODEL writes, and they reach the launcher's MCP
// provider, which connects the named server — for a stdio server, starts its
// process.
//
// So the node's declaration has to be enforced on the argument. The three
// states are distinguished on purpose: checkNodeToolAccess reads an EMPTY
// active set as "unrestricted", which is right for a tool node and says the
// opposite of what an agent with `inherit: false` wrote.
func TestTheServerNamingMCPToolsHonourTheNodesScope(t *testing.T) {
	for _, tc := range []struct {
		name    string
		active  []string
		server  string
		allowed bool
	}{
		{"a server the node declared", []string{"allowed", "other"}, "allowed", true},
		{"a server another node declared", []string{"allowed"}, "other", false},
		{"no MCP scope at all denies every server", nil, "allowed", false},
		{"the implicit default server", []string{"allowed"}, "", false},
	} {
		for _, toolName := range []string{"list_mcp_resources", "read_mcp_resource", "mcp_auth"} {
			t.Run(tc.name+"/"+toolName, func(t *testing.T) {
				var reached string
				e := scopingExecutor(t, toolName, func(input json.RawMessage) {
					reached = string(input)
				})
				node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n"}, ActiveMCPServers: tc.active}
				defs, _, err := e.resolveToolsForNode(context.Background(), node, []string{toolName})
				if err != nil {
					t.Fatalf("resolve: %v", err)
				}
				if len(defs) != 1 {
					t.Fatalf("expected the tool to resolve, got %d definitions", len(defs))
				}

				input := json.RawMessage(`{}`)
				if tc.server != "" {
					input = json.RawMessage(`{"server":"` + tc.server + `"}`)
				}
				_, err = defs[0].Execute(context.Background(), input)

				if tc.allowed {
					if err != nil {
						t.Fatalf("a server the node declared must be reachable: %v", err)
					}
					if reached == "" {
						t.Error("the underlying tool was never called")
					}
					return
				}
				if err == nil {
					t.Fatal("a server outside the node's scope must be refused")
				}
				if reached != "" {
					t.Errorf("the refusal must happen BEFORE the provider is reached (it connects the server): %q", reached)
				}
				if !strings.Contains(err.Error(), "MCP server") {
					t.Errorf("the refusal should name what was refused: %v", err)
				}
			})
		}
	}
}

// A tool node has no MCP scope to speak of — it is not an LLM turn, nothing
// picks a server name from a model — and wrapping it would refuse a call the
// author wrote explicitly.
func TestANonLLMNodeKeepsTheServerNamingToolsUnwrapped(t *testing.T) {
	var reached bool
	e := scopingExecutor(t, "list_mcp_resources", func(json.RawMessage) { reached = true })
	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "t"}}

	def := e.scopeMCPServerNamingTool(delegate.ToolDef{
		Name: "list_mcp_resources",
		Execute: func(_ context.Context, in json.RawMessage) (string, error) {
			reached = true
			return "ok", nil
		},
	}, node)
	if _, err := def.Execute(context.Background(), []byte(`{"server":"anything"}`)); err != nil {
		t.Fatalf("a tool node must not be scoped here: %v", err)
	}
	if !reached {
		t.Error("the call never reached the tool")
	}
}

func scopingExecutor(t *testing.T, toolName string, onCall func(json.RawMessage)) *ClawExecutor {
	t.Helper()
	tr := tool.NewRegistry()
	if err := tr.RegisterBuiltin(toolName, toolName, nil, func(_ context.Context, in json.RawMessage) (string, error) {
		onCall(in)
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	return &ClawExecutor{logger: iterlog.Nop(), toolRegistry: tr}
}

// The guard decodes the model's argument; so does the tool underneath. If
// they decode it differently, the guard judges a value nobody uses.
//
// encoding/json matches struct fields case-INSENSITIVELY and lets the last
// matching key win, while the claw builtin reads input["server"] out of a
// map, case-sensitively. So `{"server":"forbidden","Server":"allowed"}` was
// read as "allowed" by a struct-based guard and as "forbidden" by the call it
// was guarding — one extra capital letter and the node's MCP scope was gone.
func TestTheNodeScopeReadsTheSameServerNameTheToolWillRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		reaches string // "" = the call must be refused
	}{
		{"a plain name the node declared", `{"server":"allowed"}`, "allowed"},
		{"a plain name it did not", `{"server":"forbidden"}`, ""},
		{"the same key in two cases", `{"server":"forbidden","Server":"allowed"}`, ""},
		{"only the capitalised key", `{"Server":"allowed"}`, ""},
		{"a non-string server", `{"server":123}`, ""},
		{"no server at all", `{}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reached string
			e := scopingExecutor(t, "list_mcp_resources", func(in json.RawMessage) {
				// What the CALLEE would read, decoded the callee's way.
				var args map[string]any
				_ = json.Unmarshal(in, &args)
				if s, ok := args["server"].(string); ok && s != "" {
					reached = s
					return
				}
				reached = "default"
			})
			node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n"}, ActiveMCPServers: []string{"allowed"}}
			defs, _, err := e.resolveToolsForNode(context.Background(), node, []string{"list_mcp_resources"})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}

			_, execErr := defs[0].Execute(context.Background(), json.RawMessage(tc.body))

			if tc.reaches == "" {
				if execErr == nil {
					t.Fatalf("this body must be refused; it reached %q", reached)
				}
				if reached != "" {
					t.Errorf("the call reached %q before being refused", reached)
				}
				return
			}
			if execErr != nil {
				t.Fatalf("a server the node declared must be reachable: %v", execErr)
			}
			if reached != tc.reaches {
				t.Errorf("the tool was asked for %q, want %q", reached, tc.reaches)
			}
		})
	}
}

// A subagent selects its tools from the process registry, with no node — so
// whatever MCP the run can reach anywhere, a child conversation could reach
// too, past the `mcp:` block of the node that spawned it. claw's per-type
// allowlist is no help: nil means "every tool".
func TestASubagentGetsNoNodeScopedMCPTool(t *testing.T) {
	reg := tool.NewRegistry()
	exec := func(context.Context, json.RawMessage) (string, error) { return "ok", nil }
	for _, name := range []string{"bash", "list_mcp_resources", "read_mcp_resource", "mcp_auth"} {
		if err := reg.RegisterBuiltin(name, name, nil, exec); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.RegisterMCP("forbidden", "search", "", nil, exec); err != nil {
		t.Fatal(err)
	}

	// nil = claw's general-purpose subagent: every tool allowed.
	got := map[string]bool{}
	for _, gt := range buildSubagentTools(reg, nil) {
		got[gt.Name] = true
	}

	if !got["bash"] {
		t.Error("a subagent still gets the tools whose reach does not depend on a node")
	}
	for _, withheld := range []string{"list_mcp_resources", "read_mcp_resource", "mcp_auth", "mcp.forbidden.search"} {
		if got[withheld] {
			t.Errorf("%q reaches an MCP server, and this runner has no node to scope it by", withheld)
		}
	}
}
