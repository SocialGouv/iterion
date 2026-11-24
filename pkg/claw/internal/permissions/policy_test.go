package permissions

import "testing"

// recordingPrompter implements PermissionPrompter and records all requests.
type recordingPrompter struct {
	seen  []PermissionRequest
	allow bool
}

func (p *recordingPrompter) Decide(req *PermissionRequest) PermissionPromptDecision {
	p.seen = append(p.seen, *req)
	if p.allow {
		return PermissionPromptDecision{Allowed: true}
	}
	return PermissionPromptDecision{Reason: "not now"}
}

func TestAllowsToolsWhenActiveModeExceedsRequirement(t *testing.T) {
	// Matches Rust test: allows_tools_when_active_mode_meets_requirement
	policy := NewPermissionPolicy(ModeWorkspaceWrite).
		WithToolRequirement("read_file", ModeReadOnly).
		WithToolRequirement("write_file", ModeWorkspaceWrite)

	outcome := policy.Authorize("read_file", "{}", nil)
	if !outcome.Allowed {
		t.Errorf("expected Allow for read_file, got Deny: %s", outcome.Reason)
	}

	outcome = policy.Authorize("write_file", "{}", nil)
	if !outcome.Allowed {
		t.Errorf("expected Allow for write_file, got Deny: %s", outcome.Reason)
	}
}

func TestDeniesReadOnlyEscalationsWithoutPrompt(t *testing.T) {
	// Matches Rust test: denies_read_only_escalations_without_prompt
	policy := NewPermissionPolicy(ModeReadOnly).
		WithToolRequirement("write_file", ModeWorkspaceWrite).
		WithToolRequirement("bash", ModeDangerFullAccess)

	outcome := policy.Authorize("write_file", "{}", nil)
	if outcome.Allowed {
		t.Error("expected Deny for write_file from ReadOnly")
	}
	if outcome.Reason == "" {
		t.Error("denial should include a reason")
	}

	outcome = policy.Authorize("bash", "{}", nil)
	if outcome.Allowed {
		t.Error("expected Deny for bash from ReadOnly")
	}
}

func TestPromptsForWorkspaceWriteToDangerEscalation(t *testing.T) {
	// Matches Rust test: prompts_for_workspace_write_to_danger_full_access_escalation
	policy := NewPermissionPolicy(ModeWorkspaceWrite).
		WithToolRequirement("bash", ModeDangerFullAccess)

	prompter := &recordingPrompter{allow: true}
	outcome := policy.Authorize("bash", "echo hi", prompter)

	if !outcome.Allowed {
		t.Errorf("expected Allow after prompt, got Deny: %s", outcome.Reason)
	}
	if len(prompter.seen) != 1 {
		t.Fatalf("expected 1 prompt, got %d", len(prompter.seen))
	}
	if prompter.seen[0].ToolName != "bash" {
		t.Errorf("expected tool_name=bash, got %s", prompter.seen[0].ToolName)
	}
	if prompter.seen[0].CurrentMode != ModeWorkspaceWrite {
		t.Errorf("expected current_mode=workspace-write, got %s", prompter.seen[0].CurrentMode)
	}
	if prompter.seen[0].RequiredMode != ModeDangerFullAccess {
		t.Errorf("expected required_mode=danger-full-access, got %s", prompter.seen[0].RequiredMode)
	}
}

func TestHonorsPromptRejectionReason(t *testing.T) {
	// Matches Rust test: honors_prompt_rejection_reason
	policy := NewPermissionPolicy(ModeWorkspaceWrite).
		WithToolRequirement("bash", ModeDangerFullAccess)

	prompter := &recordingPrompter{allow: false}
	outcome := policy.Authorize("bash", "echo hi", prompter)

	if outcome.Allowed {
		t.Error("expected Deny when prompter rejects")
	}
	if outcome.Reason != "not now" {
		t.Errorf("expected reason='not now', got %q", outcome.Reason)
	}
}

func TestAppliesRuleBasedDenialsAndAllows(t *testing.T) {
	// Matches Rust test: applies_rule_based_denials_and_allows
	policy := NewPermissionPolicy(ModeReadOnly).
		WithToolRequirement("bash", ModeDangerFullAccess).
		WithPermissionRules(
			[]string{"bash(git:*)"},
			[]string{"bash(rm -rf:*)"},
			nil,
		)

	outcome := policy.Authorize("bash", `{"command":"git status"}`, nil)
	if !outcome.Allowed {
		t.Errorf("expected Allow for git command via allow rule, got Deny: %s", outcome.Reason)
	}

	outcome = policy.Authorize("bash", `{"command":"rm -rf /tmp/x"}`, nil)
	if outcome.Allowed {
		t.Error("expected Deny for rm -rf via deny rule")
	}
	if outcome.Reason == "" {
		t.Error("denial should include reason mentioning the rule")
	}
}

func TestAskRulesForcePromptEvenWhenModeAllows(t *testing.T) {
	// Matches Rust test: ask_rules_force_prompt_even_when_mode_allows
	policy := NewPermissionPolicy(ModeDangerFullAccess).
		WithToolRequirement("bash", ModeDangerFullAccess).
		WithPermissionRules(nil, nil, []string{"bash(git:*)"})

	prompter := &recordingPrompter{allow: true}
	outcome := policy.Authorize("bash", `{"command":"git status"}`, prompter)

	if !outcome.Allowed {
		t.Errorf("expected Allow after prompt, got Deny: %s", outcome.Reason)
	}
	if len(prompter.seen) != 1 {
		t.Fatal("expected exactly 1 prompt for ask rule")
	}
	if prompter.seen[0].Reason == "" {
		t.Error("prompt reason should mention ask rule")
	}
}

