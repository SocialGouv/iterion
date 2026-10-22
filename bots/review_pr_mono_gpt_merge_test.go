package bots

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/reviewtopology"
)

// reviewPrWorkflow compiles Revi's whole unit — main.bot plus the lib/ files
// its `import` lines pull in — so the merge nodes are read as the run reads
// them.
func reviewPrWorkflow(t *testing.T) *ir.Workflow {
	t.Helper()
	cr := ir.Compile(parseBotUnit(filepath.Join("review-pr", "main.bot")).File)
	if cr.HasErrors() {
		t.Fatalf("review-pr does not compile: %+v", cr.Diagnostics)
	}
	return cr.Workflow
}

// The PREMISE of #2467, and the one fact whose falsification would make the
// whole change a downgrade: `mono_family` comes from the run's credentials,
// and claude is the PREFERRED family. A launch therefore resolves `gpt` only
// where no Claude credential serves — so the merge step and the pacer, both
// pinned to the Anthropic wire, had nothing to spend. Flip the preference and
// a mono-gpt run would be one that HOLDS a Claude credential, where moving
// the merger off Opus buys nothing.
func TestMonoGptMeansNoClaudeCredential(t *testing.T) {
	both := reviewtopology.FamilySet{"claude": true, "gpt": true}
	if mode, family := reviewtopology.ResolveFamilies(both, "auto"); family != "claude" {
		t.Errorf("a launch holding both families resolves %s/%q, want the claude family — review-pr's merge twin is justified by gpt meaning claude is ABSENT", mode, family)
	}
	gptOnly := reviewtopology.FamilySet{"gpt": true}
	if mode, family := reviewtopology.ResolveFamilies(gptOnly, "auto"); family != "gpt" {
		t.Errorf("a gpt-only launch resolves %s/%q, want mono on gpt", mode, family)
	}
}

// #2467: `mono_family=gpt` is the family of the WHOLE review, the merge step
// included. The GPT merge step is a TWIN of `converge` — a node's `model:`
// resolves `${ENV:-default}` and never `{{vars.x}}`, so a runtime-selectable
// family needs a second, differently pinned node picked by a guard — and a
// twin is only honest while it shares everything but its route: the same
// prompts BY NAME (never a second copy of 450 lines of merge instructions),
// the same output schema and the same effort dial, which the published run
// table reads from the ENVIRONMENT rather than from the node that ran.
func TestReviewPrGptMergeTwinSharesEverythingButItsRoute(t *testing.T) {
	wf := reviewPrWorkflow(t)
	claude, ok := wf.Nodes["converge"].(ir.LLMNode)
	if !ok {
		t.Fatalf("converge is %T, want an LLM node", wf.Nodes["converge"])
	}
	gpt, ok := wf.Nodes["converge_gpt"].(ir.LLMNode)
	if !ok {
		t.Fatalf("converge_gpt is %T, want an LLM node — mono-gpt has no merge step without it", wf.Nodes["converge_gpt"])
	}
	cf, gf := claude.GetLLMFields(), gpt.GetLLMFields()
	co, go_ := claude.GetSchemaFields(), gpt.GetSchemaFields()
	for _, shared := range []struct{ what, claude, gpt string }{
		{"system prompt", cf.SystemPrompt, gf.SystemPrompt},
		{"user prompt", cf.UserPrompt, gf.UserPrompt},
		{"output schema", co.OutputSchema, go_.OutputSchema},
		{"reasoning_effort dial", cf.ReasoningEffort, gf.ReasoningEffort},
	} {
		if shared.claude != shared.gpt {
			t.Errorf("the merge twins disagree on their %s (%q vs %q) — the GPT merge must be the same step on another route, and the run table's effort cell is read from the environment, not from the node that ran",
				shared.what, shared.claude, shared.gpt)
		}
	}
	// Each twin writes its OWN publication, under its own name: the hand-off
	// resolves a fallback source by NODE id (the manifest's
	// `fallback_nodes`), and an artifact is written only for a node carrying
	// `publish:` — while a name shared by two nodes would make them contend
	// for one logical alias when a resume reconciles artifact owners.
	cp, gp := ir.NodePublish(wf.Nodes["converge"]), ir.NodePublish(wf.Nodes["converge_gpt"])
	if cp == "" || gp == "" || cp == gp {
		t.Errorf("the merge twins publish %q and %q — each owes its own non-empty artifact name", cp, gp)
	}
	if cf.Model == gf.Model || cf.Backend == gf.Backend {
		t.Errorf("the merge twins share a route (model %q/%q, backend %q/%q) — then one of them is dead weight", cf.Model, gf.Model, cf.Backend, gf.Backend)
	}
}

