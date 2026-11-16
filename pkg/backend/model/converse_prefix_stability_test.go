package model

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The cross-run prefix-cache promise of #3283 S3: the request claw assembles
// for revi-converse's converse_agent — the SYSTEM blocks and the TOOL
// schemas — reads the same bytes for two runs of the same node in two
// different checkouts, so a provider's prefix cache serves the second run
// from the first's prefix. What the wire carries is assembled here the way
// claw_backend serves it: BuildSystemPrompt (the resolved node prompt, the
// posture, the secrets block), then the ambient block, then the tool defs —
// the events fire too early to judge stability (llm_prompt precedes the
// ambient and secret additions), so the proof assembles the real parts in
// the real order.
//
// Scope of the promise, stated rather than implied: the secret file rides at
// its stable sandbox mount path — the spelling a sandboxed runner
// bind-mounts, the lane the gateway runs measure — so the secrets block is
// part of the comparison. A HOST (unsandboxed) run materializes its secret
// files into a per-run temp dir whose path enters the prompt; that residual
// is not stabilized by S3 (plan F9) and is out of this test's scope. The
// AGENTS.md ambient header is workspace-relative (the engine half of S3),
// and the node's system prompt carries no {{vars.*}} (the bot half): either
// one regressing shows up as a byte divergence, first one reported.
func TestConverseAssembledRequestIsByteStableAcrossWorkspaces(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	u, err := unit.LoadDir(filepath.Join(reviConverseBundle, "main.bot"))
	if err != nil {
		t.Fatalf("load revi-converse: %v", err)
	}
	compiled := ir.Compile(u.Merged)
	if compiled.HasErrors() {
		t.Fatalf("compile revi-converse: %v", compiled.Diagnostics)
	}
	wf := compiled.Workflow
	node, ok := wf.Nodes["converse_agent"].(*ir.AgentNode)
	if !ok {
		t.Fatalf("converse_agent is %T, want *ir.AgentNode", wf.Nodes["converse_agent"])
	}

	const mountPath = "/run/iterion/secrets/forge_token"
	const token = "prefix-stability-token-41ba"

	type assembled struct {
		blocks []string
		tools  []byte
	}
	build := func(t *testing.T, ws, home string) assembled {
		t.Helper()
		guard := secretguard.New([]secretguard.Secret{{
			Name:        "forge_token",
			Value:       token,
			Placeholder: secretguard.PlaceholderForName("forge_token"),
			// The sandbox mount spelling, identical in both runs: what a
			// sandboxed runner bind-mounts, and what the prompt renders.
			FilePath: mountPath,
			Env:      wf.Secrets["forge_token"].Env,
			Sinks:    append([]string(nil), wf.Secrets["forge_token"].Sinks...),
		}}, secretguard.DefaultConfig())
		tools := tool.NewRegistry()
		// The workspace is the tools' cwd at execution; the DEFS (name,
		// description, schema) are static text. Registered per workspace so
		// a def that grew a workspace-bound string would diverge here.
		if err := tool.RegisterClawBuiltinsWithEnv(tools, ws, []string{"HOME=" + home, "TMPDIR=" + t.TempDir()}); err != nil {
			t.Fatal(err)
		}
		if err := tool.RegisterClawTodo(tools); err != nil {
			t.Fatal(err)
		}
		var onClaw ModelOverrides
		onClaw.SetBackend(node.ID, delegate.BackendClaw)
		onClaw.SetModel(node.ID, "anthropic/claude-opus-5-5")
		onClaw.SetProvider(node.ID, "auto")
		e := newTestClawExecutor(NewRegistry(), wf, WithWorkDir(ws), WithToolRegistry(tools),
			WithSecretGuard(guard), WithModelOverrides(onClaw), WithLogger(iterlog.Nop()))
		e.SetVars(map[string]any{
			"workspace_dir": ws, "base_ref": "main", "pr_url": "https://forge.example/g/p/-/merge_requests/7",
			"discussion_id": "6a1f0c", "converse_question": "why does A return 2 now?",
			"trigger_note": "/revi why does A return 2 now?", "thread_context": "the thread", "replier": "op",
			"forge_base_url": "",
		})
		f, err := extractBackendFields(node)
		if err != nil {
			t.Fatalf("extract backend fields: %v", err)
		}
		task, err := e.buildTask(context.Background(), node, f, map[string]any{}, delegate.BackendClaw, &nodeBuildSession{})
		if err != nil {
			t.Fatalf("buildTask for %s: %v", ws, err)
		}
		// The blocks claw_backend assembles, in its order: the built system
		// prompt, then the ambient context when non-empty.
		blocks := []string{task.BuildSystemPrompt()}
		if ctx := clawAmbientSystemContext(task); ctx != "" {
			blocks = append(blocks, ctx)
		}
		// Tool defs as the request carries them (Execute wrappers dropped).
		proj := make([]GenerationToolJSON, 0, len(task.ToolDefs))
		for _, d := range task.ToolDefs {
			proj = append(proj, GenerationToolJSON{Name: d.Name, Description: d.Description, InputSchema: d.InputSchema})
		}
		toolsJSON, err := json.Marshal(proj)
		if err != nil {
			t.Fatalf("marshal tool defs: %v", err)
		}
		return assembled{blocks: blocks, tools: toolsJSON}
	}

	// Two checkouts of the same tree per SHAPE: same instruction bytes,
	// different absolute paths (and different depths, so an absolute
	// spelling cannot pass by coincidence). Two shapes, because the block
	// reaches its bytes two ways: the fallback's own AGENTS.md header, and
	// a CLAUDE.md whose @imports pull the same file — iterion's committed
	// CLAUDE.md is exactly `@AGENTS.md`, so that shape is the flagship lane.
	for _, shape := range []struct {
		name     string
		make     func(t *testing.T) string
		nonEmpty func(t *testing.T, ambient string)
	}{
		{
			name: "the fallback's own AGENTS.md",
			make: prefixWorkspace,
			nonEmpty: func(t *testing.T, ambient string) {
				t.Helper()
				if !strings.Contains(ambient, "## AGENTS.md (AGENTS.md)") {
					t.Fatalf("the ambient block does not carry the workspace AGENTS.md under a relative header: %.200q", ambient)
				}
			},
		},
		{
			name: "a CLAUDE.md importing it (@AGENTS.md)",
			make: prefixWorkspaceWithImports,
			nonEmpty: func(t *testing.T, ambient string) {
				t.Helper()
				if !strings.Contains(ambient, "### Imported: AGENTS.md") {
					t.Fatalf("the ambient block does not carry the @import under the REF AS WRITTEN: %.300q", ambient)
				}
			},
		},
	} {
		t.Run(shape.name, func(t *testing.T) {
			wsA := shape.make(t)
			wsB := shape.make(t)
			if wsA == wsB {
				t.Fatalf("both workspaces resolved to %s", wsA)
			}

			a := build(t, wsA, t.TempDir())
			b := build(t, wsB, t.TempDir())

			if len(a.blocks) != len(b.blocks) {
				t.Fatalf("system block count differs: %d vs %d", len(a.blocks), len(b.blocks))
			}
			for i := range a.blocks {
				if off, ok := firstDivergentByte(a.blocks[i], b.blocks[i]); !ok {
					continue
				} else {
					ctxA := a.blocks[i][max(0, off-60):min(len(a.blocks[i]), off+60)]
					ctxB := b.blocks[i][max(0, off-60):min(len(b.blocks[i]), off+60)]
					t.Fatalf("system block %d diverges at byte %d: A %q vs B %q — the cross-run prefix cache cuts here",
						i, off, ctxA, ctxB)
				}
			}
			if off, ok := firstDivergentByte(string(a.tools), string(b.tools)); ok {
				t.Fatalf("tool schemas diverge at byte %d: A %q vs B %q", off,
					a.tools[max(0, off-60):min(len(a.tools), off+60)],
					b.tools[max(0, off-60):min(len(b.tools), off+60)])
			}
			// And the promise is not vacuous: the ambient block really
			// carries this shape's instruction bytes, workspace-relative.
			if len(a.blocks) < 2 {
				t.Fatalf("no ambient block at all: %.200q", a.blocks)
			}
			shape.nonEmpty(t, a.blocks[1])
		})
	}
}

