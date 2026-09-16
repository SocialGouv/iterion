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

// The claw face, and the reason the guard above cannot be backend-blind.
//
// On claw the assembled list is not an allowlist over an ambient surface — it
// IS the surface: buildTask resolves ToolDefs only when the list is non-empty,
// and claw_backend sets opts.Tools from ToolDefs and nothing else. So an empty
// list there means "no tools at all", the opposite of what it means for the
// CLI backends. A tool-less interactive claw node must therefore still be
// granted ask_user, or the tool loop that carries it never exists — the
// assignment site says as much: "claw needs the tool loop active for ask_user".
func TestAssembleEffectiveTools_ClawKeepsAskUserOnAToolLessNode(t *testing.T) {
	e := &ClawExecutor{botID: "tester"}
	f := backendFields{id: "n", interaction: ir.InteractionHuman}

	got := e.assembleEffectiveTools(f, delegate.BackendClaw, nil, false)

	var hasAskUser bool
	for _, name := range got {
		if name == askUserToolName {
			hasAskUser = true
		}
	}
	if !hasAskUser {
		t.Errorf("a tool-less claw node with interaction must keep %q — on claw an empty "+
			"list is \"no tools\", not \"no restriction\"; got %v", askUserToolName, got)
	}
}

// Same asymmetry for interaction: async. Worse there than for the blocking
// case: the blocking one degrades to the _needs_interaction JSON protocol,
// while the async pair has no fallback — the model is instructed to call
// tools that were never bound.
func TestAssembleEffectiveTools_ClawKeepsTheAsyncPairOnAToolLessNode(t *testing.T) {
	e := &ClawExecutor{botID: "tester"}
	f := backendFields{id: "n", interaction: ir.InteractionAsync}

	got := e.assembleEffectiveTools(f, delegate.BackendClaw, nil, false)

	for _, want := range []string{delegate.AskUserAsyncToolName, delegate.AwaitAnswersToolName} {
		var found bool
		for _, name := range got {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("a tool-less async claw node must keep %q, got %v", want, got)
		}
	}
}
