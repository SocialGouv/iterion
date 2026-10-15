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
// that NO incoming edge maps is absent from the input on every run, and the
// graph compiles: what the read then delivers is each surface's own form of
// nothing, never an error (absentReadDelivers).
// adr-cartograph's ADR survey shipped that way from 2.0.0 to 2.0.2
// (`scan_adrs -> survey_code` carried no `with`; the surveyor never saw the
// inventory it is told not to re-author). The strict dry run flags the read on
// the paths it visits; this test asks the compiled graph, for every node,
// visited or not.
//
// The surfaces walked are the fields the runtime resolves against the node's
// own input (every buildNodeInputRS call site in pkg/runtime): an agent's,
// judge's or LLM router's system and user prompts; a human node's
// instructions, LLM system prompt and review_url; a tool's command, script,
// postcondition and connector-action params; a compute node's expressions.
// Each field is its own surface, so a field dropped from the walk is seen
// (a floor on the fields the catalogue reads, a fixture per field below).
// NOT walked, on purpose, because the runtime resolves them against the
// RUN's payload or another node's output: a router's `over:`, a loop cap
// expression, a foreach collection, a fail message, an emit's or subbot's
// `with`, an edge's own `with` values (the SOURCE node's output), the human
// interaction prompt (the question map). `images:` templates are kept as raw
// strings by the IR and cannot be seen.
//
// A field mapped by SOME incoming edge but not by others is legitimate (a
// loop's back-edge overlays what changes per iteration; a first pass reads an
// empty `fail_log`) and is not this test's business.
func TestCatalogInputReadsAreMappedByAnIncomingEdge(t *testing.T) {
	targets := catalogWorkflowFiles(t)
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
			if r.Delivers == "" {
				t.Errorf("%s: %s — the %q surface states no reading for an absent key: absentReadDelivers has no case for it, so this report cannot say what the read delivers", path, r, r.Surface)
				continue
			}
			t.Errorf("%s: %s — no incoming edge maps it, so the input never carries it: %s (map it with `with { %s: \"{{outputs.<producer>.%s}}\" }` on the edge that reaches the node, or read {{outputs.<producer>.%s}} directly)",
				path, r, r.Delivers, r.Field, r.Field, r.Field)
		}
	}
	// Every field the catalogue reads today must have contributed — a walk
	// that silently loses one would otherwise stay green. The fields with no
	// live reader (router prompts, a human node's LLM system prompt and
	// review_url, a tool's postcondition and action params) are held by
	// their fixtures in TestInputReadGuardSeesEverySurface.
	for _, surface := range []string{
		"agent system prompt", "agent user prompt",
		"judge system prompt", "judge user prompt",
		"human instructions",
		"tool command", "tool script",
		"compute expr",
	} {
		if counts[surface] == 0 {
			t.Errorf("the %q surface contributed no {{input.*}} read across the catalogue — the reference walk for it broke, or the catalogue no longer reads it", surface)
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
// node kind, each carrying exactly one read no incoming edge maps on EVERY
// field of that kind the walk covers: a field the walk lost would leave its
// read unreported, and the exact-set comparison bites. The last fixture maps
// every read and must report nothing.
//
// Every walked surface also owes a reading of what an absent key delivers
// there, since the report quotes it: the fixtures cover them all, so a
// surface added to the walk with no case in absentReadDelivers is red here.
func TestInputReadGuardSeesEverySurface(t *testing.T) {
	cases := []struct {
		file string
		want []string
	}{
		{"agent_prompt.bot", []string{"reader.sys_never (agent system prompt)", "reader.never_mapped (agent user prompt)"}},
		{"judge_prompt.bot", []string{"review.judge_sys_never (judge system prompt)", "review.judge_never (judge user prompt)"}},
		{"llm_router.bot", []string{"pick.router_never_mapped (router system prompt)", "pick.router_user_never (router user prompt)"}},
		{"human_prompt.bot", []string{"gate.human_never (human instructions)", "gate.gate_sys_never (human system prompt)"}},
		{"review_url.bot", []string{"gate.sys_never_mapped (human system prompt)", "gate.url_never_mapped (human review_url)"}},
		{"tool_command.bot", []string{"probe.cmd_never (tool command)", "probe.pc_never (tool postcondition)"}},
		{"tool_script.bot", []string{"probe.script_never (tool script)"}},
		{"action_params.bot", []string{"comment.action_never (tool action param)"}},
		{"compute_expr.bot", []string{"leak.never_mapped (compute expr)"}},
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
			if r.Delivers == "" {
				t.Errorf("%s: %s — the %q surface has no case in absentReadDelivers, so the report cannot say what the read delivers there", path, r, r.Surface)
			}
		}
		sort.Strings(got)
		want := append([]string(nil), tc.want...)
		sort.Strings(want)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s: unmapped reads = %v, want %v", path, got, want)
		}
	}
}

