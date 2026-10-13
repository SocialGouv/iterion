package runner

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
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
// runtime.WithWorkDir. The executor's WorkDir stays unset there — Claw's file
// tools keep the pod's workspace — so the fallback screen's sandbox probe has
// only this spec to learn the run's repository from, and the pod's cwd is a
// different repository with a different devcontainer.
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
	// The blast radius #2167 would not take: the executor's tool workspace is
	// NOT moved by answering the probe.
	if spec.WorkDir != "" {
		t.Errorf("ExecutorSpec.WorkDir = %q, want empty — it re-roots Claw's file tools (resolveExecutorWorkspace), "+
			"which the pod leaves at its own workspace", spec.WorkDir)
	}
}
