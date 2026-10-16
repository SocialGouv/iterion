package dryrun

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// `validate --exec` runs the engine's parallel-branch admission over the dry
// run's own executor, so what that executor answers about a node's
// `fallbacks:` routes decides a verdict an operator reads before launching.
// The guarantee this surface owes is agreement with the run it simulates: the
// dry run reads a route the way dispatch will, where the launch it judges
// decides every var — and keeps the author's own reading of a route that
// launch could still move, since the value it would read there is one it
// INVENTED and the launch may set it to anything.
//
// The rows are run through Run(), not through the seam: the method agreeing
// with dispatch says nothing about the verdict `validate --exec` prints, which
// is what a lying static gate costs.
func fanOutWithOneRoute(varsBlock, when string) string {
	return fanOutWithOneRouteSpelled(varsBlock, "{{vars.fb}}", when)
}

// fanOutWithOneRouteSpelled is the same pair with the route's `backend:`
// written as a row needs it: a `{{vars.…}}` template, an env reference, a
// name. The three spellings are read by three different arms, and only the
// program a row declares decides which.
func fanOutWithOneRouteSpelled(varsBlock, routeBackend, when string) string {
	return fanOutWithNodeAndRouteSpelled(varsBlock, "claw", routeBackend, when)
}

// fanOutWithNodeAndRouteSpelled takes the NODE's backend as well, for a row
// whose route must RESOLVE off claw: the compiler refuses a claw node routed to
// a CLI backend by any spelling it can read (C176, a declared `tools:` list
// means nothing there), so such a row declares its node on a CLI backend and
// the crossing it reads is CLI → CLI.
func fanOutWithNodeAndRouteSpelled(varsBlock, nodeBackend, routeBackend, when string) string {
	gate := ""
	if when != "" {
		gate = fmt.Sprintf("\n      when: %q", when)
	}
	return fanOutOfTwoAgents(varsBlock, "", fmt.Sprintf(`  model: "claude-opus-4-7"
  backend: %q
  output: verdict
  tools: [read_file]
  fallbacks:
    alt:
      backend: %q
      model: "anthropic/claude-haiku-4-5"`, nodeBackend, routeBackend)+gate+"\n")
}

// fanOutWithTwoRoutes is the pair whose branches carry TWO `fallbacks:` routes:
// the first the program names outright under no gate, the second under a
// `when:` gate. It is the shape that parts a reading of what the launch decides
// per NODE from one per ROUTE — the refusal's reason rests on the first route,
// which no launch moves, while only the second is the launch's to decide.
func fanOutWithTwoRoutes(varsBlock, nodeBackend, ungated, gated, when string) string {
	return fanOutOfTwoAgents(varsBlock, "", fmt.Sprintf(`  model: "claude-opus-4-7"
  backend: %q
  output: verdict
  tools: [read_file]
  fallbacks:
    alt:
      backend: %q
      model: "anthropic/claude-haiku-4-5"
    rescue:
      backend: %q
      model: "anthropic/claude-haiku-4-5"
      when: %q
`, nodeBackend, ungated, gated, when))
}

// fanOutWithAnUngatedWidenerAndAGatedRoute is the pair whose TOOL surface is
// widened by a route no launch moves — claw's `auto_memory:` append, reached
// through an ungated route — while a SECOND route is gated on a var the launch
// decides. It is the cell that parts a clause worded per SUBJECT from one
// worded about the reading: the subject the surface arm names is a tool, not a
// route, so a per-subject clause has nothing finer than the node to answer for
// and would promise a verdict the launch does not give back.
func fanOutWithAnUngatedWidenerAndAGatedRoute(varsBlock, when string) string {
	return fanOutOfTwoAgents(varsBlock, "", fmt.Sprintf(`  model: "claude-opus-4-7"
  backend: "codex"
  output: verdict
  tools: [read_file]
  auto_memory: on
  fallbacks:
    alt:
      backend: "claw"
      model: "anthropic/claude-haiku-4-5"
    rescue:
      backend: "pi"
      model: "anthropic/claude-haiku-4-5"
      when: %q
`, when))
}

