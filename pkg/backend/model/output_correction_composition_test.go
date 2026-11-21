package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/hostmcp"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func producerOwnedContext() context.Context {
	return WithOutputCorrectionOwnership(WithRunID(context.Background(), "producer-run"), OutputCorrectionOwnership{Protocol: OutputCorrectionProtocol, Authority: store.RunExecutionAuthority{RunID: "producer-run", Epoch: "original-epoch"}})
}
func producerExecutor(backend delegate.Backend, opts ...ClawExecutorOption) *ClawExecutor {
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, backend)
	return NewClawExecutor(NewRegistry(), verdictWorkflow(), append([]ClawExecutorOption{WithBackendRegistry(reg), WithRetryPolicy(RetryPolicy{MaxAttempts: 1})}, opts...)...)
}
func requireProducerFailure(t *testing.T, e *ClawExecutor, ctx context.Context, node ir.Node) (*OutputCorrectionFailure, context.Context) {
	t.Helper()
	_, err := e.Execute(ctx, node, nil)
	var failure *OutputCorrectionFailure
	if !errors.As(err, &failure) {
		t.Fatalf("Execute did not transfer typed failure: %T %v", err, err)
	}
	t.Cleanup(failure.Handle().Release)
	return failure, WithOutputCorrectionHandle(ctx, failure.Handle())
}
func TestProducerCompositionOwnedLocalAndFinalization(t *testing.T) {
	calls, finished := 0, 0
	backend := persistentRouteBackend(func(_ context.Context, task delegate.Task) (delegate.Result, error) {
		calls++
		out := map[string]any{"verdict": true}
		if calls == 3 || calls == 5 {
			out["reason"] = "done"
		}
		return delegate.Result{Output: out, Tokens: 10, CostUSD: 1, SessionID: "s"}, nil
	})
	e := producerExecutor(backend, WithEventHooks(EventHooks{OnNodeFinished: func(_ string, out map[string]any) {
		finished++
		if out["_tokens"] == nil {
			t.Fatal("missing bill at finalization")
		}
	}}))
	node := verdictJudge(delegate.BackendClaudeCode)
	failure, ctx := requireProducerFailure(t, e, producerOwnedContext(), node)
	if calls != 1 || finished != 0 {
		t.Fatalf("owned first answer calls=%d finalizations=%d", calls, finished)
	}
	first, usage, err := e.CorrectOutputWithUsage(ctx, node, failure.Output(), failure.validation)
	if err != nil || first["reason"] != nil || usage.Tokens != 10 {
		t.Fatalf("invalid answered candidate: %+v %+v %v", first, usage, err)
	}
	final, usage, err := e.CorrectOutputWithUsage(ctx, node, first, ValidateOutput(first, e.schemas["verdict_schema"]))
	if err != nil || final["reason"] != "done" || final["_tokens"] != 30 || usage.CostUSD != 1 {
		t.Fatalf("next round: %+v %+v %v", final, usage, err)
	}
	if err := e.FinalizeCorrectedOutput(ctx, node, final); err != nil {
		t.Fatal(err)
	}
	if err := e.FinalizeCorrectedOutput(ctx, node, final); err != nil {
		t.Fatal(err)
	}
	failure.Handle().Release()
	failure.Handle().Release()
	if finished != 1 {
		t.Fatalf("finalizer fired %d times", finished)
	}
	if _, _, err := e.CorrectOutputWithUsage(ctx, node, final, nil); !errors.Is(err, ErrOutputCorrectionReleased) {
		t.Fatalf("released handle reused: %v", err)
	}
	out, err := e.Execute(context.Background(), node, nil)
	if err != nil || calls != 5 || out["reason"] != "done" || finished != 2 {
		t.Fatalf("local adapter lost: calls=%d finalizations=%d err=%v", calls, finished, err)
	}
}
func TestProducerCompositionEligibilityAndScrub(t *testing.T) {
	for _, tc := range []struct {
		name            string
		out             map[string]any
		fallback, owned bool
		want            int
		typed           bool
	}{
		{"type", map[string]any{"verdict": "wrong", "reason": "x"}, false, true, 1, true},
		{"API", map[string]any{"text": "API Error: provider failure"}, true, true, 1, false},
		{"missing owned", map[string]any{"verdict": true}, false, true, 1, true},
		{"parse owned", map[string]any{"text": "candidate"}, true, true, 1, true},
		{"missing local", map[string]any{"verdict": true}, false, false, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			e := producerExecutor(persistentRouteBackend(func(context.Context, delegate.Task) (delegate.Result, error) {
				calls++
				return delegate.Result{Output: copyCorrectionOutput(tc.out), ParseFallback: tc.fallback}, nil
			}))
			ctx := context.Background()
			if tc.owned {
				ctx = producerOwnedContext()
			}
			_, err := e.Execute(ctx, verdictJudge(delegate.BackendClaudeCode), nil)
			var failure *OutputCorrectionFailure
			typed := errors.As(err, &failure)
			if typed {
				defer failure.Handle().Release()
			}
			if calls != tc.want || typed != tc.typed {
				t.Fatalf("calls=%d typed=%v err=%v", calls, typed, err)
			}
		})
	}
	const secret = "registered-secret-3009"
	wf := verdictWorkflow()
	wf.Schemas["verdict_schema"].Fields = append(wf.Schemas["verdict_schema"].Fields, &ir.SchemaField{Name: "choice", Type: ir.FieldTypeString, EnumValues: []string{"allowed"}})
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, persistentRouteBackend(func(context.Context, delegate.Task) (delegate.Result, error) {
		return delegate.Result{Output: map[string]any{"verdict": true, "choice": secret}}, nil
	}))
	guard := secretguard.New([]secretguard.Secret{{Name: "chosen", Value: secret}}, secretguard.DefaultConfig())
	e := NewClawExecutor(NewRegistry(), wf, WithBackendRegistry(reg), WithSecretGuard(guard))
	node := verdictJudge(delegate.BackendClaudeCode)
	_, err := e.Execute(producerOwnedContext(), node, nil)
	var failure *OutputCorrectionFailure
	if !errors.As(err, &failure) || strings.Contains(err.Error(), secret) {
		t.Fatalf("scrub lost typed failure or leaked secret: %v", err)
	}
	defer failure.Handle().Release()
	snapshot := failure.Output()
	snapshot["choice"] = "mutated"
	if failure.Output()["choice"] != secret {
		t.Fatal("caller mutated original evidence")
	}
	if _, _, err := e.CorrectOutputWithUsage(WithOutputCorrectionHandle(context.Background(), failure.Handle()), node, nil, nil); !errors.Is(err, ErrOutputCorrectionIneligible) {
		t.Fatalf("missing immutable ownership accepted: %v", err)
	}
	changed := WithOutputCorrectionOwnership(producerOwnedContext(), OutputCorrectionOwnership{Protocol: 1, Authority: store.RunExecutionAuthority{RunID: "producer-run", Epoch: "successor"}})
	if _, _, err := e.CorrectOutputWithUsage(WithOutputCorrectionHandle(changed, failure.Handle()), node, nil, nil); !errors.Is(err, ErrOutputCorrectionIneligible) {
		t.Fatalf("borrowed successor epoch: %v", err)
	}
}
func TestProducerCompositionTwoInvocationsKeepTheirSessions(t *testing.T) {
	var mu sync.Mutex
	initials := 0
	both := make(chan struct{})
	backend := persistentRouteBackend(func(_ context.Context, task delegate.Task) (delegate.Result, error) {
		if task.SessionID != "" {
			return delegate.Result{Output: map[string]any{"verdict": true, "reason": task.SessionID}, SessionID: task.SessionID}, nil
		}
		id := strings.TrimSpace(task.UserPrompt)
		mu.Lock()
		initials++
		if initials == 2 {
			close(both)
		}
		mu.Unlock()
		<-both
		return delegate.Result{Output: map[string]any{"verdict": true}, SessionID: id}, nil
	})
	e := producerExecutor(backend)
	e.prompts["A"] = &ir.Prompt{Body: "A"}
	e.prompts["B"] = &ir.Prompt{Body: "B"}
	var wg sync.WaitGroup
	for _, id := range []string{"A", "B"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			node := verdictJudge(delegate.BackendClaudeCode)
			node.UserPrompt = id
			failure, ctx := requireProducerFailure(t, e, producerOwnedContext(), node)
			out, _, err := e.CorrectOutputWithUsage(ctx, node, nil, failure.validation)
			if err != nil || out["reason"] != id {
				t.Errorf("branch %s got %v err=%v", id, out, err)
			}
			failure.Handle().Release()
		}()
	}
	wg.Wait()
}
func TestProducerCompositionAbsoluteDeadlineAndArrivedAnswer(t *testing.T) {
	for _, answer := range []bool{false, true} {
		t.Run(map[bool]string{false: "NoAnswer", true: "Answered"}[answer], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				e := producerExecutor(persistentRouteBackend(func(ctx context.Context, _ delegate.Task) (delegate.Result, error) {
					calls++
					if calls == 1 {
						time.Sleep(1500 * time.Millisecond)
						return delegate.Result{Output: map[string]any{"verdict": true}}, nil
					}
					<-ctx.Done()
					if answer {
						return delegate.Result{Output: map[string]any{"verdict": true, "reason": "arrived"}, Tokens: 3}, nil
					}
					return delegate.Result{Tokens: 3}, ctx.Err()
				}))
				node := verdictJudge(delegate.BackendClaudeCode)
				node.Timeout = "2s"
				start := time.Now()
				failure, ctx := requireProducerFailure(t, e, producerOwnedContext(), node)
				out, usage, err := e.CorrectOutputWithUsage(ctx, node, nil, failure.validation)
				if time.Since(start) != 2*time.Second || usage.Tokens != 3 {
					t.Fatalf("deadline reset or usage lost: elapsed=%s usage=%+v", time.Since(start), usage)
				}
				var deadline *ErrNodeDeadline
				if answer {
					if err != nil || out["reason"] != "arrived" {
						t.Fatalf("arrived answer became NoAnswer: %v %v", out, err)
					}
				} else if !errors.As(err, &deadline) {
					t.Fatalf("cut not named: %v", err)
				}
			})
		})
	}
}
func TestProducerCompositionGrantAndMemoryLifetime(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	grants := hostmcp.NewRegistry()
	revoked := 0
	calls := 0
	var token, dir string
	backend := persistentRouteBackend(func(_ context.Context, task delegate.Task) (delegate.Result, error) {
		calls++
		token = task.BoardRunToken
		if _, ok := grants.Lookup(token); !ok {
			t.Fatal("grant revoked before round")
		}
		if task.AutoMemoryDir == "" {
			t.Fatal("mirror missing")
		}
		dir = task.AutoMemoryDir
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("mirror released before round: %v", err)
		}
		if calls == 2 {
			if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("correction wrote this"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		out := map[string]any{"verdict": true}
		if calls == 2 {
			out["reason"] = "done"
		}
		return delegate.Result{Output: out, SessionID: "owned-session"}, nil
	})
	e := producerExecutor(backend, WithWorkDir(t.TempDir()), WithStoreDir(t.TempDir()), WithBotID("producer"), WithAutoMemoryOverride("on"), WithBoardRegister(func(caps []string, source string) string {
		tok := hostmcp.MintToken()
		if err := grants.Register(tok, caps, source, "", store.RunTrust("trusted"), true, ""); err != nil {
			t.Fatal(err)
		}
		return tok
	}), WithBoardRevoke(func(tok string) { revoked++; grants.Revoke(tok) }))
	e.SetSandbox(unmetSandboxRun{})
	e.SetBoardEndpoint("http://127.0.0.1:1/board")
	e.SetConnectorEndpoint("http://127.0.0.1:1/connectors")
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "judge"}, LLMFields: ir.LLMFields{Backend: delegate.BackendClaudeCode, Model: "test/test-model", UserPrompt: "ask"}, SchemaFields: ir.SchemaFields{OutputSchema: "verdict_schema"}, Capabilities: []string{connection.CapConnectors}, AutoMemory: "on"}
	failure, ctx := requireProducerFailure(t, e, producerOwnedContext(), node)
	if revoked != 0 {
		t.Fatalf("initial defer revoked handle grant %d", revoked)
	}
	if _, _, err := e.CorrectOutputWithUsage(ctx, node, nil, failure.validation); err != nil {
		t.Fatal(err)
	}
	failure.Handle().Release()
	failure.Handle().Release()
	if _, ok := grants.Lookup(token); ok || revoked != 1 {
		t.Fatalf("grant not released once: live=%v revokes=%d", ok, revoked)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("mirror still exists: %v", err)
	}
	task := delegate.Task{WorkDir: *e.workDir.Load(), StoreDir: e.storeDir, Model: "test/test-model"}
	cleanup := e.applyAutoMemory(context.Background(), &task, backendFields{id: "next", autoMemory: "on"}, delegate.BackendClaudeCode)
	defer cleanup()
	data, err := os.ReadFile(filepath.Join(task.AutoMemoryDir, "MEMORY.md"))
	if err != nil || !strings.Contains(string(data), "correction wrote this") {
		t.Fatalf("final round memory not synced: %q %v", data, err)
	}
}
func TestProducerCompositionClawContinuationAndPersistFinalizer(t *testing.T) {
	mock := newMockClient(toolUseEvents("first", "structured_output", `{"verdict":true}`, 50, 20), toolUseEvents("second", "structured_output", `{"verdict":true,"reason":"yes"}`, 30, 10))
	registry := NewRegistry()
	registry.Register("test", func(string) (api.APIClient, error) { return mock, nil })
	finished := 0
	e := newTestClawExecutor(registry, verdictWorkflow(), WithEventHooks(EventHooks{OnNodeFinished: func(string, map[string]any) { finished++ }}))
	node := verdictJudge("")
	node.Session = ir.SessionPersist
	failure, ctx := requireProducerFailure(t, e, producerOwnedContext(), node)
	e.sessions.save("producer-run", nodeSessionKey(node), []api.Message{persistedText("SIBLING MUST NOT BE BORROWED")})
	out, usage, err := e.CorrectOutputWithUsage(ctx, node, nil, failure.validation)
	if err != nil || usage.Tokens != 40 {
		t.Fatalf("claw round: %+v %v", usage, err)
	}
	calls := mock.getCalls()
	if len(calls) != 2 || textOccurrences(calls[1].Messages, "SIBLING MUST NOT BE BORROWED") != 0 || textOccurrences(calls[1].Messages, reaskPrompt) != 1 || !strings.Contains(lastUserText(t, calls[1].Messages), schemaRetryFeedbackMarker) {
		t.Fatal("round restarted or lost feedback")
	}
	if len(calls[1].Tools) != 1 || calls[1].Tools[0].Name != "structured_output" {
		t.Fatal("continued round retained tools")
	}
	if err := e.FinalizeCorrectedOutput(ctx, node, out); err != nil {
		t.Fatal(err)
	}
	if err := e.FinalizeCorrectedOutput(ctx, node, out); err != nil {
		t.Fatal(err)
	}
	blob := out[delegate.SessionStateBlobKey].([]byte)
	sid, _ := out[delegate.SessionIDKey].(string)
	fresh := newTestClawExecutor(registry, verdictWorkflow())
	if err := fresh.UnpackSession(producerOwnedContext(), delegate.BackendClaw, sid, blob); err != nil {
		t.Fatalf("cold unpack: %v", err)
	}
	replay := fresh.sessions.load("producer-run", nodeSessionKey(node))
	if textOccurrences(replay, schemaRetryFeedbackMarker) != 1 || textOccurrences(replay, "SIBLING MUST NOT BE BORROWED") != 0 {
		t.Fatal("fresh executor lost corrected conversation")
	}
	if len(out[delegate.SessionStateBlobKey].([]byte)) == 0 || finished != 1 || out["_tokens"] != 110 {
		t.Fatalf("corrected persist finalization: %+v finished=%d", out, finished)
	}
}

