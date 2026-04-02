package tool

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// noop is a no-op execute function for testing.
func noop(_ context.Context, _ json.RawMessage) (string, error) { return "{}", nil }

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

func TestRegisterBuiltin(t *testing.T) {
	r := NewRegistry()
	err := r.RegisterBuiltin("git_diff", "Show git diff", nil, noop)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Len() != 1 {
		t.Fatalf("expected 1 tool, got %d", r.Len())
	}
}

func TestRegisterBuiltinRejectsDots(t *testing.T) {
	r := NewRegistry()
	err := r.RegisterBuiltin("mcp.foo.bar", "bad", nil, noop)
	if err == nil {
		t.Fatal("expected error for dotted builtin name")
	}
}

func TestRegisterBuiltinRejectsEmpty(t *testing.T) {
	r := NewRegistry()
	err := r.RegisterBuiltin("", "empty", nil, noop)
	if err == nil {
		t.Fatal("expected error for empty builtin name")
	}
}

func TestRegisterMCP(t *testing.T) {
	r := NewRegistry()
	err := r.RegisterMCP("github", "create_issue", "Create an issue", nil, noop)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	td, err := r.Resolve("mcp.github.create_issue")
	if err != nil {
		t.Fatalf("unexpected resolve error: %v", err)
	}
	if td.Origin.Kind != OriginMCP {
		t.Errorf("expected OriginMCP, got %v", td.Origin.Kind)
	}
	if td.Origin.Server != "github" {
		t.Errorf("expected server 'github', got %q", td.Origin.Server)
	}
}

func TestRegisterMCPRejectsEmptyServer(t *testing.T) {
	r := NewRegistry()
	err := r.RegisterMCP("", "tool", "bad", nil, noop)
	if err == nil {
		t.Fatal("expected error for empty server name")
	}
}

func TestRegisterMCPRejectsEmptyTool(t *testing.T) {
	r := NewRegistry()
	err := r.RegisterMCP("server", "", "bad", nil, noop)
	if err == nil {
		t.Fatal("expected error for empty tool name")
	}
}

func TestRegisterMCPRejectsDottedServer(t *testing.T) {
	r := NewRegistry()
	err := r.RegisterMCP("my.server", "tool", "bad", nil, noop)
	if err == nil {
		t.Fatal("expected error for dotted server name")
	}
}

func TestRegisterMCPRejectsDottedTool(t *testing.T) {
	r := NewRegistry()
	err := r.RegisterMCP("server", "my.tool", "bad", nil, noop)
	if err == nil {
		t.Fatal("expected error for dotted tool name")
	}
}

// ---------------------------------------------------------------------------
// Collision detection
// ---------------------------------------------------------------------------

func TestCollisionBuiltinBuiltin(t *testing.T) {
	r := NewRegistry()
	_ = r.RegisterBuiltin("git_diff", "first", nil, noop)
	err := r.RegisterBuiltin("git_diff", "second", nil, noop)
	if err == nil {
		t.Fatal("expected collision error")
	}
	if !strings.Contains(err.Error(), "collision") {
		t.Errorf("expected collision in error message, got: %v", err)
	}
}

func TestCollisionMCPMCP(t *testing.T) {
	r := NewRegistry()
	_ = r.RegisterMCP("github", "create_issue", "first", nil, noop)
	err := r.RegisterMCP("github", "create_issue", "second", nil, noop)
	if err == nil {
		t.Fatal("expected collision error")
	}
}

func TestNoCollisionSameToolDifferentServers(t *testing.T) {
	r := NewRegistry()
	err := r.RegisterMCP("github", "list_repos", "GitHub repos", nil, noop)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	err = r.RegisterMCP("gitlab", "list_repos", "GitLab repos", nil, noop)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Len() != 2 {
		t.Fatalf("expected 2 tools, got %d", r.Len())
	}
}

