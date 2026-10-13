package bots

import (
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// A prompt names an input field in two ways, and only one of them is checked
// by anything else. `{{input.x}}` is a template: the compiler resolves it
// (C034 when the schema has no such field) and
// `TestLLMInputFieldsReachTheirOwnPrompt` measures what it delivers. "read
// `input.x`" in the prose of a procedure is TEXT: the compiler sees a
// sentence, the renderer leaves it alone, and the model is told to reason
// about a value nothing hands it. That is the #1598 shape — plumbing
// described as done, delivering nothing — in the one spelling no other guard
// walks, and the trap a drop leaves behind
// (docs/agents/bots/input-reaches-the-model.md).
//
// Measured: secured-renovacy's `security_audit` was told to refuse a silent
// auto-bump when "the CURRENT pin (input.current_version) is flagged as
// malware", on a node whose input carried no current version on any route —
// the rule reasoned about a value the model never saw.
//
// SHOWN, not carried. The question is the one that page opens with: does a
// channel of THAT node put the value in front of the model? "Can the node's
// input hold the key" is strictly weaker and answers yes for every field the
// sibling ratchet baselines as reaching no channel — the predicate would
// exempt the whole recorded debt and admit new dead prose on it. So "shown"
// is read, never assumed:
//
//   - an agent or a judge: `missingInputFields`, the ratchet's own reading —
//     the node's channels rendered through the real resolver with a marker
//     per field, including the no-user-prompt case where the whole input is
//     the user message;
//   - an LLM router: the same marker reading over the prompts the runtime
//     resolves against that node's input;
//   - a human node: per surface, because its two surfaces have different
//     readers and each is handed the whole input beside its body — the
//     operator reads the instructions and the pause's inbound payload, the
//     stand-in model reads the system prompt and the whole input as its user
//     message. Only an `interaction: review` system prompt is read alone:
//     that companion's user message is the change under review
//     (humanReaderGetsWholeInput);
//   - plus the names another namespace's reference puts on screen
//     ({{vars.x}}, {{outputs.p.x}}): the value IS in front of the model, and
//     what the prose owes is the spelling the prompt renders.
//
// A value only a sibling's SESSION carries is not shown: a fork is dropped
// rather than risk a cross-provider 400 (pkg/backend/delegate,
// shouldDropSessionFork), so the agent can start with nothing but these
// prompts, and the ratchet holds that shape as debt rather than delivery.
//
// `_`-prefixed names are runtime plumbing, out of scope as everywhere else.
// No exemption list: prose is the cheapest text in the bundle to change, so
// the remedy is always available — reword the sentence to the reference the
// prompt renders, or carry the field the procedure needs and render it.
//
// The REPORT is per site too, because each of its three sentences depends on
// the hit: who carries the key (proseCarrier — the entry floor is a maybe for
// an undefaulted var), who reads that surface's body (proseReader — the
// operator at a pause, a review companion, a model), and the reference that
// renders for that reader (proseRemedy — `{{vars.x}}` where `{{input.x}}`
// would be substituted by the empty string). A shape with no words is
// reported as such rather than printed wrong.
func TestCatalogPromptProseNamesAnInputTheNodeShows(t *testing.T) {
	targets := catalogWorkflowFiles()
	if len(targets) == 0 {
		t.Fatal("no catalog workflows found — discovery glob likely broke")
	}

	bodies := map[string]int{}
	for _, path := range targets {
		u := unit.LoadDir(path)
		if u.Merged == nil {
			continue // parse failures belong to TestCatalogBotsParseAndCompileClean
		}
		cr := ir.Compile(u.Merged)
		if cr.Workflow == nil {
			continue
		}
		hits, counts := deadProseInputRefs(t, cr.Workflow)
		for surface, n := range counts {
			bodies[surface] += n
		}
		for _, h := range hits {
			if h.Carrier == "" || h.Reader == "" || h.Remedy == "" {
				t.Errorf("%s: %s — the report cannot be written: carrier=%q reader=%q remedy=%q (a surface or a carrier shape proseCarrier / proseReader / proseRemedy has no case for)",
					path, h, h.Carrier, h.Reader, h.Remedy)
				continue
			}
			t.Errorf("%s: %s", path, h.report())
		}
	}
	// Every prompt surface the catalogue writes today must have contributed a
	// body — a walk that silently lost one would otherwise stay green on the
	// 300-odd bodies the agents alone supply. The surfaces with no live body
	// (a router's prompts, a human node's LLM system prompt) are held by
	// their fixture in TestProseGuardSeesEverySurface.
	for _, surface := range []string{
		"agent system prompt", "agent user prompt",
		"judge system prompt", "judge user prompt",
		"human instructions",
	} {
		if bodies[surface] == 0 {
			t.Errorf("the %q surface contributed no prompt body across the catalogue — the surface walk for it broke, or the catalogue no longer writes it", surface)
		}
	}
	keys := make([]string, 0, len(bodies))
	total := 0
	for k, v := range bodies {
		keys = append(keys, k)
		total += v
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+itoa(bodies[k]))
	}
	t.Logf("%d prompt bodies swept across %d workflows: %s", total, len(targets), strings.Join(parts, " "))
}