// The twin's own route, read the way dispatch reads it: claw on an
// `openai/…` model with no provider hint — the one combination that resolves
// (the auto-resolver sends an unpinned model through claude_code, which does
// not know `openai/*`), and the route the usage-cap pre-flight must not find
// on the Anthropic wire.
func TestReviewPrGptMergeTwinLeavesTheAnthropicWire(t *testing.T) {
	wf := reviewPrWorkflow(t)
	node, ok := wf.Nodes["converge_gpt"].(*ir.AgentNode)
	if !ok {
		t.Fatalf("converge_gpt is %T, want an agent node — mono-gpt has no merge step without it", wf.Nodes["converge_gpt"])
	}
	fields := node.GetLLMFields()
	unset := func(string) string { return "" }
	if lands := ir.ExpandWithDefault(fields.Backend, unset); lands != delegate.BackendClaw {
		t.Errorf("converge_gpt backend %q lands on %q, want claw — an openai/* model resolves nowhere else", fields.Backend, lands)
	}
	if lands := ir.ExpandWithDefault(fields.Model, unset); !strings.HasPrefix(lands, "openai/") {
		t.Errorf("converge_gpt model %q lands on %q, want an openai/… spec — on claw the prefix IS the route", fields.Model, lands)
	}
	if lands := ir.ExpandWithDefault(fields.Provider, unset); lands != "" {
		t.Errorf("converge_gpt provider %q lands on %q, want no hint — a leftover hint describes a route this node does not take, and the credential walk reads it", fields.Provider, lands)
	}
	for _, r := range model.AnthropicWireRoutes(model.RouteView{Workflow: wf}) {
		if r.NodeID == "converge_gpt" {
			t.Errorf("converge_gpt is an Anthropic-wire route (%+v) — a mono-gpt review would park on a spent Claude window again", r)
		}
	}
	// claw bounds a declared tool loop at 5 steps when the node names no
	// number (claw_backend), while the audit tier lets the merge file 40
	// board issues before it writes the report: at the default the node would
	// stop mid-publication.
	const auditFindingCap = 40
	if steps := node.ToolMaxSteps; steps < auditFindingCap {
		t.Errorf("converge_gpt declares tool_max_steps %d, want at least the audit tier's %d-finding cap — one board create per finding, plus the skill read and the report write", steps, auditFindingCap)
	}
}