func TestHookAllowStillRespectsAskRules(t *testing.T) {
	// Matches Rust test: hook_allow_still_respects_ask_rules
	policy := NewPermissionPolicy(ModeReadOnly).
		WithToolRequirement("bash", ModeDangerFullAccess).
		WithPermissionRules(nil, nil, []string{"bash(git:*)"})

	override := OverrideAllow
	context := &PermissionContext{
		OverrideDecision: &override,
		OverrideReason:   "hook approved",
	}

	prompter := &recordingPrompter{allow: true}
	outcome := policy.AuthorizeWithContext("bash", `{"command":"git status"}`, context, prompter)

	if !outcome.Allowed {
		t.Errorf("expected Allow after prompt, got Deny: %s", outcome.Reason)
	}
	if len(prompter.seen) != 1 {
		t.Fatal("expected prompt even with hook Allow due to ask rule")
	}
}

func TestHookDenyShortCircuitsPermissionFlow(t *testing.T) {
	// Matches Rust test: hook_deny_short_circuits_permission_flow
	policy := NewPermissionPolicy(ModeDangerFullAccess).
		WithToolRequirement("bash", ModeDangerFullAccess)

	override := OverrideDeny
	context := &PermissionContext{
		OverrideDecision: &override,
		OverrideReason:   "blocked by hook",
	}

	outcome := policy.AuthorizeWithContext("bash", "{}", context, nil)

	if outcome.Allowed {
		t.Error("expected Deny with hook deny override")
	}
	if outcome.Reason != "blocked by hook" {
		t.Errorf("expected reason='blocked by hook', got %q", outcome.Reason)
	}
}

func TestHookAskForcesPrompt(t *testing.T) {
	// Matches Rust test: hook_ask_forces_prompt
	policy := NewPermissionPolicy(ModeDangerFullAccess).
		WithToolRequirement("bash", ModeDangerFullAccess)

	override := OverrideAsk
	context := &PermissionContext{
		OverrideDecision: &override,
		OverrideReason:   "hook requested confirmation",
	}

	prompter := &recordingPrompter{allow: true}
	outcome := policy.AuthorizeWithContext("bash", "{}", context, prompter)

	if !outcome.Allowed {
		t.Errorf("expected Allow after prompt, got Deny: %s", outcome.Reason)
	}
	if len(prompter.seen) != 1 {
		t.Fatal("expected prompt from hook ask override")
	}
	if prompter.seen[0].Reason != "hook requested confirmation" {
		t.Errorf("expected reason='hook requested confirmation', got %q", prompter.seen[0].Reason)
	}
}

func TestAllowModePermitsEverything(t *testing.T) {
	policy := NewPermissionPolicy(ModeAllow).
		WithToolRequirement("bash", ModeDangerFullAccess)

	outcome := policy.Authorize("bash", "{}", nil)
	if !outcome.Allowed {
		t.Errorf("expected Allow in Allow mode, got Deny: %s", outcome.Reason)
	}
}

func TestDenyRuleTakesPrecedenceOverAllowMode(t *testing.T) {
	policy := NewPermissionPolicy(ModeAllow).
		WithPermissionRules(nil, []string{"bash(rm:*)"}, nil)

	outcome := policy.Authorize("bash", `{"command":"rm -rf /"}`, nil)
	if outcome.Allowed {
		t.Error("deny rule should take precedence over Allow mode")
	}
}

func TestPromptModeAllowsWhenSufficientLevel(t *testing.T) {
	// Prompt mode (level 3) >= DangerFullAccess (level 2), so tools requiring
	// DangerFullAccess are allowed without prompting.
	policy := NewPermissionPolicy(ModePrompt).
		WithToolRequirement("bash", ModeDangerFullAccess)

	outcome := policy.Authorize("bash", "{}", nil)
	if !outcome.Allowed {
		t.Errorf("Prompt(3) >= DangerFullAccess(2) should Allow, got Deny: %s", outcome.Reason)
	}
}

func TestPromptModePromptsForHigherRequirement(t *testing.T) {
	// When a tool requires Allow mode (level 4) and current mode is Prompt (3),
	// the escalation path triggers a prompt.
	policy := NewPermissionPolicy(ModePrompt).
		WithToolRequirement("dangerous_tool", ModeAllow)

	// Without prompter → deny
	outcome := policy.Authorize("dangerous_tool", "{}", nil)
	if outcome.Allowed {
		t.Error("expected Deny in Prompt mode without prompter when requiring Allow")
	}

	// With prompter → prompt
	prompter := &recordingPrompter{allow: true}
	outcome = policy.Authorize("dangerous_tool", "{}", prompter)
	if !outcome.Allowed {
		t.Error("expected Allow in Prompt mode with approving prompter")
	}
}

func TestUnknownToolDefaultsToDangerFullAccess(t *testing.T) {
	policy := NewPermissionPolicy(ModeWorkspaceWrite)

	outcome := policy.Authorize("unknown_tool", "{}", nil)
	if outcome.Allowed {
		t.Error("unknown tool should default to DangerFullAccess requirement and be denied from WorkspaceWrite")
	}
}
