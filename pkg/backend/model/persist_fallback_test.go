package model

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

type persistentRouteBackend func(context.Context, delegate.Task) (delegate.Result, error)

func (f persistentRouteBackend) Execute(ctx context.Context, task delegate.Task) (delegate.Result, error) {
	return f(ctx, task)
}

func newPersistFallbackExecutor(t *testing.T, backend persistentRouteBackend) (*ClawExecutor, *ir.AgentNode) {
	t.Helper()
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaw, backend)
	tr := tool.NewRegistry()
	for _, name := range []string{"read_file", "diagnostic_shell", "todo_write"} {
		if err := tr.RegisterBuiltin(name, name, json.RawMessage(`{"type":"object"}`), nil); err != nil {
			t.Fatal(err)
		}
	}
	wf := &ir.Workflow{Permission: "deny", PermissionAllow: []string{"Read"}, PermissionAsk: []string{"diagnostic_shell"}}
	e := NewClawExecutor(NewRegistry(), wf, WithBackendRegistry(reg), WithToolRegistry(tr), WithLogger(iterlog.Nop()),
		WithWorkDir(t.TempDir()), WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}))
	n := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "author"},
		LLMFields: ir.LLMFields{Backend: "claw", Model: "openai/gpt-5.6-terra"},
		Session:   ir.SessionPersist, SessionSlot: "conversation", Tools: []string{"read_file", "diagnostic_shell"}, AutoMemory: "off",
		Fallbacks: []ir.Fallback{{Name: "claude", Backend: "claw", Model: "anthropic/claude-opus-5", On: []string{"usage_window", "unavailable", "transient_exhausted"}}},
	}
	return e, n
}

func persistedText(text string) api.Message {
	return api.Message{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: text}}}
}

func persistedResult(task delegate.Task) delegate.Result {
	return delegate.Result{BackendName: "claw", Output: map[string]any{"ok": true}, SessionID: task.SessionID, SessionFingerprint: clawSessionFingerprint(task.Model)}
}

func assertPersistentRoutePolicy(t *testing.T, task delegate.Task) {
	t.Helper()
	var names []string
	for _, def := range task.ToolDefs {
		names = append(names, def.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"diagnostic_shell", "read_file", "todo_write"}) {
		t.Fatalf("route tools = %v", names)
	}
	if decision, _ := task.Permission.Evaluate("Write", map[string]any{"file_path": "file"}); decision != permission.Deny {
		t.Fatalf("write policy = %v, want deny", decision)
	}
}

