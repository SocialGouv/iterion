package dryrun

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// `validate --exec` decides a node's `fallbacks:` routes on a FULLY SPECIFIED
// launch alone — every var the program declares carrying a value its author
// defaulted or its caller supplied. There, the dry run answers what the
// executor answers, so the verdict an operator reads before launching is the
// verdict the run takes.
//
// Where the launch leaves a var undecided, this package seeds a shape of its
// type (VarValue) and NOTHING before the launch decides the value: a verdict
// taken on that shape is a verdict about no run. So every route the launch may
// still move — one whose `backend:` is templated, one carrying a `when:` gate —
// keeps the reading its author wrote: the template is the unresolved route it
// is, and the gated route counts as one every launch takes. A route that names
// a backend outright under no gate is the program's own, and is decided here as
// on any launch.
//
// Which vars a FIELD reads decides nothing: the dry run reads no expression
// shape to admit a fan-out. The pessimism it costs is bounded by one line in
// the refusal — the vars the launch must decide, and the `--var` that decides
// them.
func TestValidateExecDecidesAFallbackRouteOnlyOnAFullySpecifiedLaunch(t *testing.T) {
	requireUnsetRouteEnv(t)
	for _, tc := range []struct {
		name   string
		src    string // the whole program, where the fields below cannot spell it
		vars   string
		node   string // the node's own `backend:`; claw when empty
		route  string // the route's `backend:`
		when   string
		clean  bool
		names  []string
		absent []string
		// decides is a launch this program is ADMITTED on, pointed at by the
		// vars the refusal names: where the pessimistic reading is the whole
		// reason the fan-out is refused, supplying them gives the verdict
		// back. Where it is not — a route off claw no launch moves — the row
		// carries none, and what `--var` ends there is the READING, asserted
		// over the corpus by
		// TestTheRefusalsClauseEndsThePessimisticReading.
		decides map[string]any
		why     string
	}{
		{
			name:  "every var decided, and a route the program resolves onto claw",
			vars:  `  fb: string = "claw"`,
			route: "{{vars.fb}}",
			clean: true,
			why:   "the launch decides every var this program declares, so the dry run reads the chain dispatch will walk: it stays on claw, where the branch's `tools:` list is its bound",
		},
		{
			name:   "every var decided, and a route the program resolves off claw",
			vars:   `  fb: string = "claude_code"`,
			route:  "{{vars.fb}}",
			clean:  false,
			names:  []string{"claude_code"},
			absent: []string{"{{vars.fb}}", "--var"},
			why:    "the route leaves claw for one the declared list does not bound, and the refusal names the route the run takes — nothing here is read pessimistically, so nothing sends the author to `--var`",
		},
		{
			name:    "a var the launch must decide, and a route whose `backend:` is templated",
			vars:    "  fb: string = \"claw\"\n  rescue: string",
			route:   "{{vars.fb}}",
			clean:   false,
			names:   []string{"{{vars.fb}}", "`rescue`", "--var rescue="},
			absent:  []string{"falls back to claw"},
			decides: map[string]any{"rescue": "on"},
			why:     "`rescue` has no value before the launch, so this launch is not the one the program runs on, and a template is a route the launch may still move: the dry run keeps the author's spelling and names the var that decides the route — deliberately, without reading which vars that field happens to read",
		},
		{
			name:    "a var the launch must decide, and a route carrying a `when:` gate",
			vars:    "  rescue: string",
			node:    "codex",
			route:   "claude_code",
			when:    "vars.rescue == 'on'",
			clean:   false,
			names:   []string{"falls back to claude_code", "`rescue`", "--var rescue="},
			absent:  []string{"still spelled as"},
			decides: map[string]any{"rescue": "off"},
			why:     "`--var rescue=on` is a launch this program allows and the engine refuses, so the gate counts the route IN — and the route's own `backend:` is a name the program decides, so the refusal names the route the run would take and says what decides whether it is taken",
		},
		{
			name:  "a var the launch must decide, and routes the program names under no gate",
			vars:  "  rescue: string",
			node:  "codex",
			route: "claw",
			clean: true,
			why:   "nothing about this node's routes is the launch's to decide — the route names its backend and no gate hides it — so an undecided var costs this fan-out nothing",
		},
		{
			name:    "a var the launch must decide, and the node's own `backend:` templated",
			src:     fanOutWithTemplatedBackend(`  b: string`),
			clean:   false,
			names:   []string{"{{vars.b}}", "`b`", "--var b="},
			absent:  []string{"runs on x"},
			decides: map[string]any{"b": "claw"},
			why:     "the node's own `backend:` is a routing field like a route's: the launch decides the template, so it stays as written — named as its author wrote it, never as the shape this package seeded — and the refusal names the `--var` that decides where the node runs",
		},
		{
			name:    "a var the launch must decide, and the routes the TOOL surface reads",
			src:     fanOutWithAutoMemoryRoute("  rescue: string", "vars.rescue == 'on'"),
			clean:   false,
			names:   []string{"write_file", "`rescue`", "--var rescue="},
			decides: map[string]any{"rescue": "off"},
			why:     "the gated route is counted IN, and on claw `auto_memory:` appends write_file to a node declaring `tools: [read_file]` — the pessimistic reading reaches the author through the TOOL surface here, and that wording owes the same `--var` as the route wordings; it is also the arm that runs FIRST, so a refusal silent here is silent for the whole cell",
		},
		{
			name:   "a var the launch must decide, and a gated route onto the node's OWN backend",
			src:    fanOutWithTwoRoutes("  rescue: string", "codex", "claude_code", "codex", "vars.rescue == 'on'"),
			clean:  false,
			names:  []string{"falls back to claude_code"},
			absent: []string{"--var", "`rescue`"},
			why:    "the gated route names the backend the node already runs on, so the reading drops it (authoredRoutes) and costs this verdict nothing: the refusal rests on the ungated route alone, and a clause sending the author to `--var` would name a reading the verdict did not take",
		},
		{
			name:   "a var the launch must decide, and a gated route onto a backend an earlier route names",
			src:    fanOutWithTwoRoutes("  rescue: string", "codex", "claude_code", "claude_code", "vars.rescue == 'on'"),
			clean:  false,
			names:  []string{"falls back to claude_code"},
			absent: []string{"--var", "`rescue`"},
			why:    "same cell read the other way: the gated route adds no backend the answer does not already carry, so the launch moves nothing about this verdict",
		},
		{
			name:  "a var the launch must decide, and an ungated route off claw beside a gated one",
			src:   fanOutWithTwoRoutes("  rescue: string", "codex", "claude_code", "pi", "vars.rescue == 'on'"),
			clean: false,
			names: []string{"falls back to claude_code", "`rescue`", "--var rescue="},
			why:   "the gated route onto pi IS one this reading counts in, so the clause is true and names the var — and the row carries no `decides`, because the reason rests on the UNGATED route off claw, which every launch takes: what `--var` ends here is the pessimistic reading, not the refusal",
		},
		{
			name:  "a var the launch must decide, and a tool an UNGATED route put on the surface",
			src:   fanOutWithAnUngatedWidenerAndAGatedRoute("  rescue: string", "vars.rescue == 'on'"),
			clean: false,
			names: []string{"write_file", "`rescue`", "--var rescue="},
			why:   "the clause is true — the verdict does count the gated route onto pi in — and it carries no `decides`, because the tool it names came from the UNGATED route onto claw: the surface arm answers for a TOOL, which is no route, so the only honest claim here is about the reading",
		},
		{
			name:  "two vars the launch must decide, and a templated route under a gate",
			vars:  "  fb: string = \"claw\"\n  escalate: string\n  rescue: string",
			route: "{{vars.fb}}",
			when:  "vars.rescue == 'on'",
			clean: false,
			names: []string{
				"{{vars.fb}}",
				"`escalate`, `rescue`",
				"--var escalate=",
				"--var rescue=",
			},
			decides: map[string]any{"escalate": "off", "rescue": "off"},
			why:     "both readings are in play and both vars are the launch's: the refusal names every var that decides the route, in one line the author can act on",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := tc.node
			if node == "" {
				node = "claw"
			}
			src := tc.src
			if src == "" {
				src = fanOutWithNodeAndRouteSpelled(tc.vars, node, tc.route, tc.when)
			}
			clean, reason := admissionVerdict(t, src, nil)
			if clean != tc.clean {
				t.Fatalf("admitted = %v, want %v: %s\n  %s", clean, tc.clean, tc.why, reason)
			}
			if tc.clean {
				return
			}
			for _, want := range tc.names {
				if !strings.Contains(reason, want) {
					t.Errorf("the refusal does not say %q, so the author cannot act on it: %s", want, reason)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(reason, absent) {
					t.Errorf("the refusal carries %q, which this program does not say: %s", absent, reason)
				}
			}
			if tc.decides == nil {
				return
			}
			if clean, reason := admissionVerdict(t, src, tc.decides); !clean {
				t.Errorf("the refusal sends the author to `--var`, and that launch is refused too: %v → %s", tc.decides, reason)
			}
		})
	}
}

