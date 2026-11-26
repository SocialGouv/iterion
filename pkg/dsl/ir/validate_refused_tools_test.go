package ir

import (
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
)

// refusedCode is the code under test.
const refusedCode = DiagToolRefusedOnClaw

// rerouteShape matches any wording that sends the author to another backend
// or node kind, which cannot serve a tool node's registry recipe.
var rerouteShape = regexp.MustCompile("(?i)route|agent node|move it|claude_code|backend that|another backend|switch")

// refusedProbe is the name the scenarios below declare. C159's three arms are
// driven through an injected entry over a name the catalog really holds and
// the production list does not, so every reduction, scope and hedge below is
// exercised on a refusal whose words the test controls; the shipped entries
// are witnessed by TestAShippedRefusedToolWarnsAndStillCompiles.
const refusedProbe = "web_search"

// probeRefusal is shaped like the refusals this list carries: a reason in the
// present tense, and a remedy naming where the capability IS served.
var probeRefusal = toolcatalog.ClawRefusal{
	Reason: "iterion serves this name on no claw host, so claw registers it and answers nothing",
	Remedy: "Do the work in this node, or route it to claude_code, whose native Agent tool " +
		"(legacy name Task) serves it",
}

// withRefusedProbe installs probeRefusal for the duration of the test.
func withRefusedProbe(t *testing.T) {
	t.Helper()
	t.Cleanup(toolcatalog.SetClawRefusedBuiltinForTest(refusedProbe, probeRefusal))
}

// TestAShippedRefusedToolWarnsAndStillCompiles drives the production list,
// not an injection: a claw node declaring cron_create — a tool claw registers
// and iterion refuses, since no scheduler ever fires its entry — earns C159
// quoting the catalog's reason and remedy, and the workflow still compiles.
// `agent` and the probe stay silent: the subagent runner is wired (#1940), and
// the probe is not on the shipped list.
func TestAShippedRefusedToolWarnsAndStillCompiles(t *testing.T) {
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  tools: [read_file, cron_create, agent, "+refusedProbe+"]\n"))
	got := diagsFor(cr, refusedCode)
	if len(got) != 1 {
		t.Fatalf("want exactly one C159, for cron_create, got %v", got)
	}
	r, refused := toolcatalog.ClawRefusesBuiltin("cron_create")
	if !refused {
		t.Fatal("cron_create is not on the shipped refused list")
	}
	for _, want := range []string{`"cron_create"`, r.Reason, r.Remedy} {
		if !strings.Contains(got[0], want) {
			t.Errorf("the C159 message does not carry %q: %s", want, got[0])
		}
	}
	if cr.HasErrors() {
		t.Errorf("C159 must be a warning — the node still runs and the call earns a typed refusal: %v", cr.Diagnostics)
	}
	if got := diagsFor(cr, DiagUnknownTool); len(got) > 0 {
		t.Fatalf("the declared names are ones the registry HOLDS, so C135 has nothing to say: %v", got)
	}
}

// TestRefusedToolOnClawIsAWarning: the probe resolves on claw — the registry
// holds it, so C135 is silent — and every call would earn a typed refusal
// because iterion serves it on no host. The author pays a run to learn that;
// the compiler knows it from the source.
//
// A warning, not an error: the node still runs and the refusal reaches the
// model as a tool error it can act on, and philosophy #1 warns rather than
// rejects.
func TestRefusedToolOnClawIsAWarning(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  tools: [read_file, "+refusedProbe+"]\n"))
	got := diagsFor(cr, refusedCode)
	if len(got) != 1 {
		t.Fatalf("want exactly one C159 for the probe on claw, got %+v", cr.Diagnostics)
	}
	if !strings.Contains(got[0], `"`+refusedProbe+`"`) {
		t.Errorf("the message must name the tool: %s", got[0])
	}
	if !strings.Contains(got[0], "claude_code") {
		t.Errorf("the message must name where the capability IS served: %s", got[0])
	}
	if cr.HasErrors() {
		t.Error("C159 must be a warning — the node still runs, and the refusal is a tool error the model can act on")
	}
	if len(diagsFor(cr, DiagUnknownTool)) != 0 {
		t.Error("C159 and C135 must stay disjoint: the probe is a name claw HAS")
	}
}

// TestRefusedToolOnClaudeCodeIsSilent: the list is claw's, so a node on
// another backend has nothing to read from it — the parity doctrine says
// nothing where the capability is served.
func TestRefusedToolOnClaudeCodeIsSilent(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claude_code\"\n  tools: [read_file, "+refusedProbe+"]\n"))
	if got := diagsFor(cr, refusedCode); len(got) > 0 {
		t.Fatalf("claude_code serves subagents; want no C159, got %v", got)
	}
}