// The re-plumb, as a class rather than as nine edits: a twin's output is read
// through `pr_gate` ALONE, which carries the merge's payload and its engine
// telemetry forward under one set of names. A reader that reaches for a
// twin's output directly sees the empty string on every run the other twin
// served — the published run table would then describe a merge step that did
// not run.
//
// Counted rather than enumerated, so the next reader joins the guard by
// existing. Comment lines are excluded: naming the reference in prose is
// free, spelling it in a template is the claim.
//
// Counted PER TWIN, and on the dotted prefix: `outputs.converge` alone is a
// prefix of `outputs.converge_gpt`, so one tally over both twins moves on
// both sides of the equality at once — dropping a gpt mapping from a gate
// edge would decrement the bundle count and the gate count together and the
// equality would hold.
func TestReviewPrEveryMergeOutputReaderGoesThroughPrGate(t *testing.T) {
	wf := reviewPrWorkflow(t)
	refs := []string{"outputs.converge.", "outputs.converge_gpt."}

	onTheGateEdges := map[string]int{}
	for _, e := range wf.Edges {
		if e.To != "pr_gate" {
			continue
		}
		if e.From != "converge" && e.From != "converge_gpt" {
			t.Errorf("pr_gate also fires from %q — every incoming edge must carry the merge payload, or a reader downstream reads the empty string", e.From)
		}
		for _, dm := range e.With {
			for _, ref := range refs {
				onTheGateEdges[ref] += strings.Count(dm.Raw, ref)
			}
		}
	}
	for _, ref := range refs {
		if onTheGateEdges[ref] == 0 {
			t.Fatalf("no edge into pr_gate reads `%s` — that twin's payload and telemetry reach nothing", ref)
		}
	}

	inTheBundle := map[string]int{}
	paths, err := filepath.Glob(filepath.Join("review-pr", "*.bot"))
	if err != nil {
		t.Fatal(err)
	}
	libs, err := filepath.Glob(filepath.Join("review-pr", "lib", "*.bot"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range append(paths, libs...) {
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatal(rerr)
		}
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "##") {
				continue
			}
			for _, ref := range refs {
				inTheBundle[ref] += strings.Count(line, ref)
			}
		}
	}
	for _, ref := range refs {
		if inTheBundle[ref] != onTheGateEdges[ref] {
			t.Errorf("the bundle reads `%s` %d time(s) but only %d of them are on an edge into pr_gate — thread the value through pr_gate's input instead, so no reader has to know which twin ran",
				ref, inTheBundle[ref], onTheGateEdges[ref])
		}
	}
}