// The other half of the rule, asked of the seams themselves: on a fully
// specified launch the dry run answers EXACTLY what the executor answers, for
// every node of the program and on all three seams admission reads. A reading
// of its own, however careful, is a second source of truth — and the drift it
// buys is a verdict `validate --exec` prints and `iterion run` contradicts.
func TestOnAFullySpecifiedLaunchTheDryRunAnswersWhatTheExecutorAnswers(t *testing.T) {
	requireUnsetRouteEnv(t)
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"a route the program resolves onto claw", fanOutWithNodeAndRouteSpelled(`  fb: string = "claw"`, "claw", "{{vars.fb}}", "")},
		{"a route the program resolves off claw", fanOutWithNodeAndRouteSpelled(`  fb: string = "claude_code"`, "claw", "{{vars.fb}}", "")},
		{"a gate the program's own defaults leave false", fanOutWithNodeAndRouteSpelled("  fb: string = \"claude_code\"\n  rescue: string = \"off\"", "claw", "{{vars.fb}}", "vars.rescue == 'on'")},
		{"a gate the program's own defaults leave true", fanOutWithNodeAndRouteSpelled("  fb: string = \"claude_code\"\n  rescue: string = \"on\"", "claw", "{{vars.fb}}", "vars.rescue == 'on'")},
		{"a route an env reference defaults", fanOutWithNodeAndRouteSpelled(`  fb: string = "claw"`, "claw", "${"+unsetRouteEnv+":-claw}", "")},
		{"the node's own `backend:` read from a var the program defaults", fanOutWithTemplatedBackend(`  b: string = "claw"`)},
		{"the workflow `default_backend:` read from a var the program defaults", fanOutWithTemplatedWorkflowDefault(`  d: string = "claw"`)},
		{"a program that declares no var at all", fanOutWithNodeAndRouteSpelled("  unused: string = \"x\"", "claw", "claw", "")},
		{"the tool surface's own routes", fanOutWithAutoMemoryRoute(`  rescue: string = "on"`, "vars.rescue == 'on'")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf := compileBot(t, tc.src)
			x := NewExecutor(wf, true, nil, nil)
			// The vars a pass is seeded with: the declared defaults, then the
			// launch over them — what the engine's runInitState resolves and
			// hands SetVars (the launch inputs here are nil, so the shapes
			// launchInputs invents stand in for them).
			vars := launchInputs(wf, nil, true)
			for name, v := range wf.Vars {
				if v != nil && v.HasDefault {
					if _, ok := vars[name]; !ok {
						vars[name] = v.Default
					}
				}
			}
			// Both executors read this ONE map, so a seeding that silently
			// drifts cannot redden the rows: pin the launch they describe —
			// every declared default is in it at the value the program
			// declares, or the rows are no longer about a launch the real
			// command takes.
			for name, v := range wf.Vars {
				if v == nil || !v.HasDefault {
					continue
				}
				if got, ok := vars[name]; !ok || !reflect.DeepEqual(got, v.Default) {
					t.Fatalf("declared default for %q is seeded as %v, want %v: the rows below no longer describe a launch the real command takes", name, got, v.Default)
				}
			}
			x.SetVars(vars)
			program := model.NewProgramExecutor(wf)
			program.SetVars(vars)
			for id, node := range wf.Nodes {
				if _, ok := node.(ir.LLMNode); !ok {
					continue
				}
				if got, want := x.EffectiveBackendName(node), program.EffectiveBackendName(node); got != want {
					t.Errorf("node %q: the dry run routes to %q, the executor to %q", id, got, want)
				}
				if got, want := x.EffectiveFallbackBackends(node), program.EffectiveFallbackBackends(node); !slices.Equal(got, want) {
					t.Errorf("node %q: the dry run reads routes %v, the executor %v", id, got, want)
				}
				for _, ultra := range []bool{false, true} {
					if got, want := x.EffectiveToolNames(node, ultra), program.EffectiveToolNames(node, ultra); !slices.Equal(got, want) {
						t.Errorf("node %q (ultracode=%v): the dry run reads the surface %v, the executor %v", id, ultra, got, want)
					}
				}
				if why := x.UndecidedRouteReading(node); why != "" {
					t.Errorf("node %q: the dry run reports a reading the launch must decide on a launch that decides every var: %s", id, why)
				}
			}
		})
	}
}