func TestNoCollisionBuiltinAndMCPSameName(t *testing.T) {
	r := NewRegistry()
	_ = r.RegisterBuiltin("run_tests", "built-in", nil, noop)
	err := r.RegisterMCP("ci", "run_tests", "MCP", nil, noop)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Resolution
// ---------------------------------------------------------------------------

func TestResolveBuiltinExact(t *testing.T) {
	r := NewRegistry()
	_ = r.RegisterBuiltin("git_diff", "diff", nil, noop)

	td, err := r.Resolve("git_diff")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if td.QualifiedName != "git_diff" {
		t.Errorf("expected 'git_diff', got %q", td.QualifiedName)
	}
}

func TestResolveMCPExact(t *testing.T) {
	r := NewRegistry()
	_ = r.RegisterMCP("github", "create_issue", "Create issue", nil, noop)

	td, err := r.Resolve("mcp.github.create_issue")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if td.QualifiedName != "mcp.github.create_issue" {
		t.Errorf("expected 'mcp.github.create_issue', got %q", td.QualifiedName)
	}
}

func TestResolveMCPShorthand(t *testing.T) {
	r := NewRegistry()
	_ = r.RegisterMCP("github", "create_issue", "Create issue", nil, noop)

	// No builtin named "create_issue", only one MCP tool with that suffix.
	td, err := r.Resolve("create_issue")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if td.QualifiedName != "mcp.github.create_issue" {
		t.Errorf("expected 'mcp.github.create_issue', got %q", td.QualifiedName)
	}
}

func TestResolveShorthandPrefersBuiltin(t *testing.T) {
	r := NewRegistry()
	_ = r.RegisterBuiltin("run_tests", "built-in", nil, noop)
	_ = r.RegisterMCP("ci", "run_tests", "MCP", nil, noop)

	// Exact match on builtin wins over shorthand.
	td, err := r.Resolve("run_tests")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if td.Origin.Kind != OriginBuiltin {
		t.Errorf("expected builtin to win exact match, got %v", td.Origin.Kind)
	}
}

func TestResolveShorthandAmbiguous(t *testing.T) {
	r := NewRegistry()
	_ = r.RegisterMCP("github", "list_repos", "GitHub", nil, noop)
	_ = r.RegisterMCP("gitlab", "list_repos", "GitLab", nil, noop)

	_, err := r.Resolve("list_repos")
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("expected 'ambiguous' in error, got: %v", err)
	}
}

func TestResolveUnknown(t *testing.T) {
	r := NewRegistry()
	_, err := r.Resolve("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("expected 'unknown' in error, got: %v", err)
	}
}

func TestResolveUnknownMCPQualified(t *testing.T) {
	r := NewRegistry()
	_, err := r.Resolve("mcp.nope.nada")
	if err == nil {
		t.Fatal("expected error for unknown MCP tool")
	}
}

// ---------------------------------------------------------------------------
// Listing
// ---------------------------------------------------------------------------

func TestList(t *testing.T) {
	r := NewRegistry()
	_ = r.RegisterBuiltin("git_diff", "diff", nil, noop)
	_ = r.RegisterMCP("github", "create_issue", "issue", nil, noop)

	all := r.List()
	if len(all) != 2 {
		t.Fatalf("expected 2, got %d", len(all))
	}
}

func TestListByOrigin(t *testing.T) {
	r := NewRegistry()
	_ = r.RegisterBuiltin("git_diff", "diff", nil, noop)
	_ = r.RegisterBuiltin("run_tests", "tests", nil, noop)
	_ = r.RegisterMCP("github", "create_issue", "issue", nil, noop)

	builtins := r.ListByOrigin(OriginBuiltin)
	if len(builtins) != 2 {
		t.Fatalf("expected 2 builtins, got %d", len(builtins))
	}

	mcps := r.ListByOrigin(OriginMCP)
	if len(mcps) != 1 {
		t.Fatalf("expected 1 MCP, got %d", len(mcps))
	}
}

// ---------------------------------------------------------------------------
// ParseMCPName
// ---------------------------------------------------------------------------

func TestParseMCPNameValid(t *testing.T) {
	server, toolName, err := ParseMCPName("mcp.github.create_issue")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if server != "github" {
		t.Errorf("expected server 'github', got %q", server)
	}
	if toolName != "create_issue" {
		t.Errorf("expected tool 'create_issue', got %q", toolName)
	}
}

func TestParseMCPNameInvalid(t *testing.T) {
	cases := []string{
		"git_diff",
		"mcp.",
		"mcp.server",
		"mcp.server.",
		"mcp..tool",
	}
	for _, c := range cases {
		_, _, err := ParseMCPName(c)
		if err == nil {
			t.Errorf("expected error for %q", c)
		}
	}
}

func TestIsMCPName(t *testing.T) {
	if !IsMCPName("mcp.github.create_issue") {
		t.Error("expected true for MCP name")
	}
	if IsMCPName("git_diff") {
		t.Error("expected false for builtin name")
	}
}

// ---------------------------------------------------------------------------
// ListByServer
// ---------------------------------------------------------------------------

func TestListByServer(t *testing.T) {
	r := NewRegistry()
	_ = r.RegisterMCP("github", "create_issue", "issue", nil, noop)
	_ = r.RegisterMCP("github", "list_repos", "repos", nil, noop)
	_ = r.RegisterMCP("gitlab", "list_repos", "repos", nil, noop)

	gh := r.ListByServer("github")
	if len(gh) != 2 {
		t.Fatalf("expected 2 github tools, got %d", len(gh))
	}
	gl := r.ListByServer("gitlab")
	if len(gl) != 1 {
		t.Fatalf("expected 1 gitlab tool, got %d", len(gl))
	}
	none := r.ListByServer("unknown")
	if len(none) != 0 {
		t.Fatalf("expected 0 tools for unknown server, got %d", len(none))
	}
}

