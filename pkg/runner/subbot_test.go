package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

const subbotTestChild = `## tool-only child: no API keys, no sandbox needed
schema out:
  validated: bool
  echoed: string

vars:
  ticket: string = "none"

tool work:
  command: ` + "`" + `printf '{"validated":true,"echoed":"%s"}' {{vars.ticket}}` + "`" + `
  output: out

workflow child:
  entry: work
  work -> done
`

const subbotTestParent = `schema vout:
  validated: bool
  echoed: string

subbot run_ticket:
  source: "child.bot"
  with { ticket: "T-1" }
  output: vout

workflow parent:
  entry: run_ticket
  run_ticket -> done when validated
  run_ticket -> fail
`

func writeSubbotFixture(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestResolveSubbotSource pins the two places a pod may find a child bot,
// in order: beside the parent bundle, then the baked catalogue (the case of
// a parent materialised from a stored bundle, whose sibling bundles are the
// deployment's). A child in neither is a typed error naming both.
func TestResolveSubbotSource(t *testing.T) {
	root := t.TempDir()
	// A baked catalogue: <root>/bots/golden-master/extend.bot
	catalogue := filepath.Join(root, "bots")
	writeSubbotFixture(t, filepath.Join(catalogue, "golden-master"), "main.bot", subbotTestChild)
	extend := writeSubbotFixture(t, filepath.Join(catalogue, "golden-master"), "extend.bot", subbotTestChild)
	// A parent beside which the sibling exists.
	beside := filepath.Join(root, "beside", "modernize")
	writeSubbotFixture(t, beside, "main.bot", subbotTestParent)
	sibling := writeSubbotFixture(t, filepath.Join(root, "beside", "golden-master"), "extend.bot", subbotTestChild)
	// A parent materialised alone (stored bundle): no sibling next to it.
	alone := filepath.Join(root, "materialised", "modernize")
	writeSubbotFixture(t, alone, "main.bot", subbotTestParent)

	t.Run("beside the parent wins", func(t *testing.T) {
		got, err := resolveSubbotSource("../golden-master/extend.bot", beside, []string{catalogue})
		if err != nil || got != sibling {
			t.Fatalf("got %q, %v; want %q", got, err, sibling)
		}
	})
	t.Run("a materialised parent falls back to the baked catalogue", func(t *testing.T) {
		got, err := resolveSubbotSource("../golden-master/extend.bot", alone, []string{catalogue})
		if err != nil || got != extend {
			t.Fatalf("got %q, %v; want %q", got, err, extend)
		}
	})
	t.Run("no parent directory at all: the catalogue still serves", func(t *testing.T) {
		got, err := resolveSubbotSource("../golden-master/extend.bot", "", []string{catalogue})
		if err != nil || got != extend {
			t.Fatalf("got %q, %v; want %q", got, err, extend)
		}
	})
	t.Run("found nowhere: a typed error naming both places", func(t *testing.T) {
		_, err := resolveSubbotSource("../absent-bot/x.bot", alone, []string{catalogue})
		if err == nil || !strings.Contains(err.Error(), "beside the parent") || !strings.Contains(err.Error(), "baked catalogue") {
			t.Fatalf("err = %v, want both places named", err)
		}
	})
	// A subbot names a bundle, not a file on the pod: the two ways to reach
	// past the parent's collection and the catalogue are refused even when
	// the file exists — existence is exactly what a traversal probes for.
	outside := writeSubbotFixture(t, filepath.Join(root, "outside"), "x.bot", subbotTestChild)
	t.Run("an absolute path is refused", func(t *testing.T) {
		_, err := resolveSubbotSource(outside, beside, []string{catalogue})
		if err == nil || !strings.Contains(err.Error(), "absolute path") {
			t.Fatalf("err = %v, want the absolute-path refusal", err)
		}
	})
	t.Run("a `..` chain out of the collection and the catalogue is refused", func(t *testing.T) {
		_, err := resolveSubbotSource("../../outside/x.bot", beside, []string{catalogue})
		if err == nil || !strings.Contains(err.Error(), "outside the parent's bundle collection") {
			t.Fatalf("err = %v, want the containment refusal (the file exists)", err)
		}
	})
	t.Run("a catalogue <file> that climbs back out is refused", func(t *testing.T) {
		_, err := resolveSubbotSource("../golden-master/../../outside/x.bot", alone, []string{catalogue})
		if err == nil {
			t.Fatal("err = nil, want a refusal: the catalogue fallback must not serve a path outside its root")
		}
	})
}

func subbotTestRunner(t *testing.T) (*Runner, store.RunStore) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop(), SandboxOverride: "none"}}
	return r, st
}

