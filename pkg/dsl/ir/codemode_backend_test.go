package ir

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
)

// TestCodemodeIsRefusedOnABackendThatCannotServeIt is the cell's evidence, on
// both sides: a node routed to a backend whose registry cell says
// typed-refused is refused at compile (C339), and the same node on claw is
// not. It starts from `.bot` TEXT rather than from a hand-built Workflow,
// because what it has to catch is a chain — the parser, the encoder, the
// compiler's carry and the validator — and a struct built by hand passes over
// every one of them.
func TestCodemodeIsRefusedOnABackendThatCannotServeIt(t *testing.T) {
	bot := func(backend string) string {
		return `
agent a:
  model: "m"
  backend: "` + backend + `"
  codemode: on

workflow w:
  entry: a
  a -> done
`
	}

	r := compileFile(t, bot("codex"))
	if got := countCode(r, DiagCodemodeBackendUnsupported); got != 1 {
		t.Fatalf("C339 on a codex node declaring codemode: on: %d time(s), want 1: %+v", got, r.Diagnostics)
	}
	var d Diagnostic
	for _, got := range r.Diagnostics {
		if got.Code == DiagCodemodeBackendUnsupported {
			d = got
		}
	}
	// A refusal that does not say WHAT was refused, where it came from and
	// what to do instead is a dead end the operator cannot act on — the
	// clause ErrCapabilityUnsupported.Remedy exists for.
	for _, want := range []string{"codex", "codemode: on (from node)", "claw"} {
		if !strings.Contains(d.Message, want) {
			t.Errorf("the refusal does not name %q: %q", want, d.Message)
		}
	}
	if d.Severity != SeverityError {
		t.Errorf("C339 severity = %v, want error — the executor refuses the dispatch, it does not degrade", d.Severity)
	}

	// The other side of the cell: the backend that serves it is not refused.
	if got := countCode(compileFile(t, bot("claw")), DiagCodemodeBackendUnsupported); got != 0 {
		t.Errorf("C339 fired %d time(s) on claw, whose cell says %q", got, backendcaps.Verdict(backendcaps.Claw, backendcaps.Codemode))
	}
}

// TestCodemodeRefusalFollowsTheRegistryRatherThanAList is the guarantee test
// behind the one above: the SITE it exercises is one backend, and what this
// slice claims is a property of every backend in the registry. An independent
// reader — the registry itself, not the validator's view of it — decides what
// each node should get, and the compiler's verdicts are compared against it.
//
// It is what catches a validator that hard-codes "claw" (green on the test
// above, wrong the day a second loop serves codemode) and a cell edited
// without its refusal.
func TestCodemodeRefusalFollowsTheRegistryRatherThanAList(t *testing.T) {
	var served, refused int
	for _, backend := range backendcaps.Names() {
		r := compileFile(t, `
agent a:
  model: "m"
  backend: "`+backend+`"
  codemode: on

workflow w:
  entry: a
  a -> done
`)
		got := countCode(r, DiagCodemodeBackendUnsupported)
		want := 0
		if backendcaps.Verdict(backend, backendcaps.Codemode) == backendcaps.TypedRefused {
			want = 1
			refused++
		} else {
			served++
		}
		if got != want {
			t.Errorf("backend %q: cell %q, C339 fired %d time(s), want %d",
				backend, backendcaps.Verdict(backend, backendcaps.Codemode), got, want)
		}
	}
	// A matrix with one empty arm proves nothing about the other: a
	// validator that refused everything, or nothing, would pass a loop that
	// never saw both answers.
	if served == 0 || refused == 0 {
		t.Fatalf("the registry gave %d serving and %d refusing backend(s); this test needs both arms", served, refused)
	}
}

