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

// The catalogue names ROLES, never vendors (ADR-169): a node id, a var name
// and a routing guard say what a step IS, and which backend or model serves
// it stays a routing question answered by literals and llmroute cells. The
// names are what an operator's cells, a run's published table and a fixer's
// hand-off all address, so a vendor name here is a vocabulary every
// deployment inherits.
func TestReviewPrCatalogueNamesRolesNotVendors(t *testing.T) {
	wf := reviewPrWorkflow(t)
	for _, vendor := range []string{"claude", "gpt", "anthropic", "openai", "zai", "glm", "codex"} {
		for id := range wf.Nodes {
			if strings.Contains(strings.ToLower(id), vendor) {
				t.Errorf("node %q carries the vendor name %q — the catalogue names the role (`reviewer`, `reviewer_alternate`, `converge`) and the route decides who serves it", id, vendor)
			}
		}
		for name := range wf.Vars {
			if strings.Contains(strings.ToLower(name), vendor) {
				t.Errorf("var %q carries the vendor name %q", name, vendor)
			}
		}
	}
	// `mono_family` stays DECLARED and decides nothing: a launch that still
	// sends it is admitted and ignored, where an undeclared name would be
	// refused as an unknown input before any injection could map it.
	if v := wf.Vars["mono_family"]; v == nil {
		t.Error("review-pr no longer declares mono_family — every integration pinned to it would be refused as an unknown input; it is retired, not removed, until #2699 migrates the launch vars")
	}
	if v := wf.Vars[reviewtopology.VarReviewMode]; v == nil {
		t.Fatal("review-pr no longer declares review_mode — the topology is not injectable")
	}
}

// ONE merge step, on the trunk, whatever the topology ran: `converge` reads
// `outputs.merge_reviews` (the collector that folds whichever reviewers
// fired) and is the only node feeding `pr_gate`. Two merge nodes selected by
// a family var — the #2467 shape — is what this replaces: it made the merge
// family a second routing decision nobody could see from the run's table.
func TestReviewPrHasOneMergeStepOnOneRoute(t *testing.T) {
	wf := reviewPrWorkflow(t)
	if _, ok := wf.Nodes["converge"]; !ok {
		t.Fatal("review-pr has no `converge` node")
	}
	var intoMerge, outOfMerge []string
	for _, e := range wf.Edges {
		switch {
		case e.To == "converge":
			intoMerge = append(intoMerge, e.From)
		case e.From == "converge":
			outOfMerge = append(outOfMerge, e.To)
		}
	}
	if !slices.Equal(intoMerge, []string{"merge_reviews"}) {
		t.Errorf("edges into converge: %v — the merge must read the reviewers' COLLECTOR, which is what makes one node serve a single and a paired review alike", intoMerge)
	}
	if !slices.Equal(outOfMerge, []string{"pr_gate"}) {
		t.Errorf("edges out of converge: %v — the gate is the one node the merge feeds", outOfMerge)
	}
	// Its own route, as dispatch reads it: claw on an `openai/…` model with
	// no provider hint — the one combination that resolves (the
	// auto-resolver would send it to claude_code, which does not know the
	// prefix).
	llm, ok := wf.Nodes["converge"].(ir.LLMNode)
	if !ok {
		t.Fatalf("converge is %T, want an LLM node", wf.Nodes["converge"])
	}
	f := llm.GetLLMFields()
	if got := ir.ExpandWithDefault(f.Backend, func(string) string { return "" }); got != delegate.BackendClaw {
		t.Errorf("converge's backend default resolves to %q, want %q", got, delegate.BackendClaw)
	}
	if got := ir.ExpandWithDefault(f.Model, func(string) string { return "" }); !strings.HasPrefix(got, "openai/") {
		t.Errorf("converge's model default resolves to %q — on claw the `openai/` prefix IS the route", got)
	}
	if got := strings.TrimSpace(ir.ExpandWithDefault(f.Provider, func(string) string { return "" })); got != "" {
		t.Errorf("converge carries the provider hint %q — on claw a hint describes a wire this node does not take to the credential walk", got)
	}
}