// fanOutOfTwoAgents is the program every row of this surface is judged on: two
// agents the router fans out to over ONE worktree, so the engine's
// parallel-branch admission decides whether the program may run at all. The
// branch body is the fixture's variable — one `fallbacks:` route, the node's
// own `backend:`, a runtime opt-in — and the workflow block takes what a row
// needs there (`default_backend:`).
//
// One fixture for every row, so a verdict cannot come from a difference in the
// program that a row did not mean to declare.
func fanOutOfTwoAgents(varsBlock, workflowFields, branchFields string) string {
	branch := "agent %s:\n" + branchFields
	return `schema verdict:
  ok: bool

vars:
` + varsBlock + `

agent survey:
  model: "claude-opus-4-7"
  output: verdict

router split:
  mode: fan_out_all

` + fmt.Sprintf(branch, "b1") + "\n" + fmt.Sprintf(branch, "b2") + `
judge join:
  model: "claude-opus-4-7"
  output: verdict
  await: wait_all

workflow fan:
` + workflowFields + `  worktree: none
  sandbox: none
  entry: survey
  budget:
    max_iterations: 20
  survey -> split
  split -> b1
  split -> b2
  b1 -> join
  b2 -> join
  join -> done
`
}

func TestTheDryRunsAdmissionReadsARouteItsOwnProgramAnswers(t *testing.T) {
	requireUnsetRouteEnv(t)
	for _, tc := range []struct {
		name   string
		vars   string
		node   string // the node's own `backend:`; claw when empty
		route  string // the route's `backend:`; the `{{vars.fb}}` template when empty
		when   string
		clean  bool
		names  string // the refusal names this
		absent string // and never this
		why    string
	}{
		{
			name:  "a route the program's own default resolves to claw",
			vars:  `  fb: string = "claw"`,
			clean: true,
			why:   "the chain stays on claw, where the branch's `tools:` list is its bound — the reading #1778 removed refused this fan-out on the spelling alone",
		},
		{
			name:  "a route the program's own default resolves off claw",
			vars:  `  fb: string = "claude_code"`,
			clean: false,
			names: "claude_code",
			why:   "the route leaves claw for one the declared list does not bound, and the reason names what the program resolves to",
		},
		{
			name:   "a route resting on a var the dry run invented",
			vars:   `  fb: string`,
			clean:  false,
			names:  "{{vars.fb}}",
			absent: "falls back to x",
			why:    "the dry run's value for an unsupplied var is its own shape, not the program's: the launch may put any backend there, so the route is read as the unresolved one it is, in the author's own spelling",
		},
		{
			name:   "a gate the launch decides",
			vars:   "  fb: string = \"claude_code\"\n  rescue: string",
			when:   "vars.rescue == 'on'",
			clean:  false,
			names:  "{{vars.fb}}",
			absent: "falls back to claude_code",
			why:    "`--var rescue=on` is a launch this program allows and the engine refuses, so a dry run that read its invented shape through the gate and dropped the route would report CLEAN on a fan-out the run kills at the router — and the launch this verdict is taken on decides nothing about this node's routes, so the route stays the spelling its author wrote",
		},
		{
			name:   "that same gate written as a subscript",
			vars:   "  fb: string = \"claude_code\"\n  rescue: string",
			when:   "vars['rescue'] == 'on'",
			clean:  false,
			names:  "{{vars.fb}}",
			absent: "falls back to claude_code",
			why:    "the row above, one spelling further: no expression shape decides admission, so the subscript — which reads the vars namespace whole — is the same cell as the dotted path, and a reading that parsed the two apart was wrong on one of them twice",
		},
		{
			name:   "that same gate over the middle of an enum var's domain",
			vars:   "  fb: string = \"claude_code\"\n  rescue: string [enum: \"off\", \"maybe\", \"on\"]",
			when:   "vars.rescue == 'maybe'",
			clean:  false,
			names:  "{{vars.fb}}",
			absent: "falls back to claude_code",
			why:    "the two passes take the two ENDS of a declared domain, never its middle, so an enum var is no exception: `--var rescue=maybe` is a launch this program allows and the engine refuses",
		},
		{
			name:  "a gate the program's own defaults leave false",
			vars:  "  fb: string = \"claude_code\"\n  rescue: string = \"off\"",
			when:  "vars.rescue == 'on'",
			clean: true,
			why:   "the gate reads values the program itself declares, so the dry run knows the chain this program walks carries no route off claw",
		},
		{
			name:   "a route the program names onto claw, under a gate the launch decides",
			vars:   "  fb: string = \"claw\"\n  rescue: string",
			when:   "vars.rescue == 'on'",
			clean:  false,
			names:  "`rescue`",
			absent: "falls back to claw",
			why:    "the deliberate cost of deciding routes on a fully specified launch alone: this one leaves a var undecided, so the template stays as written although the program's own default resolves it to claw, and the refusal names the var and the `--var` that decides it. `iterion run` admits every launch of this program (`rescue` on, off, absent), and `iterion validate --exec --var rescue=off` reads it clean",
		},
		{
			name:   "that same route under a gate read by subscript",
			vars:   "  fb: string = \"claw\"\n  rescue: string",
			when:   "vars['rescue'] == 'on'",
			clean:  false,
			names:  "`rescue`",
			absent: "falls back to claw",
			why:    "the spelling of the gate moves nothing, here as above",
		},
		{
			name:  "a route an env reference defaults to claw, under a gate the program does not decide",
			vars:  "  rescue: string",
			route: "${" + unsetRouteEnv + ":-claw}",
			when:  "vars.rescue == 'on'",
			clean: true,
			why:   "the route's field reads no var at all: this dry run resolves it exactly as dispatch does, to the default its author wrote",
		},
		{
			name:   "a route an env reference defaults off claw, under that same gate",
			vars:   "  rescue: string",
			node:   "codex",
			route:  "${" + unsetRouteEnv + ":-claude_code}",
			when:   "vars.rescue == 'on'",
			clean:  false,
			names:  "falls back to claude_code",
			absent: "${",
			why:    "the gate's pessimism keeps the route, and the refusal names the backend it resolves to: quoting the `${VAR:-default}` spelling as if it were a backend names a route no registry has. The node is codex because the compiler refuses the same crossing off a CLAW node (C176) for every spelling it can read, which is what leaves the cell to this reading at all",
		},
		{
			name:  "a route whose env reference resolves to nothing, under that same gate",
			vars:  "  rescue: string",
			route: "${" + unsetRouteEnv + "}",
			when:  "vars.rescue == 'on'",
			clean: true,
			why:   "a route that resolves to no backend inherits the node's, which is what dispatch does with it, so it adds none to read — and the node's own is claw",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := tc.route
			if route == "" {
				route = "{{vars.fb}}"
			}
			node := tc.node
			if node == "" {
				node = "claw"
			}
			wf := compileBot(t, fanOutWithNodeAndRouteSpelled(tc.vars, node, route, tc.when))
			r, err := Run(context.Background(), wf, Options{})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			var reasons []string
			for _, p := range r.Passes {
				reasons = append(reasons, p.Failure)
			}
			got := strings.Join(reasons, " | ")
			if r.Clean() != tc.clean {
				t.Fatalf("clean = %v, want %v: %s\n  %s", r.Clean(), tc.clean, tc.why, got)
			}
			if tc.clean {
				return
			}
			if !strings.Contains(got, "WORKSPACE_SAFETY") {
				t.Fatalf("the dry run is unclean for another reason than admission: %s", got)
			}
			if tc.names != "" && !strings.Contains(got, tc.names) {
				t.Errorf("the refusal does not name %q, so the author cannot act on it: %s", tc.names, got)
			}
			if tc.absent != "" && strings.Contains(got, tc.absent) {
				t.Errorf("the refusal carries %q, which the program does not say: %s", tc.absent, got)
			}
		})
	}
}