// ---------------------------------------------------------------------------
// MCP Wildcard
// ---------------------------------------------------------------------------

func TestIsMCPWildcard(t *testing.T) {
	if !IsMCPWildcard("mcp.claude_code.*") {
		t.Error("expected true for mcp.claude_code.*")
	}
	if !IsMCPWildcard("mcp.codex.*") {
		t.Error("expected true for mcp.codex.*")
	}
	if IsMCPWildcard("mcp.github.create_issue") {
		t.Error("expected false for non-wildcard MCP name")
	}
	if IsMCPWildcard("git_diff") {
		t.Error("expected false for builtin name")
	}
	if IsMCPWildcard("mcp..*") {
		t.Error("expected false for empty server")
	}
}

func TestParseMCPWildcard(t *testing.T) {
	server, err := ParseMCPWildcard("mcp.claude_code.*")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if server != "claude_code" {
		t.Errorf("expected 'claude_code', got %q", server)
	}

	// Invalid cases.
	for _, c := range []string{"mcp.github.tool", "git_diff", "mcp.*", "mcp..*", "mcp.a.b.*"} {
		_, err := ParseMCPWildcard(c)
		if err == nil {
			t.Errorf("expected error for %q", c)
		}
	}
}

// ---------------------------------------------------------------------------
// Adapter — ToGoaiTool
// ---------------------------------------------------------------------------

