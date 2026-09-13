package model

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestAsyncInteractionRefusesResolvedBackendBeforeDispatch(t *testing.T) {
	for _, backend := range []string{"codex", "kimi", "grok", "custom_without_async"} {
		t.Run(backend, func(t *testing.T) {
			fake := &providerScriptedBackend{}
			reg := delegate.NewRegistry()
			reg.Register(backend, fake)
			exec := newFallbackExecutor(reg, EventHooks{})
			exec.defaultBackend = backend
			node := fallbackAgentNode("asker", "auto", "")
			node.Interaction = ir.InteractionAsync
			_, err := exec.Execute(context.Background(), node, nil)
			var unsupported *delegate.ErrCapabilityUnsupported
			if !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "interaction: async") {
				t.Errorf("async on resolved backend %s must be refused, got %v", backend, err)
			}
			if unsupported != nil && (unsupported.Backend != backend || unsupported.NodeID != "asker") {
				t.Errorf("refusal lost its resolved route or node: %+v", unsupported)
			}
			if len(fake.calls) != 0 {
				t.Errorf("unsupported backend was called %d times", len(fake.calls))
			}
		})
	}
}

type asyncCapableRecorder struct{ providerScriptedBackend }

func (*asyncCapableRecorder) SupportsAsyncQuestions() bool { return true }

func TestAsyncInteractionAllowsCapabilityOnAnOutOfTreeBackend(t *testing.T) {
	backend := &asyncCapableRecorder{}
	reg := delegate.NewRegistry()
	reg.Register("custom_async", backend)
	exec := newFallbackExecutor(reg, EventHooks{})
	node := fallbackAgentNode("asker", "custom_async", "")
	node.Interaction = ir.InteractionAsync
	if _, err := exec.Execute(context.Background(), node, nil); err != nil {
		t.Fatal(err)
	}
	if len(backend.calls) != 1 {
		t.Fatalf("capable backend calls = %d, want 1", len(backend.calls))
	}
}

func TestAsyncInteractionChecksTheSelectedPiTransport(t *testing.T) {
	t.Setenv("ITERION_PI_MODE", "print")
	reg := delegate.DefaultRegistry(nil)
	exec := newFallbackExecutor(reg, EventHooks{})
	node := fallbackAgentNode("asker", "pi", "")
	node.Interaction = ir.InteractionAsync
	_, err := exec.Execute(context.Background(), node, nil)
	var unsupported *delegate.ErrCapabilityUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("Pi print mode must refuse async before spawning the CLI: %v", err)
	}
	t.Setenv("ITERION_PI_MODE", "rpc")
	b, err := reg.Resolve("pi")
	if err != nil || !b.(delegate.AsyncQuestionBackend).SupportsAsyncQuestions() {
		t.Fatalf("Pi RPC must retain its async capability: %v", err)
	}
}

func TestAsyncInteractionRefusesAnUnsupportedFallback(t *testing.T) {
	head := &asyncCapableRecorder{providerScriptedBackend{fail: map[string]error{"": &delegate.ErrTransient{Reason: "unavailable"}}}}
	tail := &providerScriptedBackend{}
	reg := delegate.NewRegistry()
	reg.Register("claude_code", head)
	reg.Register("codex", tail)
	exec := newFallbackExecutor(reg, EventHooks{})
	exec.retry.MaxAttempts = 1
	node := fallbackAgentNode("asker", "claude_code", "")
	node.Interaction = ir.InteractionAsync
	node.Fallbacks = []ir.Fallback{{Name: "backup", Backend: "codex", Model: "m", On: []string{"any"}}}
	_, err := exec.Execute(context.Background(), node, nil)
	var unsupported *delegate.ErrCapabilityUnsupported
	if !errors.As(err, &unsupported) || unsupported.Backend != "codex" {
		t.Fatalf("fallback must preserve its capability refusal: %v", err)
	}
	if len(head.calls) == 0 || len(tail.calls) != 0 {
		t.Fatalf("backend calls: primary=%d unsupported fallback=%d", len(head.calls), len(tail.calls))
	}
}

func TestAsyncInteractionBuiltInCapabilities(t *testing.T) {
	t.Setenv("ITERION_PI_MODE", "rpc")
	reg := delegate.DefaultRegistry(nil)
	reg.Register("claw", &ClawBackend{})
	for name, want := range map[string]bool{"claw": true, "claude_code": true, "pi": true, "codex": false, "kimi": false, "grok": false} {
		b, err := reg.Resolve(name)
		if err != nil {
			t.Fatal(err)
		}
		capable, ok := b.(delegate.AsyncQuestionBackend)
		if got := ok && capable.SupportsAsyncQuestions(); got != want {
			t.Errorf("%s supports async = %v, want %v", name, got, want)
		}
	}
}
