package model

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/ambient"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func TestAmbientContextPolicyMeetsTheBackendsVerdict(t *testing.T) {
	cases := []struct {
		name                       string
		override, node, wf, envDef string
		backend                    string
		want                       ambient.Policy
		wantSource                 string
		wantRefused, wantWarned    bool
		wantInRefusal              []string
	}{
		{name: "a declared value an enforcing backend applies", node: "none", backend: "claude_code", want: ambient.None, wantSource: "node"},
		{name: "a declared value kimi applies", node: "workspace", backend: "kimi", want: ambient.Workspace, wantSource: "node"},
		{name: "all on opencode is its native loading", node: "all", backend: "opencode", want: ambient.All, wantSource: "node"},
		{
			name: "a declared value kimi cannot apply", node: "none", backend: "kimi", want: ambient.None, wantSource: "node", wantRefused: true,
			wantInRefusal: []string{`"kimi"`, "ambient_context: none (from node)", "AGENTS.md", "workspace, all"},
		},
		{
			name: "a workflow value is declared too", wf: "operator", backend: "opencode", want: ambient.Operator, wantSource: "workflow", wantRefused: true,
			wantInRefusal: []string{"ambient_context: operator (from workflow)", "reads a file in"},
		},
		{
			name: "so is the environment", envDef: "none", backend: "grok", want: ambient.None, wantSource: "env", wantRefused: true,
			wantInRefusal: []string{"ambient_context: none (from env)"},
		},
		{
			name: "and a run override", override: "workspace", node: "all", backend: "opencode", want: ambient.Workspace, wantSource: "run_override", wantRefused: true,
			wantInRefusal: []string{"ambient_context: workspace (from run_override)", "global AGENTS.md", "declare a policy it applies (all)"},
		},
		{name: "a declared value grok was never measured against", node: "workspace", backend: "grok", want: ambient.Workspace, wantSource: "node", wantWarned: true},
		{name: "the default on a backend that refuses it", backend: "opencode", want: ambient.Workspace, wantSource: "default"},
		{name: "the default on an unmeasured backend", backend: "grok", want: ambient.Workspace, wantSource: "default"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			e := &ClawExecutor{
				logger:                   iterlog.New(iterlog.LevelInfo, &buf),
				ambientContextOverride:   c.override,
				wfAmbientContext:         c.wf,
				ambientContextEnvDefault: c.envDef,
			}
			got, source, err := e.ambientContextPolicy(backendFields{id: "n", ambientContext: c.node}, c.backend)
			if got != c.want || source != c.wantSource {
				t.Errorf("policy = %v (from %s), want %v (from %s)", got, source, c.want, c.wantSource)
			}
			var refused *delegate.ErrCapabilityUnsupported
			if gotRefused := errors.As(err, &refused); gotRefused != c.wantRefused {
				t.Fatalf("refused = %v (err %v), want %v", gotRefused, err, c.wantRefused)
			}
			if err != nil && !c.wantRefused {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.wantRefused {
				if refused.NodeID != "n" || refused.Backend != c.backend {
					t.Errorf("refusal names node %q on %q, want n on %q", refused.NodeID, refused.Backend, c.backend)
				}
				for _, want := range c.wantInRefusal {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal %q does not say %q", err, want)
					}
				}
			}
			if warned := strings.Contains(buf.String(), "is not enforced on backend"); warned != c.wantWarned {
				t.Errorf("warned = %v, want %v (log: %q)", warned, c.wantWarned, buf.String())
			}
		})
	}
}

// ambientRecorder is a backend that records the tasks it is handed.
type ambientRecorder struct {
	tasks []delegate.Task
	fail  error
}

func (b *ambientRecorder) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.tasks = append(b.tasks, task)
	if b.fail != nil {
		return delegate.Result{}, b.fail
	}
	return delegate.Result{Output: map[string]any{"ok": true}, BackendName: delegate.BackendClaudeCode}, nil
}

func TestAmbientContextRefusesTheResolvedBackendBeforeDispatch(t *testing.T) {
	for _, backend := range []string{"kimi", "opencode", "grok"} {
		t.Run(backend, func(t *testing.T) {
			fake := &ambientRecorder{}
			reg := delegate.NewRegistry()
			reg.Register(backend, fake)
			prompts := 0
			exec := newFallbackExecutor(reg, EventHooks{OnLLMPrompt: func(string, string, string) { prompts++ }})
			node := fallbackAgentNode("reviewer", backend, "")
			node.AmbientContext = "none"
			_, err := exec.Execute(context.Background(), node, nil)
			var refused *delegate.ErrCapabilityUnsupported
			if !errors.As(err, &refused) || !strings.Contains(err.Error(), "ambient_context: none (from node)") {
				t.Fatalf("none on %s must be refused, got %v", backend, err)
			}
			if refused.Backend != backend || refused.NodeID != "reviewer" {
				t.Errorf("refusal lost its route or node: %+v", refused)
			}
			if len(fake.tasks) != 0 || prompts != 0 {
				t.Errorf("the refused node still dispatched (%d calls) or announced its prompt (%d)", len(fake.tasks), prompts)
			}
		})
	}
}

func TestAmbientContextRefusesAFallbackThatCannotHonourIt(t *testing.T) {
	head := &ambientRecorder{fail: &delegate.ErrTransient{Reason: "unavailable"}}
	tail := &ambientRecorder{}
	reg := delegate.NewRegistry()
	reg.Register("claude_code", head)
	reg.Register("kimi", tail)
	exec := newFallbackExecutor(reg, EventHooks{})
	exec.retry.MaxAttempts = 1
	node := fallbackAgentNode("reviewer", "claude_code", "")
	node.AmbientContext = "operator"
	node.Fallbacks = []ir.Fallback{{Name: "backup", Backend: "kimi", Model: "m", On: []string{"any"}}}
	_, err := exec.Execute(context.Background(), node, nil)
	var refused *delegate.ErrCapabilityUnsupported
	if !errors.As(err, &refused) || refused.Backend != "kimi" {
		t.Fatalf("the kimi route must be refused, got %v", err)
	}
	if len(head.tasks) == 0 || len(tail.tasks) != 0 {
		t.Fatalf("backend calls: primary=%d refused fallback=%d", len(head.tasks), len(tail.tasks))
	}
}

func TestAmbientContextHandsTheBackendItsPolicyAndSource(t *testing.T) {
	for _, c := range []struct {
		name, backend, node string
		want                ambient.Policy
		source              string
	}{
		{"the default on a backend that refuses declared values", "opencode", "", ambient.Workspace, "default"},
		{"a declared value kimi applies", "kimi", "workspace", ambient.Workspace, "node"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := &ambientRecorder{}
			reg := delegate.NewRegistry()
			reg.Register(c.backend, fake)
			exec := newFallbackExecutor(reg, EventHooks{})
			node := fallbackAgentNode("n", c.backend, "")
			node.AmbientContext = c.node
			if _, err := exec.Execute(context.Background(), node, nil); err != nil {
				t.Fatal(err)
			}
			if len(fake.tasks) != 1 {
				t.Fatalf("calls = %d, want 1", len(fake.tasks))
			}
			if got := fake.tasks[0]; got.AmbientContext != c.want || got.AmbientContextSource != c.source {
				t.Errorf("task carries %v (from %q), want %v (from %q)", got.AmbientContext, got.AmbientContextSource, c.want, c.source)
			}
		})
	}
}