// TestCodemodeInheritedFromTheWorkflowIsRefusedWhereItWouldRun holds the
// difference between this check and the honesty clause it replaces (C338,
// retired): C338 warned once per DECLARED line, because an inherited default
// reaching every node would have buried the one line the author wrote. C339
// is the opposite shape on purpose — it fires wherever the capability WOULD
// be asked for, because that is where the dispatch is refused, and a
// diagnostic on the workflow line would leave the operator to find which of
// its nodes the engine will stop.
//
// The node that declares `codemode: off` against the workflow's `on` is the
// third state the attribute exists for, and it is not refused.
func TestCodemodeInheritedFromTheWorkflowIsRefusedWhereItWouldRun(t *testing.T) {
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "codex"

agent b:
  model: "m"
  backend: "codex"

agent c:
  model: "m"
  backend: "codex"
  codemode: off

agent d:
  model: "m"
  backend: "claw"

workflow w:
  entry: a
  codemode: on
  a -> b
  b -> c
  c -> d
  d -> done
`)
	seen := map[string]int{}
	for _, d := range r.Diagnostics {
		if d.Code == DiagCodemodeBackendUnsupported {
			seen[d.NodeID]++
			if !strings.Contains(d.Message, "(from workflow)") {
				t.Errorf("the refusal on %q does not say the value was inherited: %q", d.NodeID, d.Message)
			}
		}
	}
	for _, want := range []struct {
		node string
		n    int
	}{
		{"a", 1}, // inherits on, on a backend that refuses
		{"b", 1}, // the second one: a check that fires once per workflow would miss it
		{"c", 0}, // an explicit off beats the workflow's on
		{"d", 0}, // inherits on, on the backend that serves it
	} {
		if got := seen[want.node]; got != want.n {
			t.Errorf("C339 on %q: %d time(s), want %d (all: %v)", want.node, got, want.n, seen)
		}
	}
}

// TestCodemodeOnAnUndecidedRouteIsLeftToTheDispatch pins what this check
// deliberately does NOT answer. A node with no `backend:` and a workflow with
// no default has no route the source decides, and guessing one here would
// refuse a `.bot` that runs — the executor asks the same question of the
// backend it actually resolved (executor_codemode.go). It is C267's division,
// and the reason the engine's refusal is not redundant with this one.
func TestCodemodeOnAnUndecidedRouteIsLeftToTheDispatch(t *testing.T) {
	r := compileFile(t, `
agent a:
  model: "m"
  codemode: on

workflow w:
  entry: a
  a -> done
`)
	if got := countCode(r, DiagCodemodeBackendUnsupported); got != 0 {
		t.Errorf("C339 fired %d time(s) on a node whose route the source does not decide: %+v", got, r.Diagnostics)
	}
}

// TestOnImpliesDeclared pins the invariant that lets both halves of this
// refusal carry ONE guard instead of two. `!policy.On` is sufficient only
// because the default rung is OFF; the day a rung makes the default on,
// `Declared()` becomes load-bearing again and this test is the alarm.
//
// It is written as an exhaustive walk of the rungs rather than three
// hand-picked cases, so a FOURTH rung — a run override is the next one owed —
// cannot be added without being judged here.
func TestOnImpliesDeclared(t *testing.T) {
	spellings := []string{"", "on", "off", "ON", " on ", "OFF"}
	sawOn, sawDefault := false, false
	for _, node := range spellings {
		for _, workflow := range spellings {
			p := ResolveCodemode(node, workflow)
			if p.On && !p.Declared() {
				t.Errorf("node=%q workflow=%q resolved to %+v: ON from a rung that is not a declaration — the single guard in validateCodemodeBackends and in codemodePolicy is no longer sufficient", node, workflow, p)
			}
			if p.On {
				sawOn = true
			}
			if p.Source == CodemodeFromDefault {
				sawDefault = true
				if p.On {
					t.Errorf("the default rung resolved ON (%+v); the default is off, and the refusal would start firing on nodes nobody wrote", p)
				}
			}
		}
	}
	if !sawOn || !sawDefault {
		t.Fatalf("the walk did not exercise both arms (on=%v default=%v)", sawOn, sawDefault)
	}
}

// TestTheFallbackRoutesAreJudgedToo covers the branch an adversarial round
// found uncovered: deleting the whole `for _, fallback := range` loop, an
// ERROR-severity refusal that blocks compilation, left the suite green.
//
// The dedupe is a second, separate assertion: a fallback naming the SAME
// backend as the primary must not be reported twice, and a mutant that drops
// `route != primary` also survived.
func TestTheFallbackRoutesAreJudgedToo(t *testing.T) {
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode: on
  fallbacks:
    one:
      backend: "codex"
      model: "m"
    two:
      backend: "kimi"
      model: "m"

workflow w:
  entry: a
  a -> done
`)
	if got := countCode(r, DiagCodemodeBackendUnsupported); got != 2 {
		t.Errorf("C339 on a claw primary with two refusing fallbacks: %d, want 2: %+v", got, r.Diagnostics)
	}
	var named []string
	for _, d := range r.Diagnostics {
		if d.Code == DiagCodemodeBackendUnsupported {
			named = append(named, d.Message)
		}
	}
	for _, want := range []string{`fallback "one"`, `fallback "two"`} {
		found := false
		for _, m := range named {
			if strings.Contains(m, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no refusal names %s: %v", want, named)
		}
	}

	// The dedupe: a fallback on the SAME refusing backend as the primary is
	// one verdict, not two.
	dup := compileFile(t, `
agent a:
  model: "m"
  backend: "codex"
  codemode: on
  fallbacks:
    same:
      backend: "codex"
      model: "m2"

workflow w:
  entry: a
  a -> done
`)
	if got := countCode(dup, DiagCodemodeBackendUnsupported); got != 1 {
		t.Errorf("C339 on a codex primary with a codex fallback: %d, want 1 (the route is the same one)", got)
	}
}

