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

// TestClawSandboxRefusesAskCapablePolicy: an Ask decision cannot cross
// the IPC to pause the parent run, so a policy that can produce one is
// refused loudly — whatever its nominal mode (an explicit ask rule
// outranks mode deny).
func TestClawSandboxRefusesAskCapablePolicy(t *testing.T) {
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
			if err == nil || !strings.Contains(err.Error(), "Ask decision") {
				t.Fatalf("expected the Ask-capable refusal, got %v", err)
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