// TestRefusedToolUnresolvedBackendIsSilent: with no backend anywhere the
// resolver falls through to env and host credential detection, so the compiler
// cannot know which backend serves — the same silence C135 keeps there.
func TestRefusedToolUnresolvedBackendIsSilent(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, toolsWorkflow("  tools: [read_file, "+refusedProbe+"]\n"))
	if got := diagsFor(cr, refusedCode); len(got) > 0 {
		t.Fatalf("want no C159 on an unresolved backend, got %v", got)
	}
}

// TestRefusedToolOnClawFallbackRoute: the route inherits the SAME list and
// exists to serve when the primary is already failing — a tool refused there
// turns the safety net into a second disappointment.
func TestRefusedToolOnClawFallbackRoute(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  tools: [read_file, "+refusedProbe+"]\n"+
			"  fallbacks:\n    api:\n      backend: \"claw\"\n      model: \"anthropic/claude-opus-5\"\n"))
	got := diagsFor(cr, refusedCode)
	if len(got) != 1 {
		t.Fatalf("want one C159 for the claw route, got %+v", cr.Diagnostics)
	}
	if !strings.Contains(got[0], `"api"`) {
		t.Errorf("the message must name the route: %s", got[0])
	}
}

// TestRefusedToolReportedOncePerNodeAcrossRoutes: several claw routes share
// one tools list, so repeating the message per route says nothing new.
func TestRefusedToolReportedOncePerNodeAcrossRoutes(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  tools: [read_file, "+refusedProbe+"]\n"+
			"  fallbacks:\n    api:\n      backend: \"claw\"\n      model: \"anthropic/claude-opus-5\"\n"+
			"    gpt:\n      backend: \"claw\"\n      model: \"openai/gpt-5.5\"\n"))
	if got := diagsFor(cr, refusedCode); len(got) != 1 {
		t.Fatalf("want a single C159 for the node, got %v", got)
	}
}

// TestRefusedToolOnClawPrimaryNotDoubleReported: a claw node with a claw route
// is reported for its own backend, once — the route adds no new fact.
func TestRefusedToolOnClawPrimaryNotDoubleReported(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  tools: ["+refusedProbe+"]\n"+
			"  fallbacks:\n    gpt:\n      backend: \"claw\"\n      model: \"openai/gpt-5.5\"\n"))
	got := diagsFor(cr, refusedCode)
	if len(got) != 1 {
		t.Fatalf("want a single C159, got %v", got)
	}
	if strings.Contains(got[0], "fallback") {
		t.Errorf("the node's own backend is the one that refuses first: %s", got[0])
	}
}

// TestRefusedToolSilentWhenNotDeclared: the diagnostic reads the author's own
// list. Nothing else on a claw node may raise it.
func TestRefusedToolSilentWhenNotDeclared(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  tools: [read_file, bash]\n"))
	if got := diagsFor(cr, refusedCode); len(got) > 0 {
		t.Fatalf("want no C159 for a list that does not name a refused tool, got %v", got)
	}
}

// recoveryAgentToolsWorkflow is a legitimate Verified Action (command and
// postcondition differ) whose rung-4 recovery agent declares `tools` — the
// third author-written list that reaches claw's registry.
func recoveryAgentToolsWorkflow(tools, workflowBody string) string {
	return `prompt p:
  hi

tool build:
  command: "make build"
  goal: "the build passes"
  postcondition: "test -f ./out/bin"
  policy: recover
  recovery:
    max_agent_attempts: 1
    agent_tools: [` + tools + `]

workflow w:
` + workflowBody + `  entry: build
  build -> done
`
}

// TestRefusedRecoveryAgentToolOnClaw: executor_verified_action hand-builds an
// ir.AgentNode carrying `agent_tools:` verbatim and dispatches it through the
// same claw registry, so a refused name is refused there too — at the moment
// C135's own message calls the worst one, after the deterministic rungs have
// already failed. The identical entry on an agent node's own `tools:` is
// warned about, so silence here is the hole.
func TestRefusedRecoveryAgentToolOnClaw(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, recoveryAgentToolsWorkflow("read_file, "+refusedProbe, "  default_backend: \"claw\"\n"))
	got := diagsFor(cr, refusedCode)
	if len(got) != 1 {
		t.Fatalf("want exactly one C159 for the probe on a rung-4 recovery agent, got %+v", cr.Diagnostics)
	}
	for _, want := range []string{"agent_tools", `"` + refusedProbe + `"`, "claude_code"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("the message must name %s: %s", want, got[0])
		}
	}
	if cr.HasErrors() {
		t.Error("C159 must stay a warning on the recovery list too")
	}
	if len(diagsFor(cr, DiagUnknownTool)) != 0 {
		t.Error("C159 and C135 must stay disjoint: the probe is a name claw HAS")
	}
}