// TestProseGuardSeesEverySurface proves the walk and the predicate on one
// fixture, on EVERY surface the walk covers and for each verdict: a field
// shown by the node's own prompt passes, a field on screen under another
// namespace's reference passes, a field the whole-input user message carries
// passes, while a carried-but-unrendered field and an uncarried one are both
// reported. A surface dropped from the walk loses its hits, and the
// exact-set comparison bites.
//
// The human lanes are the per-reader half: on the `llm_or_human` gate both
// surfaces pass their carried field (the operator's inbound payload, the
// stand-in model's whole-input user message) and report the field no route
// carries, while the review gate reports the SAME carried field in the
// system prompt its companion reads alone and passes it in the instructions
// the operator reads.
func TestProseGuardSeesEverySurface(t *testing.T) {
	path := filepath.Join("testdata", "prose_input_refs", "surfaces.bot")
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

	hits, counts := deadProseInputRefs(t, cr.Workflow)
	got := make([]string, 0, len(hits))
	for _, h := range hits {
		got = append(got, h.String())
	}
	sort.Strings(got)
	want := []string{
		"gate.gate_sys_absent (human system prompt) [uncarried]",
		"gate.human_absent (human instructions) [uncarried]",
		"pick.router_sys_dead (router system prompt) [uncarried]",
		"pick.router_user_dead (router user prompt) [carried]",
		"reader.sys_dead (agent system prompt) [carried]",
		"reader.user_dead (agent user prompt) [uncarried]",
		"review.judge_sys_dead (judge system prompt) [carried]",
		"review.judge_user_dead (judge user prompt) [uncarried]",
		"seed.nowhere (agent user prompt) [uncarried]",
		"seed.steering (agent user prompt) [carried]",
		"ship.ship_dead (human system prompt) [carried]",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("dead prose refs =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	for _, surface := range []string{
		"agent system prompt", "agent user prompt",
		"judge system prompt", "judge user prompt",
		"router system prompt", "router user prompt",
		"human instructions", "human system prompt",
	} {
		if counts[surface] == 0 {
			t.Errorf("the fixture's %q body was not walked", surface)
		}
	}
	// Every hit the fixture produces owes the three sentences the report
	// quotes, so a surface added to the walk with no reader — or a carrier
	// shape with no words — is red here rather than printing a falsehood.
	for _, h := range hits {
		if h.Carrier == "" || h.Reader == "" || h.Remedy == "" {
			t.Errorf("%s: carrier=%q reader=%q remedy=%q — the report has no words for this hit", h, h.Carrier, h.Reader, h.Remedy)
		}
	}
}

// The report is read by the author who has to act on it, so each of its
// three sentences must be true AT THE SITE: the carrier the node really has
// (the entry floor is a maybe for an undefaulted var), the reader of THAT
// surface (the operator at a pause, not a model), and a remedy that renders
// for that reader (`{{vars.X}}` where `{{input.X}}` would be substituted by
// the empty string). The round that added the per-reader lanes left the twin
// message speaking of a schema carrier and of "the model" everywhere.
func TestTheProseReportNamesTheCarrierTheReaderAndTheRemedy(t *testing.T) {
	for _, tc := range []struct {
		file, hit        string
		want, mustNotSay []string
	}{
		{
			file: filepath.Join("testdata", "prose_input_refs", "human_entry.bot"),
			hit:  "gate.undefaulted (human instructions) [carried]",
			want: []string{
				"the run's entry floor carries it ONLY when the launch supplies the var `undefaulted`",
				"the OPERATOR reads this text at the pause",
				"Render `{{vars.undefaulted}}`",
			},
			mustNotSay: []string{
				"the input carries the key: the node's input schema",
				"the model reads this text",
				"Render it in this node's own prompt",
			},
		},
		{
			file: filepath.Join("testdata", "prose_input_refs", "surfaces.bot"),
			hit:  "ship.ship_dead (human system prompt) [carried]",
			want: []string{
				"the incoming edge `gate -> ship` maps it in its `with`",
				"the review companion reads this text",
				"Render it in this node's own prompt (`{{input.ship_dead}}`)",
			},
			mustNotSay: []string{"entry floor", "the OPERATOR reads"},
		},
		{
			file: filepath.Join("testdata", "prose_input_refs", "surfaces.bot"),
			hit:  "reader.sys_dead (agent system prompt) [carried]",
			want: []string{
				"the node's input schema \"reader_in\" declares it",
				"the node's model reads this text",
			},
			mustNotSay: []string{"the OPERATOR reads", "entry floor"},
		},
		{
			file: filepath.Join("testdata", "prose_input_refs", "surfaces.bot"),
			hit:  "gate.gate_sys_absent (human system prompt) [uncarried]",
			want: []string{
				"the key is on no carrier",
				"the stand-in model reads this text, beside the node's whole input as its user message",
			},
			mustNotSay: []string{"the OPERATOR reads", "the review companion reads"},
		},
		{
			file: filepath.Join("testdata", "prose_input_refs", "surfaces.bot"),
			hit:  "seed.steering (agent user prompt) [carried]",
			want: []string{
				"the run's entry floor carries it (the var's default)",
				"the node's model reads this text",
				"Render `{{vars.steering}}`",
			},
			mustNotSay: []string{"ONLY when the launch supplies", "input schema"},
		},
		{
			file: filepath.Join("testdata", "prose_input_refs", "surfaces.bot"),
			hit:  "seed.nowhere (agent user prompt) [uncarried]",
			want: []string{
				"it names no var of the workflow, so the run's entry floor cannot seed it either (only a forced launch key could)",
				"the node's model reads this text",
				"carry the field: declare it on the input schema",
			},
			mustNotSay: []string{"the input carries the key"},
		},
		{
			file: filepath.Join("testdata", "prose_input_refs", "surfaces.bot"),
			hit:  "gate.human_absent (human instructions) [uncarried]",
			want: []string{
				"the key is on no carrier",
				"the OPERATOR reads this text at the pause",
				"carry the field: declare it on the input schema",
			},
			mustNotSay: []string{"the model reads this text"},
		},
	} {
		u := unit.LoadDir(tc.file)
		if u.Merged == nil {
			t.Fatalf("%s: no file", tc.file)
		}
		cr := ir.Compile(u.Merged)
		if cr.Workflow == nil {
			t.Fatalf("%s: no workflow", tc.file)
		}
		hits, _ := deadProseInputRefs(t, cr.Workflow)
		var report string
		for _, h := range hits {
			if h.String() == tc.hit {
				report = h.report()
			}
		}
		if report == "" {
			t.Errorf("%s: no hit %q — the fixture no longer exercises this shape", tc.file, tc.hit)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(report, want) {
				t.Errorf("%s / %s: the report does not say %q:\n  %s", tc.file, tc.hit, want, report)
			}
		}
		for _, no := range tc.mustNotSay {
			if strings.Contains(report, no) {
				t.Errorf("%s / %s: the report says %q, which is false at this site:\n  %s", tc.file, tc.hit, no, report)
			}
		}
	}
}

// A human node as the ENTRY node is the shape of two shipped examples
// (examples/human-in-the-loop.bot, examples/human-file-upload.bot): its
// input is the run's floor rather than an edge's mappings, so the vars that
// declare a DEFAULT are in the payload the pause shows and an undefaulted
// one the launch may omit is not. The guard owes that difference, or it
// reports a value the operator is reading.
func TestProseGuardCreditsTheEntryFloorOfAHumanNode(t *testing.T) {
	path := filepath.Join("testdata", "prose_input_refs", "human_entry.bot")
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
	hits, counts := deadProseInputRefs(t, cr.Workflow)
	got := make([]string, 0, len(hits))
	for _, h := range hits {
		got = append(got, h.String())
	}
	sort.Strings(got)
	want := []string{"gate.undefaulted (human instructions) [carried]"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("dead prose refs = %v, want %v", got, want)
	}
	if counts[surfaceHumanInstructions] != 1 {
		t.Errorf("the fixture's instructions body was walked %d times, want 1", counts[surfaceHumanInstructions])
	}
}

// proseHit is one prose reference to an input field no channel of the node
// shows. Carried separates the two remedies: a field the input holds needs a
// render (or a reword), a field it does not hold needs the plumbing too.
type proseHit struct {
	Node    string
	Prompt  string
	Surface string
	Field   string
	Carried bool
	// The three facts the report rests on, filled where the workflow and the
	// node are in hand: WHO carries the key, WHO reads this surface's body,
	// and the reference that renders for THAT reader. Each is a sentence,
	// and "" is "no case for this shape" — the reporter then refuses to
	// speak rather than print a carrier the node does not have, a reader
	// that does not exist on the surface, or a remedy that renders nothing
	// there (absentReadDelivers is the same discipline on the mirror guard).
	Carrier string
	Reader  string
	Remedy  string
}

func (h proseHit) String() string {
	state := "uncarried"
	if h.Carried {
		state = "carried"
	}
	return h.Node + "." + h.Field + " (" + h.Surface + ") [" + state + "]"
}

// report is the sentence the guard prints for one hit: the defect, then the
// three facts that make it actionable at THIS site.
func (h proseHit) report() string {
	return "prompt " + strconv.Quote(h.Prompt) + " of node " + strconv.Quote(h.Node) +
		" reasons about `input." + h.Field + "` in its PROSE, and no channel of that node shows it — " +
		h.Carrier + ", and " + h.Reader + ". " + h.Remedy
}

// proseCarrier says where the key the prose names comes from, in the words
// of the runtime that builds the input (buildNodeInputRS): the node's input
// schema, an incoming edge's `with`, or — on the workflow's ENTRY node — the
// run's own floor. The floor is the one carrier that is a MAYBE: it seeds
// only the vars that declare a default, so an undefaulted var is in the
// input only when the launch supplies it (ir.NodeInputKeys calls the entry
// set an upper bound for exactly this reason).
func proseCarrier(w *ir.Workflow, id, field string, carried bool) string {
	v := w.Vars[field]
	switch {
	case !carried && id == w.Entry:
		return "the key is on no carrier: the node's input schema does not declare it, no incoming edge's `with` maps it, and it names no var of the workflow, so the run's entry floor cannot seed it either (only a forced launch key could)"
	case !carried:
		return "the key is on no carrier: not on the node's input schema, not mapped by any incoming edge's `with`"
	}
	var by []string
	if s := w.Schemas[ir.NodeInputSchema(w.Nodes[id])]; s != nil {
		for _, f := range s.Fields {
			if f != nil && f.Name == field {
				by = append(by, "the node's input schema "+strconv.Quote(s.Name)+" declares it")
			}
		}
	}
	for _, e := range w.Edges {
		if e == nil || e.To != id {
			continue
		}
		for _, m := range e.With {
			if m != nil && m.Key == field {
				by = append(by, "the incoming edge `"+e.From+" -> "+e.To+"` maps it in its `with`")
			}
		}
	}
	if id == w.Entry && v != nil {
		if v.HasDefault {
			by = append(by, "the run's entry floor carries it (the var's default)")
		} else {
			by = append(by, "the run's entry floor carries it ONLY when the launch supplies the var `"+field+"` — it declares no default, and the floor seeds only defaulted vars")
		}
	}
	if len(by) == 0 {
		return ""
	}
	return "the input carries the key: " + strings.Join(by, "; ")
}

// proseReader is who reads the body of ONE surface — the fact the
// per-surface lane rests on (humanReaderGetsWholeInput): a human node's
// instructions are the OPERATOR's at the pause, a review gate's system
// prompt is its companion's, an `llm`/`llm_or_human` gate's system prompt is
// the stand-in model's, and every other prompt is the node's own model's.
// A human node's system prompt exists in no other mode: compileHumans keeps
// it only where a companion model runs, and warns (C313) where it does not.
func proseReader(surface string, n ir.Node) string {
	switch surface {
	case surfaceHumanInstructions:
		return "the OPERATOR reads this text at the pause (no model runs on an `interaction: human` gate), beside the pause's inbound payload"
	case surfaceHumanSystemPrompt:
		hn, ok := n.(*ir.HumanNode)
		if !ok {
			return ""
		}
		if hn.Interaction == ir.InteractionReview {
			return "the review companion reads this text, and nothing else of the input (its user message is the change under review and the dialogue)"
		}
		return "the stand-in model reads this text, beside the node's whole input as its user message"
	case "agent system prompt", "agent user prompt",
		"judge system prompt", "judge user prompt",
		"router system prompt", "router user prompt":
		return "the node's model reads this text"
	}
	return ""
}

// proseRemedy is the reference that renders FOR THAT READER, which is not
// always `{{input.X}}`: where the carrier is the entry floor and the key is
// a var, `{{vars.X}}` is the only reference that always resolves — the
// operator-facing renderer substitutes the EMPTY STRING for a key the input
// lacks (Engine.renderHumanInstructions,
// TestHumanInstructionsDropAnAbsentInputToTheEmptyString), so an
// `{{input.X}}` on a maybe-carried key is the silent nothing again, and a
// model's prompt would show the raw placeholder instead.
func proseRemedy(w *ir.Workflow, id, field string, carried bool) string {
	if _, isVar := w.Vars[field]; isVar {
		return "Render `{{vars." + field + "}}`, the reference that resolves on every route (the var's value, default or launched), or reword the sentence to a reference the prompt renders."
	}
	if carried {
		return "Render it in this node's own prompt (`{{input." + field + "}}`), or reword the sentence to a reference the prompt renders."
	}
	return "Reword the sentence to the reference the prompt renders (`{{outputs.<producer>." + field + "}}`), or carry the field: declare it on the input schema and map it on the edges that reach the node, then render it in this node's own prompt."
}

// deadProseInputRefs lists, for one workflow, every `input.<field>` a prompt
// names in its prose that no channel of its own node shows — one per node,
// prompt and field — and the number of bodies walked per surface.
func deadProseInputRefs(t *testing.T, w *ir.Workflow) ([]proseHit, map[string]int) {
	t.Helper()
	ids := make([]string, 0, len(w.Nodes))
	for id := range w.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	counts := map[string]int{}
	var hits []proseHit
	for _, id := range ids {
		surfaces := promptSurfacesOf(w, w.Nodes[id])
		if len(surfaces) == 0 {
			continue
		}
		carried := ir.NodeInputKeys(w, id)
		for _, s := range surfaces {
			counts[s.surface]++
			shown := inputsOnScreen(t, w, id, surfaces, s)
			for _, field := range proseInputRefs(s.body) {
				if strings.HasPrefix(field, "_") || shown[field] {
					continue
				}
				hits = append(hits, proseHit{
					Node: id, Prompt: s.prompt, Surface: s.surface, Field: field, Carried: carried[field],
					Carrier: proseCarrier(w, id, field, carried[field]),
					Reader:  proseReader(s.surface, w.Nodes[id]),
					Remedy:  proseRemedy(w, id, field, carried[field]),
				})
			}
		}
	}
	return hits, counts
}

// promptSurface is one prompt body the runtime resolves against a node's own
// input, named as the mirror guard names it (catalog_input_mapping_test.go,
// inputReadsOf).
type promptSurface struct {
	surface string
	prompt  string
	body    string
	refs    []*ir.Ref
}

// A human node's two surfaces are named apart because their readers are:
// humanReaderGetsWholeInput judges them one by one.
const (
	surfaceHumanInstructions = "human instructions"
	surfaceHumanSystemPrompt = "human system prompt"
)

// promptSurfacesOf lists them. A tool's command, a compute node's
// expressions and a human node's `review_url` are resolved against the same
// input but are not prompts: no model reads them, so prose cannot live
// there. The human INTERACTION prompt is left out for the reason the mirror
// guard leaves it out — the runtime resolves the question map against the
// run, not against this node's input.
func promptSurfacesOf(w *ir.Workflow, n ir.Node) []promptSurface {
	var out []promptSurface
	add := func(surface, name string) {
		if name == "" {
			return
		}
		p := w.Prompts[name]
		if p == nil {
			return
		}
		out = append(out, promptSurface{surface: surface, prompt: name, body: p.Body, refs: p.TemplateRefs})
	}
	switch x := n.(type) {
	case *ir.AgentNode:
		add("agent system prompt", x.SystemPrompt)
		add("agent user prompt", x.UserPrompt)
	case *ir.JudgeNode:
		add("judge system prompt", x.SystemPrompt)
		add("judge user prompt", x.UserPrompt)
	case *ir.RouterNode:
		add("router system prompt", x.SystemPrompt)
		add("router user prompt", x.UserPrompt)
	case *ir.HumanNode:
		add(surfaceHumanInstructions, x.Instructions)
		add(surfaceHumanSystemPrompt, x.SystemPrompt)
	}
	return out
}

// inputsOnScreen is every name the reader of ONE surface has in front of
// them: the input fields a channel delivers to that reader, plus the names
// another namespace's reference renders in the bodies they read.
//
// An agent, a judge and an LLM router have one reader for all their
// surfaces — the model is handed its system and user prompts together — so
// the answer is the node's, and for an agent or a judge the delivery half IS
// the ratchet's verdict (the same call, so the two guards can never disagree
// about one field). A router is not an ir.LLMNode — the ratchet skips it
// because it passes its input through to its output — so the same marker
// reading runs over its prompts.
//
// A human node has two readers, with disjoint bodies, and each is handed the
// whole input beside its own body — bar a review gate's companion, which
// reads its system prompt alone (humanReaderGetsWholeInput).
func inputsOnScreen(t *testing.T, w *ir.Workflow, id string, surfaces []promptSurface, judged promptSurface) map[string]bool {
	t.Helper()
	node := w.Nodes[id]
	// The bodies the judged surface's reader reads, and whether that reader
	// also receives the whole input.
	read, wholeInput := surfaces, false
	if hn, isHuman := node.(*ir.HumanNode); isHuman {
		read, wholeInput = []promptSurface{judged}, humanReaderGetsWholeInput(hn, judged.surface)
	}
	on := map[string]bool{}
	for _, s := range read {
		for _, r := range s.refs {
			if r == nil || r.Kind == ir.RefInput || len(r.Path) == 0 {
				continue
			}
			// The last segment is the value's own name: {{vars.x}} and
			// {{outputs.p.x}} both show "x". A whole {{outputs.p}} is
			// serialized, so p's output fields are on screen too.
			on[r.Path[len(r.Path)-1]] = true
			if r.Kind == ir.RefOutputs && len(r.Path) == 1 {
				if os := w.Schemas[ir.NodeOutputSchema(w.Nodes[r.Path[0]])]; os != nil {
					for _, f := range os.Fields {
						if f != nil {
							on[f.Name] = true
						}
					}
				}
			}
		}
	}

	fields := inputFields(t, w, id, ir.NodeInputSchema(node))
	if wholeInput {
		for _, f := range fields {
			on[f.name] = true
		}
		// The ENTRY node's input also carries the run's own floor, which
		// inputFields does not list: the vars that declare a DEFAULT (the
		// launch payload too, but nothing static can name it). A human node
		// is the entry of two shipped examples, and what the floor seeds is
		// in the questions the pause shows (pkg/runtime/engine_resolve.go,
		// TestBuildNodeInput_EntryFloorSeedsOnlyDefaultedVars). An
		// UNDEFAULTED var is not: the launch may omit it, so it stays
		// reportable.
		if id == w.Entry {
			for name, v := range w.Vars {
				if v != nil && v.HasDefault {
					on[name] = true
				}
			}
		}
		return on
	}
	if len(fields) == 0 {
		return on
	}
	if _, isLLM := node.(ir.LLMNode); isLLM {
		r, ok := missingInputFields(t, w, id)
		if !ok {
			// It declines a node whose input carries nothing, and this one
			// carries fields: the two guards would be reading different
			// inputs. Stop rather than judge prose against half a reading.
			t.Fatalf("%s: the ratchet's reading declined a node whose input carries %d field(s)", id, len(fields))
		}
		for _, f := range fields {
			on[f.name] = true
		}
		for _, m := range r.missing {
			delete(on, m)
		}
		return on
	}
	// An LLM router with no user prompt receives its whole input as the
	// user message, like an agent (pkg/backend/model/executor_template.go,
	// buildUserMessage).
	if r, isRouter := node.(*ir.RouterNode); isRouter && r.UserPrompt == "" {
		for _, f := range fields {
			on[f.name] = true
		}
		return on
	}
	var bodies []string
	var refs []*ir.Ref
	for _, s := range read {
		bodies = append(bodies, s.body)
		refs = append(refs, s.refs...)
	}
	for name := range renderedInputNames(fields, bodies, refs) {
		on[name] = true
	}
	return on
}

// humanReaderGetsWholeInput reports whether the reader of one of a human
// node's surfaces is handed the node's whole input beside the body — in
// which case every key the input carries is on their screen, whatever that
// body renders.
//
// `instructions:` is the OPERATOR's, at the pause: the studio lists every
// non-plumbing key of the node's resolved input above the answer form —
// persistPause / pauseReviewGate store that input as
// store.Interaction.Questions (TestHumanPause), gateInboundItems renders it
// declared or not (studio/src/lib/gateInbound.test.ts), and
// HumanPromptForm's schema-less branch hands the same map to PauseForm.
// A key whose value resolves to nothing is dropped as an empty row, which a
// rendered reference to it would show as nothing too.
//
// The system prompt is the stand-in MODEL's, and its user message is the
// whole input, serialized — there is no user-prompt field on a human node
// (pkg/backend/model/executor_human.go: buildUserMessage("", input, td),
// TestHumanLLMUserMessageIsTheWholeInput). The exception is
// `interaction: review`: that companion's user message is the change under
// review and the dialogue (pkg/runtime/review.go, buildCompanionMessage,
// TestAReviewCompanionReadsItsInputThroughItsSystemPromptAlone), so only
// what the body itself renders reaches it. In the modes with no model at all
// (`human`, `none`, `async`, `human_or_host`) nothing reads a system prompt,
// so nothing is on screen there either.
func humanReaderGetsWholeInput(hn *ir.HumanNode, surface string) bool {
	if surface == surfaceHumanInstructions {
		return true
	}
	return hn.Interaction == ir.InteractionLLM || hn.Interaction == ir.InteractionLLMOrHuman
}

// promptTemplateRef matches one `{{…}}` reference, including the literal
// delimiter form `{{"{{"}}`: what is left after removing them is prose.
var promptTemplateRef = regexp.MustCompile(`\{\{[^}]*\}\}`)

// proseInputDotted matches `input.<field>` written as text.
var proseInputDotted = regexp.MustCompile(`\binput\.([A-Za-z_][A-Za-z0-9_]*)`)

// proseInputRefs lists the input fields a prompt body names outside any
// template reference, in order of appearance, without repetition.
func proseInputRefs(body string) []string {
	prose := promptTemplateRef.ReplaceAllString(body, " ")
	var fields []string
	for _, m := range proseInputDotted.FindAllStringSubmatch(prose, -1) {
		if !slices.Contains(fields, m[1]) {
			fields = append(fields, m[1])
		}
	}
	return fields
}