// The PAYLOAD half of the same re-plumb, which the telemetry guard above
// cannot see: pr_gate has two predecessors and each maps the whole contract
// from its own twin, so a field ONE edge omits reads empty on every run that
// twin served — a published review with no findings, i.e. approval by
// omission on this repo's own required gate.
//
// Nothing else reports it. `pr_gate_input` declares no optional field, yet
// C308 warns only when NO incoming edge maps a field a body reads
// (ir/diagnostics_codes.go), so one of two edges mapping it keeps the
// compiler silent; and a convergence that legitimately splits its contract
// across predecessors (evolve's `aggregate_review` takes claude_* from one
// edge and gpt_* from the other) makes the same rule wrong catalogue-wide.
// It is asserted HERE, where the two predecessors are TWINS of one step and
// each therefore owes the whole input.
//
// A gate edge reading the OTHER twin's output needs no guard: the twins sit
// on mutually exclusive branches, so C036 refuses the reference at compile
// time ("targets node %q which is not reachable before %q"), and
// reviewPrWorkflow fails closed on it. What C036 does NOT refuse is a
// merge-owned field sourced from a COMMON ANCESTOR — `merge_reviews` is
// reachable before either twin — so presence of the key proves nothing about
// where the value comes from. Both halves are asserted here: the field the
// merge step PRODUCES is owed from the edge's own twin, and every other field
// is owed identically by both edges once each edge's own source is normalised
// away. The produced set is read from the twins' declared output schema, so a
// field added to the merge contract is covered the day it lands.
func TestReviewPrBothGateEdgesMapTheWholePayload(t *testing.T) {
	wf := reviewPrWorkflow(t)
	schema := wf.Schemas["pr_gate_input"]
	if schema == nil || len(schema.Fields) == 0 {
		t.Fatal("pr_gate_input declares no field — this guard lost its subject")
	}

	gateEdges := 0
	mapped := map[string]int{}
	// What each edge maps onto each key, kept PER SOURCE: the payload half of
	// the re-plumb is a claim about where a value comes from, not only about
	// which keys are present.
	raw := map[string]map[string]string{}
	var sources []string
	// The fields the merge step itself produces — the union over the gate's
	// own predecessors' output schemas, so an edge from a node declaring a
	// wider contract widens the obligation rather than escaping it.
	produced := map[string]bool{}
	for _, e := range wf.Edges {
		if e.To != "pr_gate" {
			continue
		}
		gateEdges++
		sources = append(sources, e.From)
		if llm, ok := wf.Nodes[e.From].(ir.LLMNode); ok {
			if out := wf.Schemas[llm.GetSchemaFields().OutputSchema]; out != nil {
				for _, f := range out.Fields {
					if f != nil {
						produced[f.Name] = true
					}
				}
			}
		}
		for _, dm := range e.With {
			mapped[dm.Key]++
			if raw[dm.Key] == nil {
				raw[dm.Key] = map[string]string{}
			}
			raw[dm.Key][e.From] = strings.TrimSpace(dm.Raw)
		}
	}
	if gateEdges < 2 {
		t.Fatalf("pr_gate has %d incoming edge(s) — the merge twins both feed it, or one family's review publishes nothing", gateEdges)
	}
	if len(produced) == 0 {
		t.Fatal("no edge into pr_gate comes from a node with a declared output schema — the source half of this guard lost its subject")
	}
	slices.Sort(sources)
	// The spelling every edge's mapping is normalised to, so the two twins'
	// mappings are comparable: they differ by route, never by what they map.
	const anyTwin = "outputs.<the twin that ran>."
	for _, f := range schema.Fields {
		if mapped[f.Name] != gateEdges {
			t.Errorf("%d of %d edge(s) into pr_gate map pr_gate_input.%s — a field one twin's edge omits reads empty on every run that twin served, and pr_gate is both what publish_review posts and what the hand-off resolves (manifest `produces`)",
				mapped[f.Name], gateEdges, f.Name)
			continue
		}
		if produced[f.Name] {
			// The merge step produces it, so the gate owes the copy of the
			// twin that RAN. Sourced from a shared ancestor instead, the
			// published verdict is a reviewer's raw array while
			// `total_findings` still comes from the twin — the count and the
			// table disagree, and the hand-off seeds the fixer with
			// un-thresholded, un-capped, un-cross-confirmed findings.
			for _, src := range sources {
				want := "{{outputs." + src + "." + f.Name + "}}"
				if got := raw[f.Name][src]; got != want {
					t.Errorf("%s -> pr_gate maps pr_gate_input.%s to %q, want %q — the merge step declares that field in its own output, so the gate owes the twin's copy and not a common ancestor's (C036 refuses the other twin's output, never an ancestor's)",
						src, f.Name, got, want)
				}
			}
			continue
		}
		// Not the merge's own: a shared read (a var, an ancestor both twins
		// cross, the engine telemetry of whichever twin ran). Whatever it is,
		// both edges owe the SAME one, modulo the twin they come from.
		first, want := sources[0], strings.ReplaceAll(raw[f.Name][sources[0]], "outputs."+sources[0]+".", anyTwin)
		for _, src := range sources[1:] {
			if got := strings.ReplaceAll(raw[f.Name][src], "outputs."+src+".", anyTwin); got != want {
				t.Errorf("pr_gate_input.%s reads %q from %s but %q from %s — the twins are one step on two routes, so a field neither of them declares must reach the gate the same way on both edges",
					f.Name, want, first, got, src)
			}
		}
	}
	for key := range mapped {
		if !slices.ContainsFunc(schema.Fields, func(f *ir.SchemaField) bool { return f != nil && f.Name == key }) {
			t.Errorf("an edge into pr_gate maps %q, which pr_gate_input does not declare — the value arrives nowhere", key)
		}
	}
}