// TestRefusedRecoveryAgentToolSilentWithoutClawDefault: the synthetic node
// declares no backend of its own, so only the workflow default can make it
// claw — the same silence C135 keeps on an unresolved backend.
func TestRefusedRecoveryAgentToolSilentWithoutClawDefault(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, recoveryAgentToolsWorkflow("read_file, "+refusedProbe, ""))
	if got := diagsFor(cr, refusedCode); len(got) > 0 {
		t.Fatalf("an unresolved backend must not be guessed at, got %v", got)
	}
}

// TestRefusedRecoveryAgentToolSilentWhenNotDeclared keeps the new arm narrow:
// a recovery list naming only served tools says nothing.
func TestRefusedRecoveryAgentToolSilentWhenNotDeclared(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, recoveryAgentToolsWorkflow("read_file, bash", "  default_backend: \"claw\"\n"))
	if got := diagsFor(cr, refusedCode); len(got) > 0 {
		t.Fatalf("want no C159 for a recovery list of served tools, got %v", got)
	}
}

// TestEveryCompileTimeMessageStatesTheRefusalAsCertain: a listed name is
// refused by registerClawTool whatever the host wires — a registry passed in
// ClawDefaults, or an exec of its own through RegisterClawTool — so no C159
// message may promise a host that serves it. A hedge there would tell an
// operator their deployment can lift a refusal no deployment can.
//
// Three messages make the claim, and the guard takes all three: C159's arms
// (the node's own backend, its first claw `fallbacks:` route, a Verified
// Action's rung-4 `recovery: agent_tools:`). A host that wires no subagent
// runner is a run-time fact about `agent`, which is not on the list and earns
// no C159 (toolcatalog.ClawUnwiredSubagentRefusal answers it).
func TestEveryCompileTimeMessageStatesTheRefusalAsCertain(t *testing.T) {
	withRefusedProbe(t)
	for _, tc := range []struct {
		name string
		code DiagCode
		cr   *CompileResult
	}{
		{"C159 node backend", refusedCode, compileToolsSrc(t, toolsWorkflow(
			"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  tools: ["+refusedProbe+"]\n"))},
		{"C159 fallback route", refusedCode, compileToolsSrc(t, toolsWorkflow(
			"  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  tools: ["+refusedProbe+"]\n"+
				"  fallbacks:\n    api:\n      backend: \"claw\"\n      model: \"anthropic/claude-opus-5\"\n"))},
		{"C159 rung-4 recovery", refusedCode, compileToolsSrc(t,
			recoveryAgentToolsWorkflow(refusedProbe, "  default_backend: \"claw\"\n"))},
	} {
		got := diagsFor(tc.cr, tc.code)
		if len(got) != 1 {
			t.Fatalf("%s: want one %s, got %+v", tc.name, tc.code, tc.cr.Diagnostics)
		}
		for _, hedge := range []string{"unless", "host serves", "MAY"} {
			if strings.Contains(got[0], hedge) {
				t.Errorf("%s: the warning promises a host that serves the tool (%q), but every host refuses a listed name: %s",
					tc.name, hedge, got[0])
			}
		}
		if !strings.Contains(got[0], "the call is refused") {
			t.Errorf("%s: the warning does not state that the call is refused: %s", tc.name, got[0])
		}
	}
}

// TestRefusedToolReportedOncePerRepeatedName: a `tools:` list is a list, and
// an author can name the same tool twice. C135's own reduction over the same
// field (unresolvableToolNames) deduplicates, so the two codes report the
// same source shape the same way — one row per refused NAME, not one per
// occurrence of it on the line.
func TestRefusedToolReportedOncePerRepeatedName(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  tools: ["+refusedProbe+", read_file, "+refusedProbe+"]\n"))
	if got := diagsFor(cr, refusedCode); len(got) != 1 {
		t.Fatalf("want one C159 for a name listed twice, got %d: %v", len(got), got)
	}
}