// On a launch that decides every var, each route is read on its own account: a
// route the PROGRAM gates off is out of the chain whatever the other routes
// carry, which is the reading dispatch takes (resolveChain, fallbackWhenActive)
// — and the one a node read at the pessimism of its loosest gate loses.
func TestARouteTheProgramGatesOffIsOutOfTheChainOnALaunchItDecides(t *testing.T) {
	src := twoGatedRoutes("  rescue: string = \"off\"\n  escalate: string = \"off\"")
	clean, reason := admissionVerdict(t, src, nil)
	if !clean {
		t.Errorf("this fan-out is refused although the only route that leaves claw is one the program's own `escalate: \"off\"` gates off, and `iterion run` admits every launch of it: %s", reason)
	}
	// The same program with that var left to the launch: the gate is then one
	// no value of this program's decides, the route counts, and the refusal
	// names the var that decides it — the row above is not a clean verdict
	// taken for the wrong reason.
	clean, reason = admissionVerdict(t, twoGatedRoutes("  rescue: string = \"off\"\n  escalate: string"), nil)
	if clean {
		t.Errorf("`--var escalate=on` is a launch this program allows and the engine refuses, so a CLEAN verdict here is the lie this pessimism exists to prevent")
	}
	for _, want := range []string{"{{vars.cli}}", "`escalate`", "--var escalate="} {
		if !strings.Contains(reason, want) {
			t.Errorf("the refusal does not say %q, so the author cannot act on it: %s", want, reason)
		}
	}
}

