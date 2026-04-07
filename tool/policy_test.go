package tool

import (
	"errors"
	"testing"
)

// ---------------------------------------------------------------------------
// Nil (open) policy
// ---------------------------------------------------------------------------

func TestNilPolicyAllowsEverything(t *testing.T) {
	var p *Policy // nil = open
	for _, name := range []string{"git_diff", "run_command", "mcp.github.create_issue", "anything"} {
		if !p.IsAllowed(name) {
			t.Errorf("nil policy should allow %q", name)
		}
		if err := p.Check(name); err != nil {
			t.Errorf("nil policy Check(%q) returned error: %v", name, err)
		}
	}
}

func TestOpenPolicyHelper(t *testing.T) {
	p := OpenPolicy()
	if p != nil {
		t.Fatal("OpenPolicy should return nil")
	}
}

// ---------------------------------------------------------------------------
// Deny-all policy
// ---------------------------------------------------------------------------

func TestDenyAllPolicy(t *testing.T) {
	p := DenyAllPolicy()
	for _, name := range []string{"git_diff", "run_command", "mcp.github.create_issue"} {
		if p.IsAllowed(name) {
			t.Errorf("deny-all policy should reject %q", name)
		}
		err := p.Check(name)
		if err == nil {
			t.Errorf("deny-all policy Check(%q) should return error", name)
		}
		if !errors.Is(err, ErrToolDenied) {
			t.Errorf("expected ErrToolDenied, got %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Wildcard "*"
// ---------------------------------------------------------------------------

func TestWildcardAllowsAll(t *testing.T) {
	p := NewPolicy("*")
	for _, name := range []string{"git_diff", "run_command", "mcp.github.create_issue"} {
		if !p.IsAllowed(name) {
			t.Errorf("wildcard policy should allow %q", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Exact match
// ---------------------------------------------------------------------------

func TestExactMatch(t *testing.T) {
	p := NewPolicy("git_diff", "run_command")

	if !p.IsAllowed("git_diff") {
		t.Error("should allow git_diff")
	}
	if !p.IsAllowed("run_command") {
		t.Error("should allow run_command")
	}
	if p.IsAllowed("write_file") {
		t.Error("should deny write_file")
	}
	if p.IsAllowed("mcp.github.create_issue") {
		t.Error("should deny mcp.github.create_issue")
	}
}

// ---------------------------------------------------------------------------
// Prefix match (namespace wildcard)
// ---------------------------------------------------------------------------

func TestPrefixMatch(t *testing.T) {
	p := NewPolicy("mcp.github.*")

	if !p.IsAllowed("mcp.github.create_issue") {
		t.Error("should allow mcp.github.create_issue")
	}
	if !p.IsAllowed("mcp.github.list_prs") {
		t.Error("should allow mcp.github.list_prs")
	}
	if p.IsAllowed("mcp.slack.post_message") {
		t.Error("should deny mcp.slack.post_message")
	}
	if p.IsAllowed("git_diff") {
		t.Error("should deny git_diff")
	}
}

// ---------------------------------------------------------------------------
// Mixed patterns
// ---------------------------------------------------------------------------

func TestMixedPatterns(t *testing.T) {
	p := NewPolicy("git_diff", "run_command", "mcp.github.*")

	allowed := []string{"git_diff", "run_command", "mcp.github.create_issue", "mcp.github.list_prs"}
	for _, name := range allowed {
		if !p.IsAllowed(name) {
			t.Errorf("should allow %q", name)
		}
	}

	denied := []string{"write_file", "mcp.slack.post_message", "patch"}
	for _, name := range denied {
		if p.IsAllowed(name) {
			t.Errorf("should deny %q", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Check error wrapping
// ---------------------------------------------------------------------------

func TestCheckErrorContainsToolName(t *testing.T) {
	p := NewPolicy("git_diff")
	err := p.Check("run_command")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrToolDenied) {
		t.Errorf("expected ErrToolDenied, got %v", err)
	}
	// Error message should mention the tool name.
	if got := err.Error(); !contains(got, "run_command") {
		t.Errorf("error should mention tool name, got: %s", got)
	}
}

// ---------------------------------------------------------------------------
// CheckContext on *Policy (ToolChecker interface)
// ---------------------------------------------------------------------------

func TestPolicyCheckContext(t *testing.T) {
	p := NewPolicy("git_diff", "mcp.github.*")

	ctx := PolicyContext{
		NodeID:   "agent1",
		NodeKind: "agent",
		ToolName: "git_diff",
		Vars:     map[string]interface{}{"env": "test"},
	}
	if err := p.CheckContext(ctx); err != nil {
		t.Errorf("CheckContext should allow git_diff, got: %v", err)
	}

	ctx.ToolName = "mcp.github.create_issue"
	if err := p.CheckContext(ctx); err != nil {
		t.Errorf("CheckContext should allow mcp.github.create_issue, got: %v", err)
	}

	ctx.ToolName = "write_file"
	if err := p.CheckContext(ctx); err == nil {
		t.Error("CheckContext should deny write_file")
	}
}

func TestNilPolicyCheckContext(t *testing.T) {
	var p *Policy
	err := p.CheckContext(PolicyContext{ToolName: "anything"})
	if err != nil {
		t.Errorf("nil policy CheckContext should allow everything, got: %v", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
