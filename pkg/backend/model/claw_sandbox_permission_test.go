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

// TestClawSandboxRefusesAPauseNothingDeferred: under the served channel a
// permission pause must bind to an ask THIS launcher deferred — the
// container's own question and marker never reach the human. A scripted
// crossing returning one without any deferral is refused (the hostile or
// stale container case); the bound path is witnessed at the binding level
// (TestCrossingBindsARelayedPauseToItsDeferredAsk).
func TestClawSandboxRefusesAPauseNothingDeferred(t *testing.T) {
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
	if err == nil || !strings.Contains(err.Error(), "no permission_ask for") {
		t.Fatalf("a pause nothing deferred must be refused, got err=%v res=%+v", err, res.Output)
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

// TestClawSandboxRefusesContainerToolsBesideAskCapablePolicy: the
// channel's enabled scope is launcher-placed asks only - the launcher
// consumes the approval AND executes the call, so it cannot certify a
// container-executed one. A node pairing an ask-capable policy with
// container-executed tools is refused explicitly, before the spawn,
// never degraded to per-call denials the model silently works around.
func TestClawSandboxRefusesContainerToolsBesideAskCapablePolicy(t *testing.T) {
	t.Parallel()
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	task := delegate.Task{
		NodeID:     "synth",
		Model:      "anthropic/claude-opus-5",
		Sandbox:    fakeSandboxRun{},
		Permission: mustPolicy(t, permission.ModeDeny, []string{"WebFetch(*)"}, []string{"bash"}, nil),
		ToolDefs: []delegate.ToolDef{
			{Name: "bash", Description: "shell"},
			{Name: "mcp_srv_greet", Description: "launcher-placed"},
		},
	}
	_, err := b.Execute(context.Background(), task)
	if err == nil || !strings.Contains(err.Error(), "approval channel serves launcher-placed tools only") {
		t.Fatalf("expected the explicit container-tools refusal, got %v", err)
	}
	// The same node WITHOUT the ask rule stays admitted: the scope is the
	// ask, not the container tools.
	task.Permission = mustPolicy(t, permission.ModeDeny, []string{"WebFetch(*)"}, nil, nil)
	if _, err := b.Execute(context.Background(), task); err != nil {
		if strings.Contains(err.Error(), "approval channel serves launcher-placed tools only") {
			t.Fatalf("a deny-only policy does not enter the channel scope: %v", err)
		}
	}
}

// TestClawSandboxRefusesOnlyContainerToolsThatCanAsk: the pre-spawn
// refusal follows the policy, not the tool list - an ask rule scoped to
// launcher-placed tools leaves the container tools on their mode default,
// and that node IS served.
func TestClawSandboxRefusesOnlyContainerToolsThatCanAsk(t *testing.T) {
	t.Parallel()
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	// ask rule on the launcher tool only, mode deny: bash lands on deny.
	task := delegate.Task{
		NodeID:     "synth",
		Model:      "anthropic/claude-opus-5",
		Sandbox:    fakeSandboxRun{},
		Permission: mustPolicy(t, permission.ModeDeny, []string{"WebFetch(*)"}, []string{"mcp_srv_greet"}, nil),
		ToolDefs: []delegate.ToolDef{
			{Name: "bash", Description: "shell"},
			{Name: "mcp_srv_greet", Description: "launcher-placed"},
		},
	}
	if _, err := b.Execute(context.Background(), task); err != nil {
		if strings.Contains(err.Error(), "approval channel serves launcher-placed tools only") {
			t.Fatalf("a container tool whose verdict is deny does not refuse the node: %v", err)
		}
	}
	// Mode ask: every container tool can pause, the refusal returns.
	task.Permission = mustPolicy(t, permission.ModeAsk, nil, nil, nil)
	if _, err := b.Execute(context.Background(), task); err == nil || !strings.Contains(err.Error(), "approval channel serves launcher-placed tools only") {
		t.Fatalf("mode ask can pause on bash - the node must refuse, got %v", err)
	}
	// A SCOPED ask rule naming the container tool also refuses: the rule
	// can fire on some invocation of it, and that invocation would
	// degrade to a per-call denial the launcher cannot serve.
	task.Permission = mustPolicy(t, permission.ModeDeny, []string{"WebFetch(*)"}, []string{"bash(git push:*)"}, nil)
	if _, err := b.Execute(context.Background(), task); err == nil || !strings.Contains(err.Error(), "approval channel serves launcher-placed tools only") {
		t.Fatalf("a scoped ask rule on bash can fire on git push - the node must refuse, got %v", err)
	}
}