// twoGatedRoutes is the pair whose branches carry TWO gated routes: one that
// leaves claw, under a gate the row's vars block decides or not, and one that
// stays on claw under a gate of its own — so only the first one's reading moves
// the verdict. The route off claw is spelled as a template because the compiler
// refuses every claw → CLI crossing it can read (C176).
func twoGatedRoutes(varsBlock string) string {
	return fanOutOfTwoAgents(varsBlock+"\n  cli: string = \"claude_code\"", "", `  model: "claude-opus-4-7"
  backend: "claw"
  output: verdict
  tools: [read_file]
  fallbacks:
    escalated:
      backend: "{{vars.cli}}"
      model: "anthropic/claude-haiku-4-5"
      when: "vars.escalate == 'on'"
    alt:
      backend: "claw"
      model: "anthropic/claude-haiku-4-5"
      when: "vars.rescue == 'on'"
`)
}

// The reading above rests on a route's `backend:` and its `when:` gate, and on
// neither its `model:` nor its `provider:`: the chain collapses two elements
// only where their backends are equal as well, so a template in either of
// those cannot move a backend into or out of the answer — while reading them
// would take the pessimistic side on every program that pins a route's model
// from a var.
func TestAModelTemplateDoesNotMoveARouteIntoOrOutOfTheAnswer(t *testing.T) {
	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "a"},
		LLMFields: ir.LLMFields{Backend: "claw"},
		Tools:     []string{"read_file"},
		Fallbacks: []ir.Fallback{
			{Name: "templated-model", Backend: "claude_code", Model: "{{vars.m}}"},
			{Name: "pinned-model", Backend: "claude_code", Model: "anthropic/claude-haiku-4-5"},
		},
	}
	wf := &ir.Workflow{
		Nodes: map[string]ir.Node{"a": node},
		Vars:  map[string]*ir.Var{"m": {Name: "m", Type: ir.VarString}},
	}
	for _, tc := range []struct {
		name  string
		given map[string]any
		vars  map[string]any
	}{
		{"the model var is the dry run's own shape", nil, map[string]any{"m": "x"}},
		{"the model var is the caller's", map[string]any{"m": "anthropic/claude-haiku-4-5"}, map[string]any{"m": "anthropic/claude-haiku-4-5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := &Executor{wf: wf, given: tc.given}
			x.SetVars(tc.vars)
			got := x.EffectiveFallbackBackends(node)
			if len(got) != 1 || got[0] != "claude_code" {
				t.Errorf("EffectiveFallbackBackends = %v, want [claude_code]: a `model:` template changes which ELEMENTS the chain collapses, never the set of backends it may run on", got)
			}
		})
	}
}