// hostRouteEnv is the environment name the row below routes a claw node with:
// C176 refuses every claw → CLI crossing the COMPILER can read — an authored
// name, a `${VAR:-codex}` default — so the one reading left for a pre-run
// analysis is a reference only the HOST answers.
const hostRouteEnv = "ITERION_TEST_HOST_ROUTE"

// The refusal says what the LAUNCH decides, and nothing where the launch
// decides nothing — asserted on the one wording that names a route leaving
// claw (a `tools:` list stops being that node's bound there), over one program
// read twice: gated on a var the launch must decide, and ungated.
//
// The ungated half is the guard that keeps the clause honest: the route is the
// host's to decide, no field of it is the launch's, and a refusal sending its
// author to `--var` would name a cause the verdict did not use.
func TestTheRefusalNamesTheLaunchOnlyWhereTheLaunchDecidesTheRoute(t *testing.T) {
	t.Setenv(hostRouteEnv, "codex")
	route := "${" + hostRouteEnv + "}"
	clean, reason := admissionVerdict(t, fanOutWithNodeAndRouteSpelled("  rescue: string", "claw", route, "vars.rescue == 'on'"), nil)
	if clean {
		t.Fatalf("precondition: a claw node whose route this host resolves onto codex is refused — the `tools:` list is its bound on claw alone")
	}
	for _, want := range []string{"route onto codex", "`rescue`", "--var rescue="} {
		if !strings.Contains(reason, want) {
			t.Errorf("the refusal does not say %q, so the author cannot act on it: %s", want, reason)
		}
	}
	clean, reason = admissionVerdict(t, fanOutWithNodeAndRouteSpelled("  rescue: string", "claw", route, ""), nil)
	if clean {
		t.Fatalf("precondition: the same route under no gate is refused too — what bounds the node did not change")
	}
	if strings.Contains(reason, "--var") || strings.Contains(reason, "`rescue`") {
		t.Errorf("the refusal sends the author to `--var` for a route nothing about this launch decides: %s", reason)
	}
}

