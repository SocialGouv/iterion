package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