// The arms of the authored reading that no compiled program reaches, and the
// one distinction this package keeps between its two readings of "made up".
func TestWhatTheAuthoredReadingOfARouteAnswers(t *testing.T) {
	strVar := map[string]*ir.Var{"m": {Name: "m", Type: ir.VarString}}
	node := func(fbs ...ir.Fallback) *ir.AgentNode {
		return &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "a"},
			LLMFields: ir.LLMFields{Backend: "claw"},
			Tools:     []string{"read_file"},
			Fallbacks: fbs,
		}
	}

	t.Run("without the program nothing is known", func(t *testing.T) {
		if (&Executor{}).launchDecidesTheProgramsRoutes() {
			t.Error("a launch reads as fully specified with no program in hand: the unknown side must be the pessimistic one")
		}
	})

	t.Run("a var the caller supplied is not one the launch must decide", func(t *testing.T) {
		x := &Executor{wf: &ir.Workflow{Vars: strVar}, given: map[string]any{"m": "claw"}}
		if got := x.varsTheLaunchMustDecide(); len(got) != 0 {
			t.Errorf("varsTheLaunchMustDecide = %v for a var the caller supplied", got)
		}
		if !x.launchDecidesTheProgramsRoutes() {
			t.Error("a launch that supplies the program's one undefaulted var reads as under-specified")
		}
	})

	t.Run("it is not the provenance reading", func(t *testing.T) {
		x := &Executor{wf: &ir.Workflow{Vars: strVar}}
		if _, invented := x.inventedVar([]string{"m"}); invented {
			t.Error("provenance calls a typed var's shape invented: that reading is about a json var's SHAPE, and merging the two would report an unresolved-ref finding on every unsupplied string var")
		}
		if got := x.varsTheLaunchMustDecide(); len(got) != 1 || got[0] != "m" {
			t.Errorf("varsTheLaunchMustDecide = %v: a verdict reads the VALUE, and an unsupplied string var with no default has none before the launch, whatever its type", got)
		}
	})

	t.Run("the authored reading drops what resolves no backend", func(t *testing.T) {
		n := node(
			ir.Fallback{Name: "gated", Backend: "{{vars.m}}", Model: "anthropic/claude-haiku-4-5"},
			ir.Fallback{Name: "inherits", Model: "anthropic/claude-haiku-4-5"},
			ir.Fallback{Name: "give-up", Action: ir.FallbackActionSkip},
		)
		x := &Executor{wf: &ir.Workflow{Nodes: map[string]ir.Node{"a": n}, Vars: strVar}}
		x.SetVars(map[string]any{"m": "x"})
		got := x.EffectiveFallbackBackends(n)
		if len(got) != 1 || got[0] != "{{vars.m}}" {
			t.Errorf("EffectiveFallbackBackends = %v, want [{{vars.m}}]: the author's spelling for the templated route the launch decides, and nothing for a route that names no backend of its own or executes none at all", got)
		}
	})

	t.Run("a route that names the node's own backend is not one more to read", func(t *testing.T) {
		n := node(ir.Fallback{Name: "other-credential", Backend: "claw", Provider: "zai", Model: "anthropic/claude-haiku-4-5"})
		x := &Executor{wf: &ir.Workflow{Nodes: map[string]ir.Node{"a": n}, Vars: strVar}}
		x.SetVars(map[string]any{"m": "x"})
		if got := x.EffectiveFallbackBackends(n); len(got) != 0 {
			t.Errorf("EffectiveFallbackBackends = %v for a route that names the node's own backend: the seam answers the routes that LEAVE it, and its caller reads the node's own through EffectiveBackendName", got)
		}
	})

	t.Run("a node with no route carries no reading for the launch to decide", func(t *testing.T) {
		n := node()
		x := &Executor{wf: &ir.Workflow{Nodes: map[string]ir.Node{"a": n}, Vars: strVar}}
		x.SetVars(map[string]any{"m": "x"})
		if why := x.UndecidedRouteReading(n); why != "" {
			t.Errorf("UndecidedRouteReading = %q for a node whose `backend:` the program names and which carries no route", why)
		}
	})
}