// The topology router, as the ENGINE walks it: a guarded edge that holds
// pre-empts the unconditional fallback beside it, so each launch reaches one
// reviewer (or, paired, both) and never an empty router.
func TestReviewPrTopologyReachesTheReviewersItSays(t *testing.T) {
	wf := reviewPrWorkflow(t)
	reaches := func(open ir.EdgeFilter, id string) bool {
		if wf.Nodes[id] == nil {
			return false
		}
		return wf.CanReachVia(open, func(n ir.Node) bool { return n != nil && n.NodeID() == id })
	}
	for _, tc := range []struct {
		what   string
		vars   map[string]any
		reache map[string]bool
	}{
		{
			what:   "single, guard tier: the reviewer alone",
			vars:   map[string]any{"review_mode": "single", "review_tier": "guard"},
			reache: map[string]bool{"reviewer": true, "reviewer_alternate": false, "reviewer_glance": false, "fan": false},
		},
		{
			what:   "single, glance tier: the cheaper reviewer alone",
			vars:   map[string]any{"review_mode": "single", "review_tier": "glance"},
			reache: map[string]bool{"reviewer_glance": true, "reviewer": false, "reviewer_alternate": false, "fan": false},
		},
		{
			what:   "paired: the fan-out and both reviewers",
			vars:   map[string]any{"review_mode": "paired", "review_tier": "guard"},
			reache: map[string]bool{"fan": true, "reviewer": true, "reviewer_alternate": true},
		},
		{
			what:   "audit forces the fan-out whatever the mode says",
			vars:   map[string]any{"review_mode": "single", "review_tier": "audit"},
			reache: map[string]bool{"fan": true, "reviewer": true, "reviewer_alternate": true},
		},
		{
			// The retired spellings still travel in integration launch
			// vars; reviewtopology maps them, but a map that skipped the
			// injection must not leave the router empty.
			what:   "an unknown mode degrades to the cheap path",
			vars:   map[string]any{"review_mode": "dual", "review_tier": "guard"},
			reache: map[string]bool{"reviewer": true, "reviewer_alternate": false},
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			open := wf.LaunchVarEdgeFilter(tc.vars)
			for id, want := range tc.reache {
				if got := reaches(open, id); got != want {
					t.Errorf("reaches(%s) = %v, want %v", id, got, want)
				}
			}
			if !reaches(open, "converge") || !reaches(open, "pr_gate") {
				t.Error("every topology must reach the merge step and the gate")
			}
		})
	}
}

// A merge output is read through `pr_gate` ALONE, which carries the payload
// and the engine telemetry forward under one set of names. A reader that
// reaches for `outputs.converge.` directly binds the run's report to one
// node's identity instead of to the step — the shape that, with two merge
// nodes, described a step that had not run.
//
// Counted rather than enumerated, so the next reader joins the guard by
// existing. Comment lines are excluded: naming the reference in prose is
// free, spelling it in a template is the claim.
func TestReviewPrEveryMergeOutputReaderGoesThroughPrGate(t *testing.T) {
	wf := reviewPrWorkflow(t)
	const ref = "outputs.converge."

	onTheGateEdges := 0
	for _, e := range wf.Edges {
		if e.To != "pr_gate" {
			continue
		}
		if e.From != "converge" {
			t.Errorf("pr_gate also fires from %q — every incoming edge must carry the merge payload, or a reader downstream reads the empty string", e.From)
		}
		for _, dm := range e.With {
			onTheGateEdges += strings.Count(dm.Raw, ref)
		}
	}
	if onTheGateEdges == 0 {
		t.Fatalf("no edge into pr_gate reads `%s` — the merge's payload and telemetry reach nothing", ref)
	}

	inTheBundle := 0
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
			inTheBundle += strings.Count(line, ref)
		}
	}
	if inTheBundle != onTheGateEdges {
		t.Errorf("the bundle reads `%s` %d time(s) but only %d of them are on the edge into pr_gate — thread the value through pr_gate's input instead",
			ref, inTheBundle, onTheGateEdges)
	}
}