func TestProducerCompositionRouterStaysLocalUnderOwnership(t *testing.T) {
	backend := &stubBackend{results: []delegate.Result{{Output: map[string]any{"selected_route": "agent_a"}, Tokens: 100}, {Output: map[string]any{"selected_route": "agent_a", "reasoning": "fits"}, Tokens: 40}}}
	e := newDelegateTestExecutor(backend, EventHooks{})
	node := &ir.RouterNode{BaseNode: ir.BaseNode{ID: "pick"}, LLMFields: ir.LLMFields{Backend: "test_backend", SystemPrompt: "sys"}, RouterMode: ir.RouterLLM}
	out, err := e.Execute(producerOwnedContext(), node, map[string]any{"_route_candidates": []string{"agent_a", "agent_b"}})
	if err != nil || backend.calls != 2 || out["reasoning"] != "fits" || out["_tokens"] != 140 {
		t.Fatalf("owned router no longer local: calls=%d out=%v err=%v", backend.calls, out, err)
	}
}
func TestProducerCompositionHandleIsOpaqueAndReleaseCanPrecedeRound(t *testing.T) {
	calls := 0
	released := 0
	e := producerExecutor(persistentRouteBackend(func(context.Context, delegate.Task) (delegate.Result, error) {
		calls++
		return delegate.Result{Output: map[string]any{"verdict": true}}, nil
	}))
	node := verdictJudge(delegate.BackendClaudeCode)
	failure, ctx := requireProducerFailure(t, e, producerOwnedContext(), node)
	h := failure.Handle()
	h.lifetime.revoke = func() { released++ }
	if _, err := json.Marshal(h); err == nil {
		t.Fatal("live handle serialized as JSON")
	}
	if _, err := bson.Marshal(h); err == nil {
		t.Fatal("live handle serialized as BSON")
	}
	if fmt.Sprintf("%+v", h) != "<output correction handle>" || fmt.Sprintf("%#v", h) != "<output correction handle>" {
		t.Fatal("handle formatting exposes private task")
	}
	h.Release()
	h.Release()
	if _, _, err := e.CorrectOutputWithUsage(ctx, node, nil, nil); !errors.Is(err, ErrOutputCorrectionReleased) {
		t.Fatalf("released handle reused: %v", err)
	}
	if calls != 1 || released != 1 {
		t.Fatalf("no-round exit calls=%d releases=%d", calls, released)
	}
}
