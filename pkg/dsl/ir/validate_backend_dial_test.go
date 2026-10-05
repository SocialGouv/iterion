package ir

import (
	"fmt"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// #1389 — a backend written as a dial is screened by what it RESOLVES to
// ---------------------------------------------------------------------------

// The SOURCE reading: a dial is read by its DEFAULT, never by the shell
// that happens to be compiling. A verdict that moved with the ambient
// environment would compile one artifact two ways — and the compiler runs
// in the server and the runner pods, neither of which dispatches the node.
func TestSourceBackendReading(t *testing.T) {
	t.Setenv("C1389_SET", "kimi")
	cases := []struct{ in, want string }{
		// The dial's default decides, whatever the shell says.
		{"${C1389_SET:-claw}", "claw"},
		{"${C1389_SET}", ""},
		{"claw", "claw"},
		{"claude_code", "claude_code"},
		{"  claw  ", "claw"},
		// The whole point: a dial's default IS the backend, the same
		// reading resolveBackendName and runtime.backendIsClaw take.
		{"${C1389_UNSET:-claude_code}", "claude_code"},
		{"${C1389_UNSET:-claw}", "claw"},
		{"${C1389_UNSET:-${C1389_ALSO_UNSET:-grok}}", "grok"},
		// No opinion: the run decides these.
		{"", ""},
		{"auto", ""},
		{"${C1389_UNSET:-auto}", ""},
		{"${C1389_UNSET}", ""},
		// A `{{vars.x}}` is resolved against the RUN's vars, which a
		// launch may override — the source text does not decide it.
		{"{{vars.backend}}", ""},
		// A reference the expansion could not close is not a backend
		// name: `${` surviving the read means nothing answered it.
		{"${", ""},
		{"${C1389_UNSET:-${", ""},
	}
	for _, c := range cases {
		if got := sourceBackend.name(c.in); got != c.want {
			t.Errorf("sourceBackend.name(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The RUN reading, used by the launch-time screen only: there the process
// environment IS the route, because that process dispatches the node.
func TestRunBackendReading(t *testing.T) {
	t.Setenv("C1389_SET", "kimi")
	cases := []struct{ in, want string }{
		{"${C1389_SET:-claw}", "kimi"},
		{"${C1389_SET}", "kimi"},
		{"${C1389_UNSET:-claw}", "claw"},
		{"${C1389_UNSET}", ""},
		{"claw", "claw"},
		{"auto", ""},
		{"{{vars.backend}}", ""},
	}
	for _, c := range cases {
		if got := runBackend.name(c.in); got != c.want {
			t.Errorf("runBackend.name(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A field PRESENT but undecided here is not an ABSENT field. The runtime
// falls through to `default_backend:` only on "" and `auto`; on a
// `{{vars.x}}` or a `${X}` nothing answers it hands the text on as the
// backend name. Substituting the workflow default for those screened a
// backend the run will never use — and turned a valid workflow INVALID
// on a crossing that does not exist.
func TestEffectiveNodeBackend_DoesNotSubstituteForAnUndecidedField(t *testing.T) {
	cases := []struct{ node, wf, want string }{
		{"{{vars.b}}", "claw", ""},
		{"${C1389_UNSET}", "claw", ""},
		{"${", "claw", ""},
		// Absent or `auto`: the chain continues, as it always did.
		{"", "claw", "claw"},
		{"auto", "claw", "claw"},
		// And a workflow default the source does not decide is not a
		// backend either.
		{"", "{{vars.b}}", ""},
	}
	for _, c := range cases {
		if got := effectiveNodeBackend(c.node, c.wf); got != c.want {
			t.Errorf("effectiveNodeBackend(%q, %q) = %q, want %q", c.node, c.wf, got, c.want)
		}
	}
}

// The whole-compiler consequence of the rule above, on the shape the
// runtime already supports and tests (routing_template_test.go): a node
// on `{{vars.b}}` = claude_code, a workflow default of claw, and a route
// to claude_code is NOT a crossing — the node and the route are the same
// backend at run time.
func TestTemplatedNodeBackendIsNotScreenedAsTheWorkflowDefault(t *testing.T) {
	src := "vars:\n  b: string = \"claude_code\"\n\n" +
		"agent a:\n  backend: \"{{vars.b}}\"\n  model: \"claude-opus-5\"\n  system: p\n  tools: [read_file]\n" +
		"  fallbacks:\n    cli:\n      backend: \"claude_code\"\n      model: \"claude-opus-5\"\n" +
		"\nprompt p:\n  hi\n\nworkflow w:\n  default_backend: \"claw\"\n  entry: a\n  a -> done\n"
	if got := countCode(compileFallbackSrc(t, src), DiagFallbackUnsafeCross); got != 0 {
		t.Errorf("C176 count = %d, want 0 — the node's own backend is a template, not the workflow default", got)
	}
}

// The runtime precedence, with dials at both levels: a node dial that
// answers nothing falls through to the workflow default, exactly as
// resolveBackendName does when resolveRoutingField comes back empty.
func TestEffectiveNodeBackend_WithDials(t *testing.T) {
	cases := []struct{ node, wf, want string }{
		{"${C1389_UNSET:-claw}", "claude_code", "claw"},
		{"", "${C1389_UNSET:-claw}", "claw"},
		{"auto", "${C1389_UNSET:-kimi}", "kimi"},
		{"${C1389_UNSET:-auto}", "claude_code", "claude_code"},
		// A node dial with no default is a choice the SOURCE does not
		// make: it does not fall through — see
		// TestEffectiveNodeBackend_DoesNotSubstituteForAnUndecidedField.
		{"${C1389_UNSET}", "${C1389_UNSET:-grok}", ""},
		{"${C1389_UNSET}", "${C1389_ALSO_UNSET}", ""},
	}
	for _, c := range cases {
		if got := effectiveNodeBackend(c.node, c.wf); got != c.want {
			t.Errorf("effectiveNodeBackend(%q, %q) = %q, want %q", c.node, c.wf, got, c.want)
		}
	}
}

// dialSpellings renders one source twice — every backend written as a
// literal, then every backend written as a dial with that literal as its
// default. The property is that the compiler answers the SAME thing:
// spelling a backend as an env dial must not change a verdict.
func dialSpellings(src string) (literal, dialled string) {
	var b strings.Builder
	n := 0
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		for _, key := range []string{"backend: \"", "default_backend: \""} {
			if strings.HasPrefix(trimmed, key) && strings.HasSuffix(trimmed, "\"") {
				value := trimmed[len(key) : len(trimmed)-1]
				indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
				line = fmt.Sprintf("%s%s${C1389_DIAL_%d:-%s}\"", indent, key, n, value)
				n++
				break
			}
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return src, b.String()
}

// The screens a `""` backend short-circuited: the tools inversion, the
// session-continuity refusal, the primary-route gate check and the
// effort drift. Each is exercised on a source whose backends are then
// re-spelled as dials — the verdicts must not move.
func TestBackendDialKeepsEveryCrossingScreen(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want DiagCode
	}{
		{
			name: "tools inversion: a claw node with no tools: list crossing to a CLI backend",
			src: "agent x:\n  backend: \"claw\"\n  model: \"anthropic/claude-sonnet-4-6\"\n  system: p\n" +
				"  fallbacks:\n    cli:\n      backend: \"claude_code\"\n      model: \"claude-opus-5\"\n" +
				"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n",
			want: DiagFallbackUnsafeCross,
		},
		{
			name: "session continuity across a backend change",
			src: "agent x:\n  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  system: p\n  session: persist\n  tools: [bash]\n" +
				"  fallbacks:\n    c:\n      backend: \"claw\"\n      model: \"anthropic/claude-sonnet-4-6\"\n" +
				"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n",
			want: DiagFallbackUnsafeCross,
		},
		{
			name: "the PRIMARY route cannot enforce the gate",
			src: "agent x:\n  backend: \"codex\"\n  model: \"gpt-5\"\n  system: p\n  permission: deny\n  tools: [read_file]\n" +
				"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n",
			want: DiagFallbackUnsafeCross,
		},
		{
			name: "effort drift onto a backend with no dial",
			src: "agent x:\n  backend: \"claw\"\n  model: \"anthropic/claude-sonnet-4-6\"\n  system: p\n  reasoning_effort: high\n  tools: [bash]\n" +
				"  fallbacks:\n    k:\n      backend: \"kimi\"\n      model: \"kimi-code/k3\"\n" +
				"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n",
			want: DiagFallbackDrift,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			literal, dialled := dialSpellings(tc.src)
			lit := countCode(compileFallbackSrc(t, literal), tc.want)
			if lit == 0 {
				t.Fatalf("the literal spelling does not fire %s — the case proves nothing\n%s", tc.want, literal)
			}
			if got := countCode(compileFallbackSrc(t, dialled), tc.want); got != lit {
				t.Errorf("%s: literal spelling = %d, dialled spelling = %d — a dial turned the screen off\n%s",
					tc.want, lit, got, dialled)
			}
		})
	}
}

// The launch-time route takes the same reading: an operator's
// `--fallback` is screened against what the node's dial resolves to, and
// against what the route's own dial resolves to.
func TestApplyRunFallback_ReadsDialsOnBothSides(t *testing.T) {
	src := "agent x:\n  backend: \"${C1389_UNSET:-claw}\"\n  model: \"anthropic/claude-sonnet-4-6\"\n  system: p\n" +
		"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n"
	// A FRESH workflow per route: ApplyRunFallback writes the accepted
	// route into the IR, and a node that carries one is then skipped —
	// so a second call on the same workflow answers about nothing.
	fresh := func() *Workflow {
		w := compileFallbackSrc(t, src).Workflow
		if w == nil {
			t.Fatal("the fixture does not compile")
		}
		return w
	}
	// A CLI route crosses the boundary — spelled either way.
	for _, route := range []Fallback{
		{Backend: "claude_code", Model: "claude-opus-5"},
		{Backend: "${C1389_UNSET:-claude_code}", Model: "claude-opus-5"},
	} {
		refusals := ApplyRunFallback(fresh(), []Fallback{route}, false, nil, nil)
		if len(refusals) != 1 {
			t.Errorf("route %q: %d refusals, want 1 (the claw node has no tools: list)", route.Backend, len(refusals))
			continue
		}
		if !strings.Contains(refusals[0], "routes a claw node to a CLI backend") {
			t.Errorf("route %q refused for the wrong reason: %s", route.Backend, refusals[0])
		}
	}
	// A route to the SAME backend is not a crossing — and reading the
	// route by its spelling made `${…:-claw}` look like a different
	// backend from `claw`, refusing a route that changes nothing.
	for _, route := range []Fallback{
		{Backend: "claw", Model: "anthropic/glm-5.2"},
		{Backend: "${C1389_UNSET:-claw}", Model: "anthropic/glm-5.2"},
	} {
		if refusals := ApplyRunFallback(fresh(), []Fallback{route}, false, nil, nil); len(refusals) != 0 {
			t.Errorf("route %q to the node's OWN backend was refused: %v", route.Backend, refusals)
		}
	}
}

// The two screens outside validateFallbacks that read a ROUTE's backend:
// the async-capability refusal and the unresolvable-tools report. Both
// compared a spelling, so a dialled route escaped them as well.
func TestBackendDialIsReadByTheOtherRouteScreens(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want DiagCode
	}{
		{
			name: "async: a route to a backend with no async question tools",
			src: "agent a:\n  backend: claude_code\n  model: \"m\"\n  interaction: async\n  tools: [bash]\n" +
				"  fallbacks:\n    backup:\n      backend: \"${C1389_UNSET:-codex}\"\n      model: \"n\"\n" +
				"workflow w:\n  entry: a\n  a -> done\n",
			want: DiagAsyncBackendUnsupported,
		},
		{
			name: "tools: a claw route cannot resolve a name the node declares",
			src: "agent a:\n  backend: claude_code\n  model: \"m\"\n  tools: [run_command]\n" +
				"  fallbacks:\n    backup:\n      backend: \"${C1389_UNSET:-claw}\"\n      model: \"anthropic/glm-5.2\"\n" +
				"workflow w:\n  entry: a\n  a -> done\n",
			want: DiagUnknownTool,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The route above is written as a dial; the literal oracle is
			// the same source with that dial written out.
			literal := strings.NewReplacer(
				"${C1389_UNSET:-codex}", "codex",
				"${C1389_UNSET:-claw}", "claw",
			).Replace(tc.src)
			lit := countCode(compileFallbackSrc(t, literal), tc.want)
			if lit == 0 {
				t.Fatalf("the literal spelling does not fire %s — the case proves nothing\n%s", tc.want, literal)
			}
			if got := countCode(compileFallbackSrc(t, tc.src), tc.want); got != lit {
				t.Errorf("%s: literal spelling = %d, dialled spelling = %d — a dial turned the screen off\n%s",
					tc.want, lit, got, tc.src)
			}
		})
	}
}

// The sandbox-coupling warning (C136) also reads a route's backend: an
// external-hook gate backend needs a host-side run, and a dialled route to
// one used to look like an unrecognised name.
func TestBackendDialIsReadByTheSandboxCouplingWarning(t *testing.T) {
	src := "agent a:\n  backend: claude_code\n  model: \"m\"\n  permission: deny\n  tools: [read_file]\n" +
		"  fallbacks:\n    backup:\n      backend: \"${C1389_UNSET:-grok}\"\n      model: \"grok-4\"\n" +
		"workflow w:\n  entry: a\n  a -> done\n"
	literal := strings.ReplaceAll(src, "${C1389_UNSET:-grok}", "grok")

	lit := countCode(compileFallbackSrc(t, literal), DiagGatedCLIBackendSandbox)
	if lit == 0 {
		t.Fatalf("the literal spelling does not fire C136 — the case proves nothing\n%s", literal)
	}
	if got := countCode(compileFallbackSrc(t, src), DiagGatedCLIBackendSandbox); got != lit {
		t.Errorf("C136: literal spelling = %d, dialled spelling = %d — a dial turned the warning off", lit, got)
	}
}

// The compile verdict must not move with the shell. `ir.Compile` runs in
// the server pod, in the runner pod and on a laptop; a dial read from
// whichever environment happens to be compiling gives one artifact three
// verdicts, and turns a required CI check red on a file nobody touched.
func TestCompileVerdictIgnoresTheAmbientEnvironment(t *testing.T) {
	src := "agent a:\n  backend: \"${C1389_GATE:-claude_code}\"\n  model: \"m\"\n  permission: deny\n  tools: [read_file]\n" +
		"\nprompt p:\n  hi\n\nworkflow w:\n  entry: a\n  a -> done\n"
	// The dial's default can enforce the gate: clean.
	if got := countCode(compileFallbackSrc(t, src), DiagFallbackUnsafeCross); got != 0 {
		t.Fatalf("C176 count = %d with no env set, want 0 — the fixture is wrong", got)
	}
	// Setting the dial to a backend that CANNOT must not change the
	// verdict of the source: that decision belongs to the launch screen,
	// which runs where the dial is actually read.
	t.Setenv("C1389_GATE", "codex")
	if got := countCode(compileFallbackSrc(t, src), DiagFallbackUnsafeCross); got != 0 {
		t.Errorf("C176 count = %d with the dial set in the environment, want 0 — the compile verdict moved with the shell", got)
	}
	// The launch-time screen DOES read it: that process dispatches the node.
	if got := runBackend.name("${C1389_GATE:-claude_code}"); got != "codex" {
		t.Errorf("runBackend.name = %q, want %q — the launch screen must read the environment it runs in", got, "codex")
	}
}

// The three route screens whose literal and dialled answers DIFFER, each
// with the direction that separates them: the count-equality oracle above
// cannot move when both spellings give the same answer.
func TestBackendDialChangesTheseVerdicts(t *testing.T) {
	head := func(props, fallbacks string) string {
		return "agent a:\n  backend: \"claw\"\n  model: \"anthropic/claude-sonnet-4-6\"\n  system: p\n" +
			props + "  fallbacks:\n" + fallbacks +
			"\nprompt p:\n  hi\n\nworkflow w:\n  entry: a\n  a -> done\n"
	}
	t.Run("C177: a dialled route to a backend that HAS the effort dial does not drift", func(t *testing.T) {
		src := head("  reasoning_effort: high\n  tools: [bash]\n",
			"    other:\n      backend: \"${C1389_UNSET:-pi}\"\n      model: \"anthropic/claude-sonnet-4-6\"\n")
		if got := countCode(compileFallbackSrc(t, src), DiagFallbackDrift); got != 0 {
			t.Errorf("C177 count = %d, want 0 — pi carries a reasoning-effort dial, dialled or not", got)
		}
	})
	t.Run("C136: a dialled claw ROUTE is an Ask the sandbox cannot pause for", func(t *testing.T) {
		// The node itself is NOT claw: only the route can raise this, so
		// the route arm is the one under test.
		src := "agent a:\n  backend: \"claude_code\"\n  model: \"m\"\n  system: p\n  permission: ask\n  tools: [bash]\n" +
			"  fallbacks:\n    c:\n      backend: \"${C1389_UNSET:-claw}\"\n      model: \"anthropic/glm-5.2\"\n" +
			"\nprompt p:\n  hi\n\nworkflow w:\n  entry: a\n  a -> done\n"
		if got := countCode(compileFallbackSrc(t, src), DiagGatedCLIBackendSandbox); got == 0 {
			t.Errorf("C136 count = 0, want ≥ 1 — a dialled claw route cannot pause for an Ask either")
		}
	})
	t.Run("C047: memory on a dialled claw node is not warned about", func(t *testing.T) {
		src := "agent a:\n  backend: \"${C1389_UNSET:-claw}\"\n  model: \"anthropic/glm-5.2\"\n  system: p\n  tools: [bash]\n" +
			"  memory:\n    enabled: true\n    scope: \"notes\"\n" +
			"\nprompt p:\n  hi\n\nworkflow w:\n  entry: a\n  a -> done\n"
		if got := countCode(compileFallbackSrc(t, src), DiagMemoryNotSupported); got != 0 {
			t.Errorf("C047 count = %d, want 0 — the node IS claw, and `memory:` is claw's", got)
		}
		// The same node on a backend that really does not consume it.
		off := strings.ReplaceAll(src, "${C1389_UNSET:-claw}", "${C1389_UNSET:-claude_code}")
		if got := countCode(compileFallbackSrc(t, off), DiagMemoryNotSupported); got == 0 {
			t.Error("C047 count = 0 on a dialled claude_code node, want ≥ 1 — the warning must still reach a dial")
		}
	})
	t.Run("C173: a dialled route that changes backend still needs its own model", func(t *testing.T) {
		src := head("  tools: [bash]\n", "    cli:\n      backend: \"${C1389_UNSET:-claude_code}\"\n")
		if got := countCode(compileFallbackSrc(t, src), DiagFallbackMalformed); got == 0 {
			t.Errorf("C173 count = 0, want ≥ 1 — a model spec is no more portable through a dial")
		}
	})
	t.Run("C173: a route the source does not decide inherits at run time", func(t *testing.T) {
		src := head("  tools: [bash]\n", "    cli:\n      backend: \"${C1389_UNSET}\"\n")
		if got := countCode(compileFallbackSrc(t, src), DiagFallbackMalformed); got != 0 {
			t.Errorf("C173 count = %d, want 0 — nothing here says the route changes backend", got)
		}
	})
}

// The sandbox refusal of the launch route: the codex CLI cannot run inside
// the sandbox, and the route used to be recognised by its spelling — so a
// dialled one was accepted and died at dispatch, which is what the guard
// exists to prevent.
func TestApplyRunFallback_RefusesADialledCodexRouteInASandbox(t *testing.T) {
	src := "agent a:\n  backend: \"claude_code\"\n  model: \"m\"\n  system: p\n  tools: [read_file]\n" +
		"\nprompt p:\n  hi\n\nworkflow w:\n  entry: a\n  a -> done\n"
	for _, spelling := range []string{"codex", "${C1389_UNSET:-codex}"} {
		w := compileFallbackSrc(t, src).Workflow
		if w == nil {
			t.Fatal("the fixture does not compile")
		}
		refusals := ApplyRunFallback(w, []Fallback{{Backend: spelling, Model: "gpt-5"}}, true, nil, nil)
		if len(refusals) != 1 {
			t.Errorf("route %q in a sandbox: %d refusals, want 1", spelling, len(refusals))
			continue
		}
		if !strings.Contains(refusals[0], "codex CLI") {
			t.Errorf("route %q refused for the wrong reason: %s", spelling, refusals[0])
		}
	}
}

// C088: a provider chain has no effect on a backend that ignores the
// hint. The backend was read raw, so a dialled node escaped the warning
// (and a dialled claude_code node would have escaped nothing it should).
func TestBackendDialIsReadByTheProviderHintWarning(t *testing.T) {
	src := func(backend string) string {
		return "agent a:\n  backend: \"" + backend + "\"\n  model: \"anthropic/claude-sonnet-4-6\"\n  system: p\n" +
			"  provider: \"anthropic,zai\"\n  tools: [bash]\n" +
			"\nprompt p:\n  hi\n\nworkflow w:\n  entry: a\n  a -> done\n"
	}
	// claw ignores the hint: the warning must reach a dialled node too.
	if got := countCode(compileFallbackSrc(t, src("claw")), DiagProviderChainIgnored); got == 0 {
		t.Fatal("the literal spelling does not fire C088 — the case proves nothing")
	}
	if got := countCode(compileFallbackSrc(t, src("${C1389_UNSET:-claw}")), DiagProviderChainIgnored); got == 0 {
		t.Error("C088 count = 0 on a dialled claw node, want ≥ 1 — a dial turned the warning off")
	}
	// claude_code consumes it, dialled or not.
	if got := countCode(compileFallbackSrc(t, src("${C1389_UNSET:-claude_code}")), DiagProviderChainIgnored); got != 0 {
		t.Errorf("C088 count = %d on a dialled claude_code node, want 0", got)
	}
}

// A ROUTE spelled `auto` is not "the chain decides": resolveChain
// normalises `auto` on a route's `provider:` and never on its `backend:`,
// so the registry is asked for a backend nobody registered and the route
// dies at the moment the chain is needed. Reading it as a step took every
// screen off it.
func TestRouteSpelledAutoKeepsItsScreens(t *testing.T) {
	src := func(route string) string {
		return "agent a:\n  backend: \"claw\"\n  model: \"anthropic/claude-sonnet-4-6\"\n  system: p\n" +
			"  fallbacks:\n    r:\n      backend: \"" + route + "\"\n" +
			"\nprompt p:\n  hi\n\nworkflow w:\n  entry: a\n  a -> done\n"
	}
	for _, spelling := range []string{"auto", "${C1389_UNSET:-auto}"} {
		r := compileFallbackSrc(t, src(spelling))
		if got := countCode(r, DiagFallbackMalformed); got == 0 {
			t.Errorf("route %q: C173 count = 0, want ≥ 1 — a route that changes backend needs its own model", spelling)
		}
		if got := countCode(r, DiagFallbackUnsafeCross); got == 0 {
			t.Errorf("route %q: C176 count = 0, want ≥ 1 — the crossing screens must still see it", spelling)
		}
	}
	// On a NODE, `auto` IS a step of the chain: the workflow default
	// decides, and that is what the runtime does.
	if got := effectiveNodeBackend("auto", "claw"); got != "claw" {
		t.Errorf("a node's `auto` = %q, want the workflow default", got)
	}
}

// A bare `$VAR` is not a backend called `$VAR`. Naming it one refused a
// gated workflow that was fine, and passed a claw⇄CLI crossing that was
// not — in both directions, silently.
func TestBareDollarBackendIsNotABackendName(t *testing.T) {
	if got, decides := sourceBackend.field("$C1389_BARE"); got != "" || decides {
		t.Errorf(`sourceBackend.field("$C1389_BARE") = (%q, %v), want ("", false)`, got, decides)
	}
	if got := sourceBackend.routeName("$C1389_BARE"); got != "" {
		t.Errorf(`sourceBackend.routeName("$C1389_BARE") = %q, want ""`, got)
	}
	// The gated workflow that used to be refused: the compiler cannot
	// name the backend, so it must not claim the gate is lost.
	src := "agent a:\n  backend: \"$C1389_BARE\"\n  model: \"m\"\n  permission: deny\n  tools: [read_file]\n" +
		"\nprompt p:\n  hi\n\nworkflow w:\n  default_backend: \"claw\"\n  entry: a\n  a -> done\n"
	if got := countCode(compileFallbackSrc(t, src), DiagFallbackUnsafeCross); got != 0 {
		t.Errorf("C176 count = %d, want 0 — `$C1389_BARE` is not a backend that cannot enforce a gate", got)
	}
	// The RUN reading answers it, because there the environment IS the route.
	t.Setenv("C1389_BARE", "codex")
	if got := runBackend.name("$C1389_BARE"); got != "codex" {
		t.Errorf("runBackend.name = %q, want %q", got, "codex")
	}
}

// The launch screen reads the environment it runs in, and the compile
// screen does not: a case where the two readings DISAGREE is the only one
// that can tell them apart.
func TestApplyRunFallback_UsesTheRunReadingNotTheSourceOne(t *testing.T) {
	t.Setenv("C1389_LAUNCH", "claude_code")
	src := "agent a:\n  backend: \"${C1389_LAUNCH:-claw}\"\n  model: \"anthropic/claude-sonnet-4-6\"\n  system: p\n" +
		"\nprompt p:\n  hi\n\nworkflow w:\n  entry: a\n  a -> done\n"
	w := compileFallbackSrc(t, src).Workflow
	if w == nil {
		t.Fatal("the fixture does not compile")
	}
	// The SOURCE says claw; the RUN says claude_code. A claw route is
	// then a CLI→claw crossing onto a node with no tools: list, which is
	// the refusal — and it is invisible to the source reading.
	if got := sourceBackend.name("${C1389_LAUNCH:-claw}"); got != "claw" {
		t.Fatalf("the source reading = %q, want claw — the fixture proves nothing", got)
	}
	refusals := ApplyRunFallback(w, []Fallback{{Backend: "claw", Model: "anthropic/glm-5.2"}}, false, nil, nil)
	if len(refusals) != 1 {
		t.Fatalf("%d refusals, want 1: %v", len(refusals), refusals)
	}
	if !strings.Contains(refusals[0], "crosses the claw⇄CLI boundary") {
		t.Errorf("refused for the wrong reason: %s", refusals[0])
	}

	// The ROUTE's own dial, the other side of the same seam: a claw node
	// and a route whose dial the environment points at a CLI backend. The
	// source reading calls the route claw and sees no crossing; the run
	// reading sees the one that will happen.
	t.Setenv("C1389_LAUNCH_ROUTE", "claude_code")
	clawNode := compileFallbackSrc(t, "agent a:\n  backend: \"claw\"\n  model: \"anthropic/claude-sonnet-4-6\"\n  system: p\n"+
		"\nprompt p:\n  hi\n\nworkflow w:\n  entry: a\n  a -> done\n").Workflow
	if clawNode == nil {
		t.Fatal("the fixture does not compile")
	}
	if got := sourceBackend.routeName("${C1389_LAUNCH_ROUTE:-claw}"); got != "claw" {
		t.Fatalf("the source reading of the route = %q, want claw — the fixture proves nothing", got)
	}
	routed := ApplyRunFallback(clawNode, []Fallback{{Backend: "${C1389_LAUNCH_ROUTE:-claw}", Model: "claude-opus-5"}}, false, nil, nil)
	if len(routed) != 1 {
		t.Fatalf("%d refusals for a dialled route, want 1: %v", len(routed), routed)
	}
	if !strings.Contains(routed[0], "routes a claw node to a CLI backend") {
		t.Errorf("the dialled route refused for the wrong reason: %s", routed[0])
	}
}

// The `default_backend:` fall-through the memory, provider-hint and
// auto-memory screens gained: a node that names no backend of its own
// runs on the workflow's, and the warning must read that one.
func TestWorkflowDefaultReachesTheNodeLevelScreens(t *testing.T) {
	src := func(def, extra string) string {
		return "agent a:\n  model: \"anthropic/claude-sonnet-4-6\"\n  system: p\n  tools: [bash]\n" + extra +
			"\nprompt p:\n  hi\n\nworkflow w:\n  default_backend: \"" + def + "\"\n  entry: a\n  a -> done\n"
	}
	mem := "  memory:\n    enabled: true\n    scope: \"notes\"\n"
	if got := countCode(compileFallbackSrc(t, src("claude_code", mem)), DiagMemoryNotSupported); got == 0 {
		t.Error("C047 count = 0 with default_backend: claude_code, want ≥ 1 — the node runs on the workflow default")
	}
	if got := countCode(compileFallbackSrc(t, src("${C1389_UNSET:-claw}", mem)), DiagMemoryNotSupported); got != 0 {
		t.Errorf("C047 count = %d with a dialled claw default, want 0", got)
	}
	hint := "  provider: \"anthropic,zai\"\n"
	if got := countCode(compileFallbackSrc(t, src("claw", hint)), DiagProviderChainIgnored); got == 0 {
		t.Error("C088 count = 0 with default_backend: claw, want ≥ 1")
	}
	if got := countCode(compileFallbackSrc(t, src("claude_code", hint)), DiagProviderChainIgnored); got != 0 {
		t.Errorf("C088 count = %d with default_backend: claude_code, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// #1606 — a backend written as `{{vars.x}}` is screened by what the LAUNCH
// resolves, the way a dial is screened by what the environment resolves
// ---------------------------------------------------------------------------

// The RUN reading with the launch's vars: the template resolves first, the
// field's own `${…}` expansion after — resolveRoutingField's order — and a
// reference the launch cannot answer stays a field nothing decided. A nil
// view is the compiler's reading, unchanged.
func TestRunBackendReading_WithVars(t *testing.T) {
	t.Setenv("C1606_SET", "kimi")
	view := map[string]any{
		"b":    "claw",
		"dial": "${C1606_SET:-claw}",
		"raw":  "${C1606_SET}",
	}
	run := runBackend.withVars(view)
	cases := []struct{ in, want string }{
		{"{{vars.b}}", "claw"},
		{"{{ vars.b }}", "claw"},
		// The template-then-env order: the var's VALUE is expanded as the
		// field's own text would be, by default and by the environment.
		{"{{vars.dial}}", "kimi"},
		{"{{vars.raw}}", "kimi"},
		// What the view cannot answer is not a backend name.
		{"{{vars.missing}}", ""},
		{"{{vars.cfg.on}}", ""},
		// A literal and a dial read exactly as without the view — vars do
		// not leak into a field that names no var.
		{"claw", "claw"},
		{"${C1606_UNSET:-claw}", "claw"},
	}
	for _, c := range cases {
		if got := run.name(c.in); got != c.want {
			t.Errorf("runBackend.withVars(view).name(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The launch vars view: the declared defaults under the launch's overrides,
// and a launch value for a var the workflow does not declare is dropped —
// the two rules resolveVars runs the run itself by.
func TestLaunchVarsView(t *testing.T) {
	w := &Workflow{Vars: map[string]*Var{
		"b":         {Name: "b", Type: VarString, HasDefault: true, Default: "claw"},
		"nodefault": {Name: "nodefault", Type: VarString},
		"n":         {Name: "n", Type: VarInt, HasDefault: true, Default: int64(3)},
	}}
	if got := launchVarsView(&Workflow{}, map[string]string{"b": "x"}); got != nil {
		t.Errorf("no vars declared: view = %v, want nil — nothing may change hands", got)
	}
	view := launchVarsView(w, map[string]string{"b": "codex", "nodefault": "claude_code", "zz": "grok"})
	// Values are typed as resolveVars types them — ResolveVarText is the
	// shared reading — so the int default is an int64, not its text.
	want := map[string]any{"b": "codex", "nodefault": "claude_code", "n": int64(3)}
	if len(view) != len(want) {
		t.Fatalf("view = %v, want %v", view, want)
	}
	for k, v := range want {
		if view[k] != v {
			t.Errorf("view[%q] = %v, want %v (full view %v)", k, view[k], v, view)
		}
	}
}

// The screen reads a `{{vars.x}}` node backend by what the launch resolves —
// the crossing refusals C176 applies to a literal apply to the reference, on
// the node's side and on the route's.
func TestApplyRunFallback_ReadsVarsOnBothSides(t *testing.T) {
	src := "vars:\n  b: string = \"claw\"\n  route_b: string = \"claude_code\"\n\n" +
		"agent x:\n  backend: \"{{vars.b}}\"\n  model: \"anthropic/claude-sonnet-4-6\"\n  system: p\n" +
		"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n"
	// A FRESH workflow per route: ApplyRunFallback writes the accepted route
	// into the IR, and a node that carries one is then skipped.
	fresh := func() *Workflow {
		w := compileFallbackSrc(t, src).Workflow
		if w == nil {
			t.Fatal("the fixture does not compile")
		}
		return w
	}
	// A CLI route crosses the boundary — spelled literally, or through a
	// var on the ROUTE's side.
	for _, route := range []Fallback{
		{Backend: "claude_code", Model: "claude-opus-5"},
		{Backend: "{{vars.route_b}}", Model: "claude-opus-5"},
	} {
		refusals := ApplyRunFallback(fresh(), []Fallback{route}, false, nil, nil)
		if len(refusals) != 1 {
			t.Errorf("route %q: %d refusals, want 1 (the claw node has no tools: list)", route.Backend, len(refusals))
			continue
		}
		if !strings.Contains(refusals[0], "routes a claw node to a CLI backend") {
			t.Errorf("route %q refused for the wrong reason: %s", route.Backend, refusals[0])
		}
	}
	// A route to the SAME backend is not a crossing.
	if refusals := ApplyRunFallback(fresh(), []Fallback{{Backend: "claw", Model: "anthropic/glm-5.2"}}, false, nil, nil); len(refusals) != 0 {
		t.Errorf("a route to the node's own (vars-resolved) backend was refused: %v", refusals)
	}
}

// The launch's override, not the declared default, is the run's reading:
// the same source screens as claude_code when `--var b=claude_code` launches
// it, and as claw when `--var b=claw` does.
func TestApplyRunFallback_LaunchVarDecidesWhatTheSourceLeftOpen(t *testing.T) {
	src := func(def string) string {
		return "vars:\n  b: string = \"" + def + "\"\n\n" +
			"agent x:\n  backend: \"{{vars.b}}\"\n  model: \"anthropic/claude-sonnet-4-6\"\n  system: p\n" +
			"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n"
	}
	fresh := func(def string) *Workflow {
		w := compileFallbackSrc(t, src(def)).Workflow
		if w == nil {
			t.Fatal("the fixture does not compile")
		}
		return w
	}
	// The source's default is claw, the launch says claude_code: the run
	// dispatches claude_code, so a claude_code route crosses nothing.
	agent := fresh("claw")
	if refusals := ApplyRunFallback(agent, []Fallback{{Backend: "claude_code", Model: "claude-opus-5"}}, false,
		map[string]string{"b": "claude_code"}, nil); len(refusals) != 0 {
		t.Errorf("the launch's reading (claude_code) was overridden by the source's default: %v", refusals)
	}
	// And the other direction: the source's default is claude_code, the
	// launch says claw — a CLI route on the tools-less claw node is the
	// refusal, and it is invisible to whoever reads only the default.
	refusals := ApplyRunFallback(fresh("claude_code"), []Fallback{{Backend: "claude_code", Model: "claude-opus-5"}}, false,
		map[string]string{"b": "claw"}, nil)
	if len(refusals) != 1 {
		t.Fatalf("%d refusals, want 1: %v", len(refusals), refusals)
	}
	if !strings.Contains(refusals[0], "routes a claw node to a CLI backend") {
		t.Errorf("refused for the wrong reason: %s", refusals[0])
	}
}

// The session-continuity refusal is one of the four predicates that used to
// short-circuit on the empty reading: inherit across a backend change has no
// cross-backend meaning, vars-resolved or not.
func TestApplyRunFallback_SessionContinuityCrossingOnAVarsBackend(t *testing.T) {
	agent := applyAgent("work", "{{vars.b}}", "", []string{"read_file"}, nil)
	agent.Session = SessionInherit
	w := &Workflow{
		Nodes: map[string]Node{"work": agent},
		Vars:  map[string]*Var{"b": {Name: "b", Type: VarString, HasDefault: true, Default: "claude_code"}},
	}
	// A declared tools: list makes the CLI→claw direction no inversion, so
	// only the session predicate can fire.
	refusals := ApplyRunFallback(w, []Fallback{{Backend: "claw", Model: "anthropic/glm-5.2"}}, false, nil, nil)
	if len(refusals) == 0 {
		t.Fatal("no refusal — a session: inherit node changed backend through a vars-resolved crossing")
	}
	if !strings.Contains(refusals[0], "session continuity has no cross-backend meaning") {
		t.Errorf("refused for the wrong reason: %s", refusals[0])
	}
}

// What the launch cannot decide stays undecided — never a guess. A var the
// workflow does not declare is dropped by the run itself (resolveVars), so
// the screen must not resolve it either; a declared var with no default and
// no launch value resolves to nothing either. Both screen exactly as before
// the vars reached ApplyRunFallback.
func TestApplyRunFallback_WhatTheLaunchCannotAnswerStaysUndecided(t *testing.T) {
	src := "agent x:\n  backend: \"{{vars.zz}}\"\n  model: \"anthropic/claude-sonnet-4-6\"\n  system: p\n" +
		"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n"
	w := compileFallbackSrc(t, src).Workflow
	if w == nil {
		t.Fatal("the fixture does not compile")
	}
	// zz is undeclared: the screen must not resolve it, and dispatch does
	// not either — the run's vars never carry it. resolveVars drops it, and
	// the executor seeding takes the same rule (runview.BuildExecutor;
	// an undeclared key allowed through by --allow-unknown-inputs rides as
	// a run INPUT for subbot forwarding, never as a var — its witness is
	// TestBuildExecutor_UndeclaredLaunchVarDoesNotReachTheExecutorVars).
	// An undecided field does NOT fall through to default_backend: (that
	// rule is TestEffectiveNodeBackend_DoesNotSubstituteForAnUndecidedField's).
	refusals := ApplyRunFallback(w, []Fallback{{Backend: "claude_code", Model: "claude-opus-5"}}, false,
		map[string]string{"zz": "claw"}, nil)
	if len(refusals) != 0 {
		t.Errorf("an undeclared var resolved for the screen: %v", refusals)
	}
	if got := runBackend.withVars(launchVarsView(w, map[string]string{"zz": "claw"})).name("{{vars.zz}}"); got != "" {
		t.Errorf("undeclared {{vars.zz}} resolved to %q, want undecided", got)
	}
}

// The view reads a var's text the way resolveVars does at dispatch: through
// LookupEnv, the bot-vars overlay then the process environment (ADR-093). A
// view on another reading screens a backend the run never uses, or misses
// one it does.
func TestLaunchVarsView_ReadsVarTextThroughTheOverlayThenTheProcessEnv(t *testing.T) {
	SetEnvOverlay(func(name string) (string, bool) {
		if name == "ITERION_ZZPROBE_BACKEND" {
			return "claw", true
		}
		return "", false
	})
	defer SetEnvOverlay(nil)
	w := &Workflow{Vars: map[string]*Var{
		"b": {Name: "b", Type: VarString, HasDefault: true, Default: "${ITERION_ZZPROBE_BACKEND}"},
		"o": {Name: "o", Type: VarString},
	}}
	// The overlay says claw and the process environment nothing: dispatch
	// stores claw — the view must too.
	if got := launchVarsView(w, nil)["b"]; got != "claw" {
		t.Errorf("view[b] = %q, want claw — dispatch reads a stored bot var", got)
	}
	// Overrides take the same reading (resolveVars expands both), here with
	// the process env answering a name the overlay does not hold.
	t.Setenv("C1606_OVERRIDE", "kimi")
	if got := launchVarsView(w, map[string]string{"o": "${C1606_OVERRIDE}"})["o"]; got != "kimi" {
		t.Errorf("view[o] = %q, want kimi — an override's ${…} is expanded as the run expands it", got)
	}
}

// Dispatch expands TWICE: resolveVars expands the var's text, then
// resolveRoutingField expands the field the value lands in. The view stores
// the first expansion and the reader runs the second, so a value that is
// itself a reference resolves to the same backend on both sides.
func TestLaunchVarsView_ExpandsAsManyTimesAsDispatch(t *testing.T) {
	t.Setenv("C1606_INNER", "${C1606_OUTER}")
	t.Setenv("C1606_OUTER", "claw")
	w := &Workflow{Vars: map[string]*Var{
		"b": {Name: "b", Type: VarString, HasDefault: true, Default: "${C1606_INNER}"},
	}}
	got := runBackend.withVars(launchVarsView(w, nil)).name("{{vars.b}}")
	if got != "claw" {
		t.Errorf("run reading of {{vars.b}} = %q, want claw — dispatch expands the substituted value a second time", got)
	}
}

// The screen-level consequence: a var default written against an ITERION_
// name a stored bot var answers. Dispatch runs the node on claw, so the
// claude_code run-fallback route is the refusal it is on any claw node; with
// the name answered nowhere the node is undecided and nothing is refused.
func TestApplyRunFallback_VarsBackendReadThroughTheOverlay(t *testing.T) {
	src := "vars:\n  b: string = \"${ITERION_ZZPROBE_BACKEND}\"\n\n" +
		"agent x:\n  backend: \"{{vars.b}}\"\n  model: \"anthropic/claude-sonnet-4-6\"\n  system: p\n" +
		"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n"
	fresh := func() *Workflow {
		w := compileFallbackSrc(t, src).Workflow
		if w == nil {
			t.Fatal("the fixture does not compile")
		}
		return w
	}
	route := []Fallback{{Backend: "claude_code", Model: "claude-opus-5"}}
	t.Setenv("ITERION_ZZPROBE_BACKEND", "")
	// Answered nowhere: dispatch resolves the var to "" and the node is
	// undecided — no refusal.
	if refusals := ApplyRunFallback(fresh(), route, false, nil, nil); len(refusals) != 0 {
		t.Errorf("refused an undecided node: %v", refusals)
	}
	// A stored bot var answering it IS the run's reading.
	SetEnvOverlay(func(name string) (string, bool) {
		if name == "ITERION_ZZPROBE_BACKEND" {
			return "claw", true
		}
		return "", false
	})
	defer SetEnvOverlay(nil)
	refusals := ApplyRunFallback(fresh(), route, false, nil, nil)
	if len(refusals) != 1 {
		t.Fatalf("%d refusals with the bot var set, want 1: %v", len(refusals), refusals)
	}
	if !strings.Contains(refusals[0], "routes a claw node to a CLI backend") {
		t.Errorf("refused for the wrong reason: %s", refusals[0])
	}
}

// The RUN reading drills a dotted reference into a json var's document,
// exactly as dispatch's TemplateResolver does (drillTemplatePath: maps only,
// a missing member or a non-map segment is not found — kept as written).
// A flat-name-only view left the node undecided while dispatch resolved it.
func TestRunBackendReading_DrillsJSONVars(t *testing.T) {
	t.Setenv("C1606_JSON_LEAF", "kimi")
	w := &Workflow{Vars: map[string]*Var{
		"cfg": {Name: "cfg", Type: VarJSON, HasDefault: true, Default: `{"backend": "claw"}`},
		"env": {Name: "env", Type: VarJSON, HasDefault: true, Default: `{"backend": "${C1606_JSON_LEAF:-claw}"}`},
		"s":   {Name: "s", Type: VarString, HasDefault: true, Default: "claw"},
	}}
	view := launchVarsView(w, map[string]string{"cfg": `{"backend": "claude_code"}`})
	run := runBackend.withVars(view)
	cases := []struct{ in, want string }{
		// The launch override — parsed as the document it is — wins over
		// the declared default, and the drill reads its member.
		{"{{vars.cfg.backend}}", "claude_code"},
		// A json leaf is env-expanded the way resolveVars expands leaves
		// (braced-only, the process environment).
		{"{{vars.env.backend}}", "kimi"},
		// drillTemplatePath's misses: no such member, and a scalar holds
		// no members at all — both kept as written, hence undecided.
		{"{{vars.cfg.nope}}", ""},
		{"{{vars.s.backend}}", ""},
		// The whole document formats the way formatValue prints it.
		{"{{vars.env}}", `{"backend":"kimi"}`},
	}
	for _, c := range cases {
		if got := run.name(c.in); got != c.want {
			t.Errorf("runBackend.withVars(view).name(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The reviewer's executed bypass: a node whose backend is a drilled json
// reference screened as undecided, so the tools-inversion refusal was
// SKIPPED — while dispatch resolved "claw" and the crossing was real.
// (IR built directly: the launchVarsView contract is on the compiled Var,
// and a json object default's .bot spelling rides the escape profiles —
// irrelevant to what the screen does with the parsed document.)
func TestApplyRunFallback_DrilledJSONVarIsScreened(t *testing.T) {
	fresh := func() *Workflow {
		return &Workflow{
			Nodes: map[string]Node{"x": applyAgent("x", "{{vars.cfg.backend}}", "", nil, nil)},
			Vars: map[string]*Var{
				"cfg": {Name: "cfg", Type: VarJSON, HasDefault: true, Default: `{"backend": "claw"}`},
			},
		}
	}
	refusals := ApplyRunFallback(fresh(), []Fallback{{Backend: "claude_code", Model: "claude-opus-5"}}, false, nil, nil)
	if len(refusals) != 1 {
		t.Fatalf("%d refusals, want 1 — dispatch resolves the node to claw and the crossing is real", len(refusals))
	}
	if !strings.Contains(refusals[0], "routes a claw node to a CLI backend") {
		t.Errorf("refused for the wrong reason: %s", refusals[0])
	}
	if refusals := ApplyRunFallback(fresh(), []Fallback{{Backend: "claw", Model: "anthropic/glm-5.2"}}, false, nil, nil); len(refusals) != 0 {
		t.Errorf("a route to the node's own (drilled) backend was refused: %v", refusals)
	}
}

// The five names varExpandFn answers from engine state — PROJECT_DIR,
// BUNDLE_DIR, BUNDLE_SKILLS_DIR, PROJECT_MEMORY_DIR, PROJECT_SCRATCH_DIR —
// read "" through the screen's process-env expansion, where dispatch reads
// a path: decided-empty where the run is decided-full is a disagreement.
// A var whose text references one stays UNDECIDED for the screen — the same
// posture as a var nothing answers anywhere else.
func TestLaunchVarsView_EngineSuppliedNamesStayUndecided(t *testing.T) {
	for _, name := range []string{"PROJECT_DIR", "BUNDLE_DIR", "BUNDLE_SKILLS_DIR", "PROJECT_MEMORY_DIR", "PROJECT_SCRATCH_DIR"} {
		w := &Workflow{Vars: map[string]*Var{
			"b": {Name: "b", Type: VarString, HasDefault: true, Default: "${" + name + "}/x"},
		}}
		if got := runBackend.withVars(launchVarsView(w, nil)).name("{{vars.b}}"); got != "" {
			t.Errorf("{{vars.b}} with ${%s} in its text = %q, want undecided — the path comes from engine state the screen does not have", name, got)
		}
	}
	// Nested behind a default: the engine name is consulted inside-out.
	w := &Workflow{Vars: map[string]*Var{
		"b": {Name: "b", Type: VarString, HasDefault: true, Default: "${C1606_UNSET:-${PROJECT_DIR}}"},
	}}
	if got := runBackend.withVars(launchVarsView(w, nil)).name("{{vars.b}}"); got != "" {
		t.Errorf("a nested ${PROJECT_DIR} = %q, want undecided", got)
	}
	// Control: an ordinary dial still decides, on its default.
	w.Vars["d"] = &Var{Name: "d", Type: VarString, HasDefault: true, Default: "${C1606_UNSET:-claw}"}
	if got := runBackend.withVars(launchVarsView(w, nil)).name("{{vars.d}}"); got != "claw" {
		t.Errorf("a plain dial = %q, want claw — the guard must not swallow ordinary expansions", got)
	}
}

// A coercion failure does not drop the var at dispatch: resolveVars logs and
// runs the RAW value, env-expanded (engine_resolve.go's read fallback), and a
// FLAT {{vars.b}} reads it fine. Omitting the var from the view read the node
// as undecided — and let a real crossing sail through a launch the pre-drill
// screen refused.
func TestLaunchVarsView_CoercionFailureFallsBackLikeResolveVars(t *testing.T) {
	w := &Workflow{Vars: map[string]*Var{
		"b": {Name: "b", Type: VarInt, HasDefault: true, Default: int64(1)},
	}}
	// --var b=claw cannot coerce to int: dispatch stores the raw "claw".
	if got := runBackend.withVars(launchVarsView(w, map[string]string{"b": "claw"})).name("{{vars.b}}"); got != "claw" {
		t.Errorf("{{vars.b}} with a non-coercible override = %q, want claw — dispatch falls back to the raw value", got)
	}
}

func TestApplyRunFallback_CoercionFailureKeepsTheFlatReading(t *testing.T) {
	fresh := func() *Workflow {
		return &Workflow{
			Nodes: map[string]Node{"x": applyAgent("x", "{{vars.b}}", "", nil, nil)},
			Vars:  map[string]*Var{"b": {Name: "b", Type: VarInt, HasDefault: true, Default: int64(1)}},
		}
	}
	refusals := ApplyRunFallback(fresh(), []Fallback{{Backend: "claude_code", Model: "claude-opus-5"}}, false,
		map[string]string{"b": "claw"}, nil)
	if len(refusals) != 1 {
		t.Fatalf("%d refusals, want 1 — dispatch runs the node on claw, the crossing is real", len(refusals))
	}
	if !strings.Contains(refusals[0], "routes a claw node to a CLI backend") {
		t.Errorf("refused for the wrong reason: %s", refusals[0])
	}
}

// The engine-name probe reads a var's text the way dispatch expands THAT
// TYPE: a json var's string leaves get the braced-only reading and its keys
// are never expanded, so a bare `$PROJECT_DIR` in a leaf is DATA — the var
// stays, the drill resolves. Probing the raw text with the full reading
// omitted the whole var and took the screen off a real crossing.
func TestLaunchVarsView_JSONProbeReadsLeavesBracedOnly(t *testing.T) {
	w := &Workflow{Vars: map[string]*Var{
		"cfg": {Name: "cfg", Type: VarJSON, HasDefault: true, Default: `{"backend":"claw","note":"$PROJECT_DIR"}`},
		"doc": {Name: "doc", Type: VarJSON, HasDefault: true, Default: `{"backend":"${PROJECT_DIR}"}`},
	}}
	run := runBackend.withVars(launchVarsView(w, nil))
	if got := run.name("{{vars.cfg.backend}}"); got != "claw" {
		t.Errorf("{{vars.cfg.backend}} = %q, want claw — a bare $NAME in a json leaf is data, the var must not be omitted", got)
	}
	// A braced ${…} in a leaf IS an expansion dispatch runs (on the leaf) —
	// the engine-supplied name keeps the whole var undecided.
	if got := run.name("{{vars.doc.backend}}"); got != "" {
		t.Errorf("{{vars.doc.backend}} = %q, want undecided — the leaf expands ${PROJECT_DIR} at dispatch", got)
	}
}

func TestApplyRunFallback_JSONLeafHoldingDataKeepsTheScreen(t *testing.T) {
	fresh := func() *Workflow {
		return &Workflow{
			Nodes: map[string]Node{"x": applyAgent("x", "{{vars.cfg.backend}}", "", nil, nil)},
			Vars: map[string]*Var{
				"cfg": {Name: "cfg", Type: VarJSON, HasDefault: true, Default: `{"backend":"claw","note":"$PROJECT_DIR"}`},
			},
		}
	}
	refusals := ApplyRunFallback(fresh(), []Fallback{{Backend: "claude_code", Model: "claude-opus-5"}}, false, nil, nil)
	if len(refusals) != 1 {
		t.Fatalf("%d refusals, want 1 — the note is data, the backend member resolves to claw, the crossing is real", len(refusals))
	}
	if !strings.Contains(refusals[0], "routes a claw node to a CLI backend") {
		t.Errorf("refused for the wrong reason: %s", refusals[0])
	}
}

// The accepted over-conservatism, pinned so it is a choice rather than an
// accident: a name CONSULTED but discarded — `${SET:-${PROJECT_DIR}}` with
// SET in the environment — omits the var all the same, where dispatch
// decides the outer value. The inside-out expansion consults the inner
// segment before the outer short-circuits, and tracking whether a consulted
// name's value reaches the result would price the fix above the shape's
// frequency. Undecided never refuses a route the run would take; it only
// screens less, which is the pre-#1606 posture for every var.
func TestLaunchVarsView_ConsultedButDiscardedStaysUndecided(t *testing.T) {
	t.Setenv("C1606_OUTER_SET", "claw")
	w := &Workflow{Vars: map[string]*Var{
		"b": {Name: "b", Type: VarString, HasDefault: true, Default: "${C1606_OUTER_SET:-${PROJECT_DIR}}"},
	}}
	if got := runBackend.withVars(launchVarsView(w, nil)).name("{{vars.b}}"); got != "" {
		t.Errorf("{{vars.b}} = %q, want undecided — the probe cannot see that dispatch discards the inner expansion", got)
	}
}

// The probe sees an engine-supplied name reached through an INDIRECTION:
// resolveBracedSegment parses the outer name from the resolved inner text,
// so `${${A}}` with A=PROJECT_DIR in the environment consults PROJECT_DIR in
// dispatch's varExpandFn. A probe lookup answering "" for everything
// resolved the inner segment to nothing and never saw it — the screen then
// read the var as decided-empty where dispatch reads a path. The probe
// answers non-listed names through LookupEnv (the reading the screen itself
// expands with); a listed name's consult is still recorded.
func TestLaunchVarsView_IndirectEngineNameStaysUndecided(t *testing.T) {
	t.Setenv("C1606_INDIRECT", "PROJECT_DIR")
	w := &Workflow{Vars: map[string]*Var{
		"b": {Name: "b", Type: VarString, HasDefault: true, Default: "${${C1606_INDIRECT}}"},
	}}
	if _, ok := launchVarsView(w, nil)["b"]; ok {
		t.Errorf("view[b] present — dispatch resolves ${${C1606_INDIRECT}} to the PROJECT_DIR path; the var must stay undecided")
	}
}

// An agent, a judge or an llm router with no `model:` compiles with
// ITERION_DEFAULT_SUPERVISOR_MODEL read through LookupEnv — the reading the
// executor's router fallback and the supervisor apply at run time — so a
// stored bot var reaches the compiled program, not only the run.
func TestResolveSupervisorModel_ReadsTheBotVarOverlay(t *testing.T) {
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	SetEnvOverlay(func(name string) (string, bool) {
		if name == "ITERION_DEFAULT_SUPERVISOR_MODEL" {
			return "anthropic/claude-haiku-4-5", true
		}
		return "", false
	})
	defer SetEnvOverlay(nil)
	if got := resolveSupervisorModel(""); got != "anthropic/claude-haiku-4-5" {
		t.Errorf("resolveSupervisorModel(\"\") = %q, want the bot var's model", got)
	}
	if got := resolveSupervisorModel("openai/gpt-6"); got != "openai/gpt-6" {
		t.Errorf("an explicit model lost to the default: %q", got)
	}
}

// The probe reads names as dispatch does: an indirection whose inner name
// only a stored bot var answers still consults PROJECT_DIR — dispatch expands
// it to the workDir path, which the screen cannot know — so the view leaves
// the var undecided.
func TestLaunchVarsView_ProbeSeesAnIndirectionThroughTheOverlay(t *testing.T) {
	t.Setenv("ITERION_ZZ_WHICH", "")
	SetEnvOverlay(func(name string) (string, bool) {
		if name == "ITERION_ZZ_WHICH" {
			return "PROJECT_DIR", true
		}
		return "", false
	})
	defer SetEnvOverlay(nil)
	w := &Workflow{Vars: map[string]*Var{
		"b": {Name: "b", Type: VarString, HasDefault: true, Default: "${${ITERION_ZZ_WHICH}}"},
	}}
	if v, ok := launchVarsView(w, nil)["b"]; ok {
		t.Errorf("view[b] = %q, want the var left undecided — dispatch reads a path the screen cannot", v)
	}
}

// A coercion failure runs the RAW text expanded as dispatch expands it —
// through the overlay — so the view reads the same backend.
func TestLaunchVarsView_CoercionFallbackReadsTheOverlay(t *testing.T) {
	t.Setenv("ITERION_ZZ_FLAG", "")
	SetEnvOverlay(func(name string) (string, bool) {
		if name == "ITERION_ZZ_FLAG" {
			return "claw", true
		}
		return "", false
	})
	defer SetEnvOverlay(nil)
	w := &Workflow{Vars: map[string]*Var{
		"b": {Name: "b", Type: VarBool, HasDefault: true, Default: "${ITERION_ZZ_FLAG}"},
	}}
	if got := launchVarsView(w, nil)["b"]; got != "claw" {
		t.Errorf("view[b] = %#v, want \"claw\" — dispatch's coercion fallback expands through the overlay", got)
	}
}
