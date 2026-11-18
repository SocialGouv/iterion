package model

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestReviewPrAssembledRequestIsByteStableAcrossWorkspaces is review-pr's
// S3 witness (#3283 S6): the request claw assembles for a reviewer node —
// the SYSTEM blocks and the TOOL schemas — reads the same bytes for two runs
// of reviewer_alternate in two different checkouts, so a provider's prefix
// cache serves the second run from the first's prefix. review_system carries
// no template since 0.9.25 (the bots-package guard names one at its source);
// the AGENTS.md ambient header is workspace-relative (the S3 engine half);
// the secret files ride their stable sandbox mount paths — the assembled
// proof covers all three, first divergent byte reported.
//
// Mutations that redden it: a workspace-bound template back into
// review_system; the absolute header spelling back into claw_ambient.go.
func TestReviewPrAssembledRequestIsByteStableAcrossWorkspaces(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	wf, nodes := reviewPrReviewerNodes(t)
	var node *ir.JudgeNode
	for _, n := range nodes {
		if n.ID == "reviewer_alternate" {
			node = n
		}
	}
	if node == nil {
		t.Fatal("reviewer_alternate not found")
	}

	build := func(t *testing.T, ws, home string) ([]string, []byte) {
		t.Helper()
		guard := secretguard.New([]secretguard.Secret{
			{
				Name:        "forge_token",
				Value:       "prefix-stability-forge-41ba",
				Placeholder: secretguard.PlaceholderForName("forge_token"),
				FilePath:    "/run/iterion/secrets/forge_token",
				Env:         wf.Secrets["forge_token"].Env,
				Sinks:       append([]string(nil), wf.Secrets["forge_token"].Sinks...),
			},
			{
				Name:        "tracker_token",
				Value:       "prefix-stability-tracker-41ba",
				Placeholder: secretguard.PlaceholderForName("tracker_token"),
				FilePath:    "/run/iterion/secrets/tracker_token",
				Env:         wf.Secrets["tracker_token"].Env,
				Sinks:       append([]string(nil), wf.Secrets["tracker_token"].Sinks...),
			},
		}, secretguard.DefaultConfig())
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
		e := newTestClawExecutor(NewRegistry(), wf, WithWorkDir(ws), WithToolRegistry(tools),
			WithSecretGuard(guard), WithModelOverrides(onClaw), WithLogger(iterlog.Nop()))
		e.SetVars(map[string]any{
			"workspace_dir": ws, "base_ref": "main", "scope_notes": "", "review_tier": "guard",
			"prior_pushback": "", "tracker_api_base": "", "tracker_user": "", "ticket_refs": "",
			"ticket_context": "auto", "source_branch": "", "pr_url": "",
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
		var blocks []string
		blocks = append(blocks, task.BuildSystemPrompt())
		if ctx := clawAmbientSystemContext(task); ctx != "" {
			blocks = append(blocks, ctx)
		}
		proj := make([]GenerationToolJSON, 0, len(task.ToolDefs))
		for _, d := range task.ToolDefs {
			proj = append(proj, GenerationToolJSON{Name: d.Name, Description: d.Description, InputSchema: d.InputSchema})
		}
		toolsJSON, err := json.Marshal(proj)
		if err != nil {
			t.Fatalf("marshal tool defs: %v", err)
		}
		return blocks, toolsJSON
	}

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
			t.Parallel()
			wsA := shape.make(t)
			wsB := shape.make(t)
			if wsA == wsB {
				t.Fatalf("both workspaces resolved to %s", wsA)
			}
			aBlocks, aTools := build(t, wsA, t.TempDir())
			bBlocks, bTools := build(t, wsB, t.TempDir())
			if len(aBlocks) != len(bBlocks) {
				t.Fatalf("system block count differs: %d vs %d", len(aBlocks), len(bBlocks))
			}
			for i := range aBlocks {
				if off, ok := firstDivergentByte(aBlocks[i], bBlocks[i]); ok {
					t.Fatalf("system block %d diverges at byte %d: A %q vs B %q — the cross-run prefix cache cuts here",
						i, off, aBlocks[i][max(0, off-60):min(len(aBlocks[i]), off+60)], bBlocks[i][max(0, off-60):min(len(bBlocks[i]), off+60)])
				}
			}
			if off, ok := firstDivergentByte(string(aTools), string(bTools)); ok {
				t.Fatalf("tool schemas diverge at byte %d: A %q vs B %q", off,
					aTools[max(0, off-60):min(len(aTools), off+60)],
					bTools[max(0, off-60):min(len(bTools), off+60)])
			}
			// The promise is not vacuous: the ambient block really carries
			// this shape's instruction bytes, workspace-relative.
			if len(aBlocks) < 2 {
				t.Fatalf("no ambient block at all: %.200q", aBlocks)
			}
			shape.nonEmpty(t, aBlocks[1])
		})
	}
}
