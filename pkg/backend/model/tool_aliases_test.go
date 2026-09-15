package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestToolAliasesAndPolicyUseTheSameMCPResolution(t *testing.T) {
	for _, count := range []int{0, 1, 2} {
		t.Run(string(rune('0'+count)), func(t *testing.T) {
			registry := tool.NewRegistry()
			_ = registry.RegisterBuiltin("read_file", "", nil, jsonExec("builtin"))
			if count > 0 {
				_ = registry.RegisterMCP("one", "Read", "", nil, jsonExec("mcp"))
			}
			if count > 1 {
				_ = registry.RegisterMCP("two", "Read", "", nil, jsonExec("mcp2"))
			}
			wf := &ir.Workflow{}
			node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "x"}, ActiveMCPServers: []string{"one", "two"}}
			for _, enabled := range []bool{false, true} {
				ctx := tool.WithBuiltinAliases(context.Background(), enabled)
				ex := newTestClawExecutor(NewRegistry(), wf, WithToolRegistry(registry), WithToolPolicy(tool.NewPolicy("Read")))
				defs, err := ex.resolveToolsForNode(ctx, node, []string{"Read"})
				if count == 2 {
					if err == nil || !strings.Contains(err.Error(), "ambiguous") {
						t.Fatalf("ambiguity: %v", err)
					}
					continue
				}
				if !enabled && count == 0 {
					if err == nil || !strings.Contains(err.Error(), "requires.iterion") {
						t.Fatalf("missing floor: %v", err)
					}
					continue
				}
				if err != nil || len(defs) != 1 {
					t.Fatalf("resolve: %v %v", defs, err)
				}
				// The invocation context is fresh, like a sandbox relay. Authorization
				// must have been captured from the engine context when the tool was built.
				result, err := defs[0].Execute(context.Background(), json.RawMessage(`{}`))
				if !enabled {
					if !errors.Is(err, tool.ErrToolDenied) {
						t.Fatalf("legacy policy changed: %v", err)
					}
					continue
				}
				want := "builtin"
				if count == 1 {
					want = "mcp"
				}
				if err != nil || result != want {
					t.Fatalf("execute: %q %v, want %s", result, err, want)
				}
				// An alias beside a unique MCP must not accidentally grant its builtin.
				canonical, err := ex.resolveToolsForNode(ctx, node, []string{"read_file"})
				if err != nil {
					t.Fatal(err)
				}
				_, err = canonical[0].Execute(context.Background(), json.RawMessage(`{}`))
				if (count == 1) != errors.Is(err, tool.ErrToolDenied) {
					t.Fatalf("MCP policy granted builtin: %v", err)
				}
			}
		})
	}
}

func TestAmbiguousAliasPolicyDeniesCanonicalTool(t *testing.T) {
	tr := tool.NewRegistry()
	_ = tr.RegisterBuiltin("read_file", "", nil, jsonExec("should not run"))
	_ = tr.RegisterMCP("a", "Read", "", nil, jsonExec("a"))
	_ = tr.RegisterMCP("b", "Read", "", nil, jsonExec("b"))
	for _, checker := range []tool.ToolChecker{tool.NewPolicy("*", "Read"), tool.BuildChecker([]string{"*"}, map[string][]string{"x": {"*", "Read"}}, nil)} {
		ex := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithToolRegistry(tr), WithToolPolicy(checker))
		defs, err := ex.resolveToolsForNode(tool.WithBuiltinAliases(context.Background(), true), &ir.AgentNode{BaseNode: ir.BaseNode{ID: "x"}}, []string{"read_file"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = defs[0].Execute(context.Background(), nil)
		if !errors.Is(err, tool.ErrToolDenied) || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("ambiguous policy accepted: %v", err)
		}
	}
}

func TestAliasAndCanonicalToolAreAdvertisedOnce(t *testing.T) {
	tr := tool.NewRegistry()
	_ = tr.RegisterBuiltin("read_file", "", nil, jsonExec("ok"))
	ex := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithToolRegistry(tr))
	defs, err := ex.resolveToolsForNode(tool.WithBuiltinAliases(context.Background(), true), &ir.AgentNode{BaseNode: ir.BaseNode{ID: "x"}}, []string{"Read", "read_file", "Read"})
	if err != nil || len(defs) != 1 || defs[0].Name != "read_file" {
		t.Fatalf("duplicate provider schemas: %v %v", defs, err)
	}
}

func TestAliasDedupDoesNotHideDifferentToolsWithTheSameProviderName(t *testing.T) {
	tr := tool.NewRegistry()
	_ = tr.RegisterMCP("a_b", "Read", "", nil, jsonExec("one"))
	_ = tr.RegisterMCP("a", "b_Read", "", nil, jsonExec("two"))
	ex := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithToolRegistry(tr))
	_, err := ex.resolveToolsForNode(tool.WithBuiltinAliases(context.Background(), true), &ir.AgentNode{BaseNode: ir.BaseNode{ID: "x"}}, []string{"Read", "mcp.a.b_Read"})
	if err == nil || !strings.Contains(err.Error(), "same model tool name") {
		t.Fatalf("provider-name collision hidden by dedup: %v", err)
	}
}

func TestAliasPolicyUsesRegistryIdentityNotProviderName(t *testing.T) {
	tr := tool.NewRegistry()
	_ = tr.RegisterMCP("one", "Read", "", nil, jsonExec("MCP"))
	_ = tr.RegisterBuiltin("mcp_one_Read", "", nil, jsonExec("different builtin"))
	ex := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithToolRegistry(tr), WithToolPolicy(tool.NewPolicy("Read")))
	defs, err := ex.resolveToolsForNode(tool.WithBuiltinAliases(context.Background(), true), &ir.AgentNode{BaseNode: ir.BaseNode{ID: "x"}}, []string{"mcp_one_Read"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = defs[0].Execute(context.Background(), nil)
	if !errors.Is(err, tool.ErrToolDenied) {
		t.Fatalf("MCP alias granted a different registry identity: %v", err)
	}
}