// The merge guard reads `vars.mono_family` ALONE, so it follows the launch's
// PIN and not the topology the run ends up with. Both documented readings are
// pinned here, because the README's route table and the node comment state
// them (and a predicate on `outputs.tier_expand.effective_review_mode` would
// not be launch-decidable, which would leave both twins reachable and defeat
// the prune the whole change rests on).
//
// A launch that keeps the pin while fanning BOTH reviewers — `review_tier:
// audit`, or an explicit `--var review_mode=dual --var mono_family=gpt` —
// merges on the twin. Deliberate: that pin is the operator's explicit choice,
// and on a GPT-only tenant (where reviewtopology.InjectAll injects it)
// sending the merge back to the Anthropic wire is the failure #2467 closes.
// A webhook launch the publisher resolved DUAL carries no `mono_family` at
// all (InjectIfDeclaredFamilies deletes it), so the var falls back to its
// declared "claude" default and the Anthropic merge step holds.
func TestReviewPrTheMergeGuardFollowsTheLaunchPinNotTheTopology(t *testing.T) {
	wf := reviewPrWorkflow(t)
	reaches := func(open ir.EdgeFilter, id string) bool {
		return wf.CanReachVia(open, func(n ir.Node) bool { return n != nil && n.NodeID() == id })
	}
	for _, tc := range []struct {
		what                      string
		vars                      map[string]any
		wantFan, wantGptMergeOnly bool
	}{
		{
			what:             "audit keeps a pinned gpt family on the twin",
			vars:             map[string]any{"review_mode": "mono", "mono_family": "gpt", "review_tier": "audit"},
			wantFan:          true,
			wantGptMergeOnly: true,
		},
		{
			what:             "an explicitly dual launch keeps a pinned gpt family on the twin",
			vars:             map[string]any{"review_mode": "dual", "mono_family": "gpt", "review_tier": "guard"},
			wantFan:          true,
			wantGptMergeOnly: true,
		},
		{
			what:             "a publisher-resolved dual launch carries no pin, so the declared claude default merges",
			vars:             map[string]any{"review_mode": "dual", "review_tier": "guard"},
			wantFan:          true,
			wantGptMergeOnly: false,
		},
		{
			what:             "audit on the claude family merges on Opus",
			vars:             map[string]any{"review_mode": "mono", "mono_family": "claude", "review_tier": "audit"},
			wantFan:          true,
			wantGptMergeOnly: false,
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			open := wf.LaunchVarEdgeFilter(tc.vars)
			if got := reaches(open, "fan"); got != tc.wantFan {
				t.Errorf("reaches(fan) = %v, want %v", got, tc.wantFan)
			}
			gpt, claude := reaches(open, "converge_gpt"), reaches(open, "converge")
			if gpt != tc.wantGptMergeOnly || claude == tc.wantGptMergeOnly {
				t.Errorf("reaches(converge_gpt) = %v, reaches(converge) = %v — want the gpt twin alone = %v; the guard is `vars.mono_family == 'gpt'` and the README's route table and lib/nodes.bot both say so",
					gpt, claude, tc.wantGptMergeOnly)
			}
		})
	}
}

// The hand-off declaration, as the resolver reads it: `pr_gate` is the primary
// because it is the one node BOTH twins feed, and the fallback list is a UNION
// over every node it names, de-duped by anchor keeping the FIRST copy
// (pkg/server.loadReviewFindings — pinned there by
// TestTheReviewFallbackUnionsEveryNodeAndKeepsTheFirstCopy). So the ORDER is
// the claim this file owes: a merge twin ahead of `merge_reviews` is what gives
// a finding both families raised its merged form, with its bumped severity and
// its cross-confirmed flag, instead of a reviewer's raw copy.
func TestReviewPrHandsOffThroughTheNodeBothTwinsFeed(t *testing.T) {
	man := loadCatalogManifests(t)["review-pr"]
	if man == nil {
		t.Fatal("review-pr has no manifest")
	}
	var spec *bundle.ProducedArtifact
	for i := range man.Produces {
		if man.Produces[i].Kind == bundle.HandoffKindReview {
			spec = &man.Produces[i]
		}
	}
	if spec == nil {
		t.Fatal("review-pr declares no `produces: kind: review` — a GPT-family review would seed no fixer")
	}
	if spec.Node != "pr_gate" {
		t.Errorf("the review is produced by %q — only pr_gate is fed by BOTH merge twins, so naming a twin hands the other family's fixer nothing", spec.Node)
	}
	firstMerge, rawArrays := -1, -1
	for i, node := range spec.FallbackNodes {
		if (node == "converge" || node == "converge_gpt") && firstMerge < 0 {
			firstMerge = i
		}
		if node == "merge_reviews" {
			rawArrays = i
		}
	}
	if firstMerge < 0 || rawArrays < 0 {
		t.Fatalf("fallback_nodes = %v — it owes both a merge twin (the merged copy) and merge_reviews (the raw arrays the merge's prose degradation needs)", spec.FallbackNodes)
	}
	if firstMerge > rawArrays {
		t.Errorf("fallback_nodes = %v lists merge_reviews before a merge twin — the de-dup keeps the first copy, so the fixer would be seeded with the reviewers' raw form of a finding whose merged form exists", spec.FallbackNodes)
	}
	for _, twin := range []string{"converge", "converge_gpt"} {
		if !slices.Contains(spec.FallbackNodes, twin) {
			t.Errorf("fallback_nodes = %v omits %q — the family it merges would fall straight through to the raw arrays", spec.FallbackNodes, twin)
		}
	}
}

