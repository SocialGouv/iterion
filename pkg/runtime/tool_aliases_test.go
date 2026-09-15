package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Only the provider decision is scripted: the real engine compiles the DSL,
// sets the bundle contract, builds the Claw task and executes actual file,
// search and shell tools. No external model or credential is involved.
type aliasToolBackend struct {
	calls     int
	failFirst bool
	repair    bool
}

func (b *aliasToolBackend) Execute(ctx context.Context, task delegate.Task) (delegate.Result, error) {
	b.calls++
	if b.failFirst && b.calls == 1 {
		return delegate.Result{}, fmt.Errorf("fixture interrupted before tools")
	}
	calls := map[string]json.RawMessage{
		"read_file": json.RawMessage(`{"path":"proof.txt"}`),
		"grep":      json.RawMessage(`{"pattern":"alias-proof","path":"proof.txt"}`),
		"bash":      json.RawMessage(`{"command":"cat proof.txt"}`),
	}
	if b.repair {
		calls["bash"] = json.RawMessage(`{"command":"printf repaired > marker; cat proof.txt"}`)
	}
	seen := map[string]bool{}
	for _, def := range task.ToolDefs {
		input, ok := calls[def.Name]
		if !ok {
			continue
		}
		result, err := def.Execute(ctx, input)
		if err != nil {
			return delegate.Result{}, fmt.Errorf("%s: %w", def.Name, err)
		}
		if !strings.Contains(result, "alias-proof") {
			return delegate.Result{}, fmt.Errorf("%s did not read real workspace: %q", def.Name, result)
		}
		seen[def.Name] = true
	}
	if len(seen) != 3 {
		return delegate.Result{}, fmt.Errorf("missing real tools: %v", seen)
	}
	return delegate.Result{Output: map[string]any{"answer": "verified"}, BackendName: delegate.BackendClaw}, nil
}

const aliasesEngineSource = `schema out:
  answer: string
prompt p:
  Read the proof.
agent probe:
  backend: claw
  model: "openai/gpt-test"
  system: p
  output: out
  tools: [Read, Bash, Grep]
  tool_policy: [Read, Bash, Grep]
workflow probe:
  entry: probe
  worktree: none
  probe -> done
`