// TestTheVerdictDoesNotMoveWithTheCompilingEnvironment holds the property
// backendReader's own doc states and nothing held for C339: the source
// reading resolves `${VAR:-name}` to the name WRITTEN IN THE SOURCE, so one
// artifact compiles identically on every machine. A reading that consulted
// the ambient environment would make a `.bot` compile on a developer's laptop
// and fail in CI, or the reverse.
//
// The verdict it produces also has to SAY it was taken from the default,
// because the dispatch expands the same text from the pod's environment
// (ir.ExpandEnvWithDefault) and may land on another backend entirely.
func TestTheVerdictDoesNotMoveWithTheCompilingEnvironment(t *testing.T) {
	const src = `
agent a:
  model: "m"
  backend: "${ITERION_RVA_CODEMODE_BACKEND:-codex}"
  codemode: on

workflow w:
  entry: a
  a -> done
`
	// The environment says claw; the source default says codex.
	t.Setenv("ITERION_RVA_CODEMODE_BACKEND", "claw")
	r := compileFile(t, src)
	if got := countCode(r, DiagCodemodeBackendUnsupported); got != 1 {
		t.Fatalf("C339 = %d, want 1 — the verdict moved with the compiling machine's environment: %+v", got, r.Diagnostics)
	}
	for _, d := range r.Diagnostics {
		if d.Code != DiagCodemodeBackendUnsupported {
			continue
		}
		if !strings.Contains(d.Message, "codex") {
			t.Errorf("the verdict does not name the backend the SOURCE decides: %q", d.Message)
		}
		if !strings.Contains(d.Message, "environment dial") {
			t.Errorf("a dialled route's verdict does not say where its backend name came from, so an operator reads a verdict about a backend they overrode: %q", d.Message)
		}
	}
}