// TestRefusedRecoveryAgentToolReportedOncePerRepeatedName: the rung-4 recovery
// list is reduced by the same helper, so it inherits the same guarantee.
func TestRefusedRecoveryAgentToolReportedOncePerRepeatedName(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, recoveryAgentToolsWorkflow(refusedProbe+", read_file, "+refusedProbe, "  default_backend: \"claw\"\n"))
	if got := diagsFor(cr, refusedCode); len(got) != 1 {
		t.Fatalf("want one C159 for a recovery list naming the probe twice, got %d: %v", len(got), got)
	}
}

// TestRefusedToolNameIsReportedTrimmed: the catalog predicates trim, so a
// padded entry still draws the warning about the tool it names — and the name
// the message quotes is the TOOL's, not the author's whitespace, which is what
// a reader greps for (C135's reduction trims the same way).
//
// The padding's own cost is not this warning's subject. The registry resolves
// an entry as written, so a padded one fails the node at dispatch before any
// call — pinned, for the production registry, by pkg/backend/model's
// TestAPaddedToolEntryNeverReachesTheRefusal.
func TestRefusedToolNameIsReportedTrimmed(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  tools: [\" "+refusedProbe+" \", read_file]\n"))
	got := diagsFor(cr, refusedCode)
	if len(got) != 1 {
		t.Fatalf("want one C159 for a padded refused name, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], `"`+refusedProbe+`"`) {
		t.Errorf("the message quotes the author's padding instead of the tool name: %s", got[0])
	}
}

// TestARefusedRegistryCommandOnAToolNodeWarnsOnEveryBackend: a tool node's
// bare `command:` names a tool in the registry RegisterClawAll builds, and
// ClawExecutor runs every tool node whatever the workflow's backend, so
// `command: cron_create` is refused on every run that reaches it — with no
// backend declared too. A tool node has no model to read the refusal, and
// rerouting cannot serve it, so the hint is the tool node's own and offers no
// backend that serves the tool, and the warning points at the
// `command:` line. Only the registry recipe is read: `cron_create ` carries a
// space, so it is a shell command that sh runs, and a served name says nothing.
func TestARefusedRegistryCommandOnAToolNodeWarnsOnEveryBackend(t *testing.T) {
	for _, backend := range []string{"claw", "claude_code", ""} {
		t.Run("backend="+backend, func(t *testing.T) {
			defaultBackend := ""
			if backend != "" {
				defaultBackend = "  default_backend: \"" + backend + "\"\n"
			}
			src := `tool arm:
  command: cron_create

tool shell:
  command: "cron_create "

tool reader:
  command: read_file

workflow w:
` + defaultBackend + `  entry: arm
  arm -> shell
  shell -> reader
  reader -> done
`
			cr := compileToolsSrc(t, src)
			var got []Diagnostic
			for _, d := range cr.Diagnostics {
				if d.Code == refusedCode {
					got = append(got, d)
				}
			}
			if len(got) != 1 {
				t.Fatalf("want exactly one C159, for the registry command cron_create, got %v", got)
			}
			d := got[0]
			r, _ := toolcatalog.ClawRefusesBuiltin("cron_create")
			for _, want := range []string{`"arm"`, "command:", `"cron_create"`, r.Reason, r.Remedy} {
				if !strings.Contains(d.Message, want) {
					t.Errorf("the C159 message does not carry %q: %s", want, d.Message)
				}
			}
			if rerouteShape.MatchString(d.Hint) || d.Hint == HintFor(refusedCode) {
				t.Errorf("a tool node's C159 hint tells the author to reroute, which cannot serve a registry recipe: %q", d.Hint)
			}
			if !strings.Contains(d.Hint, "whatever the backend") {
				t.Errorf("a tool node's C159 hint does not say the recipe runs on claw's registry whatever the backend: %q", d.Hint)
			}
			if d.Property != "command" || d.Line != 2 {
				t.Errorf("C159 on a tool node points at property %q line %d, want the `command:` line (command, 2)", d.Property, d.Line)
			}
			if cr.HasErrors() {
				t.Errorf("C159 stays a warning on a tool node: %v", cr.Diagnostics)
			}
		})
	}
}

