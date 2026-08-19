package bots

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// TestVerifyProbeLoopIterationWiring guards the cost-saving verify_probe
// pre-check shared byte-for-byte by the three sibling loop bots
// (branch-improve-loop / whole-improve-loop / feature-dev). verify_probe lets
// the deterministic gate REUSE an already-authored <scratch_dir>/verify.sh
// instead of paying the LLM verify_build (~$0.69/13k tok) on every pass. The
// guard only skips verify_build on passes 2+ (iteration>0), so pass 1 of every
// run regenerates verify.sh for the current tree.
//
// That per-pass distinction hinges on the continuation-loop iteration reaching
// the tool. {{loop.*}} refs do NOT resolve inside a tool command — only
// input/vars/run do (see pkg/backend/model/executor_tool.go
// resolveCommandTemplate + its {{run.id}} special-case comment). So the
// iteration MUST be plumbed in as node input via the campaign→verify_probe edge
// data-mapping. If someone "simplifies" that back to `{{loop.…}}` directly in
// the command, the ref survives verbatim, int() sees the literal brace string,
// iteration collapses to 0 forever, and verify_build is NEVER skipped — a silent
// cost façade (P2 appears implemented but saves nothing). This test fails that
// regression at build time.
func TestVerifyProbeLoopIterationWiring(t *testing.T) {
	bots := []string{
		"branch-improve-loop/main.bot",
		"whole-improve-loop/main.bot",
		"feature-dev/main.bot",
		"instrument/main.bot",
	}
	for _, rel := range bots {
		rel := rel
		t.Run(rel, func(t *testing.T) {
			src, err := os.ReadFile(rel)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			pr := parser.Parse(rel, string(src))
			if pr.File == nil {
				t.Fatalf("parse produced no File")
			}
			cr := ir.Compile(pr.File)
			if cr.Workflow == nil {
				t.Fatalf("compile produced no Workflow")
			}
			wf := cr.Workflow

			raw, ok := wf.Nodes["verify_probe"]
			if !ok {
				t.Fatal("no verify_probe node — P2 pre-check missing")
			}
			probe, ok := raw.(*ir.ToolNode)
			if !ok {
				t.Fatalf("verify_probe is %T, want *ir.ToolNode (a deterministic tool node, no LLM)", raw)
			}

			// (1) The command reads {{input.iteration}} and carries NO
			// unresolvable {{loop.*}} ref.
			sawInputIter := false
			for _, r := range probe.CommandRefs {
				if r.Kind == ir.RefLoop {
					t.Errorf("verify_probe command has a {{loop.%v}} ref — loop refs do NOT resolve in a tool command; plumb the iteration through node input instead", r.Path)
				}
				if r.Kind == ir.RefInput && len(r.Path) > 0 && r.Path[0] == "iteration" {
					sawInputIter = true
				}
			}
			if !sawInputIter {
				t.Error("verify_probe command does not read {{input.iteration}} — the skip guard has no per-pass signal, so it can never distinguish pass 1 from passes 2+")
			}

			// (2) The campaign→verify_probe edge maps iteration ← the
			// continuation_loop iteration (the engine resolves the edge
			// data-mapping with full loop context, unlike the tool command).
			var edge *ir.Edge
			for _, e := range wf.Edges {
				if e.From == "campaign" && e.To == "verify_probe" {
					edge = e
					break
				}
			}
			if edge == nil {
				t.Fatal("no campaign→verify_probe edge")
			}
			mappedFromLoop := false
			for _, m := range edge.With {
				if m.Key != "iteration" {
					continue
				}
				for _, r := range m.Refs {
					if r.Kind == ir.RefLoop && len(r.Path) == 2 &&
						r.Path[0] == "continuation_loop" && r.Path[1] == "iteration" {
						mappedFromLoop = true
					}
				}
			}
			if !mappedFromLoop {
				t.Error("campaign→verify_probe edge does not map iteration ← {{loop.continuation_loop.iteration}} — passes 2+ can never see iteration>0, so verify_build is never skipped and P2 saves nothing")
			}
		})
	}
}