// GenerationToolJSON is the wire projection of one tool def (name,
// description, schema) — the bytes the request carries, minus the Execute
// closure json.Marshal cannot encode.
type GenerationToolJSON struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// firstDivergentByte returns the offset of the first byte the two strings
// differ at, ok=false for identical inputs (or when one is a prefix of the
// other with equal content up to the shorter's end — reported through the
// length difference at the caller).
func firstDivergentByte(a, b string) (int, bool) {
	if a == b {
		return 0, false
	}
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i, true
		}
	}
	return n, true
}

// prefixWorkspace is a git checkout of the same tree as every other
// workspace this test builds: one commit on main, an AGENTS.md the ambient
// walk loads (no CLAUDE.md — the fallback's file), one source file.
func prefixWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := gittest.Cmd(ws, args...)
		cmd.Env = append(cmd.Env, "HOME="+t.TempDir())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	write("AGENTS.md", "# Workspace instructions\n\nRead the diff before answering. Be concise.\n")
	write("app.go", "package app\n\nfunc A() int { return 1 }\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base with instructions")
	return ws
}

// TestConverseWireRequestIsByteStableAcrossWorkspaces is the wire-level
// witness of the same promise: two full claw executions of the REAL
// converse_agent node on two checkouts, the scripted model capturing what
// arrives in CreateMessageRequest — SystemBlocks (the built prompt and the
// ambient block, joined) and the tool list. No secret guard here, so the
// secrets block is absent by construction (the task-level test above
// carries it at the sandbox mount spelling); what this adds is the proof
// that the assembly path itself — claw_backend's block order, the
// recovery/nudge preamble excluded — reproduces the same bytes on the
// wire, not just at task build.
func TestConverseWireRequestIsByteStableAcrossWorkspaces(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"bash", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH: the converse loop shells out to it", bin)
		}
	}
	u, err := unit.LoadDir(filepath.Join(reviConverseBundle, "main.bot"))
	if err != nil {
		t.Fatalf("load revi-converse: %v", err)
	}
	compiled := ir.Compile(u.Merged)
	if compiled.HasErrors() {
		t.Fatalf("compile revi-converse: %v", compiled.Diagnostics)
	}
	wf := compiled.Workflow
	node, ok := wf.Nodes["converse_agent"].(*ir.AgentNode)
	if !ok {
		t.Fatalf("converse_agent is %T, want *ir.AgentNode", wf.Nodes["converse_agent"])
	}
	capture := func(t *testing.T, ws string) (string, []byte) {
		t.Helper()
		home := t.TempDir()
		mdl := &converseModel{ws: ws, answer: "Re: op — « why? »", forge: "gitlab"}
		reg := NewRegistry()
		reg.Register("anthropic", func(string) (api.APIClient, error) { return mdl, nil })
		tools := tool.NewRegistry()
		if err := tool.RegisterClawBuiltinsWithEnv(tools, ws, []string{"HOME=" + home, "TMPDIR=" + t.TempDir()}); err != nil {
			t.Fatal(err)
		}
		if err := tool.RegisterClawTodo(tools); err != nil {
			t.Fatal(err)
		}
		var onClaw ModelOverrides
		onClaw.SetBackend(node.ID, delegate.BackendClaw)
		onClaw.SetModel(node.ID, "anthropic/claude-opus-5-5")
		onClaw.SetProvider(node.ID, "auto")
		e := newTestClawExecutor(reg, wf, WithWorkDir(ws), WithToolRegistry(tools),
			WithModelOverrides(onClaw), WithLogger(iterlog.Nop()))
		e.SetVars(map[string]any{
			"workspace_dir": ws, "base_ref": "main", "pr_url": "https://forge.example/g/p/-/merge_requests/7",
			"discussion_id": "6a1f0c", "converse_question": "why does A return 2 now?",
			"trigger_note": "/revi why does A return 2 now?", "thread_context": "", "replier": "op",
		})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := e.Execute(ctx, node, map[string]any{}); err != nil {
			t.Fatalf("converse_agent on claw (%s): %v", ws, err)
		}
		seen := mdl.observed()
		return seen.system, seen.defsJSON
	}
	// Both shapes: the fallback's own AGENTS.md and the import-carrying
	// CLAUDE.md — the flagship lane's route into the same instructions.
	for _, make := range []func(t *testing.T) string{prefixWorkspace, prefixWorkspaceWithImports} {
		sysA, defsA := capture(t, make(t))
		sysB, defsB := capture(t, make(t))
		if off, ok := firstDivergentByte(sysA, sysB); ok {
			t.Fatalf("the wire system diverges at byte %d: A %q vs B %q — the cross-run prefix cache cuts here",
				off, sysA[max(0, off-60):min(len(sysA), off+60)], sysB[max(0, off-60):min(len(sysB), off+60)])
		}
		if off, ok := firstDivergentByte(string(defsA), string(defsB)); ok {
			t.Fatalf("the wire TOOL DEFS diverge at byte %d: A %q vs B %q — the promise covers defs, not names",
				off, defsA[max(0, off-60):min(len(defsA), off+60)], defsB[max(0, off-60):min(len(defsB), off+60)])
		}
	}
}

// prefixWorkspaceWithImports is the flagship lane's shape: the workspace's
// CLAUDE.md is exactly `@AGENTS.md` — iterion's own committed spelling — so
// the ambient block reaches the same instructions through the walk's import
// expansion. A label spelled from the RESOLVED path here (`### Imported:
// /checkout/AGENTS.md`) carries the checkout's path on every run and cuts
// the cross-run prefix cache on this lane alone.
func prefixWorkspaceWithImports(t *testing.T) string {
	t.Helper()
	ws := prefixWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws, "CLAUDE.md"), []byte("@AGENTS.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := gittest.Cmd(ws, "add", "-A")
	git.Env = append(git.Env, "HOME="+t.TempDir())
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	c := gittest.Cmd(ws, "commit", "-q", "-m", "instructions")
	c.Env = append(c.Env, "HOME="+t.TempDir())
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	return ws
}