// TestARegistryCommandPaddedWithWhitespaceIsNotWarned: dispatch
// resolves a registry `command:` exactly as written (executeToolNodeRecipe
// passes node.Command untrimmed), and the recipe-kind split reads only space
// and tab as shell. A command padded with any other whitespace — U+00A0,
// U+0085, a vertical tab, a form feed — stays a registry recipe and fails at
// dispatch as an unregistered tool, never reaching the cron refusal, so C159
// predicting that refusal would name the wrong failure.
func TestARegistryCommandPaddedWithWhitespaceIsNotWarned(t *testing.T) {
	for name, pad := range map[string]string{
		"no-break space": "\u00a0",
		"next line":      "\u0085",
		"vertical tab":   "\v",
		"form feed":      "\f",
	} {
		t.Run(name, func(t *testing.T) {
			src := "tool arm:\n  command: \"cron_create" + pad + "\"\n\n" +
				"workflow w:\n  default_backend: \"claw\"\n  entry: arm\n  arm -> done\n"
			cr := compileToolsSrc(t, src)
			if got := diagsFor(cr, refusedCode); len(got) != 0 {
				t.Errorf("C159 predicts a cron refusal for a padded command dispatch never resolves: %v", got)
			}
		})
	}
}

// TestARefusedNameBesideAScriptIsNotAToolNodeRecipe: only a registry recipe
// dispatches its `command:` as a tool name. A node that also writes `script:`
// is a script recipe for the recipe-kind split (and a compile error for the
// two recipes it declares), so its `command:` never reaches the registry and
// C159 has no refusal to predict.
func TestARefusedNameBesideAScriptIsNotAToolNodeRecipe(t *testing.T) {
	cr := compileToolsSrc(t, `tool both:
  command: cron_create
  script: "echo hi"

workflow w:
  default_backend: "claw"
  entry: both
  both -> done
`)
	if got := diagsFor(cr, refusedCode); len(got) != 0 {
		t.Errorf("C159 reads the command of a node whose recipe is not a registry one: %v", got)
	}
}

// TestAgentAndToolNodeRefusedHintsAgreeInOneOutput compiles one workflow
// whose claw agent node lists cron_create and whose tool node runs it as its
// registry `command:`, so both C159 hints land in the same validate output.
// They must state one world: the agent node's hint offers rerouting to a
// backend that serves the capability and scopes that offer to the nodes it
// can reach (agent and judge), and the tool node's hint says why rerouting
// does not reach its recipe without denying that such a backend exists.
func TestAgentAndToolNodeRefusedHintsAgreeInOneOutput(t *testing.T) {
	src := `schema out:
  answer: string

prompt p:
  hi

agent x:
  system: p
  output: out
  backend: "claw"
  model: "anthropic/claude-opus-5"
  tools: [cron_create]

tool arm:
  command: cron_create

workflow w:
  entry: x
  x -> arm
  arm -> done
`
	cr := compileToolsSrc(t, src)
	var agentHint, toolHint string
	var n int
	for _, d := range cr.Diagnostics {
		if d.Code != refusedCode {
			continue
		}
		n++
		switch d.NodeID {
		case "x":
			agentHint = d.Hint
		case "arm":
			toolHint = d.Hint
		}
	}
	if n != 2 || agentHint == "" || toolHint == "" {
		t.Fatalf("want one C159 with a hint on the agent node and one on the tool node, got %d: %v", n, cr.Diagnostics)
	}
	if !strings.Contains(agentHint, "agent or judge node") {
		t.Errorf("the agent-node C159 hint offers rerouting without scoping it to the nodes rerouting reaches: %q", agentHint)
	}
	if !strings.Contains(agentHint, "whatever the backend") {
		t.Errorf("the agent-node C159 hint does not say a tool node's recipe runs on claw's registry whatever the backend: %q", agentHint)
	}
	if denial := regexp.MustCompile(`(?i)\b(no|none|nothing|never)\b[^.;:]*\bserv|\bserv\w*\s+(on|by)\s+no\b`); denial.MatchString(toolHint) {
		t.Errorf("the tool-node C159 hint says no backend serves the tool, which the agent-node hint beside it contradicts: %q", toolHint)
	}
	if !strings.Contains(toolHint, "whatever the backend") {
		t.Errorf("the tool-node C159 hint does not say why changing the backend cannot serve its recipe: %q", toolHint)
	}
	if otherBackend := regexp.MustCompile(`(?i)\b(any|other|no)\s+(other\s+)?backends?\b`); otherBackend.MatchString(toolHint) {
		t.Errorf("the tool-node C159 hint makes a claim about other backends, which the agent-node hint beside it owns: %q", toolHint)
	}
	if !strings.Contains(toolHint, "takes no `backend:`") || !strings.Contains(toolHint, "`default_backend` leaves its recipe on") {
		t.Errorf("the tool-node C159 hint does not name the lever a tool node has (no `backend:` of its own, and the workflow `default_backend` leaves its recipe on claw's registry): %q", toolHint)
	}
}