// The PAYLOAD half: `pr_gate_input` declares no optional field, and C308 only
// warns when NO incoming edge maps a field a body reads, so a single edge
// that forgets one is silent — a published review with no findings, i.e.
// approval by omission on this repo's own required gate. The fields the
// merge step PRODUCES are owed from the merge's own output (a field sourced
// from a common ancestor would publish a reviewer's raw array while
// `total_findings` comes from the merge); everything else must at least be
// mapped.
func TestReviewPrTheGateEdgeMapsTheWholePayload(t *testing.T) {
	wf := reviewPrWorkflow(t)
	schema := wf.Schemas["pr_gate_input"]
	if schema == nil || len(schema.Fields) == 0 {
		t.Fatal("pr_gate_input declares no field — this guard lost its subject")
	}
	mapped := map[string]string{}
	for _, e := range wf.Edges {
		if e.To != "pr_gate" {
			continue
		}
		if e.From != "converge" {
			t.Errorf("pr_gate fires from %q — this guard reads the merge step's single edge", e.From)
		}
		for _, dm := range e.With {
			mapped[dm.Key] = strings.TrimSpace(dm.Raw)
		}
	}
	llm, ok := wf.Nodes["converge"].(ir.LLMNode)
	if !ok {
		t.Fatalf("converge is %T, want an LLM node", wf.Nodes["converge"])
	}
	produced := map[string]bool{}
	if out := wf.Schemas[llm.GetSchemaFields().OutputSchema]; out != nil {
		for _, f := range out.Fields {
			if f != nil {
				produced[f.Name] = true
			}
		}
	}
	if len(produced) == 0 {
		t.Fatal("the merge step declares no output schema — the source half of this guard lost its subject")
	}
	for _, f := range schema.Fields {
		if f == nil {
			continue
		}
		got, ok := mapped[f.Name]
		if !ok {
			t.Errorf("pr_gate_input.%s is mapped by no edge — it reads empty, and pr_gate is both what publish_review posts and what the hand-off resolves (manifest `produces`)", f.Name)
			continue
		}
		if produced[f.Name] {
			if want := "{{outputs.converge." + f.Name + "}}"; got != want {
				t.Errorf("converge -> pr_gate maps pr_gate_input.%s to %q, want %q — the merge step produces that field, so the gate owes the merged copy and not a common ancestor's", f.Name, got, want)
			}
		}
	}
	for key := range mapped {
		if !slices.ContainsFunc(schema.Fields, func(f *ir.SchemaField) bool { return f != nil && f.Name == key }) {
			t.Errorf("the edge into pr_gate maps %q, which pr_gate_input does not declare — the value arrives nowhere", key)
		}
	}
}

// The hand-off declaration, as the resolver reads it: `pr_gate` is the
// primary because it is the node every path crosses, and the fallback list is
// a UNION over every node it names, de-duped by anchor keeping the FIRST copy
// (pkg/server.loadReviewFindings). So the ORDER is the claim this file owes:
// the merge step ahead of `merge_reviews` is what gives a finding both
// reviewers raised its merged form — bumped severity, cross-confirmed flag —
// instead of a reviewer's raw copy.
func TestReviewPrHandsOffThroughTheMergeNode(t *testing.T) {
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
		t.Fatal("review-pr declares no `produces: kind: review` — a held review would seed no fixer")
	}
	if spec.Node != "pr_gate" {
		t.Errorf("the review is produced by %q — every path crosses pr_gate, so naming another node hands the fixer what that node saw", spec.Node)
	}
	merge, rawArrays := slices.Index(spec.FallbackNodes, "converge"), slices.Index(spec.FallbackNodes, "merge_reviews")
	if merge < 0 || rawArrays < 0 {
		t.Fatalf("fallback_nodes = %v — it owes both the merge step (the merged copy) and merge_reviews (the raw arrays the merge's prose degradation needs)", spec.FallbackNodes)
	}
	if merge > rawArrays {
		t.Errorf("fallback_nodes = %v lists merge_reviews before the merge step — the de-dup keeps the first copy, so the fixer would be seeded with the reviewers' raw form of a finding whose merged form exists", spec.FallbackNodes)
	}
}