// The UNTRUSTED INPUT BOUNDARY paragraph tells the model what it holds, and
// both acting prompts of this bundle are SHARED by nodes on two different
// routes — `review_system` by the four reviewer slots, `emit_system` by the
// merge twins. A surface claim true of one route reads, on the other, as a
// capability the model does not have, or hides one it does, which is the
// paragraph's whole subject.
//
// What this guard holds is the paragraphs' SUBJECT, not their words: per
// member node, the surface its own route declares —
//
//   - a claw route is bounded by the list it declares and holds a shell only
//     where the list names one (model.assembleEffectiveTools);
//   - a CLI route declaring no list runs the backend's full native roster,
//     shell included (docs/agents/bots/untrusted-input.md);
//   - a node whose `backend:` is a dial that a deployment can point at claw
//     (`${ITERION_VIBE_BACKEND_*:-claude_code}`) is judged at its DEFAULT
//     resolution like any other CLI route, PLUS by the per-node floor
//     TestReviewPRReviewerSlotsDeclareAClawSurface, which holds the four
//     reviewer slots to a shell-naming list: on claw that list is the whole
//     surface, and a listless reviewer reviewed nothing on the pool
//     (SocialGouv/iterion#2705). `converge` is the deliberate exception the
//     family selector routes around — the claw merge is `converge_gpt`;
//
// — and the class: every node reading either prompt is classified here, so
// one added to a shared paragraph is decided instead of inheriting a sentence
// about another route, and a route that moves under a member reddens on the
// shell verdict written down for it.
//
// The PROSE ITSELF IS UNGUARDED, deliberately. Every guard of this package
// that opens a boundary paragraph reads its STRUCTURE — the marker is present
// (TestCatalogUntrustedInputBoundaryOnActingPrompts), nothing interpolates
// inside it (TestUntrustedInputBoundaryParagraphInterpolatesNothing) — and
// none reads what it claims about tools, because a predicate over what a
// sentence says models the sentence rather than the engine and converges on
// nothing. Restoring either paragraph's pre-0.9.16 wording (the claw slots
// described as claude_code with `bash`; the merge told it holds the full
// native toolset) leaves this guard and the boundary class green. Editing a
// boundary paragraph therefore means re-reading the route table above by
// hand; what the guard buys is that the table cannot drift away from the
// engine underneath it.
func TestReviewPrSharedPromptsStateEachRoutesSurface(t *testing.T) {
	wf := reviewPrWorkflow(t)
	// What the paragraph each node reads claims. `review_system`: every
	// reviewer slot holds a shell. `emit_system`: the merge step writes its
	// report and files board issues, and holds a shell on the claude route
	// alone.
	holdsShell := map[string]bool{
		"reviewer_claude": true, "reviewer_claude_glance": true,
		"reviewer_gpt": true, "reviewer_gpt_glance": true,
		"converge": true, "converge_gpt": false,
	}
	// Slots whose backend dial can take claw on some deployment: on claw the
	// declared list IS the whole surface, so these MUST declare one naming
	// the shell, and on the CLI default the same list narrows the roster by
	// design (SocialGouv/iterion#2705 — the claude twins shipped listless
	// and reviewed nothing on the pool). converge is the deliberate
	// exception: the claw merge is converge_gpt, a node of its own.
	wantsList := map[string]bool{
		"reviewer_claude": true, "reviewer_claude_glance": true,
		"reviewer_gpt": true, "reviewer_gpt_glance": true,
	}
	shared := map[string]bool{"review_system": true, "emit_system": true}
	unset := func(string) string { return "" }

	for id, node := range wf.Nodes {
		llm, ok := node.(ir.LLMNode)
		if !ok {
			continue
		}
		system := llm.GetLLMFields().SystemPrompt
		if !shared[system] {
			continue
		}
		if _, classified := holdsShell[id]; !classified {
			t.Errorf("%q reads the shared %q paragraph, which states what the node holds, and has no entry here — decide its surface and write it down", id, system)
		}
	}
	for id, wantShell := range holdsShell {
		llm, ok := wf.Nodes[id].(ir.LLMNode)
		if !ok {
			t.Errorf("%s is %T, want an LLM node", id, wf.Nodes[id])
			continue
		}
		tools := llm.GetTools()
		shell := false
		for _, declared := range tools {
			if toolcatalog.CanonicalToolName(declared) == "bash" {
				shell = true
			}
		}
		switch {
		case ir.ExpandWithDefault(llm.GetLLMFields().Backend, unset) == delegate.BackendClaw:
			if len(tools) == 0 {
				t.Errorf("%s runs on claw and declares no tools — the list IS the whole surface there, so the node holds none of what its own paragraph names", id)
			}
		case len(tools) > 0 && !wantsList[id]:
			t.Errorf("%s declares %v on a CLI route — its paragraph reads an undeclared list as the full native roster, so the claim and the surface part ways", id, tools)
		default:
			// No list on a CLI backend: the backend's whole roster.
			shell = true
		}
		if shell != wantShell {
			t.Errorf("%s holds a shell = %v, want %v — the UNTRUSTED INPUT BOUNDARY paragraph it reads says the other thing, and that paragraph exists to be true about the route the model is on", id, shell, wantShell)
		}
	}
}