// A human node's system prompt is served by two different renderers, so the
// reading an absent key gets there follows the interaction mode: the
// stand-in model of an `llm`/`llm_or_human` gate reads it through
// model.TemplateResolver and is handed the placeholder, while a review
// gate's companion gets it from Engine.renderHumanInstructions, which
// substitutes the empty string. One surface name, two consequences — the
// reason absentReadDelivers takes the node and not the surface alone.
func TestTheHumanSystemPromptsReadingFollowsTheInteractionMode(t *testing.T) {
	for _, tc := range []struct {
		file, want string
	}{
		{"human_prompt.bot", "literal {{input.X}} placeholder"},
		{"review_url.bot", "replaced by the empty string"},
	} {
		path := filepath.Join("testdata", "input_mapping", tc.file)
		u := unit.LoadDir(path)
		if u.Merged == nil {
			t.Fatalf("%s: no file", path)
		}
		cr := ir.Compile(u.Merged)
		if cr.Workflow == nil {
			t.Fatalf("%s: no workflow", path)
		}
		unmapped, _ := unmappedInputReads(cr.Workflow)
		found := false
		for _, r := range unmapped {
			if r.Surface != "human system prompt" {
				continue
			}
			found = true
			if !strings.Contains(r.Delivers, tc.want) {
				t.Errorf("%s: %s delivers %q, want it to say %q", path, r, r.Delivers, tc.want)
			}
		}
		if !found {
			t.Errorf("%s: no human system prompt read — the fixture no longer exercises this mode", path)
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
	// Delivers is what this read delivers when the input does not carry the
	// key — filled where the node is in hand, since one surface's reading
	// depends on it (absentReadDelivers).
	Delivers string
}

func (r inputRead) String() string { return r.Node + "." + r.Field + " (" + r.Surface + ")" }

// unmappedInputReads returns the reads of the workflow's non-entry nodes that
// no incoming edge maps — one per node, field and surface, however many
// times a text reads the field — and the number of reads seen per surface.
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
// that the runtime resolves against the node's own input, one surface per
// field.
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
		prompt("agent system prompt", x.SystemPrompt)
		prompt("agent user prompt", x.UserPrompt)
	case *ir.JudgeNode:
		prompt("judge system prompt", x.SystemPrompt)
		prompt("judge user prompt", x.UserPrompt)
	case *ir.RouterNode:
		prompt("router system prompt", x.SystemPrompt)
		prompt("router user prompt", x.UserPrompt)
	case *ir.HumanNode:
		prompt("human instructions", x.Instructions)
		prompt("human system prompt", x.SystemPrompt)
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
	for i := range reads {
		reads[i].Delivers = absentReadDelivers(reads[i].Surface, n)
	}
	return reads
}

// absentReadDelivers is what one surface delivers for a reference the
// node's input does not carry. The readings differ, and the difference is
// what tells an author how much the miss costs: a placeholder a model reads
// is loud, an empty string is silent, a failed expression stops the run.
//
// A text a model reads goes through model.TemplateResolver, which keeps an
// unresolvable reference as written (TestTheRendererNamesWhatItLeaves,
// TestBuildNodeInput_EntryFloorSeedsOnlyDefaultedVars) — as does a tool's
// command, where the braces reach the shell, while a script's reference
// becomes the language's null (RenderCommand / RenderScript). The
// operator-facing texts are rendered by Engine.renderHumanInstructions
// instead, which substitutes the empty string for an absent key
// (TestHumanInstructionsDropAnAbsentInputToTheEmptyString) — a human node's
// instructions, its `review_url`, and the system prompt of an
// `interaction: review` gate, whose companion is served by that same
// function (pkg/runtime/review.go, runReviewCompanion).
//
// "" for a surface this function does not name: the reporting sites refuse
// to speak for it rather than guess.
func absentReadDelivers(surface string, n ir.Node) string {
	const (
		asWritten = "the model reads the literal {{input.X}} placeholder, the resolver keeping an unresolvable reference as written (pkg/backend/model/template_resolver.go)"
		asEmpty   = "the reference is replaced by the empty string, so the text reads as if the value were blank (pkg/runtime/resume.go, Engine.renderHumanInstructions)"
	)
	switch surface {
	case "agent system prompt", "agent user prompt",
		"judge system prompt", "judge user prompt",
		"router system prompt", "router user prompt":
		return asWritten
	case "human system prompt":
		if hn, ok := n.(*ir.HumanNode); ok && hn.Interaction == ir.InteractionReview {
			return asEmpty
		}
		return asWritten
	case "human instructions", "human review_url":
		return asEmpty
	case "tool command", "tool postcondition":
		return "the shell receives the {{input.X}} braces as written (pkg/backend/model/template_resolver.go, RenderCommand)"
	case "tool script":
		return "the interpreter reads the language's null (pkg/backend/model/template_resolver.go, RenderScript)"
	case "tool action param":
		return "the connector call carries JSON null — the param nil when the whole value is the reference, the text \"null\" spliced in when it is embedded in a string (pkg/backend/model/executor_tool_action.go, renderActionParams)"
	case "compute expr":
		return "the expression reads nil, which travels on as the field's null or a zero where it types — and fails the node (EXPRESSION_FAILED, a stopped run) where it does not (pkg/dsl/expr, Engine.computeOutput)"
	}
	return ""
}
