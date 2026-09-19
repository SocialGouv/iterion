package bots

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// TestCatalogInputReadsAreMappedByAnIncomingEdge holds the catalogue to the
// runtime's rule for a node's input: it is built ONLY from the `with`
// mappings of the edges that reach the node (pkg/runtime/engine_resolve.go —
// an edge with no `with` contributes nothing; the entry node alone receives
// the run's inputs). So a `{{input.X}}` a node reads in one of its own texts
// that NO incoming edge maps renders empty on every run — silently: the graph
// compiles and the template renders an empty string. adr-cartograph's ADR
// survey shipped that way from 2.0.0 to 2.0.2 (`scan_adrs -> survey_code`
// carried no `with`; the surveyor never saw the inventory it is told not to
// re-author). The strict dry run flags the read on the paths it visits; this
// test asks the compiled graph, for every node, visited or not.
//
// The surfaces walked are the ones the runtime resolves against the node's
// own input (buildNodeInputRS): an agent's, judge's or LLM router's prompts;
// a human node's instructions, LLM system prompt and review_url; a tool's
// command, script, postcondition and connector-action params; a compute
// node's expressions (special_node.go). NOT walked, on purpose, because the
// runtime resolves them against the RUN's payload or another node's output:
// a router's `over:`, a loop cap expression, a foreach collection, a fail
// message, an emit's or subbot's `with`, an edge's own `with` values (the
// SOURCE node's output), the human interaction prompt (the question map).
// `images:` templates are kept as raw strings by the IR and cannot be seen.
//
// A field mapped by SOME incoming edge but not by others is legitimate (a
// loop's back-edge overlays what changes per iteration; a first pass reads an
// empty `fail_log`) and is not this test's business.
func TestCatalogInputReadsAreMappedByAnIncomingEdge(t *testing.T) {
	targets := catalogWorkflowFiles()
	if len(targets) == 0 {
		t.Fatal("no catalog workflows found — discovery glob likely broke")
	}

	counts := map[string]int{}
	for _, path := range targets {
		u := unit.LoadDir(path)
		if u.Merged == nil {
			continue // the parse/compile test reports it
		}
		cr := ir.Compile(u.Merged)
		if cr.Workflow == nil {
			continue
		}
		unmapped, c := unmappedInputReads(cr.Workflow)
		for k, v := range c {
			counts[k] += v
		}
		for _, r := range unmapped {
			t.Errorf("%s: %s — no incoming edge maps it, it renders empty on every run (map it with `with { %s: \"{{outputs.<producer>.%s}}\" }` on the edge that reaches the node, or read {{outputs.<producer>.%s}} directly)",
				path, r, r.Field, r.Field, r.Field)
		}
	}
	// A surface the catalogue uses today must have contributed reads — a walk
	// that silently loses a node kind would otherwise stay green.
	for _, surface := range []string{"agent prompt", "tool command", "compute expr"} {
		if counts[surface] == 0 {
			t.Errorf("the %q surface contributed no {{input.*}} read across the catalogue — the reference walk for it broke", surface)
		}
	}
	keys := make([]string, 0, len(counts))
	total := 0
	for k, v := range counts {
		keys = append(keys, k)
		total += v
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+itoa(counts[k]))
	}
	t.Logf("%d {{input.*}} reads checked across %d workflows: %s", total, len(targets), strings.Join(parts, " "))
}

// TestInputReadGuardSeesEverySurface proves the walk on one fixture per
// surface, each with exactly one read no incoming edge maps: a surface the
// walk lost would leave its fixture unreported. The last fixture maps every
// read and must report nothing.
func TestInputReadGuardSeesEverySurface(t *testing.T) {
	cases := []struct {
		file string
		want []string
	}{
		{"agent_prompt.bot", []string{"reader.never_mapped (agent prompt)"}},
		{"compute_expr.bot", []string{"leak.never_mapped (compute expr)"}},
		{"llm_router.bot", []string{"pick.router_never_mapped (router prompt)"}},
		{"review_url.bot", []string{"gate.url_never_mapped (human review_url)"}},
		{"tool_command.bot", []string{"probe.cmd_never (tool command)", "probe.pc_never (tool postcondition)"}},
		{"mapped.bot", nil},
	}
	for _, tc := range cases {
		path := filepath.Join("testdata", "input_mapping", tc.file)
		u := unit.LoadDir(path)
		if u.Merged == nil {
			t.Fatalf("%s: no file", path)
		}
		cr := ir.Compile(u.Merged)
		if cr.Workflow == nil {
			t.Fatalf("%s: no workflow", path)
		}
		for _, d := range cr.Diagnostics {
			if d.Severity == ir.SeverityError {
				t.Fatalf("%s: compile error: %s", path, d.Error())
			}
		}
		unmapped, _ := unmappedInputReads(cr.Workflow)
		got := make([]string, 0, len(unmapped))
		for _, r := range unmapped {
			got = append(got, r.String())
		}
		sort.Strings(got)
		want := append([]string(nil), tc.want...)
		sort.Strings(want)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s: unmapped reads = %v, want %v", path, got, want)
		}
	}
}

