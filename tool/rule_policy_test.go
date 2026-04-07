package tool

import (
	"errors"
	"testing"
)

// ---------------------------------------------------------------------------
// Rule matching by NodeID
// ---------------------------------------------------------------------------

func TestRulePolicyMatchByNodeID(t *testing.T) {
	rp := &RulePolicy{
		Rules: []Rule{
			{NodeIDs: []string{"agent1"}, Allow: []string{"git_diff"}},
		},
		Fallback: DenyAllPolicy(),
	}

	// agent1 can use git_diff
	err := rp.CheckContext(PolicyContext{NodeID: "agent1", ToolName: "git_diff"})
	if err != nil {
		t.Errorf("agent1 + git_diff should be allowed, got: %v", err)
	}

	// agent1 cannot use write_file (not in allow list)
	err = rp.CheckContext(PolicyContext{NodeID: "agent1", ToolName: "write_file"})
	if err == nil {
		t.Error("agent1 + write_file should be denied")
	}

	// agent2 falls through to fallback (deny all)
	err = rp.CheckContext(PolicyContext{NodeID: "agent2", ToolName: "git_diff"})
	if err == nil {
		t.Error("agent2 should fall through to deny-all fallback")
	}
}

// ---------------------------------------------------------------------------
// Rule matching by NodeKind
// ---------------------------------------------------------------------------

func TestRulePolicyMatchByNodeKind(t *testing.T) {
	rp := &RulePolicy{
		Rules: []Rule{
			{NodeKind: "judge", Allow: []string{"read_file"}},
		},
		Fallback: DenyAllPolicy(),
	}

	err := rp.CheckContext(PolicyContext{NodeKind: "judge", ToolName: "read_file"})
	if err != nil {
		t.Errorf("judge + read_file should be allowed, got: %v", err)
	}

	err = rp.CheckContext(PolicyContext{NodeKind: "agent", ToolName: "read_file"})
	if err == nil {
		t.Error("agent should fall through to deny-all fallback")
	}
}

// ---------------------------------------------------------------------------
// Rule matching by VarMatch
// ---------------------------------------------------------------------------

func TestRulePolicyMatchByVarMatch(t *testing.T) {
	rp := &RulePolicy{
		Rules: []Rule{
			{
				VarMatch: map[string]interface{}{"env": "production"},
				Allow:    []string{"read_file"},
			},
		},
		Fallback: NewPolicy("*"),
	}

	// production env — only read_file
	err := rp.CheckContext(PolicyContext{
		ToolName: "read_file",
		Vars:     map[string]interface{}{"env": "production"},
	})
	if err != nil {
		t.Errorf("production + read_file should be allowed, got: %v", err)
	}

	err = rp.CheckContext(PolicyContext{
		ToolName: "write_file",
		Vars:     map[string]interface{}{"env": "production"},
	})
	if err == nil {
		t.Error("production + write_file should be denied")
	}

	// staging env — falls through to open fallback
	err = rp.CheckContext(PolicyContext{
		ToolName: "write_file",
		Vars:     map[string]interface{}{"env": "staging"},
	})
	if err != nil {
		t.Errorf("staging should allow everything via fallback, got: %v", err)
	}
}

func TestRulePolicyVarMatchBoolAndNumber(t *testing.T) {
	rp := &RulePolicy{
		Rules: []Rule{
			{
				VarMatch: map[string]interface{}{"debug": true, "retries": 3},
				Allow:    []string{"debug_tool"},
			},
		},
		Fallback: DenyAllPolicy(),
	}

	err := rp.CheckContext(PolicyContext{
		ToolName: "debug_tool",
		Vars:     map[string]interface{}{"debug": true, "retries": 3},
	})
	if err != nil {
		t.Errorf("should match bool/number vars, got: %v", err)
	}

	err = rp.CheckContext(PolicyContext{
		ToolName: "debug_tool",
		Vars:     map[string]interface{}{"debug": false, "retries": 3},
	})
	if err == nil {
		t.Error("should not match when debug=false")
	}
}

// ---------------------------------------------------------------------------
// First-match-wins behavior
// ---------------------------------------------------------------------------