// TestReviewPrTheEmitBackendDialStopsAtTheCLIBackends holds the claude-family
// merge step's `backend:` dial to the routes the engine can actually serve it
// on — the bound the node's own `backend:` note and the README's mono-GPT
// section state.
//
// `converge` declares no `tools:`. A CLI backend reads an empty list as "no
// restriction" and keeps its whole native roster; claw resolves its ToolDefs
// from that list alone, and every capability append is gated on an
// already-restricted list (model.assembleEffectiveTools). So the one value
// this dial cannot take is `claw`: it leaves the merge holding nothing — no
// report file, no board issue, and its `capabilities:` grant inert. The claw
// merge is `converge_gpt`, which declares the list; `mono_family=gpt` selects
// it.
//
// Asked of the engine, per route, never re-derived: the two calls this
// package's capability-surface class guard makes
// (EffectiveToolNamesOnRoutes, and delegate.BoardToolsFor — the function the
// append itself calls), plus delegate.DefaultRegistry, which is what makes a
// name a CLI backend. The prose stating this is not parsed here; what is
// pinned is the engine answer it rests on, so a change that makes the claw
// route serve the grant, or stops a CLI route serving it, reddens and both
// notes are re-read.
func TestReviewPrTheEmitBackendDialStopsAtTheCLIBackends(t *testing.T) {
	const dial = "ITERION_VIBE_BACKEND_EMIT"
	// The subject: an operator-settable dial whose declared default is a CLI
	// backend. A literal `backend:` here would put the node out of this
	// class, and the notes with it.
	t.Setenv(dial, "")
	declared := reviewPrWorkflow(t).Nodes["converge"].(ir.LLMNode).GetLLMFields().Backend
	if !strings.Contains(declared, "${"+dial) {
		t.Fatalf("converge declares backend %q — this guard, and the notes it certifies, are about the %s dial", declared, dial)
	}

	// claw is not in the backend registry: it is the model executor's
	// in-process tool loop, which is why an empty `tools:` list means
	// "nothing" there and "no restriction" on the names below.
	reg := delegate.DefaultRegistry(nil)
	if _, err := reg.Resolve(delegate.BackendClaw); err == nil {
		t.Errorf("%s resolves in the backend registry — this guard reads it as the one route whose surface is the declared list alone", delegate.BackendClaw)
	}
	// The CLI backends the node's note names, each certified by the registry
	// rather than asserted: one renamed or dropped reddens here.
	cli := []string{delegate.BackendClaudeCode, delegate.BackendCodex, delegate.BackendPi, delegate.BackendKimi, delegate.BackendGrok, delegate.BackendOpenCode}
	for _, name := range cli {
		if _, err := reg.Resolve(name); err != nil {
			t.Errorf("the merge step's note names %q as a CLI backend the dial may take: %v", name, err)
		}
	}

	owed := func(t *testing.T, id string) (string, []string, []string) {
		t.Helper()
		wf := reviewPrWorkflow(t)
		node := wf.Nodes[id]
		if node == nil {
			t.Fatalf("%s is not a node of review-pr", id)
		}
		llm, ok := node.(ir.LLMNode)
		if !ok {
			t.Fatalf("%s is %T, want an LLM node", id, node)
		}
		caps := llm.GetCapabilities()
		if caps == nil {
			caps = wf.Capabilities
		}
		if !delegate.HasBoardCapability(caps) {
			t.Fatalf("%s grants no board capability — the surface this guard measures is the one that grant owes", id)
		}
		exec := model.NewProgramExecutor(wf)
		route := exec.EffectiveBackendName(node)
		return route, delegate.BoardToolsFor(caps), exec.EffectiveToolNamesOnRoutes(node, false, []string{route})
	}

	t.Run("pointed at claw the merge holds nothing", func(t *testing.T) {
		t.Setenv(dial, delegate.BackendClaw)
		route, board, held := owed(t, "converge")
		if route != delegate.BackendClaw {
			t.Fatalf("the dial resolved %s, want %s — this arm measures the route the dial names", route, delegate.BackendClaw)
		}
		if len(held) != 0 {
			t.Errorf("converge holds %v on claw — the note says the dial leaves it with no tool there, and the README tells the pre-0.9.16 profile to drop that pin because of it", held)
		}
		for _, want := range board {
			if slices.Contains(held, want) {
				t.Errorf("converge holds %s on claw: the capability append fired on an undeclared list, and the note saying the grant is inert there is now false", want)
			}
		}
	})

	t.Run("the twin serves that route with its list declared", func(t *testing.T) {
		route, board, held := owed(t, "converge_gpt")
		if route != delegate.BackendClaw {
			t.Fatalf("converge_gpt resolves %s, want %s — it is the claw merge the claude twin's dial defers to", route, delegate.BackendClaw)
		}
		for _, want := range board {
			if !slices.Contains(held, want) {
				t.Errorf("converge_gpt misses %s on claw — the route the claude twin's note defers to no longer serves the grant", want)
			}
		}
	})

	for _, name := range cli {
		t.Run("the dial reaches "+name, func(t *testing.T) {
			t.Setenv(dial, name)
			wf := reviewPrWorkflow(t)
			node := wf.Nodes["converge"]
			if route := model.NewProgramExecutor(wf).EffectiveBackendName(node); route != name {
				t.Errorf("the dial set to %q resolved %q — the note says it moves the merge between the CLI backends", name, route)
			}
			if tools := node.(ir.LLMNode).GetTools(); len(tools) != 0 {
				t.Errorf("converge declares %v — an undeclared list is what makes a CLI route unrestricted and the claw route empty, which is the whole bound this guard states", tools)
			}
		})
	}
}
