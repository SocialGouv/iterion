package model

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/rewrite"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The tool-node half of the #2007 placement decision. A tool node that opted
// into compression resolves its command through the rewriter chain
// host-side, even when the command itself runs in-container; the predicate
// must see the sandbox signal, the default under a sandbox is to stand the
// chain down (named on the run's log), and ITERION_REWRITE_SANDBOX=host is
// the escape hatch restoring the previous byte-identical placement.
func TestSandboxedToolNodeRewritePlacement(t *testing.T) {
	build := func(t *testing.T, sandboxed bool, logs *bytes.Buffer) (*ClawExecutor, string) {
		t.Helper()
		chain, log := loggingRewriter(t)
		wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}}
		opts := []ClawExecutorOption{WithWorkDir(t.TempDir()), WithRewriteChain(chain)}
		if logs != nil {
			opts = append(opts, WithLogger(iterlog.New(iterlog.LevelWarn, logs)))
		}
		exec := newTestClawExecutor(NewRegistry(), wf, opts...)
		if sandboxed {
			exec.SetSandbox(fakeSandboxRun{})
		}
		return exec, log
	}
	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "t"}, Command: "echo plain", Compress: "on"}
	resolve := func(exec *ClawExecutor) string {
		r, _ := exec.shellRecipe(context.Background(), node, map[string]any{})
		return r()
	}

	t.Run("the predicate sees the sandbox signal", func(t *testing.T) {
		t.Setenv(rewrite.SandboxEnv, "skip")
		sandboxed, _ := build(t, true, nil)
		if !sandboxed.sandboxSkipsRewrite() {
			t.Error("sandboxSkipsRewrite = false for a sandboxed executor under ITERION_REWRITE_SANDBOX=skip")
		}
		host, _ := build(t, false, nil)
		if host.sandboxSkipsRewrite() {
			t.Error("sandboxSkipsRewrite = true for a host executor under ITERION_REWRITE_SANDBOX=skip")
		}
	})

	t.Run("default: the chain stands down and names the knob", func(t *testing.T) {
		var logs bytes.Buffer
		exec, log := build(t, true, &logs)
		if got := resolve(exec); got != "echo plain" {
			t.Errorf("resolved = %q, want the verbatim command", got)
		}
		if b, _ := os.ReadFile(log); len(b) != 0 {
			t.Errorf("the chain ran at the default under a sandbox: rewriter log = %q", b)
		}
		if out := logs.String(); !strings.Contains(out, rewrite.SandboxEnv) || !strings.Contains(out, "stands down") {
			t.Errorf("the stand-down must name %s on the run's log, got: %q", rewrite.SandboxEnv, out)
		}
	})

	t.Run("host: a sandboxed node's command is still rewritten", func(t *testing.T) {
		t.Setenv(rewrite.SandboxEnv, "host")
		exec, log := build(t, true, nil)
		if got := resolve(exec); !strings.Contains(got, "echo compressed; echo plain") {
			t.Errorf("resolved = %q, want the rewritten command (byte-identical to before the flip)", got)
		}
		if b, _ := os.ReadFile(log); !strings.Contains(string(b), "echo plain") {
			t.Errorf("the chain did not run under %s=host: rewriter log = %q", rewrite.SandboxEnv, b)
		}
	})

	t.Run("skip: the chain stands down and names the knob", func(t *testing.T) {
		t.Setenv(rewrite.SandboxEnv, "skip")
		var logs bytes.Buffer
		exec, log := build(t, true, &logs)
		if got := resolve(exec); got != "echo plain" {
			t.Errorf("resolved = %q, want the verbatim command", got)
		}
		if b, _ := os.ReadFile(log); len(b) != 0 {
			t.Errorf("the chain ran despite %s=skip: rewriter log = %q", rewrite.SandboxEnv, b)
		}
		if out := logs.String(); !strings.Contains(out, rewrite.SandboxEnv) || !strings.Contains(out, "stands down") {
			t.Errorf("the stand-down must name %s on the run's log, got: %q", rewrite.SandboxEnv, out)
		}
	})

	t.Run("skip: a host node's command is still rewritten", func(t *testing.T) {
		t.Setenv(rewrite.SandboxEnv, "skip")
		exec, _ := build(t, false, nil)
		if got := resolve(exec); !strings.Contains(got, "echo compressed; echo plain") {
			t.Errorf("resolved = %q, want the rewritten command — the knob is about the sandbox", got)
		}
	})
}

// An unknown knob value is ignored loudly: the chain keeps its default
// placement and the constructor's warning names the variable.
func TestRewriteSandboxEnvUnknownValueWarns(t *testing.T) {
	t.Setenv(rewrite.SandboxEnv, "bogus")
	var logs bytes.Buffer
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}}
	newTestClawExecutor(NewRegistry(), wf, WithWorkDir(t.TempDir()), WithLogger(iterlog.New(iterlog.LevelWarn, &logs)))
	if out := logs.String(); !strings.Contains(out, rewrite.SandboxEnv) || !strings.Contains(out, `"bogus"`) {
		t.Errorf("the invalid value was not named: %q", out)
	}
}
