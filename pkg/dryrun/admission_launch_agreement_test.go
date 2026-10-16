package dryrun

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// admissionVerdict reports whether the engine's parallel-branch admission
// accepted this program on this launch, and the refusal when it did not.
//
// Keyed on the refusal admission itself raises, never on Pass.Clean: a pass
// unclean for another reason — a node that fails on a shape, a budget — says
// nothing about the question these rows ask.
func admissionVerdict(t *testing.T, src string, inputs map[string]any) (bool, string) {
	t.Helper()
	wf := compileBot(t, src)
	r, err := Run(context.Background(), wf, Options{Inputs: inputs})
	if err != nil {
		t.Fatalf("Run(%v): %v", inputs, err)
	}
	var reasons []string
	for _, p := range r.Passes {
		if strings.Contains(p.Failure, "WORKSPACE_SAFETY") {
			reasons = append(reasons, p.Failure)
		}
	}
	return len(reasons) == 0, strings.Join(reasons, " | ")
}

// The guarantee `validate --exec` owes a fan-out, held over a CORPUS of
// programs rather than a witness per spelling: a verdict the dry run takes on a
// var the PROGRAM leaves undecided — declared with no default, and the caller
// supplied none — must not be CLEAN while a launch that same program allows is
// refused at the router.
//
// The dry run reads ONE value for such a var (VarValue seeds a shape of its
// type; a declared enum domain is read at its two ends), the launch may put any
// other there, and a CLEAN verdict on a fan-out `iterion run` kills is the one
// failure an operator cannot recover from: the nodes before the router have
// already been paid for when the engine refuses.
//
// Each row carries the launches its program allows, asks the engine's own
// admission for each of them through Run — the site `validate --exec` reads,
// never the seam — and counts only where some launch IS refused: a row that
// stops being a trap fails instead of going quietly green. So a reading these
// rows do not enumerate — another seam over the same vars, another spelling of
// a var read — reddens here instead of shipping as a clean verdict.
//
// A var the program DECIDES is deliberately out of scope: `validate --exec`
// judges the program as written, so a `fb: string = "claw"` read at its own
// default is the verdict for that program, and a launch overriding it is judged
// again by the engine, against what that run holds.
func TestNoLaunchAProgramAllowsIsRefusedWhileValidateExecIsClean(t *testing.T) {
	for _, tc := range []struct {
		name     string
		src      string
		launches []map[string]any
	}{
		{
			name:     "a route's `backend:` read from a free-form var",
			src:      fanOutWithOneRoute(`  fb: string`, ""),
			launches: []map[string]any{{"fb": "claw"}, {"fb": "claude_code"}, {"fb": "codex"}},
		},
		{
			name:     "a route's `when:` gate, read by the dotted path",
			src:      fanOutWithOneRoute("  fb: string = \"claude_code\"\n  rescue: string", "vars.rescue == 'on'"),
			launches: []map[string]any{{"rescue": "on"}, {"rescue": "off"}},
		},
		{
			name:     "a route's `when:` gate, read by subscript",
			src:      fanOutWithOneRoute("  fb: string = \"claude_code\"\n  rescue: string", "vars['rescue'] == 'on'"),
			launches: []map[string]any{{"rescue": "on"}, {"rescue": "off"}},
		},
		{
			name:     "a route's `when:` gate, read by a subscript the launch computes",
			src:      fanOutWithOneRoute("  fb: string = \"claude_code\"\n  rescue: string", "vars['res' + 'cue'] == 'on'"),
			launches: []map[string]any{{"rescue": "on"}, {"rescue": "off"}},
		},
		{
			name:     "a route's `when:` gate on the middle of a declared enum domain",
			src:      fanOutWithOneRoute("  fb: string = \"claude_code\"\n  rescue: string [enum: \"off\", \"maybe\", \"on\"]", "vars.rescue == 'maybe'"),
			launches: []map[string]any{{"rescue": "off"}, {"rescue": "maybe"}, {"rescue": "on"}},
		},
		{
			name:     "the routes the TOOL surface reads, gated on a free-form var",
			src:      fanOutWithAutoMemoryRoute("  rescue: string", "vars.rescue == 'on'"),
			launches: []map[string]any{{"rescue": "on"}, {"rescue": "off"}},
		},
		{
			name:     "the routes the TOOL surface reads, gated by subscript",
			src:      fanOutWithAutoMemoryRoute("  rescue: string", "vars['rescue'] == 'on'"),
			launches: []map[string]any{{"rescue": "on"}, {"rescue": "off"}},
		},
		{
			name:     "the routes the TOOL surface reads, one of them spelled from a free-form var",
			src:      fanOutWithAutoMemoryRouteSpelled("  fb: string", "{{vars.fb}}", ""),
			launches: []map[string]any{{"fb": "claw"}, {"fb": "codex"}},
		},
		{
			name:     "the node's own `backend:` read from a free-form var",
			src:      fanOutWithTemplatedBackend(`  b: string`),
			launches: []map[string]any{{"b": "claw"}, {"b": "claude_code"}, {"b": "codex"}},
		},
		{
			name:     "the node's own `backend:` read at the middle of a declared enum domain",
			src:      fanOutWithTemplatedBackend(`  b: string [enum: "claw", "claude_code", "codex"]`),
			launches: []map[string]any{{"b": "claw"}, {"b": "claude_code"}, {"b": "codex"}},
		},
		{
			name:     "the workflow's `default_backend:` read from a free-form var",
			src:      fanOutWithTemplatedWorkflowDefault(`  d: string`),
			launches: []map[string]any{{"d": "claw"}, {"d": "claude_code"}, {"d": "codex"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var refused []string
			for _, in := range tc.launches {
				if clean, reason := admissionVerdict(t, tc.src, in); !clean {
					refused = append(refused, fmt.Sprintf("%v: %.200s", in, reason))
				}
			}
			if len(refused) == 0 {
				t.Fatalf("no launch this program allows is refused, so the row proves nothing about the guarantee: give it one the engine refuses, or drop it")
			}
			if clean, _ := admissionVerdict(t, tc.src, nil); clean {
				t.Errorf("`validate --exec` reads this program CLEAN while %d launch(es) it allows are refused at the router — the operator pays for the nodes before the router and then reads the refusal:\n  %s",
					len(refused), strings.Join(refused, "\n  "))
			}
		})
	}
}

// The MIRROR of the guarantee above, and the direction a one-sided corpus let
// ship twice: `validate --exec` must not REFUSE a program the engine admits on
// every launch of it. A refusal an operator cannot act on — it names a field
// their node never reaches — sends them to `--no-strict` or to a `readonly:`
// they do not mean, which is how a static gate stops being read at all.
//
// The rows are programs whose vars all carry a value, which is the launch a
// verdict is about (launchDecidesTheProgramsRoutes), plus the two cells an
// UNDER-specified launch still decides: a routing field the launch cannot move
// because it holds no template at all. Where a template or a gate IS the
// launch's, the reading is pessimistic and says so, naming the var and the
// `--var` that decides it — the rows of
// TestValidateExecDecidesAFallbackRouteOnlyOnAFullySpecifiedLaunch.
//
// Each row carries the launches its program allows and asks the engine's own
// admission for each of them, through Run: every one must be ADMITTED, or the
// row is not a witness of this direction and fails saying so — the vacuity
// guard of the table above, read the other way. Then the verdict `validate
// --exec` prints for the program itself must be clean.
func TestAFanOutEveryLaunchAdmitsIsNotRefusedByValidateExec(t *testing.T) {
	requireUnsetRouteEnv(t)
	for _, tc := range []struct {
		name     string
		src      string
		launches []map[string]any
	}{
		{
			name:     "a route the program decides, under a gate it decides too",
			src:      fanOutWithOneRoute("  fb: string = \"claw\"\n  rescue: string = \"off\"", "vars.rescue == 'on'"),
			launches: []map[string]any{{"rescue": "on"}, {"rescue": "off"}},
		},
		{
			name:     "the same route, under a gate read by subscript",
			src:      fanOutWithOneRoute("  fb: string = \"claw\"\n  rescue: string = \"off\"", "vars['rescue'] == 'on'"),
			launches: []map[string]any{{"rescue": "on"}, {"rescue": "off"}},
		},
		{
			name:     "a route an env reference defaults to claw, under a gate the program does not decide",
			src:      fanOutWithOneRouteSpelled("  rescue: string", "${"+unsetRouteEnv+":-claw}", "vars.rescue == 'on'"),
			launches: []map[string]any{{"rescue": "on"}, {"rescue": "off"}},
		},
		{
			name:     "a node backend the program decides, under a workflow default it never reaches",
			src:      fanOutWithOwnBackendAndWorkflowDefault(`  b: string = "claw"`+"\n"+`  d: string = "claude_code"`, "{{vars.b}}"),
			launches: []map[string]any{{"d": "claw"}, {"d": "claude_code"}, {"d": "codex"}},
		},
		{
			name:     "a node backend an env reference defaults, under that same workflow default",
			src:      fanOutWithOwnBackendAndWorkflowDefault(`  d: string`, "${"+unsetRouteEnv+":-claw}"),
			launches: []map[string]any{{"d": "claw"}, {"d": "claude_code"}, {"d": "codex"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, in := range tc.launches {
				if clean, reason := admissionVerdict(t, tc.src, in); !clean {
					t.Fatalf("launch %v is REFUSED by the engine's own admission, so this row no longer witnesses a false refusal — give it launches the engine admits, or move it to the table above:\n  %s", in, reason)
				}
			}
			if clean, reason := admissionVerdict(t, tc.src, nil); !clean {
				t.Errorf("`validate --exec` REFUSES this program while the engine admits all %d launch(es) it allows — the refusal names something the author cannot act on:\n  %s",
					len(tc.launches), reason)
			}
		})
	}
}

// subbotHandOffBot is the parent whose child hand-off is the whole question:
// the node hands the child `rescue` — the var the child's route gate reads —
// and the mapping is spelled as the row needs it, a literal or a template.
func subbotHandOffBot(mapping string) string {
	return subbotHandOffBotVars("  rescue: string", mapping)
}

// subbotHandOffBotVars is the same parent with the child's var line spelled as
// the row needs it: a var no launch decides, or one whose declared default
// decides the hand-off when the launch names no value.
func subbotHandOffBotVars(varsLine, mapping string) string {
	return `schema verdict:
  ok: bool

vars:
` + varsLine + `

subbot kid:
  source: "kid.bot"
  with { rescue: ` + mapping + ` }
  output: verdict

workflow p:
  worktree: none
  sandbox: none
  entry: kid
  kid -> done
`
}

// childHandOffVerdict runs the parent over one launch with the child wired
// under `kid`, and returns the verdict together with the refusal a child pass
// carries — the child's router, where a run refuses after the parent's and
// the child's pre-router nodes are already paid for.
func childHandOffVerdict(t *testing.T, src, child string, inputs map[string]any) (bool, string) {
	t.Helper()
	wf := compileBot(t, src)
	opts := withChild(t, child)
	opts.Inputs = inputs
	r, err := Run(context.Background(), wf, opts)
	if err != nil {
		t.Fatalf("Run(%v): %v", inputs, err)
	}
	var refusals []string
	for _, c := range r.Children {
		if strings.Contains(c.Failure, "WORKSPACE_SAFETY") {
			refusals = append(refusals, c.Failure)
		}
	}
	return r.Clean(), strings.Join(refusals, " | ")
}

// A subbot child's launch is its node's `with:` hand-off, not the operator's
// `--var`: at a run the child receives what the mappings resolve to, and
// nothing else — the reading runtime.runSubbotNode executes. The dry run
// reads that hand-off as far as the compiled IR states it: a mapping the
// compiler parsed to no reference is a literal, the same on every launch; a
// mapping that is exactly one `{{vars.x}}` reference is the parent's value of
// x, the launch's to decide; every other mapping is decided while the parent
// runs, and hands the child nothing, so the pessimistic reading stands at the
// child as it does at a top-level launch that supplied no value.
//
// The row that bites the name collision: the literal decides the child's gate
// TRUE on every launch, while the same-named var the operator could pass
// decides it either way — inherited by bare name, `validate --exec --var
// rescue=off` read CLEAN a child the run refuses on every launch.
func TestAChildsLaunchIsItsHandOffNotTheParentsVars(t *testing.T) {
	requireUnsetRouteEnv(t)
	child := fanOutWithOneRoute("  fb: string = \"claude_code\"\n  rescue: string", "vars.rescue == 'on'")
	undecided := "supplies no value for `rescue`"

	t.Run("a literal decides the child's gate on every launch, whatever --var names", func(t *testing.T) {
		src := subbotHandOffBot(`"on"`)
		for _, in := range []map[string]any{nil, {"rescue": "off"}, {"rescue": "on"}} {
			clean, refusal := childHandOffVerdict(t, src, child, in)
			if clean {
				t.Fatalf("launch %v reads CLEAN: the hand-off pins the child's `rescue` to \"on\", so the route it opens is one every launch takes, and a run refuses at the child's router", in)
			}
			if strings.Contains(refusal, undecided) {
				t.Errorf("launch %v is refused as if the launch left `rescue` undecided: the hand-off decides it — %s", in, refusal)
			}
		}
	})

	t.Run("a lone vars reference forwards the parent's value, the launch's to decide", func(t *testing.T) {
		src := subbotHandOffBot(`"{{vars.rescue}}"`)
		if clean, refusal := childHandOffVerdict(t, src, child, map[string]any{"rescue": "off"}); !clean {
			t.Fatalf("the hand-off forwards the parent's `rescue`, and that launch sets it to \"off\": the child's gate reads false, and a run admits this launch — %s", refusal)
		}
		clean, refusal := childHandOffVerdict(t, src, child, map[string]any{"rescue": "on"})
		if clean {
			t.Fatalf("the hand-off forwards the parent's `rescue`, and that launch sets it to \"on\": the child's gate reads true, and a run refuses this launch at the child's router")
		}
		if strings.Contains(refusal, undecided) {
			t.Errorf("the launch decided `rescue` through the hand-off, and the refusal reads it as undecided: %s", refusal)
		}
	})

	t.Run("a mapping the dry run cannot state leaves the child's route as the author wrote it", func(t *testing.T) {
		src := subbotHandOffBot(`"v-{{vars.rescue}}"`)
		for _, in := range []map[string]any{nil, {"rescue": "off"}} {
			clean, refusal := childHandOffVerdict(t, src, child, in)
			if clean {
				t.Fatalf("launch %v reads CLEAN: the mapping's value is decided while the parent runs, and no value the dry run could state is one a run would hold", in)
			}
			if !strings.Contains(refusal, undecided) {
				t.Errorf("launch %v is refused without naming the var the hand-off leaves undecided: %s", in, refusal)
			}
		}
	})

	t.Run("a launch that names no value leaves the child undecided", func(t *testing.T) {
		src := subbotHandOffBot(`"{{vars.rescue}}"`)
		clean, refusal := childHandOffVerdict(t, src, child, nil)
		if clean {
			t.Fatal("the parent's var is undeclared-and-unsupplied, so the hand-off decides nothing and the child's route is read as its author wrote it — not clean")
		}
		if !strings.Contains(refusal, undecided) {
			t.Errorf("the refusal does not name the var the launch must decide: %s", refusal)
		}
	})

	t.Run("the hand-off forwards the parent's declared default when the launch names no value", func(t *testing.T) {
		src := subbotHandOffBotVars(`  rescue: string = "off"`, `"{{vars.rescue}}"`)
		if clean, refusal := childHandOffVerdict(t, src, child, nil); !clean {
			t.Fatalf("the hand-off forwards the parent's default \"off\", the child's gate reads false, and a run admits this launch — %s", refusal)
		}
	})

	t.Run("a forwarded default is refused as the route it opens, not as an undecided var", func(t *testing.T) {
		src := subbotHandOffBotVars(`  rescue: string = "on"`, `"{{vars.rescue}}"`)
		clean, refusal := childHandOffVerdict(t, src, child, nil)
		if clean {
			t.Fatal("the hand-off forwards the parent's default \"on\", the child's gate reads true, and a run refuses at the child's router")
		}
		if strings.Contains(refusal, undecided) {
			t.Errorf("the hand-off decided `rescue`, and the refusal reads it as undecided: %s", refusal)
		}
		if !strings.Contains(refusal, "claude_code") {
			t.Errorf("the refusal does not name the route the forwarded default opens: %s", refusal)
		}
	})

	t.Run("a launch value beats the declared default through the hand-off, the run's own order", func(t *testing.T) {
		src := subbotHandOffBotVars(`  rescue: string = "off"`, `"{{vars.rescue}}"`)
		clean, refusal := childHandOffVerdict(t, src, child, map[string]any{"rescue": "on"})
		if clean {
			t.Fatal("the launch sets rescue=on over the default \"off\", and a run hands the child that value: the child's gate reads true and the run refuses at its router — a clean verdict here is the one a launch contradicts")
		}
		if strings.Contains(refusal, undecided) {
			t.Errorf("the launch decided `rescue` through the hand-off, and the refusal reads it as undecided: %s", refusal)
		}
		if !strings.Contains(refusal, "claude_code") {
			t.Errorf("the refusal does not name the route the launch value opens: %s", refusal)
		}
	})

	t.Run("a declared default never overrides a launch value through the hand-off", func(t *testing.T) {
		src := subbotHandOffBotVars(`  rescue: string = "on"`, `"{{vars.rescue}}"`)
		if clean, refusal := childHandOffVerdict(t, src, child, map[string]any{"rescue": "off"}); !clean {
			t.Fatalf("the launch sets rescue=off over the default \"on\", and a run hands the child \"off\": the child's gate reads false and the run admits this launch — %s", refusal)
		}
	})

	t.Run("a forwarded env-form default is expanded at the child's launch, the run's verdict", func(t *testing.T) {
		src := subbotHandOffBotVars(`  rescue: string = "${`+unsetRouteEnv+`:-on}"`, `"{{vars.rescue}}"`)
		clean, refusal := childHandOffVerdict(t, src, child, nil)
		if clean {
			t.Fatal("the hand-off forwards the env-form default as written, and the child's launch expands it to \"on\": the child's gate reads true and a run refuses at its router — not clean")
		}
		if strings.Contains(refusal, undecided) {
			t.Errorf("the forwarded default expands to a value the launch decides, and the refusal reads it as undecided: %s", refusal)
		}
		if !strings.Contains(refusal, "claude_code") {
			t.Errorf("the refusal does not name the route the expanded default opens: %s", refusal)
		}
	})
}

// unsetRouteEnv is the environment name the rows that need an unresolved
// `${VAR}` are written with. A row resting on the environment proves it:
// on a machine where this name IS set, those rows read a program other than
// the one they declare.
const unsetRouteEnv = "ITERION_TEST_UNSET_ROUTE"

func requireUnsetRouteEnv(t *testing.T) {
	t.Helper()
	if v, set := os.LookupEnv(unsetRouteEnv); set {
		t.Fatalf("%s is set to %q here, so every row spelled with it reads a different program than it declares", unsetRouteEnv, v)
	}
}

// fanOutWithOwnBackendAndWorkflowDefault is the pair whose branches name their
// OWN `backend:` under a workflow `default_backend:` the launch decides: where
// that spelling resolves to a route, the default is a field the node never
// reaches.
func fanOutWithOwnBackendAndWorkflowDefault(varsBlock, own string) string {
	return fanOutOfTwoAgents(varsBlock, "  default_backend: \"{{vars.d}}\"\n", fmt.Sprintf(`  model: "claude-opus-4-7"
  backend: %q
  output: verdict
  tools: [read_file]
`, own))
}

// The TOOL-surface seam reads the same `fallbacks:` routes admission does — the
// runtime's own opt-ins are decided per route, so a route onto claw under
// `auto_memory:` puts a file writer on a node that declared none — and a dry
// run must answer it from the one reading of those routes its admission takes.
func TestTheDryRunsToolSurfaceReadsTheRoutesItsAdmissionReads(t *testing.T) {
	for _, tc := range []struct {
		name  string
		vars  string
		when  string
		clean bool
		names string
		why   string
	}{
		{
			name:  "a route onto claw gated on a var the dry run invented",
			vars:  "  rescue: string",
			when:  "vars.rescue == 'on'",
			clean: false,
			names: "write_file",
			why:   "`--var rescue=on` is a launch this program allows: the route runs on claw, where `auto_memory: on` grants write_file to a node that declared `tools: [read_file]`",
		},
		{
			name:  "the same route, gated by subscript",
			vars:  "  rescue: string",
			when:  "vars['rescue'] == 'on'",
			clean: false,
			names: "write_file",
			why:   "the gate reads the vars namespace whole, which reaches the same unsupplied var",
		},
		{
			name:  "a route the program's own default gates off",
			vars:  `  rescue: string = "off"`,
			when:  "vars.rescue == 'on'",
			clean: true,
			why:   "the gate reads a value the program itself declares, so the dry run knows the chain this program walks carries no route onto claw",
		},
		{
			name:  "a route the program's own default gates on",
			vars:  `  rescue: string = "on"`,
			when:  "vars.rescue == 'on'",
			clean: false,
			names: "write_file",
			why:   "the route is in the chain this program walks, and claw's `auto_memory:` append is on the node's effective surface",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clean, reason := admissionVerdict(t, fanOutWithAutoMemoryRoute(tc.vars, tc.when), nil)
			if clean != tc.clean {
				t.Fatalf("admitted = %v, want %v: %s\n  %s", clean, tc.clean, tc.why, reason)
			}
			if tc.names != "" && !strings.Contains(reason, tc.names) {
				t.Errorf("the refusal does not name %q, so the author cannot act on it: %s", tc.names, reason)
			}
		})
	}
}

// The node's OWN `backend:`, and the workflow default it falls back to, are
// read from the same vars as its routes — so the same reading answers them: the
// author's spelling where the value is one this dry run invented, which is both
// the pessimistic side and the only name the author can act on.
func TestTheDryRunsAdmissionReadsTheNodeBackendItsOwnProgramAnswers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		src    string
		clean  bool
		names  string
		absent string
		why    string
	}{
		{
			name:   "a node backend read from a free-form var",
			src:    fanOutWithTemplatedBackend(`  b: string`),
			clean:  false,
			names:  "{{vars.b}}",
			absent: "runs on x",
			why:    "the launch may put any backend there, so the route is read as the unresolved one it is — and named by the spelling its author wrote, never by the shape this package seeded",
		},
		{
			name:   "a node backend read at the middle of a declared enum domain",
			src:    fanOutWithTemplatedBackend(`  b: string [enum: "claw", "claude_code", "codex"]`),
			clean:  false,
			names:  "{{vars.b}}",
			absent: "runs on claw",
			why:    "the two passes read the two ENDS of a declared domain, so `--var b=claude_code` is a launch this program allows and no pass reads",
		},
		{
			name:  "a node backend the program's own default resolves to claw",
			src:   fanOutWithTemplatedBackend(`  b: string = "claw"`),
			clean: true,
			why:   "the node runs where the program says it does, and claw bounds the branch's `tools:` list",
		},
		{
			name:  "a node backend read from a closed domain whose every value bounds the list",
			src:   fanOutWithTemplatedBackend(`  b: string [enum: "claw", "codex"]`),
			clean: false,
			names: "{{vars.b}}",
			why: "the cost of the reading above, deliberately paid: `--var b=claw` and `--var b=codex` are both admitted, so this pair is refused where no launch of it is. " +
				"Reading a closed domain value by value instead would rest on BOTH passes reaching this router — a program whose upstream shapes send one pass elsewhere leaves that pass's value unjudged — and the two readings of one var's domain would then disagree with the `fallbacks:` route half, which takes the same pessimistic side. " +
				"An author who wants the fan-out gives the var a default (`b: string [enum: \"claw\", \"codex\"] = \"claw\"`) or asserts the branch with `readonly:`",
		},
		{
			name:  "the same closed domain, with the default that escapes it",
			src:   fanOutWithTemplatedBackend(`  b: string [enum: "claw", "codex"] = "claw"`),
			clean: true,
			why:   "the program decides the route, so the reading above never fires — the escape hatch the row before it names",
		},
		{
			name:   "a node backend the program's own default resolves off claw",
			src:    fanOutWithTemplatedBackend(`  b: string = "claude_code"`),
			clean:  false,
			names:  "runs on claude_code",
			absent: "{{vars.b}}",
			why:    "the program decides this one, so the verdict names what it resolves to",
		},
		{
			name:   "the workflow's `default_backend:` read from a free-form var",
			src:    fanOutWithTemplatedWorkflowDefault(`  d: string`),
			clean:  false,
			names:  "{{vars.d}}",
			absent: "runs on x",
			why:    "a node that names no backend of its own is routed by the workflow default, which the launch decides here",
		},
		{
			name:  "a node backend the program decides, under a workflow default it never reaches",
			src:   fanOutWithOwnBackendAndWorkflowDefault(`  b: string = "claw"`+"\n"+`  d: string = "claude_code"`, "{{vars.b}}"),
			clean: true,
			why:   "a spelling that RESOLVES to a route decides where the node runs and dispatch never consults the default, so a reading that consulted it anyway refused this fan-out for a field the node never reaches, on every launch of which `iterion run` admits it",
		},
		{
			name:   "that same program with the default left to the launch",
			src:    fanOutWithOwnBackendAndWorkflowDefault(`  b: string = "claw"`+"\n"+`  d: string`, "{{vars.b}}"),
			clean:  false,
			names:  "{{vars.b}}",
			absent: "{{vars.d}}",
			why:    "the cost of deciding a routing field on a fully specified launch alone, deliberately paid: the node's own `backend:` is a template and this launch decides no template, so it stays as written — named as its author wrote it, and never as the default the node does not reach. `iterion validate --exec --var d=claw` reads the same program clean",
		},
		{
			name:  "a node backend an env reference defaults, under that same workflow default",
			src:   fanOutWithOwnBackendAndWorkflowDefault(`  d: string`, "${"+unsetRouteEnv+":-claw}"),
			clean: true,
			why:   "the env form is a routing field like the template, resolved here as dispatch resolves it",
		},
		{
			name:   "a node backend whose env reference resolves to nothing, under that same default",
			src:    fanOutWithOwnBackendAndWorkflowDefault(`  d: string`, "${"+unsetRouteEnv+"}"),
			clean:  false,
			names:  "{{vars.d}}",
			absent: "runs on x",
			why:    "this one names no route at all, so the node IS routed by the workflow default and the launch decides it — the arm the reading above must keep reaching",
		},
		{
			name:   "a node backend written `auto`, under that same default",
			src:    fanOutWithOwnBackendAndWorkflowDefault(`  d: string`, "auto"),
			clean:  false,
			names:  "{{vars.d}}",
			absent: "runs on x",
			why:    "an explicit `auto` defers like an absent field (resolveBackendName step 1), so the default routes the node here too",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clean, reason := admissionVerdict(t, tc.src, nil)
			if clean != tc.clean {
				t.Fatalf("admitted = %v, want %v: %s\n  %s", clean, tc.clean, tc.why, reason)
			}
			if tc.clean {
				return
			}
			if tc.names != "" && !strings.Contains(reason, tc.names) {
				t.Errorf("the refusal does not name %q, so the author cannot act on it: %s", tc.names, reason)
			}
			if tc.absent != "" && strings.Contains(reason, tc.absent) {
				t.Errorf("the refusal carries %q, which the program does not say: %s", tc.absent, reason)
			}
		})
	}
}

