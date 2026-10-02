package bots

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// executableTreeNoiseRefs returns every {{run.tree_noise}} reference a
// node carries in an EXECUTABLE body — a tool's command, script,
// postcondition, or an action param — where the runtime shell-escapes the
// rendering into one argument. The bang form {{!run.tree_noise}} is
// substituted verbatim (three quoted pathspecs reach the shell as three
// words) and is the legitimate executable-body spelling beside
// $ITERION_TREE_NOISE, so it is not a finding.
func executableTreeNoiseRefs(n ir.Node) []*ir.Ref {
	var out []*ir.Ref
	add := func(refs []*ir.Ref) {
		for _, r := range refs {
			if r.Kind == ir.RefRun && len(r.Path) == 1 && r.Path[0] == "tree_noise" && !r.Unquoted {
				out = append(out, r)
			}
		}
	}
	if x, ok := n.(*ir.ToolNode); ok {
		add(x.CommandRefs)
		add(x.ScriptRefs)
		add(x.PostcondRefs)
		for _, p := range x.Params {
			add(p.Refs)
		}
	}
	return out
}

// {{run.tree_noise}} is the PROMPT rendering of the canonical tree-noise
// list: pre-quoted for the shell command line an agent pastes. In an
// executable body the runtime shell-escapes that rendering into ONE
// argument that matches no file (C153), and the exclusion vanishes in
// silence — a scope gate that reads it reports an empty scope on every
// run. Executable bodies read $ITERION_TREE_NOISE instead. The class is
// discovered from the compiled IR over the whole catalogue, never from
// source text; the prompt-side count is the floor that keeps the walk
// honest.
func TestRunTreeNoiseNeverRendersInAnExecutableBody(t *testing.T) {
	targets := catalogWorkflowFiles()
	if len(targets) == 0 {
		t.Fatal("no catalog workflows found — discovery glob likely broke")
	}
	promptSide := 0
	for _, path := range targets {
		u := unit.LoadDir(path)
		if u.Merged == nil {
			continue // the parse/compile test reports it
		}
		cr := ir.Compile(u.Merged)
		if cr.Workflow == nil {
			continue
		}
		for id, n := range cr.Workflow.Nodes {
			for _, r := range executableTreeNoiseRefs(n) {
				t.Errorf("%s: node %s renders %s in an executable body — the runtime shell-escapes it into one inert argument; read $ITERION_TREE_NOISE there instead, or write {{!run.tree_noise}} for the verbatim list", path, id, r.Raw)
			}
		}
		for _, p := range cr.Workflow.Prompts {
			for _, r := range p.TemplateRefs {
				if r.Kind == ir.RefRun && len(r.Path) == 1 && r.Path[0] == "tree_noise" {
					promptSide++
				}
			}
		}
	}
	// The catalogue renders the member in its prompts at dozens of sites
	// (the bot migration of #1464); a walk that lost the reference class
	// would report zero on both sides and stay green.
	if promptSide < 20 {
		t.Errorf("only %d prompt-side {{run.tree_noise}} references found — the walk no longer sees the class", promptSide)
	}
}