// The clause a refusal carries is SELF-TERMINATING: pass the `--var` flags it
// prints and this dry run resolves the node's routing fields as the run
// resolves them, so the verdict that follows is the executor's own — and
// carries no clause, whatever it decides.
//
// Two states, and an author needs no third: a refusal WITH the clause is not
// the run's verdict, one without it is. Held as a property over a corpus rather
// than as a promise per row, because the stronger promise a row could state —
// supply them and the refusal LIFTS — is one a reading per NODE cannot keep:
// the reason beside the clause names ONE route, and the node's other routes
// decide nothing about it. Three rounds of this review loop each found another
// cell where a per-subject wording was wrong; what converges is a clause whose
// claim is about the READING, which the dry run owns outright.
//
// The vars are read out of the REFUSAL, never computed beside it: the sentence
// an author acts on is what is under test. And the clause must name EVERY var
// the program leaves undecided — a subset sends them to a launch still read
// pessimistically.
func TestTheRefusalsClauseEndsThePessimisticReading(t *testing.T) {
	requireUnsetRouteEnv(t)
	for _, tc := range []struct {
		name   string
		src    string
		clause bool
		// values is a legal launch value for every var the program leaves
		// undecided, read only for the names the refusal itself prints.
		values map[string]any
		why    string
	}{
		{
			name:   "the routes the tool surface reads, gated on an undecided var",
			src:    fanOutWithAutoMemoryRoute("  rescue: string", "vars.rescue == 'on'"),
			clause: true,
			values: map[string]any{"rescue": "off"},
			why:    "the gate counts the route in and claw's append puts write_file on the surface",
		},
		{
			name:   "an ungated route off claw beside a gated one",
			src:    fanOutWithTwoRoutes("  rescue: string", "codex", "claude_code", "pi", "vars.rescue == 'on'"),
			clause: true,
			values: map[string]any{"rescue": "off"},
			why:    "the gated route onto pi is counted in, so the reading IS pessimistic — and the launch the clause asks for leaves the fan-out refused on the ungated route, with no clause left",
		},
		{
			name:   "a tool an UNGATED route put on the surface, beside a gated route",
			src:    fanOutWithAnUngatedWidenerAndAGatedRoute("  rescue: string", "vars.rescue == 'on'"),
			clause: true,
			values: map[string]any{"rescue": "off"},
			why:    "the launch the clause asks for leaves the fan-out refused — the append is the ungated route's — and the clause is gone: the state an author reads is 'this verdict is now the run's', which is the whole promise",
		},
		{
			name:   "a gated route onto the node's own backend",
			src:    fanOutWithTwoRoutes("  rescue: string", "codex", "claude_code", "codex", "vars.rescue == 'on'"),
			clause: false,
			why:    "the reading drops that route, so no launch moves this verdict",
		},
		{
			name:   "a gated route onto a backend an earlier route already names",
			src:    fanOutWithTwoRoutes("  rescue: string", "codex", "claude_code", "claude_code", "vars.rescue == 'on'"),
			clause: false,
			why:    "the answer carries claude_code either way",
		},
		{
			name:   "a route whose `backend:` is templated",
			src:    fanOutWithNodeAndRouteSpelled("  fb: string = \"claw\"\n  rescue: string", "claw", "{{vars.fb}}", ""),
			clause: true,
			values: map[string]any{"rescue": "off"},
			why:    "an undecided var keeps the author's spelling of every templated routing field, this one included",
		},
		{
			name:   "the node's own `backend:` templated",
			src:    fanOutWithTemplatedBackend("  b: string"),
			clause: true,
			values: map[string]any{"b": "claw"},
			why:    "the same rule on the node's own field, reported through the same seam",
		},
		{
			name:   "the workflow `default_backend:` templated",
			src:    fanOutWithTemplatedWorkflowDefault("  d: string"),
			clause: true,
			values: map[string]any{"d": "claw"},
			why:    "and on the field the node inherits",
		},
		{
			name: "routes the program names under no gate",
			// The route onto claude_code is what REFUSES this pair: a route
			// onto claw would be bounded by the node's own `tools:` list and
			// the pair admitted, which is no witness for a clause.
			src:    fanOutWithNodeAndRouteSpelled("  rescue: string", "codex", "claude_code", ""),
			clause: false,
			why:    "nothing about this node's routes is the launch's, so an undecided var costs the verdict nothing and the refusal says nothing about `--var`",
		},
		{
			name:   "two undecided vars, a templated route under a gate",
			src:    fanOutWithNodeAndRouteSpelled("  fb: string = \"claw\"\n  escalate: string\n  rescue: string", "claw", "{{vars.fb}}", "vars.rescue == 'on'"),
			clause: true,
			values: map[string]any{"escalate": "off", "rescue": "off"},
			why:    "both readings are in play, and the clause names every var the launch must decide — not only the one the gate happens to read",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clean, reason := admissionVerdict(t, tc.src, nil)
			if clean {
				// Read before the clause: "refused with no clause" is what a
				// `clause: false` row states, and a CLEAN pass carries no
				// clause either — the false-clean verdict this whole surface
				// exists to prevent would satisfy the row silently.
				t.Fatalf("precondition: this row's fan-out is refused, so a clean pass says nothing about the clause: %s", tc.why)
			}
			named := varsTheRefusalNames(reason)
			if (len(named) > 0) != tc.clause {
				t.Fatalf("the refusal sends the author to supply %v, want a clause = %v: %s\n  %s", named, tc.clause, tc.why, reason)
			}
			if !tc.clause {
				return
			}
			if undecided := varsTheProgramLeavesUndecided(t, tc.src); !slices.Equal(named, undecided) {
				t.Errorf("the refusal names %v while the program leaves %v undecided: a launch missing one of them is still read pessimistically: %s", named, undecided, reason)
			}
			launch := map[string]any{}
			for _, name := range named {
				v, ok := tc.values[name]
				if !ok {
					t.Fatalf("this row supplies no launch value for %q, which the refusal sends the author to supply", name)
				}
				launch[name] = v
			}
			_, after := admissionVerdict(t, tc.src, launch)
			if rest := varsTheRefusalNames(after); len(rest) > 0 {
				t.Errorf("on the very launch the refusal asked for, it asks again for %v: the clause does not end, so the author has no way to reach the run's own verdict: %s", rest, after)
			}
		})
	}
}

