package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// An UNRESTRICTED node (`tools:` empty) that opts into interaction must stay
// unrestricted.
//
// Why this is the whole defect rather than a cosmetic detail: downstream, the
// claude_code backend treats a NON-EMPTY AllowedTools as a restrictive
// boundary and removes every native tool the list does not name
// (claudeNativeDisallowedTools). `ask_user` names none of them. So promoting
// it here turned "the author declared nothing" into "the author allowed
// exactly one tool", and the agent lost Bash, Read, Write, Edit, Glob and
// Grep in one step — it could only ask a question, which is precisely what it
// then did.
func TestAssembleEffectiveTools_InteractionDoesNotRestrictAnUnrestrictedNode(t *testing.T) {
	e := &ClawExecutor{botID: "tester"}
	f := backendFields{id: "n", interaction: ir.InteractionHuman}

	got := e.assembleEffectiveTools(f, delegate.BackendClaudeCode, nil, false)

	if len(got) != 0 {
		t.Errorf("an unrestricted node with interaction must stay unrestricted, got %v", got)
	}
}

// The opt-in still has to work where it was designed to: a node that DOES
// restrict its tools gets `ask_user` appended, so a restricted agent can still
// escalate without the author having to remember to declare it.
func TestAssembleEffectiveTools_InteractionGrantsAskUserToARestrictedNode(t *testing.T) {
	e := &ClawExecutor{botID: "tester"}
	f := backendFields{id: "n", tools: []string{"read"}, interaction: ir.InteractionHuman}

	got := e.assembleEffectiveTools(f, delegate.BackendClaudeCode, nil, false)

	var hasAskUser, hasRead bool
	for _, name := range got {
		switch name {
		case askUserToolName:
			hasAskUser = true
		case "read":
			hasRead = true
		}
	}
	if !hasAskUser {
		t.Errorf("a restricted node with interaction must be granted %q, got %v", askUserToolName, got)
	}
	if !hasRead {
		t.Errorf("the author's own declaration must survive, got %v", got)
	}
}

// Without interaction, nothing is added either way — the guard must not be
// read as "interaction now does nothing".
func TestAssembleEffectiveTools_NoInteractionAddsNothing(t *testing.T) {
	e := &ClawExecutor{botID: "tester"}
	f := backendFields{id: "n", tools: []string{"read"}}

	got := e.assembleEffectiveTools(f, delegate.BackendClaudeCode, nil, false)

	for _, name := range got {
		if name == askUserToolName {
			t.Errorf("a node without interaction must not receive %q, got %v", askUserToolName, got)
		}
	}
}

// The bench that MORDS: the consequence, not the site. buildTask is what the
// runtime actually calls, and AllowedTools is what the CLI backends read. An
// unrestricted interactive node must leave it EMPTY, because a single entry
// there is what strips the native surface.
//
// Asserted through the field the backend reads rather than through
// assembleEffectiveTools' return value: the two are joined by a
// `!sameStringSlice` condition, and it is that join — not the helper — which
// decided the agent had no tools.
func TestAssembleEffectiveTools_UnrestrictedInteractiveNodeLeavesAllowedToolsEmpty(t *testing.T) {
	e := &ClawExecutor{botID: "tester"}
	f := backendFields{id: "n", interaction: ir.InteractionHuman}

	effective := e.assembleEffectiveTools(f, delegate.BackendClaudeCode, nil, false)

	// This mirrors the assignment in buildTask: AllowedTools is only set when
	// the assembled list DIFFERS from what the author declared.
	if !sameStringSlice(effective, f.tools) {
		t.Fatalf("assembled tools diverge from the author's declaration (%v vs %v) — "+
			"buildTask would set AllowedTools, and any non-empty AllowedTools "+
			"makes claude_code strip every native tool", effective, f.tools)
	}
}