func TestToGoaiTool(t *testing.T) {
	called := false
	td := &ToolDef{
		QualifiedName: "mcp.github.create_issue",
		Description:   "Create a GitHub issue",
		InputSchema:   json.RawMessage(`{"type":"object"}`),
		Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			called = true
			return `{"id":42}`, nil
		},
		Origin: Origin{Kind: OriginMCP, Server: "github"},
	}

	gt := td.ToGoaiTool()
	if gt.Name != "mcp_github_create_issue" {
		t.Errorf("expected sanitized name, got %q", gt.Name)
	}
	if gt.Description != "Create a GitHub issue" {
		t.Errorf("unexpected description: %q", gt.Description)
	}

	result, err := gt.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"id":42}` {
		t.Errorf("unexpected result: %q", result)
	}
	if !called {
		t.Error("execute was not called")
	}
}

// ---------------------------------------------------------------------------
// ResolveAll / ResolveMap
// ---------------------------------------------------------------------------

func TestResolveAll(t *testing.T) {
	r := NewRegistry()
	_ = r.RegisterBuiltin("git_diff", "diff", nil, noop)
	_ = r.RegisterMCP("github", "create_issue", "issue", nil, noop)

	tools, err := r.ResolveAll([]string{"git_diff", "mcp.github.create_issue"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
}

func TestResolveAllError(t *testing.T) {
	r := NewRegistry()
	_, err := r.ResolveAll([]string{"nonexistent"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestResolveMap(t *testing.T) {
	r := NewRegistry()
	_ = r.RegisterBuiltin("git_diff", "diff", nil, noop)
	_ = r.RegisterMCP("github", "create_issue", "issue", nil, noop)

	m, err := r.ResolveMap([]string{"git_diff", "mcp.github.create_issue"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(m))
	}
	if _, ok := m["git_diff"]; !ok {
		t.Error("missing git_diff in map")
	}
	if _, ok := m["mcp.github.create_issue"]; !ok {
		t.Error("missing mcp.github.create_issue in map")
	}
}

// ---------------------------------------------------------------------------
// ResolveAll with shorthand
// ---------------------------------------------------------------------------

func TestResolveAllShorthand(t *testing.T) {
	r := NewRegistry()
	_ = r.RegisterMCP("github", "create_issue", "issue", nil, noop)

	tools, err := r.ResolveAll([]string{"create_issue"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Name != "mcp_github_create_issue" {
		t.Errorf("expected sanitized name, got %q", tools[0].Name)
	}
}

// ---------------------------------------------------------------------------
// Origin string
// ---------------------------------------------------------------------------

func TestOriginKindString(t *testing.T) {
	if OriginBuiltin.String() != "builtin" {
		t.Errorf("unexpected: %q", OriginBuiltin.String())
	}
	if OriginMCP.String() != "mcp" {
		t.Errorf("unexpected: %q", OriginMCP.String())
	}
	if OriginKind(99).String() != "unknown" {
		t.Errorf("unexpected: %q", OriginKind(99).String())
	}
}

// ---------------------------------------------------------------------------
// Convenience builders
// ---------------------------------------------------------------------------

func TestNewBuiltinDef(t *testing.T) {
	td := NewBuiltinDef("git_diff", "diff", nil, noop)
	if td.QualifiedName != "git_diff" {
		t.Errorf("unexpected name: %q", td.QualifiedName)
	}
	if td.Origin.Kind != OriginBuiltin {
		t.Errorf("expected OriginBuiltin")
	}
}

func TestNewMCPDef(t *testing.T) {
	td := NewMCPDef("github", "create_issue", "issue", nil, noop)
	if td.QualifiedName != "mcp.github.create_issue" {
		t.Errorf("unexpected name: %q", td.QualifiedName)
	}
	if td.Origin.Kind != OriginMCP {
		t.Errorf("expected OriginMCP")
	}
	if td.Origin.Server != "github" {
		t.Errorf("expected server 'github', got %q", td.Origin.Server)
	}
}

// ---------------------------------------------------------------------------
// mustResolve (unexported — test-only helper)
// ---------------------------------------------------------------------------

func TestMustResolvePanics(t *testing.T) {
	r := NewRegistry()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic")
		}
	}()
	r.mustResolve("nonexistent")
}

func TestMustResolveSucceeds(t *testing.T) {
	r := NewRegistry()
	_ = r.RegisterBuiltin("git_diff", "diff", nil, noop)
	td := r.mustResolve("git_diff")
	if td.QualifiedName != "git_diff" {
		t.Errorf("unexpected name: %q", td.QualifiedName)
	}
}

// ---------------------------------------------------------------------------
// Edge case: shorthand must not match builtin suffix
// ---------------------------------------------------------------------------

func TestResolveShorthandDoesNotMatchBuiltinSuffix(t *testing.T) {
	r := NewRegistry()
	// Register a builtin named "list_repos" — this is an exact match, not shorthand.
	_ = r.RegisterBuiltin("list_repos", "built-in", nil, noop)

	td, err := r.Resolve("list_repos")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if td.Origin.Kind != OriginBuiltin {
		t.Errorf("expected builtin, got %v", td.Origin.Kind)
	}
}

// ---------------------------------------------------------------------------
// Concurrent access
// ---------------------------------------------------------------------------

func TestConcurrentRegistration(t *testing.T) {
	r := NewRegistry()
	done := make(chan error, 100)

	for i := 0; i < 50; i++ {
		go func(n int) {
			name := strings.Replace("tool_XXX", "XXX", string(rune('A'+n%26))+string(rune('0'+n/26)), 1)
			done <- r.RegisterBuiltin(name, "concurrent", nil, noop)
		}(i)
	}
	for i := 0; i < 50; i++ {
		go func(n int) {
			name := strings.Replace("mtool_XXX", "XXX", string(rune('A'+n%26))+string(rune('0'+n/26)), 1)
			done <- r.RegisterMCP("server", name, "concurrent", nil, noop)
		}(i)
	}

	for i := 0; i < 100; i++ {
		<-done
	}
	// Just verify no panic — collisions are expected for some entries.
}

// ---------------------------------------------------------------------------
// Full scenario: workflow tool resolution
// ---------------------------------------------------------------------------

func TestWorkflowToolResolution(t *testing.T) {
	r := NewRegistry()

	// Simulate a workflow with builtin + MCP tools.
	_ = r.RegisterBuiltin("git_diff", "Show diff", nil, noop)
	_ = r.RegisterBuiltin("run_tests", "Run tests", nil, noop)
	_ = r.RegisterMCP("github", "create_issue", "Create issue", nil, noop)
	_ = r.RegisterMCP("github", "list_prs", "List PRs", nil, noop)
	_ = r.RegisterMCP("slack", "send_message", "Send message", nil, noop)

	// A workflow node might reference tools like this:
	refs := []string{
		"git_diff",                // builtin exact
		"run_tests",               // builtin exact
		"mcp.github.create_issue", // MCP exact
		"mcp.github.list_prs",     // MCP exact
		"send_message",            // MCP shorthand (only one server has it)
	}

	tools, err := r.ResolveAll(refs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tools) != 5 {
		t.Fatalf("expected 5 tools, got %d", len(tools))
	}

	// Verify names.
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	sort.Strings(names)

	expected := []string{
		"git_diff",
		"mcp_github_create_issue",
		"mcp_github_list_prs",
		"mcp_slack_send_message",
		"run_tests",
	}
	for i, name := range expected {
		if names[i] != name {
			t.Errorf("expected %q at index %d, got %q", name, i, names[i])
		}
	}
}