// inputRead is one {{input.<field>}} a node reads in a text the runtime
// resolves against that node's input.
type inputRead struct {
	Node    string
	Field   string
	Surface string
	Raw     string
}

func (r inputRead) String() string { return r.Node + "." + r.Field + " (" + r.Surface + ")" }

// unmappedInputReads returns the reads of the workflow's non-entry nodes that
// no incoming edge maps — one per node, field and surface, however many texts
// of the node read the field — and the number of reads seen per surface.
func unmappedInputReads(wf *ir.Workflow) ([]inputRead, map[string]int) {
	mapped := map[string]map[string]bool{}
	for _, e := range wf.Edges {
		if mapped[e.To] == nil {
			mapped[e.To] = map[string]bool{}
		}
		for _, m := range e.With {
			mapped[e.To][m.Key] = true
		}
	}
	ids := make([]string, 0, len(wf.Nodes))
	for id := range wf.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	counts := map[string]int{}
	var unmapped []inputRead
	seen := map[string]bool{}
	for _, id := range ids {
		if id == wf.Entry {
			continue // the entry node's input is the run's
		}
		for _, r := range inputReadsOf(wf, wf.Nodes[id]) {
			counts[r.Surface]++
			if mapped[id][r.Field] {
				continue
			}
			key := r.String()
			if !seen[key] {
				seen[key] = true
				unmapped = append(unmapped, r)
			}
		}
	}
	return unmapped, counts
}

// inputReadsOf lists the {{input.*}} references in the texts of one node
// that the runtime resolves against the node's own input.
func inputReadsOf(wf *ir.Workflow, n ir.Node) []inputRead {
	var reads []inputRead
	add := func(surface string, refs []*ir.Ref) {
		for _, r := range refs {
			// A bare {{input}} does not compile (C004), so Path is never empty here.
			if r == nil || r.Kind != ir.RefInput || len(r.Path) == 0 {
				continue
			}
			reads = append(reads, inputRead{Node: n.NodeID(), Field: r.Path[0], Surface: surface, Raw: r.Raw})
		}
	}
	prompt := func(surface, name string) {
		if p := wf.Prompts[name]; p != nil {
			add(surface, p.TemplateRefs)
		}
	}
	switch x := n.(type) {
	case *ir.AgentNode:
		prompt("agent prompt", x.SystemPrompt)
		prompt("agent prompt", x.UserPrompt)
	case *ir.JudgeNode:
		prompt("judge prompt", x.SystemPrompt)
		prompt("judge prompt", x.UserPrompt)
	case *ir.RouterNode:
		prompt("router prompt", x.SystemPrompt)
		prompt("router prompt", x.UserPrompt)
	case *ir.HumanNode:
		prompt("human prompt", x.Instructions)
		prompt("human prompt", x.SystemPrompt)
		add("human review_url", x.ReviewURLRefs)
	case *ir.ToolNode:
		add("tool command", x.CommandRefs)
		add("tool script", x.ScriptRefs)
		add("tool postcondition", x.PostcondRefs)
		for _, p := range x.Params {
			add("tool action param", p.Refs)
		}
	case *ir.ComputeNode:
		for _, ce := range x.Exprs {
			if ce.AST == nil {
				continue
			}
			for _, r := range ce.AST.Refs() {
				if r.Namespace != "input" || len(r.Path) == 0 {
					continue
				}
				reads = append(reads, inputRead{Node: n.NodeID(), Field: r.Path[0], Surface: "compute expr", Raw: "input." + strings.Join(r.Path, ".")})
			}
		}
	}
	return reads
}