func TestToolAliasesThroughRealEngineAndClawTools(t *testing.T) {
	pinEngineBuild(t, "v"+bundle.ToolAliasesSince)
	for _, floor := range []string{"", ">= 3.143.0", ">= " + bundle.ToolAliasesSince} {
		t.Run(floor, func(t *testing.T) {
			pr := parser.Parse("aliases.bot", aliasesEngineSource)
			if len(pr.Diagnostics) > 0 {
				t.Fatalf("parse: %v", pr.Diagnostics)
			}
			cr := ir.Compile(pr.File)
			if cr.HasErrors() {
				t.Fatalf("compile: %v", cr.Diagnostics)
			}
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "proof.txt"), []byte("alias-proof\n"), 0600); err != nil {
				t.Fatal(err)
			}
			tr := tool.NewRegistry()
			if err := tool.RegisterClawBuiltins(tr, workspace); err != nil {
				t.Fatal(err)
			}
			if err := tool.RegisterClawTodo(tr); err != nil {
				t.Fatal(err)
			}
			backend := &aliasToolBackend{}
			br := delegate.NewRegistry()
			br.Register(delegate.BackendClaw, backend)
			ex := model.NewClawExecutor(model.NewRegistry(), cr.Workflow, model.WithBackendRegistry(br), model.WithToolRegistry(tr), model.WithToolPolicy(tool.BuildChecker(nil, map[string][]string{"probe": {"Read", "Bash", "Grep"}}, nil)), model.WithLogger(iterlog.Nop()), model.WithWorkDir(workspace))
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			opts := []EngineOption{WithWorkDir(workspace), WithLogger(iterlog.Nop())}
			if floor != "" {
				opts = append(opts, WithBundle(requireBundle(t, floor)))
			}
			e := New(cr.Workflow, st, ex, opts...)
			// An opted-in parent must not leak its permission to this bare child.
			ctx := tool.WithBuiltinAliases(context.Background(), true)
			err = e.Run(ctx, "aliases", nil)
			enabled := floor == ">= "+bundle.ToolAliasesSince
			if !enabled {
				if err == nil || backend.calls != 0 || !strings.Contains(err.Error(), "requires.iterion") {
					t.Fatalf("missing floor executed: calls=%d err=%v", backend.calls, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if backend.calls != 1 {
				t.Fatalf("provider calls=%d", backend.calls)
			}
			run, err := st.LoadRun(ctx, "aliases")
			if err != nil || run.Status != store.RunStatusFinished {
				t.Fatalf("run: %+v %v", run, err)
			}
		})
	}
}

func aliasEngineForSource(t *testing.T, source, workspace string, st store.RunStore, backend *aliasToolBackend, primary delegate.Backend) *Engine {
	t.Helper()
	parsed := parser.Parse("aliases.bot", source)
	if len(parsed.Diagnostics) > 0 {
		t.Fatalf("parse: %v", parsed.Diagnostics)
	}
	cr := ir.Compile(parsed.File)
	if cr.HasErrors() {
		t.Fatalf("compile: %v", cr.Diagnostics)
	}
	tr := tool.NewRegistry()
	if err := tool.RegisterClawBuiltins(tr, workspace); err != nil {
		t.Fatal(err)
	}
	if err := tool.RegisterClawTodo(tr); err != nil {
		t.Fatal(err)
	}
	br := delegate.NewRegistry()
	br.Register(delegate.BackendClaw, backend)
	if primary != nil {
		br.Register(delegate.BackendClaudeCode, primary)
	}
	checker := tool.BuildChecker([]string{"Read", "Bash", "Grep", "postcondition:repair"}, nil, nil)
	ex := model.NewClawExecutor(model.NewRegistry(), cr.Workflow, model.WithBackendRegistry(br), model.WithToolRegistry(tr), model.WithToolPolicy(checker), model.WithWorkDir(workspace), model.WithLogger(iterlog.Nop()), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
	return New(cr.Workflow, st, ex, WithWorkDir(workspace), WithLogger(iterlog.Nop()), WithBundle(requireBundle(t, ">= "+bundle.ToolAliasesSince)))
}

func aliasFixtureStore(t *testing.T) (string, store.RunStore) {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "proof.txt"), []byte("alias-proof\n"), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return workspace, st
}

type aliasQuotaBackend struct{ calls int }

func (b *aliasQuotaBackend) Execute(context.Context, delegate.Task) (delegate.Result, error) {
	b.calls++
	return delegate.Result{}, &delegate.ErrRateLimited{Provider: delegate.BackendClaudeCode, Kind: delegate.RateLimitKindUsageWindow, Detail: "fixture quota"}
}

func TestToolAliasesReachClawFallbackAndRecoveryAgent(t *testing.T) {
	pinEngineBuild(t, "v"+bundle.ToolAliasesSince)
	t.Run("fallback", func(t *testing.T) {
		workspace, st := aliasFixtureStore(t)
		backend := &aliasToolBackend{}
		primary := &aliasQuotaBackend{}
		source := strings.Replace(aliasesEngineSource, "  backend: claw", "  backend: claude_code\n  fallbacks:\n    rescue:\n      backend: claw\n      model: \"openai/gpt-test\"\n      on: [usage_window]", 1)
		e := aliasEngineForSource(t, source, workspace, st, backend, primary)
		if err := e.Run(context.Background(), "alias-fallback", nil); err != nil {
			t.Fatal(err)
		}
		if primary.calls != 1 || backend.calls != 1 {
			t.Fatalf("route calls: %d / %d", primary.calls, backend.calls)
		}
	})
	t.Run("recovery agent_tools", func(t *testing.T) {
		workspace, st := aliasFixtureStore(t)
		backend := &aliasToolBackend{repair: true}
		const source = `tool repair:
  command: "false"
  goal: "create marker"
  postcondition: "test -f marker"
  policy: recover
  recovery:
    max_agent_attempts: 1
    agent_tools: [Read, Bash, Grep]
workflow recover_aliases:
  default_backend: claw
  entry: repair
  worktree: none
  repair -> done
`
		e := aliasEngineForSource(t, source, workspace, st, backend, nil)
		if err := e.Run(context.Background(), "alias-recovery", nil); err != nil {
			t.Fatal(err)
		}
		if backend.calls != 1 {
			t.Fatalf("recovery calls: %d", backend.calls)
		}
		marker, err := os.ReadFile(filepath.Join(workspace, "marker"))
		if err != nil || string(marker) != "repaired" {
			t.Fatalf("postcondition not achieved by tool: %q %v", marker, err)
		}
	})
}

func TestToolAliasesAreRebuiltOnFreshEngineResume(t *testing.T) {
	pinEngineBuild(t, "v"+bundle.ToolAliasesSince)
	workspace, st := aliasFixtureStore(t)
	first := &aliasToolBackend{failFirst: true}
	e := aliasEngineForSource(t, aliasesEngineSource, workspace, st, first, nil)
	if err := e.Run(context.Background(), "alias-resume", nil); err == nil {
		t.Fatal("fixture did not interrupt")
	}
	next := &aliasToolBackend{}
	fresh := aliasEngineForSource(t, aliasesEngineSource, workspace, st, next, nil)
	if err := fresh.Resume(context.Background(), "alias-resume", nil); err != nil {
		t.Fatal(err)
	}
	if first.calls != 1 || next.calls != 1 {
		t.Fatalf("calls across fresh resume: %d / %d", first.calls, next.calls)
	}
}