// The UNTRUSTED INPUT BOUNDARY paragraph tells the model what it holds, and
// this bundle shares two prompts across routes — `review_system` by the
// reviewer slots, `emit_system` by the merge. A surface claim true of one
// route reads, on the other, as a capability the model does not have or
// hides one it does, which is the paragraph's whole subject.
//
// What this guard holds is the paragraphs' SUBJECT, not their words: per
// member node, the surface its own route declares. A claw route is bounded by
// the list it declares; a CLI route declaring no list runs the backend's full
// native roster, shell included. Every node reading either prompt is
// classified here, so one added to a shared paragraph is decided instead of
// inheriting a sentence about another route.
func TestReviewPrSharedPromptsStateEachRoutesSurface(t *testing.T) {
	wf := reviewPrWorkflow(t)
	holdsShell := map[string]bool{
		"reviewer_alternate": true, "reviewer": true, "reviewer_glance": true,
		"converge": false,
	}
	shared := map[string]bool{"review_system": true, "emit_system": true}
	unset := func(string) string { return "" }

	for id, node := range wf.Nodes {
		llm, ok := node.(ir.LLMNode)
		if !ok {
			continue
		}
		if !shared[llm.GetLLMFields().SystemPrompt] {
			continue
		}
		if _, classified := holdsShell[id]; !classified {
			t.Errorf("%q reads a shared boundary paragraph, which states what the node holds, and has no entry here — decide its surface and write it down", id)
		}
	}
	for id, wantShell := range holdsShell {
		llm, ok := wf.Nodes[id].(ir.LLMNode)
		if !ok {
			t.Errorf("%s is %T, want an LLM node", id, wf.Nodes[id])
			continue
		}
		tools := llm.GetTools()
		shell := slices.ContainsFunc(tools, func(d string) bool { return toolcatalog.CanonicalToolName(d) == "bash" })
		onClaw := ir.ExpandWithDefault(llm.GetLLMFields().Backend, unset) == delegate.BackendClaw
		if onClaw && len(tools) == 0 {
			t.Errorf("%s runs on claw and declares no tools — the list IS the whole surface there, so the node holds none of what its own paragraph names", id)
		}
		if !onClaw && len(tools) == 0 {
			// No list on a CLI backend: the backend's whole roster.
			shell = true
		}
		if shell != wantShell {
			t.Errorf("%s holds a shell = %v, want %v — the UNTRUSTED INPUT BOUNDARY paragraph it reads says the other thing", id, shell, wantShell)
		}
	}
}

// The merge step's `backend:` dial serves each backend it names: on claw the
// declared list IS the node's whole surface and the board capability's append
// rides it (model.assembleEffectiveTools), so a claw merge must hold the
// three file tools AND its grant; on a CLI backend the same list narrows the
// roster instead of leaving the merge listless.
//
// Asked of the engine, per route, never re-derived: EffectiveBackendName /
// EffectiveToolNamesOnRoutes, and delegate.BoardToolsFor — the function the
// append itself calls. A change that stops a route serving the declared
// surface or the grant reddens here.
func TestReviewPrTheEmitBackendDialServesEachBackendItNames(t *testing.T) {
	const dial = "ITERION_VIBE_BACKEND_EMIT_GPT"
	t.Setenv(dial, "")
	declared := reviewPrWorkflow(t).Nodes["converge"].(ir.LLMNode).GetLLMFields().Backend
	if !strings.Contains(declared, "${"+dial) {
		t.Fatalf("converge declares backend %q — this guard, and the notes it certifies, are about the %s dial", declared, dial)
	}

	reg := delegate.DefaultRegistry(nil)
	if _, err := reg.Resolve(delegate.BackendClaw); err == nil {
		t.Errorf("%s resolves in the backend registry — this guard reads it as the route whose surface is the declared list alone", delegate.BackendClaw)
	}
	cli := []string{delegate.BackendClaudeCode, delegate.BackendCodex, delegate.BackendPi, delegate.BackendKimi, delegate.BackendGrok, delegate.BackendOpenCode}
	for _, name := range cli {
		if _, err := reg.Resolve(name); err != nil {
			t.Errorf("the merge step's dial may take %q: %v", name, err)
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

	t.Run("on claw the merge holds its declared surface and its grant", func(t *testing.T) {
		t.Setenv(dial, delegate.BackendClaw)
		route, board, held := owed(t, "converge")
		if route != delegate.BackendClaw {
			t.Fatalf("the dial resolved %s, want %s — this arm measures the route the dial names", route, delegate.BackendClaw)
		}
		for _, want := range append([]string{"read_file", "write_file", "glob"}, board...) {
			if !slices.Contains(held, want) {
				t.Errorf("converge misses %s on claw (%v) — the route no longer serves the merge's own surface or its grant", want, held)
			}
		}
	})

	for _, name := range cli {
		t.Run("the dial reaches "+name, func(t *testing.T) {
			t.Setenv(dial, name)
			wf := reviewPrWorkflow(t)
			node := wf.Nodes["converge"]
			if route := model.NewProgramExecutor(wf).EffectiveBackendName(node); route != name {
				t.Errorf("the dial set to %q resolved %q — the note says it moves the merge between the backends", name, route)
			}
			for _, want := range []string{"read_file", "write_file", "glob"} {
				if !slices.Contains(node.(ir.LLMNode).GetTools(), want) {
					t.Errorf("converge declares %q: false — without the three file tools a CLI route narrows past the merge's own work", want)
				}
			}
		})
	}
}