// TestAnUnregisteredBackendCompilesAndIsRefusedAtDispatch pins the ONE class
// where the compiler and the engine deliberately disagree, and why.
//
// The registry publishes a contract (docs/backend-capability-registry.md):
// "unknown names acquire neither capabilities nor compiler refusals" — so a
// `.bot` naming a backend this build never heard of must not be refused at
// compile for a capability nobody can answer for. But the ENGINE resolves
// that name to a real backend object, and the only loop that installs the
// codemode tool is the one whose cell says shipped: granting there would hand
// the node a Codemode nothing reads, which is the silent drop this whole
// slice exists to prevent.
func TestAnUnregisteredBackendCompilesAndIsRefusedAtDispatch(t *testing.T) {
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "an_operators_own_backend"
  codemode: on

workflow w:
  entry: a
  a -> done
`)
	if got := countCode(r, DiagCodemodeBackendUnsupported); got != 0 {
		t.Errorf("C339 fired %d time(s) on a name the registry does not carry: %+v", got, r.Diagnostics)
	}
	if CodemodeRefusedBy("an_operators_own_backend") {
		t.Error("the compiler's question answers yes for an unregistered name")
	}
	if CodemodeServedBy("an_operators_own_backend") {
		t.Error("the engine's question answers yes for an unregistered name: a node would carry Codemode to a loop that never reads it")
	}
}

// TestTheDialClauseFiresExactlyWhereTheNameCameFromADial is the NEGATIVE
// witness the clause lacked. A second adversarial round killed the guard
// three ways and the suite stayed green — including the mutant that makes
// EVERY verdict carry the clause, so a plain `backend: "codex"` is told its
// name was read from an environment dial. A message that lies about where its
// verdict came from is worse than one that says nothing.
//
// The four arms are the four places a route's text can come from, and they
// are tested together because the rule is one rule: the clause fires when the
// text the verdict was derived FROM carries a `${…}` span, wherever that text
// lives.
func TestTheDialClauseFiresExactlyWhereTheNameCameFromADial(t *testing.T) {
	const clause = "environment dial"
	for _, c := range []struct {
		name, nodeBackend, workflowDefault, fallbackBackend string
		want                                                bool
	}{
		{"a literal route says nothing about dials", `"codex"`, "", "", false},
		{"the node's own dial", `"${ITERION_RVA_X:-codex}"`, "", "", true},
		{"an absent node backend inherits a dialled default", "", `"${ITERION_RVA_X:-codex}"`, "", true},
		{"`auto` falls through to the dialled default too", `"auto"`, `"${ITERION_RVA_X:-codex}"`, "", true},
		{"a literal default under an absent node backend", "", `"codex"`, "", false},
		{"a dial whose default falls through is still a dial", `"${ITERION_RVA_X:-auto}"`, `"codex"`, "", true},
		{"a dial with an empty default is still a dial", `"${ITERION_RVA_X:-}"`, `"codex"`, "", true},
		{"a dialled fallback", `"claw"`, "", `"${ITERION_RVA_X:-kimi}"`, true},
		{"a literal fallback", `"claw"`, "", `"kimi"`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var b strings.Builder
			b.WriteString("agent a:\n  model: \"m\"\n  codemode: on\n")
			if c.nodeBackend != "" {
				fmt.Fprintf(&b, "  backend: %s\n", c.nodeBackend)
			}
			if c.fallbackBackend != "" {
				fmt.Fprintf(&b, "  fallbacks:\n    f:\n      backend: %s\n      model: \"m\"\n", c.fallbackBackend)
			}
			b.WriteString("\nworkflow w:\n  entry: a\n")
			if c.workflowDefault != "" {
				fmt.Fprintf(&b, "  default_backend: %s\n", c.workflowDefault)
			}
			b.WriteString("  a -> done\n")

			r := compileFile(t, b.String())
			var msgs []string
			for _, d := range r.Diagnostics {
				if d.Code == DiagCodemodeBackendUnsupported {
					msgs = append(msgs, d.Message)
				}
			}
			if len(msgs) == 0 {
				t.Fatalf("no C339 to judge — the fixture stopped exercising the clause:\n%s\n%+v", b.String(), r.Diagnostics)
			}
			got := false
			for _, m := range msgs {
				if strings.Contains(m, clause) {
					got = true
				}
			}
			if got != c.want {
				t.Errorf("the dial clause is %v, want %v:\n%s", got, c.want, strings.Join(msgs, "\n"))
			}
		})
	}
}

// TestThePickerRefusesWhatTheDispatchRefuses holds the launch half of C339.
// A launch override beats everything the source wrote, so it lands on a route
// the compiler never judged; the preview is then the only screen before a
// dispatch that raises ErrCapabilityUnsupported, and an adversarial round
// found it offering all three refusing backends as selectable with an empty
// unavailable_reason.
//
// The matrix walks the registry rather than naming backends, so the answer
// follows the cells the engine reads.
func TestThePickerRefusesWhatTheDispatchRefuses(t *testing.T) {
	r := compileFile(t, `
agent on_node:
  model: "m"
  codemode: on

agent off_node:
  model: "m"
  codemode: off

workflow w:
  entry: on_node
  on_node -> off_node
  off_node -> done
`)
	wf := r.Workflow
	if wf == nil {
		t.Fatalf("the fixture did not compile: %+v", r.Diagnostics)
	}
	var served, refused int
	for _, backend := range backendcaps.Names() {
		on := wf.Nodes["on_node"].(LLMNode)
		off := wf.Nodes["off_node"].(LLMNode)

		gotOn := CodemodeOverrideRefusalReason(backend, on, wf)
		// The DISPATCH'S question: this screen stands between an operator
		// and a run, so it must answer what the executor answers. Restating
		// the compiler's predicate here is what let the first draft offer
		// every name the registry does not carry.
		wantRefused := !CodemodeServedBy(backend)
		if (gotOn != "") != wantRefused {
			t.Errorf("backend %q (cell %q): the picker refuses=%v, the dispatch refuses=%v",
				backend, backendcaps.Verdict(backend, backendcaps.Codemode), gotOn != "", wantRefused)
		}
		if wantRefused {
			refused++
			for _, want := range []string{backend, "codemode: on", "claw"} {
				if !strings.Contains(gotOn, want) {
					t.Errorf("the picker's reason does not name %q: %q", want, gotOn)
				}
			}
		} else {
			served++
		}
		// A node that declared `off` is not refused anywhere: the screen
		// follows the DECLARED value, not the capability.
		if got := CodemodeOverrideRefusalReason(backend, off, wf); got != "" {
			t.Errorf("backend %q refuses a node that declared codemode: off: %q", backend, got)
		}
	}
	if served == 0 || refused == 0 {
		t.Fatalf("the registry gave %d serving and %d refusing backend(s); this test needs both arms", served, refused)
	}

	// A name the registry does not carry is the class that split the two
	// questions: the compiler stays silent about it and the dispatch
	// refuses it, so the picker must refuse it too.
	outside := "an_operators_own_backend"
	if CodemodeRefusedBy(outside) || CodemodeServedBy(outside) {
		t.Fatalf("%q is no longer outside the registry; this arm needs a name that is", outside)
	}
	if got := CodemodeOverrideRefusalReason(outside, wf.Nodes["on_node"].(LLMNode), wf); got == "" {
		t.Error("the picker offers a backend the registry does not carry; the dispatch refuses it")
	}
}
