package model

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
)

func mustPolicy(t *testing.T, mode permission.Mode, allow, ask, deny []string) *permission.Policy {
	t.Helper()
	pol, err := permission.NewPolicy(mode, allow, ask, deny)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	return pol
}

// TestClawSandboxAdmitsAskCapablePolicy: since the approval channel
// (#3513) an Ask decision no longer refuses the node — the launcher
// announces the channel pre-task and serves the pause (the inbound
// boundary test below keeps the WITHOUT-channel half refused). Whatever
// its nominal mode (an explicit ask rule outranks mode deny), Execute
// proceeds into the sandbox-runner path and fails there on the fake
// sandbox handle, never on a permission guard.
func TestClawSandboxAdmitsAskCapablePolicy(t *testing.T) {
	t.Parallel()
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	cases := map[string]*permission.Policy{
		"mode ask":             mustPolicy(t, permission.ModeAsk, []string{"WebFetch(*)"}, nil, nil),
		"mode deny + ask rule": mustPolicy(t, permission.ModeDeny, []string{"WebFetch(*)"}, []string{"Bash(git push:*)"}, nil),
	}
	for name, pol := range cases {
		t.Run(name, func(t *testing.T) {
			task := delegate.Task{
				NodeID:     "synth",
				Model:      "anthropic/claude-opus-5",
				Sandbox:    fakeSandboxRun{},
				Permission: pol,
			}
			_, err := b.Execute(context.Background(), task)
			if err == nil {
				t.Fatal("expected the fake sandbox runner to fail")
			}
			if strings.Contains(err.Error(), "Ask decision") {
				t.Fatalf("ask-capable policy still refused by a permission guard: %v", err)
			}
		})
	}
}

func TestClawSandboxPermissionPauseBoundary(t *testing.T) {
	t.Parallel()
	for _, marked := range []bool{false, true} {
		name := "ordinary question passes"
		if marked {
			name = "permission pause is refused"
		}
		t.Run(name, func(t *testing.T) {
			questions := map[string]any{delegate.AskUserQuestionKey: "Which label should I use?"}
			if marked {
				questions[permission.InteractionMarkerKey] = nil
			}
			env, err := delegate.NewResultEnvelope(delegate.IOResult{
				Output: map[string]any{"_needs_interaction": true, "_interaction_questions": questions},
				Tokens: 42, PendingConversation: []byte(`[]`), PendingToolUseID: "pending",
				PendingCommandNarrowing: &delegate.CommandNarrowingState{Command: "fixture"},
			})
			if err != nil {
				t.Fatal(err)
			}
			script, reply := envelopeScript(t, 1, env)
			b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
			res, err := b.Execute(context.Background(), delegate.Task{
				NodeID: "question", Model: "openai/gpt-5", WorkDir: t.TempDir(),
				Sandbox: &scriptedSandbox{script: script, args: []string{"", reply}},
			})
			if res.Tokens != 42 {
				t.Errorf("accounted tokens = %d, want 42", res.Tokens)
			}
			if !marked {
				if err != nil || res.Output["_needs_interaction"] != true || res.PendingToolUseID != "pending" {
					t.Fatalf("ordinary question did not retain its pause: %v, %v", res, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "unsupported permission pause") {
				t.Fatalf("expected the permission pause refusal, got %v", err)
			}
			if res.Output["_needs_interaction"] != nil || res.Output["_interaction_questions"] != nil ||
				len(res.PendingConversation) != 0 || res.PendingToolUseID != "" || res.PendingCommandNarrowing != nil {
				t.Fatal("refused result retains pause state")
			}
		})
	}
}

// TestClawSandboxPermissionPauseCrossesUnderTheChannel: since the
// approval channel (#3513), a permission-marked pause from a crossing
// whose policy can Ask is the LEGITIMATE pause vehicle — it retains its
// pause state (conversation, exact input, pending id) and flows to the
// engine's checkpoint, where the exact-approval ledger picks it up.
func TestClawSandboxPermissionPauseCrossesUnderTheChannel(t *testing.T) {
	t.Parallel()
	questions := map[string]any{
		delegate.AskUserQuestionKey:     "Allow Bash(git push:*)?",
		permission.InteractionMarkerKey: map[string]any{"tool": "bash"},
	}
	env, err := delegate.NewResultEnvelope(delegate.IOResult{
		Output:                 map[string]any{"_needs_interaction": true, "_interaction_questions": questions},
		Tokens:                 7,
		PendingConversation:    []byte(`[]`),
		PendingToolUseID:       "tu_pending",
		PendingPermissionInput: []byte(`{"command":"git push"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	script, reply := envelopeScript(t, 1, env)
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	res, err := b.Execute(context.Background(), delegate.Task{
		NodeID: "paused", Model: "openai/gpt-5", WorkDir: t.TempDir(),
		Sandbox:    &scriptedSandbox{script: script, args: []string{"", reply}},
		Permission: mustPolicy(t, permission.ModeDeny, []string{"WebFetch(*)"}, []string{"Bash(git push:*)"}, nil),
	})
	if err != nil {
		t.Fatalf("a served permission pause must not fail the node: %v", err)
	}
	if res.Output["_needs_interaction"] != true {
		t.Fatalf("the pause must survive the crossing: %v", res.Output)
	}
	if string(res.PendingPermissionInput) != `{"command":"git push"}` {
		t.Fatalf("the exact captured input must cross with the pause: %q", res.PendingPermissionInput)
	}
	if res.PendingToolUseID != "tu_pending" || res.Tokens != 7 {
		t.Fatalf("pause state incomplete: id=%q tokens=%d", res.PendingToolUseID, res.Tokens)
	}
}

// TestClawSandboxAdmitsDenyOnlyPolicy: a deny policy with no ask rules
// is enforceable inside the sandbox runner (the policy crosses the IPC
// as a pre-task envelope), so the old blanket refusal must be gone —
// Execute proceeds into the sandbox-runner path and fails there on the
// fake sandbox handle, never on the permission guard.
func TestClawSandboxAdmitsDenyOnlyPolicy(t *testing.T) {
	t.Parallel()
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	task := delegate.Task{
		NodeID:     "synth",
		Model:      "anthropic/claude-opus-5",
		Sandbox:    fakeSandboxRun{},
		Permission: mustPolicy(t, permission.ModeDeny, []string{"WebFetch(*)", "TodoWrite"}, nil, nil),
	}
	_, err := b.Execute(context.Background(), task)
	if err == nil {
		t.Fatal("expected the fake sandbox runner to fail")
	}
	if strings.Contains(err.Error(), "Ask decision") || strings.Contains(err.Error(), "cannot be enforced") {
		t.Fatalf("deny-only policy still refused by the permission guard: %v", err)
	}
}
