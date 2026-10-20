package runner

import (
	"context"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The pod's executor must bind the inbox + async-ask hooks: supervisor
// steering and operator chat are appended as store.QueuedUserMessage and
// only an InboxBinder delivers them into the agent's live turn. The pod
// was the one launch surface without them — supervisors evaluated and
// injected on cloud runs while nothing ever drained (found by the Persy
// cloud dogfood, run 01a03d70).
func TestExecutorSpecBindsInboxAndAsyncAsk(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
	msg := &queue.RunMessage{RunID: "run-spec", WorkflowName: "wf", Permission: "deny"}

	spec, usage, err := r.executorSpec(context.Background(), msg, &ir.Workflow{}, "", iterlog.Nop(), nil)
	if err != nil {
		t.Fatalf("executorSpec: %v", err)
	}
	if usage == nil {
		t.Fatal("metrics emitter missing")
	}
	if spec.Inbox == nil {
		t.Fatal("ExecutorSpec.Inbox not bound — queued supervisor/operator messages would never deliver on a pod")
	}
	if spec.AsyncAsk == nil {
		t.Fatal("ExecutorSpec.AsyncAsk not bound — ask_user_async would have no store on a pod")
	}
	if spec.Permission != "deny" {
		t.Fatalf("ExecutorSpec.Permission = %q, want run-level deny from queue", spec.Permission)
	}
	if spec.Inbox.Bind(context.Background(), msg.RunID) == nil {
		t.Fatal("inbox binder refuses to bind for the run — store not wired through")
	}
}

// A repo-bound cloud run does not execute in the pod's own workspace: it
// executes in the per-run clone executeRun hands the engine through
// runtime.WithWorkDir. The executor receives the same directory so Claw's
// relative file tools address that clone, while the fallback screen's sandbox
// probe resolves its repository instead of the pod's different devcontainer.
//
// The screen decides whether an operator's `--fallback codex:…` crossing is
// taken or refused, and it must decide that about the run that will execute.
func TestExecutorSpecNamesTheEnginesWorkDirForTheSandboxProbe(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	// Two real repositories: the pod's base workspace, and the clone this
	// message's run was given. EngineRepoRoot resolves each to itself, so a
	// probe reading the wrong one is visible as a different repository.
	podBase := gittest.SourceRepo(t)
	clone := gittest.SourceRepo(t)
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop(), WorkDir: podBase}}
	msg := &queue.RunMessage{RunID: "run-probe", WorkflowName: "wf"}

	spec, _, err := r.executorSpec(context.Background(), msg, &ir.Workflow{}, clone, iterlog.Nop(), nil)
	if err != nil {
		t.Fatalf("executorSpec: %v", err)
	}
	if got, want := runtime.EngineRepoRoot(spec.SandboxProbeWorkDir), clone; got != want {
		t.Errorf("the screen would probe the repository at %q; this run executes in %q (the pod's own workspace "+
			"is %q) — the codex screen must judge the run, not the pod", got, want, podBase)
	}
	if spec.WorkDir != clone {
		t.Errorf("ExecutorSpec.WorkDir = %q, want the run clone %q so Claw's relative file tools do not use the pod cwd",
			spec.WorkDir, clone)
	}
}

// The wire's runner half (ADR-129 §5, queue v=25): the run's FROZEN
// provider allowlist rides the message into the pod's executor spec, and
// the executor built from that spec drops every dispatch chain element
// resolving outside the list — the claiming runner enforces what the
// launch froze. Mutants: the stamp dropped from executorSpec → the spec
// assertion reds; the list dropped from runview.BuildExecutor → the
// chain assertion reds.
func TestExecutorSpecThreadsTheProvidersAllowlist(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
	msg := &queue.RunMessage{RunID: "run-allow", WorkflowName: "wf", LLMProviders: []string{"anthropic"}}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"draft": &ir.AgentNode{
			BaseNode: ir.BaseNode{ID: "draft"},
			LLMFields: ir.LLMFields{
				Backend: "claude_code",
				Model:   "anthropic/claude-opus-5",
			},
			Fallbacks: []ir.Fallback{{Name: "rescue", Backend: "codex", Provider: "zai"}},
		},
	}}
	spec, _, err := r.executorSpec(context.Background(), msg, wf, "", iterlog.Nop(), nil)
	if err != nil {
		t.Fatalf("executorSpec: %v", err)
	}
	if !slices.Equal(spec.LLMProviders, msg.LLMProviders) {
		t.Fatalf("ExecutorSpec.LLMProviders = %v, want the run's frozen list — the pod's filter rides this seam", spec.LLMProviders)
	}
	ex, err := runview.BuildExecutor(spec)
	if err != nil {
		t.Fatalf("BuildExecutor: %v", err)
	}
	if got := ex.EffectiveFallbackBackends(wf.Nodes["draft"]); len(got) != 0 {
		t.Errorf("EffectiveFallbackBackends = %v, want empty — the zai rescue resolves outside the run's allowlist and the chain drops it", got)
	}

	// The class twin: no list on the message, the same rescue is in the
	// answer — the drop is the allowlist's doing.
	msg2 := &queue.RunMessage{RunID: "run-plain", WorkflowName: "wf"}
	spec2, _, err := r.executorSpec(context.Background(), msg2, wf, "", iterlog.Nop(), nil)
	if err != nil {
		t.Fatalf("executorSpec (no list): %v", err)
	}
	ex2, err := runview.BuildExecutor(spec2)
	if err != nil {
		t.Fatalf("BuildExecutor (no list): %v", err)
	}
	if got := ex2.EffectiveFallbackBackends(wf.Nodes["draft"]); !slices.Contains(got, "codex") {
		t.Errorf("no-allowlist backends = %v, want codex — the drop must be the allowlist's doing, not a re-shape of the chain", got)
	}
}