// TestSubbotRunnerRunsChildOnPod runs a tool-only child through the closure
// the pod's engine invokes for `subbot` nodes: the child's terminal output
// comes back as the node's output, and the child ran under the parent's
// message (its own run id, linked to the parent).
func TestSubbotRunnerRunsChildOnPod(t *testing.T) {
	r, _ := subbotTestRunner(t)
	dir := t.TempDir()
	parentDir := filepath.Join(dir, "parent")
	writeSubbotFixture(t, parentDir, "main.bot", subbotTestParent)
	writeSubbotFixture(t, parentDir, "child.bot", subbotTestChild)
	msg := &queue.RunMessage{RunID: "run-parent", TenantID: "t1", OwnerID: "u1", BotID: "parent"}

	run := r.subbotRunnerFor(msg, parentDir, dir, iterlog.Nop())
	out, err := run(context.Background(), runtime.SubbotRequest{
		Source:      "child.bot",
		Vars:        map[string]any{"ticket": "T-9"},
		ParentRunID: msg.RunID,
		NodeID:      "run_ticket",
		ReattachKey: "run_ticket",
	})
	if err != nil {
		t.Fatalf("subbot runner: %v", err)
	}
	if v, _ := out["validated"].(bool); !v {
		t.Fatalf("child output = %v, want validated=true", out)
	}
	if e, _ := out["echoed"].(string); e != "T-9" {
		t.Fatalf("child output = %v, want echoed=T-9 (the `with:` vars reached the child)", out)
	}
}

// TestPodEngineRunsSubbotNodes is the pod-side end-to-end: a parent
// workflow declaring a `subbot` node, run by an engine built the way
// executeRun builds it, reaches `done`. Without the runner wired the same
// node dies with "no SubbotRunner is wired" — which is what every cloud run
// declaring a subbot did before this change.
func TestPodEngineRunsSubbotNodes(t *testing.T) {
	r, st := subbotTestRunner(t)
	dir := t.TempDir()
	parentPath := writeSubbotFixture(t, dir, "main.bot", subbotTestParent)
	writeSubbotFixture(t, dir, "child.bot", subbotTestChild)
	wf, hash, err := runview.CompileWorkflowWithHash(parentPath)
	if err != nil {
		t.Fatalf("compile parent: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "t1", "u1")
	msg := &queue.RunMessage{RunID: "run-parent", TenantID: "t1", OwnerID: "u1", BotID: "parent", WorkflowHash: hash}

	newEngine := func(t *testing.T, runID string, wired bool) *runtime.Engine {
		t.Helper()
		exec, usage, err := r.buildExecutor(ctx, msg, wf, iterlog.Nop(), nil)
		if err != nil {
			t.Fatalf("buildExecutor: %v", err)
		}
		opts := []runtime.EngineOption{
			runtime.WithLogger(iterlog.Nop()),
			runtime.WithWorkflowHash(hash),
			runtime.WithWorkDir(dir),
			runtime.WithSandboxOverride("none"),
			runtime.WithEventObserver(usage.observe),
		}
		if wired {
			opts = append(opts, runtime.WithSubbotRunner(r.subbotRunnerFor(msg, dir, dir, iterlog.Nop())))
		}
		return runtime.New(wf, st, exec, opts...)
	}

	t.Run("wired: the parent reaches done through its child", func(t *testing.T) {
		if err := newEngine(t, "run-parent", true).Run(ctx, "run-parent", nil); err != nil {
			t.Fatalf("parent run: %v", err)
		}
		run, err := st.LoadRun(ctx, "run-parent")
		if err != nil {
			t.Fatalf("load parent: %v", err)
		}
		if string(run.Status) != "finished" {
			t.Fatalf("parent status = %q, want finished", run.Status)
		}
	})
	t.Run("not wired: the pre-fix death, pinned", func(t *testing.T) {
		err := newEngine(t, "run-parent-unwired", false).Run(ctx, "run-parent-unwired", nil)
		if err == nil || !strings.Contains(err.Error(), "no SubbotRunner is wired") {
			t.Fatalf("err = %v, want the unwired death", err)
		}
	})
}

const subbotTestHumanChild = `schema answer:
  confirmed: bool

prompt ask_text:
  Confirm the ticket.

human gate:
  instructions: ask_text
  output: answer
  interaction: human

workflow child:
  entry: gate
  gate -> done
`

// TestSubbotRunnerParksOnHumanGate: a human gate inside the child pauses the
// CHILD, which is not a parent failure — the node parks until the operator
// answers. The park must ride the PARENT's ctx: the child's is cancelled the
// moment its engine returns (that cancel is the heartbeat's stop signal), and
// a park on it returned at once with `context canceled` — the gate failed the
// parent node 20 ms in instead of waiting for a human.
func TestSubbotRunnerParksOnHumanGate(t *testing.T) {
	r, st := subbotTestRunner(t)
	dir := t.TempDir()
	parentDir := filepath.Join(dir, "parent")
	writeSubbotFixture(t, parentDir, "main.bot", subbotTestParent)
	writeSubbotFixture(t, parentDir, "child.bot", subbotTestHumanChild)
	msg := &queue.RunMessage{RunID: "run-parent", TenantID: "t1", OwnerID: "u1", BotID: "parent"}

	const parentDeadline = 1500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), parentDeadline)
	defer cancel()
	start := time.Now()
	run := r.subbotRunnerFor(msg, parentDir, dir, iterlog.Nop())
	_, err := run(ctx, runtime.SubbotRequest{
		Source: "child.bot", ParentRunID: msg.RunID, NodeID: "run_ticket", ReattachKey: "run_ticket",
	})
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v after %s, want the PARENT's deadline: a `context canceled` here is the child's own cancel leaking into the park", err, elapsed)
	}
	if elapsed < parentDeadline-100*time.Millisecond {
		t.Fatalf("park returned after %s, want it held until the parent's deadline (%s)", elapsed, parentDeadline)
	}
	// The child is parked on its gate, not dead.
	idCtx := store.WithIdentity(context.Background(), "t1", "u1")
	ids, lerr := st.ListRuns(idCtx)
	if lerr != nil {
		t.Fatal(lerr)
	}
	var children int
	for _, id := range ids {
		child, cerr := st.LoadRun(idCtx, id)
		if cerr != nil || child.ParentRunID != msg.RunID {
			continue
		}
		children++
		if child.Status != store.RunStatusPausedWaitingHuman {
			t.Fatalf("child %s status = %q, want paused_waiting_human", id, child.Status)
		}
	}
	if children != 1 {
		t.Fatalf("children of the parent = %d, want exactly one", children)
	}
}