func TestRulePolicyFirstMatchWins(t *testing.T) {
	rp := &RulePolicy{
		Rules: []Rule{
			{NodeIDs: []string{"agent1"}, Allow: []string{"git_diff"}},
			{NodeIDs: []string{"agent1"}, Allow: []string{"git_diff", "write_file"}},
		},
		Fallback: DenyAllPolicy(),
	}

	// First rule matches agent1 — only git_diff allowed, not write_file
	err := rp.CheckContext(PolicyContext{NodeID: "agent1", ToolName: "write_file"})
	if err == nil {
		t.Error("first-match-wins: agent1 should only get git_diff from first rule")
	}

	err = rp.CheckContext(PolicyContext{NodeID: "agent1", ToolName: "git_diff"})
	if err != nil {
		t.Errorf("agent1 + git_diff should be allowed by first rule, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Fallback when no rule matches
// ---------------------------------------------------------------------------

func TestRulePolicyFallback(t *testing.T) {
	// Nil fallback = open
	rp := &RulePolicy{
		Rules: []Rule{
			{NodeIDs: []string{"agent1"}, Allow: []string{"git_diff"}},
		},
		Fallback: nil,
	}

	err := rp.CheckContext(PolicyContext{NodeID: "agent2", ToolName: "anything"})
	if err != nil {
		t.Errorf("nil fallback should allow everything, got: %v", err)
	}

	// Non-nil fallback
	rp.Fallback = NewPolicy("read_file")
	err = rp.CheckContext(PolicyContext{NodeID: "agent2", ToolName: "read_file"})
	if err != nil {
		t.Errorf("fallback should allow read_file, got: %v", err)
	}

	err = rp.CheckContext(PolicyContext{NodeID: "agent2", ToolName: "write_file"})
	if !errors.Is(err, ErrToolDenied) {
		t.Errorf("fallback should deny write_file, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Deny rules
// ---------------------------------------------------------------------------

func TestRulePolicyDenyRule(t *testing.T) {
	rp := &RulePolicy{
		Rules: []Rule{
			{
				NodeKind: "agent",
				Allow:    []string{"dangerous_tool"},
				Deny:     true,
			},
		},
		Fallback: NewPolicy("*"),
	}

	// Agent calling dangerous_tool → denied
	err := rp.CheckContext(PolicyContext{NodeKind: "agent", ToolName: "dangerous_tool"})
	if !errors.Is(err, ErrToolDenied) {
		t.Errorf("deny rule should block dangerous_tool, got: %v", err)
	}

	// Agent calling safe_tool → deny rule matches but tool doesn't match patterns → allowed
	err = rp.CheckContext(PolicyContext{NodeKind: "agent", ToolName: "safe_tool"})
	if err != nil {
		t.Errorf("deny rule should not block safe_tool, got: %v", err)
	}

	// Judge calling dangerous_tool → rule doesn't match, fallback allows
	err = rp.CheckContext(PolicyContext{NodeKind: "judge", ToolName: "dangerous_tool"})
	if err != nil {
		t.Errorf("judge should be allowed via fallback, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Combined conditions (NodeID + VarMatch)
// ---------------------------------------------------------------------------

func TestRulePolicyCombinedConditions(t *testing.T) {
	rp := &RulePolicy{
		Rules: []Rule{
			{
				NodeIDs:  []string{"deploy_agent"},
				VarMatch: map[string]interface{}{"env": "production"},
				Allow:    []string{"deploy_tool"},
			},
		},
		Fallback: DenyAllPolicy(),
	}

	// Both conditions met
	err := rp.CheckContext(PolicyContext{
		NodeID:   "deploy_agent",
		ToolName: "deploy_tool",
		Vars:     map[string]interface{}{"env": "production"},
	})
	if err != nil {
		t.Errorf("combined match should allow, got: %v", err)
	}

	// NodeID matches but vars don't
	err = rp.CheckContext(PolicyContext{
		NodeID:   "deploy_agent",
		ToolName: "deploy_tool",
		Vars:     map[string]interface{}{"env": "staging"},
	})
	if err == nil {
		t.Error("should deny when vars don't match")
	}

	// Vars match but NodeID doesn't
	err = rp.CheckContext(PolicyContext{
		NodeID:   "other_agent",
		ToolName: "deploy_tool",
		Vars:     map[string]interface{}{"env": "production"},
	})
	if err == nil {
		t.Error("should deny when NodeID doesn't match")
	}
}