// varsTheRefusalNames are the vars a refusal's clause sends the author to
// supply, read off the `--var NAME=` flags it prints, deduplicated and sorted.
// What an author has in hand is the sentence, so the sentence is what this
// surface is held to.
func varsTheRefusalNames(reason string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(reason, "--var ")[1:] {
		name, _, ok := strings.Cut(part, "=")
		if !ok || name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// varsTheProgramLeavesUndecided reads the PROGRAM, never the reading under
// test: the vars it declares with no default of their own, which only a launch
// value decides.
func varsTheProgramLeavesUndecided(t *testing.T, src string) []string {
	t.Helper()
	var out []string
	for name, v := range compileBot(t, src).Vars {
		if v == nil || v.HasDefault {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// twoEntriesOntoOneBackend is the pair whose branches carry TWO `fallbacks:`
// entries onto ONE backend, each with its own `when:` gate ("" = no gate).
// Exchanging the two gates writes the same two entries in the other order,
// which is what parts a reading resting on the entry its author wrote FIRST
// from one resting on the entries together.
func twoEntriesOntoOneBackend(varsBlock, nodeBackend, backend, firstWhen, secondWhen string) string {
	entry := func(name, when string) string {
		out := fmt.Sprintf("    %s:\n      backend: %q\n      model: \"anthropic/claude-haiku-4-5\"\n", name, backend)
		if when != "" {
			out += fmt.Sprintf("      when: %q\n", when)
		}
		return out
	}
	return fanOutOfTwoAgents(varsBlock, "", fmt.Sprintf(`  model: "claude-opus-4-7"
  backend: %q
  output: verdict
  tools: [read_file]
  fallbacks:
`, nodeBackend)+entry("alpha", firstWhen)+entry("beta", secondWhen))
}

// `fallbacks:` DECLARATION ORDER is not a fact about a program. Two entries
// onto one backend produce ONE route whichever is written first, so the verdict
// and the refusal — the clause included — must be the same text in both orders:
// what the launch decides about a route the answer carries once is a property
// of the entries together, never of the one that survived the dedup.
//
// The cell that pays for it: an ungated entry beside a gated one. The backend is
// in the answer on every launch, so no `--var` moves this verdict — and a clause
// printed in one order only is both a cause the verdict did not use and a hatch
// that provably moves nothing.
func TestTheRefusalDoesNotMoveWithTheOrderOfTwoEntriesOntoOneBackend(t *testing.T) {
	requireUnsetRouteEnv(t)
	for _, tc := range []struct {
		name       string
		vars       string
		node       string
		backend    string
		firstWhen  string
		secondWhen string
		clean      bool
		why        string
	}{
		{
			name:       "one entry gated, the other not",
			vars:       "  rescue: string",
			node:       "codex",
			backend:    "claude_code",
			secondWhen: "vars.rescue == 'on'",
			why:        "the ungated entry puts claude_code in the answer on every launch, so the gate decides nothing about this verdict",
		},
		{
			name:       "both entries gated",
			vars:       "  rescue: string\n  escalate: string",
			node:       "codex",
			backend:    "claude_code",
			firstWhen:  "vars.escalate == 'on'",
			secondWhen: "vars.rescue == 'on'",
			why:        "every entry onto claude_code rests on a gate the launch decides, so the reading IS pessimistic — in either order",
		},
		{
			name:       "both onto the node's own backend",
			vars:       "  rescue: string",
			node:       "codex",
			backend:    "codex",
			secondWhen: "vars.rescue == 'on'",
			clean:      true,
			why:        "a route onto the node's own backend is out of the answer, whichever entry names it first",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asWritten, reason := admissionVerdict(t, twoEntriesOntoOneBackend(tc.vars, tc.node, tc.backend, tc.firstWhen, tc.secondWhen), nil)
			exchanged, exchangedReason := admissionVerdict(t, twoEntriesOntoOneBackend(tc.vars, tc.node, tc.backend, tc.secondWhen, tc.firstWhen), nil)
			if asWritten != tc.clean || exchanged != tc.clean {
				t.Fatalf("admitted = %v (as written) / %v (entries exchanged), want %v: %s\n  %s\n  %s", asWritten, exchanged, tc.clean, tc.why, reason, exchangedReason)
			}
			if tc.clean {
				return
			}
			if reason != exchangedReason {
				t.Errorf("exchanging two `fallbacks:` entries onto one backend moved the refusal, so the reading rests on the order their author wrote them in: %s\n  as written: %s\n  exchanged:  %s", tc.why, reason, exchangedReason)
			}
		})
	}
}