const subbotTestTwoNodeChild = `schema out:
  validated: bool
  echoed: string

tool first:
  command: ` + "`" + `printf '{"validated":true,"echoed":"one"}'` + "`" + `
  output: out

tool second:
  command: ` + "`" + `printf '{"validated":true,"echoed":"two"}'` + "`" + `
  output: out

workflow child:
  entry: first
  first -> second
  second -> done
`

// TestSubbotRunnerAppliesCloudBudgetCeiling: the deployment's multitenant
// ceiling (ITERION_CLOUD_MAX_*) binds a child as it binds the parent
// executeRun clamps — a tenant bot could otherwise declare its spend in a
// subbot and leave the cap behind. Proven two ways: the child's persisted
// budget carries the ceiling, and a two-node child under a one-iteration
// ceiling does not reach its second node.
func TestSubbotRunnerAppliesCloudBudgetCeiling(t *testing.T) {
	runChild := func(t *testing.T) (map[string]any, error, *store.Run) {
		t.Helper()
		r, st := subbotTestRunner(t)
		dir := t.TempDir()
		parentDir := filepath.Join(dir, "parent")
		writeSubbotFixture(t, parentDir, "main.bot", subbotTestParent)
		writeSubbotFixture(t, parentDir, "child.bot", subbotTestTwoNodeChild)
		msg := &queue.RunMessage{RunID: "run-parent", TenantID: "t1", OwnerID: "u1", BotID: "parent"}
		run := r.subbotRunnerFor(msg, parentDir, dir, iterlog.Nop())
		out, err := run(context.Background(), runtime.SubbotRequest{
			Source: "child.bot", ParentRunID: msg.RunID, NodeID: "run_ticket", ReattachKey: "run_ticket",
		})
		idCtx := store.WithIdentity(context.Background(), "t1", "u1")
		ids, lerr := st.ListRuns(idCtx)
		if lerr != nil {
			t.Fatal(lerr)
		}
		var child *store.Run
		for _, id := range ids {
			if c, cerr := st.LoadRun(idCtx, id); cerr == nil && c.ParentRunID == msg.RunID {
				child = c
			}
		}
		if child == nil {
			t.Fatal("no child run persisted")
		}
		return out, err, child
	}

	t.Run("no ceiling: the child runs both nodes", func(t *testing.T) {
		out, err, _ := runChild(t)
		if err != nil {
			t.Fatalf("child: %v", err)
		}
		if e, _ := out["echoed"].(string); e != "two" {
			t.Fatalf("child output = %v, want the second node's", out)
		}
	})
	t.Run("ceiling of one iteration: the child is clamped and stops after its first node", func(t *testing.T) {
		t.Setenv("ITERION_CLOUD_MAX_ITERATIONS", "1")
		_, err, child := runChild(t)
		if !errors.Is(err, runtime.ErrBudgetExceeded) {
			t.Fatalf("err = %v, want the budget refusal — the ceiling did not reach the child", err)
		}
		if child.Budget == nil || child.Budget.MaxIterations != 1 {
			t.Fatalf("child persisted budget = %+v, want max_iterations=1 from the platform ceiling", child.Budget)
		}
	})
}