// fanOutWithAutoMemoryRoute is the two-branch fan-out of a codex node under
// `auto_memory: on` with one route onto claw: the pair where the verdict rests
// on the TOOL surface — claw's `auto_memory:` append puts write_file on a node
// whose `tools:` list declares a reader — and not on what bounds the route.
func fanOutWithAutoMemoryRoute(varsBlock, when string) string {
	return fanOutWithAutoMemoryRouteSpelled(varsBlock, "claw", when)
}

// fanOutWithAutoMemoryRouteSpelled takes the route's `backend:` as well, for
// the row whose append is the LAUNCH's to decide: `auto_memory:` grants
// write_file on claw and nothing on codex, so a surface taken over a route
// spelling this dry run resolves from a value it invented would answer the
// appends of a backend no run takes.
func fanOutWithAutoMemoryRouteSpelled(varsBlock, routeBackend, when string) string {
	gate := ""
	if when != "" {
		gate = fmt.Sprintf("\n      when: %q", when)
	}
	return fanOutOfTwoAgents(varsBlock, "", fmt.Sprintf(`  model: "claude-opus-4-7"
  backend: "codex"
  output: verdict
  tools: [read_file]
  auto_memory: on
  fallbacks:
    alt:
      backend: %q
      model: "anthropic/claude-haiku-4-5"`, routeBackend)+gate+"\n")
}

// fanOutWithTemplatedBackend is the two-branch fan-out whose branches read
// their own `backend:` from a var.
func fanOutWithTemplatedBackend(varsBlock string) string {
	return fanOutOfTwoAgents(varsBlock, "", `  model: "claude-opus-4-7"
  backend: "{{vars.b}}"
  output: verdict
  tools: [read_file]
`)
}

// fanOutWithTemplatedWorkflowDefault is the same pair with no `backend:` of its
// own: the workflow's `default_backend:` routes both branches, and it is read
// from a var.
func fanOutWithTemplatedWorkflowDefault(varsBlock string) string {
	return fanOutOfTwoAgents(varsBlock, "  default_backend: \"{{vars.d}}\"\n", `  model: "claude-opus-4-7"
  output: verdict
  tools: [read_file]
`)
}
