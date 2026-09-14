package tool

import (
	"context"
	"strings"
	"testing"
)

func TestBuiltinAliasResolutionKeepsMCPPrecedence(t *testing.T) {
	for _, tc := range []struct{ alias, canonical string }{{"Read", "read_file"}, {"Bash", "bash"}, {"Grep", "grep"}} {
		t.Run(tc.alias, func(t *testing.T) {
			r := NewRegistry()
			_ = r.RegisterBuiltin(tc.canonical, "", nil, noop)
			if _, err := r.Resolve(tc.alias); err == nil {
				t.Fatal("legacy resolver enabled alias without opt-in")
			}
			def, err := r.ResolveWithAliases(tc.alias)
			if err != nil || def.QualifiedName != tc.canonical {
				t.Fatalf("alias: %v %v", def, err)
			}
			for _, ref := range []string{"mcp.missing." + tc.alias, "mcp__missing__" + tc.alias, strings.ToUpper(tc.alias)} {
				if _, err := r.ResolveWithAliases(ref); err == nil {
					t.Fatalf("qualified/case-folded alias %q resolved", ref)
				}
			}
			_ = r.RegisterMCP("one", tc.alias, "", nil, noop)
			for _, resolve := range []func(string) (*ToolDef, error){r.Resolve, r.ResolveWithAliases} {
				def, err = resolve(tc.alias)
				if err != nil || def.QualifiedName != "mcp.one."+tc.alias {
					t.Fatalf("unique MCP: %v %v", def, err)
				}
			}
			_ = r.RegisterMCP("two", tc.alias, "", nil, noop)
			for _, resolve := range []func(string) (*ToolDef, error){r.Resolve, r.ResolveWithAliases} {
				if _, err = resolve(tc.alias); err == nil || !strings.Contains(err.Error(), "ambiguous") {
					t.Fatalf("ambiguity hidden: %v", err)
				}
			}
			_ = r.RegisterBuiltin(tc.alias, "", nil, noop)
			def, err = r.ResolveWithAliases(tc.alias)
			if err != nil || def.QualifiedName != tc.alias {
				t.Fatalf("exact builtin lost: %v %v", def, err)
			}
		})
	}
}

func TestBuiltinAliasContextDoesNotInheritIntoBareChild(t *testing.T) {
	parent := WithBuiltinAliases(context.Background(), true)
	if !BuiltinAliasesEnabled(parent) {
		t.Fatal("parent opt-in absent")
	}
	child := WithBuiltinAliases(parent, false)
	if BuiltinAliasesEnabled(child) {
		t.Fatal("child inherited parent's opt-in")
	}
}