func TestClawPersistFallbackSurvivesColdResume(t *testing.T) {
	for _, failure := range []struct {
		name string
		err  error
	}{
		{"quota", &delegate.ErrRateLimited{Kind: delegate.RateLimitKindUsageWindow}},
		{"unavailable", &delegate.ErrModelUnavailable{Model: "gpt", Detail: "unavailable"}},
		{"transient", &delegate.ErrTransient{Reason: "network"}},
	} {
		t.Run(failure.name, func(t *testing.T) {
			ctx := WithRunID(context.Background(), "run")
			var calls []string
			backend := persistentRouteBackend(func(ctx context.Context, task delegate.Task) (delegate.Result, error) {
				calls = append(calls, task.Model)
				assertPersistentRoutePolicy(t, task)
				_, sessions := runtimeContextFrom(ctx)
				if strings.HasPrefix(task.Model, "openai/") {
					sessions.save("run", task.SessionSlot, []api.Message{persistedText("failed partial")})
					return delegate.Result{}, failure.err
				}
				prior := sessions.load("run", task.SessionSlot)
				if len(prior) != 1 || prior[0].Content[0].Text != "remember correction" {
					t.Fatalf("fallback lost last-good history: %+v", prior)
				}
				if task.SessionID != "session" {
					t.Errorf("fallback session ID = %q", task.SessionID)
				}
				sessions.save("run", task.SessionSlot, append(prior, persistedText("Claude completed")))
				return persistedResult(task), nil
			})
			e, n := newPersistFallbackExecutor(t, backend)
			e.sessions.save("run", n.SessionSlot, []api.Message{persistedText("remember correction")})
			out, err := e.Execute(ctx, n, map[string]any{delegate.SessionIDKey: "session", delegate.SessionFingerprintKey: "claw:openai"})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(calls, []string{n.Model, n.Fallbacks[0].Model}) || out["_fallback_used"] != true || out[delegate.SessionFingerprintKey] != "claw:anthropic" {
				t.Fatalf("fallback calls=%v output=%v", calls, out)
			}
			blob, _ := out[delegate.SessionStateBlobKey].([]byte)
			if len(blob) == 0 {
				t.Fatal("fallback did not pack durable history")
			}
			cold, _ := newPersistFallbackExecutor(t, func(ctx context.Context, task delegate.Task) (delegate.Result, error) {
				assertPersistentRoutePolicy(t, task)
				_, sessions := runtimeContextFrom(ctx)
				prior := sessions.load("run", task.SessionSlot)
				if task.Model != n.Model || task.SessionID != "session" || len(prior) != 2 || prior[1].Content[0].Text != "Claude completed" {
					t.Fatalf("cold primary lost fallback session: task=%s/%s history=%+v", task.Model, task.SessionID, prior)
				}
				return persistedResult(task), nil
			})
			_, err = cold.Execute(ctx, n, map[string]any{delegate.SessionIDKey: out[delegate.SessionIDKey], delegate.SessionFingerprintKey: out[delegate.SessionFingerprintKey], delegate.SessionStateKey: blob})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestClawPersistResumeAcrossProviderRoutes(t *testing.T) {
	for _, route := range []struct {
		name, fingerprint, destination string
		cooldown                       bool
	}{
		{"GPT to Claude", "claw:openai", "anthropic/claude-opus-5", true},
		{"Claude to GPT", "claw:anthropic", "openai/gpt-5.6-terra", false},
		{"Claude to Claude", "claw:anthropic", "anthropic/claude-opus-5", true},
		{"unknown origin", "", "openai/gpt-5.6-terra", false},
	} {
		for _, pending := range []struct {
			name, answer string
			args         map[string]any
		}{
			{"ask_user", "Use the existing workspace", map[string]any{"question": "Which workspace?"}},
			{"diagnostic_shell", "yes", map[string]any{"command": "git status --short"}},
		} {
			t.Run(route.name+"/"+pending.name, func(t *testing.T) {
				ctx := WithRunID(context.Background(), "run")
				var received []delegate.Task
				warming := route.cooldown
				e, n := newPersistFallbackExecutor(t, func(ctx context.Context, task delegate.Task) (delegate.Result, error) {
					if warming && strings.HasPrefix(task.Model, "openai/") {
						return delegate.Result{}, &delegate.ErrRateLimited{Kind: delegate.RateLimitKindUsageWindow, ResetAt: time.Now().Add(time.Hour)}
					}
					if !warming {
						received = append(received, task)
					}
					return persistedResult(task), nil
				})
				if warming {
					if _, err := e.Execute(ctx, n, map[string]any{}); err != nil {
						t.Fatal(err)
					}
					warming = false
				}
				prior := []api.Message{{Role: "assistant", Content: []api.ContentBlock{
					{Type: "thinking", Text: "provider-private"},
					{Type: "tool_use", ID: "unrelated", Name: "ask_user"},
					{Type: "tool_use", ID: "toolu_pending", Name: pending.name, Input: pending.args},
				}}}
				conversation, err := json.Marshal(prior)
				if err != nil {
					t.Fatal(err)
				}
				input := map[string]any{delegate.SessionIDKey: "session", delegate.SessionFingerprintKey: route.fingerprint,
					delegate.ResumeConversationKey: json.RawMessage(conversation), delegate.ResumePendingToolUseIDKey: "toolu_pending", delegate.ResumeAnswerKey: pending.answer}
				if pending.name == "diagnostic_shell" {
					input[permission.GrantInputKey] = permission.GrantRuleFor(pending.name, pending.args, false)
				}
				if _, err := e.Execute(ctx, n, input); err != nil {
					t.Fatal(err)
				}
				if len(received) != 1 {
					t.Fatalf("resume calls = %d, want one (cooled primary skipped)", len(received))
				}
				task := received[0]
				assertPersistentRoutePolicy(t, task)
				if task.Model != route.destination || task.SessionID == "" || (route.fingerprint != "" && task.SessionID != "session") || task.ResumeAnswer != pending.answer || task.ResumePendingToolUseID != "toolu_pending" {
					t.Fatalf("lost resumed answer: model=%s session=%s pending=%s answer=%q", task.Model, task.SessionID, task.ResumePendingToolUseID, task.ResumeAnswer)
				}
				var resumed []api.Message
				if err := json.Unmarshal(task.ResumeConversation, &resumed); err != nil {
					t.Fatal(err)
				}
				if len(resumed) != 1 || len(resumed[0].Content) != 1 {
					t.Fatalf("unsafe resume: %s", task.ResumeConversation)
				}
				name, args, ok := findPendingToolUse(resumed, "toolu_pending")
				if !ok || name != pending.name || args["command"] != pending.args["command"] || args["question"] != pending.args["question"] {
					t.Fatalf("changed pending call: %s", task.ResumeConversation)
				}
				if pending.name == "diagnostic_shell" {
					if decision, _ := task.Permission.Evaluate(pending.name, pending.args); decision != permission.Allow {
						t.Fatalf("lost exact approval: %v", decision)
					}
					if decision, _ := task.Permission.Evaluate(pending.name, map[string]any{"command": "git log"}); decision != permission.Ask {
						t.Fatalf("approval widened: %v", decision)
					}
				}
			})
		}
	}
}

func TestClawPersistDoesNotFallbackOnAuth(t *testing.T) {
	calls := 0
	failure := &delegate.ErrAuthFailed{Provider: "openai", Detail: "expired"}
	e, n := newPersistFallbackExecutor(t, func(context.Context, delegate.Task) (delegate.Result, error) {
		calls++
		return delegate.Result{}, failure
	})
	_, err := e.Execute(WithRunID(context.Background(), "run"), n, map[string]any{})
	if !errors.Is(err, failure) || calls != 1 {
		t.Fatalf("auth calls=%d error=%v", calls, err)
	}
}

func TestClawPersistProviderChangeSanitizesWarmSlot(t *testing.T) {
	for _, tc := range []struct {
		fingerprint, model string
		preserve           bool
	}{
		{"claw:anthropic", "openai/gpt-5.6-terra", true},
		{"claw:unknown", "openai/gpt-5.6-terra", false},
		{"claw:", "openai/gpt-5.6-terra", false},
		{"claw:invalid:provider", "openai/gpt-5.6-terra", false},
		{"claude_code:anthropic", "openai/gpt-5.6-terra", false},
		{"", "openai/gpt-5.6-terra", false},
		{"claw:anthropic", "invalid-spec", false},
	} {
		t.Run(tc.fingerprint+"/"+tc.model, func(t *testing.T) {
			e, n := newPersistFallbackExecutor(t, func(ctx context.Context, task delegate.Task) (delegate.Result, error) {
				_, sessions := runtimeContextFrom(ctx)
				prior := sessions.load("run", task.SessionSlot)
				if tc.preserve {
					if task.SessionID != "session" || len(prior) != 1 || prior[0].Content[0].Text != "keep this" {
						t.Fatalf("lost warm history: session=%s messages=%+v", task.SessionID, prior)
					}
				} else if len(prior) != 0 || task.SessionID == "session" {
					t.Fatalf("retained unknown session: session=%s messages=%+v", task.SessionID, prior)
				}
				return persistedResult(task), nil
			})
			n.Model, n.Fallbacks = tc.model, nil
			e.sessions.save("run", n.SessionSlot, []api.Message{persistedText("keep this"), {Role: "assistant", Content: []api.ContentBlock{
				{Type: "thinking", Text: "private"}, {Type: "tool_use", ID: "orphan", Name: "read_file"},
			}}})
			if _, err := e.Execute(WithRunID(context.Background(), "run"), n, map[string]any{delegate.SessionIDKey: "session", delegate.SessionFingerprintKey: tc.fingerprint}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestClawPersistRejectsInvalidPendingResume(t *testing.T) {
	for _, conversation := range []string{
		``,
		`{`,
		`[]`,
		`[{"role":"assistant","content":[{"type":"tool_use","id":"other","name":"ask_user"}]}]`,
		`[{"role":"user","content":[{"type":"tool_use","id":"pending","name":"ask_user"}]}]`,
		`[{"role":"assistant","content":[{"type":"tool_use","id":"pending","name":"ask_user"},{"type":"tool_use","id":"pending","name":"ask_user"}]}]`,
		`[{"role":"assistant","content":[{"type":"tool_use","id":"pending","name":"ask_user"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"pending","content":"already answered"}]}]`,
	} {
		t.Run(conversation, func(t *testing.T) {
			calls := 0
			e, n := newPersistFallbackExecutor(t, func(context.Context, delegate.Task) (delegate.Result, error) { calls++; return delegate.Result{}, nil })
			_, err := e.Execute(WithRunID(context.Background(), "run"), n, map[string]any{
				delegate.ResumeConversationKey: json.RawMessage(conversation), delegate.ResumePendingToolUseIDKey: "pending", delegate.ResumeAnswerKey: "yes",
			})
			if err == nil || !strings.Contains(err.Error(), "claw persisted resume:") || calls != 0 {
				t.Fatalf("invalid resume dispatched: calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestChainNonpersistentClawStillDropsResume(t *testing.T) {
	e := &ClawExecutor{}
	build := e.newElementBuilder("author", "claw", &backendScriptedBackend{name: "claw"}, func(context.Context, string) (*delegate.Task, error) {
		return &delegate.Task{NodeID: "author", Model: "openai/gpt-5.6-terra", SessionID: "session", SessionFingerprint: "claw:openai",
			ResumeConversation: json.RawMessage(`[{"role":"assistant"}]`), ResumePendingToolUseID: "pending", ResumeAnswer: "yes"}, nil
	})
	_, _, task, err := build(context.Background(), 1, chainElement{Backend: "claw", Model: "anthropic/claude-opus-5"})
	if err != nil {
		t.Fatal(err)
	}
	if task.SessionID != "" || task.SessionFingerprint != "" || len(task.ResumeConversation) != 0 || task.ResumePendingToolUseID != "" || task.ResumeAnswer != "" {
		t.Fatal("nonpersistent fallback retained resume state")
	}
}
