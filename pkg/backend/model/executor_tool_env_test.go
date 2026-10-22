package model

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// toolBodyEnvNode is a tool node carrying the three bodies a reader gets,
// with their references parsed as the compiler parses them.
func toolBodyEnvNode(command, script, postcondition string) *ir.ToolNode {
	return &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "body"},
		Command:       command,
		CommandRefs:   mustRefs(command),
		Script:        script,
		ScriptRefs:    mustRefs(script),
		Postcondition: postcondition,
		PostcondRefs:  mustRefs(postcondition),
	}
}

// The environment reading comes FIRST, at all three sites. An upstream value
// that names an environment variable is substituted into the body AFTER the
// reading, so it stays the inert text the escaper made of it: reading the
// environment afterwards would take that value's own reference back out of
// the single quotes and hand the shell whatever the environment carries —
// which is why ADR-130's decision is an opt-out and never a re-order.
//
// Both spellings are exercised, and the BRACED one is the attack under this
// scanner: it answers `${NAME}` and leaves a bare `$NAME` to the shell, so a
// witness that only carried `$NAME` would stay green with the reading moved
// after the substitution.
func TestExpansionStillPrecedesSubstitution(t *testing.T) {
	// The attacker's value: shell metacharacters that would close the
	// protective quotes and run a command of their own.
	t.Setenv("ITERION_TEST_BODY_INJECT", "'; echo pwned; echo '")
	e := &ClawExecutor{}
	node := toolBodyEnvNode("echo {{input.msg}}", "x = {{input.msg}}", "test {{input.msg}}")
	ctx := context.Background()

	for _, payload := range []string{"${ITERION_TEST_BODY_INJECT}", "$ITERION_TEST_BODY_INJECT"} {
		input := map[string]any{"msg": payload}
		shell, _ := e.shellRecipe(ctx, node, input)
		script, _ := e.scriptRecipe(ctx, node, input)
		for _, tc := range []struct {
			where string
			got   string
			want  string
		}{
			{"command", shell(), "echo '" + payload + "'"},
			{"script", script(), `x = "` + payload + `"`},
			{"postcondition", e.postconditionBody(ctx, node, input), "test '" + payload + "'"},
		} {
			if tc.got != tc.want {
				t.Errorf("the %s resolved to %q, want %q — a substituted value must reach the reader as inert text", tc.where, tc.got, tc.want)
			}
			if strings.Contains(tc.got, "pwned") {
				t.Errorf("the %s carries the environment's value of a SUBSTITUTED reference (%s): the reading ran after the substitution", tc.where, payload)
			}
		}
	}
}

// Every braced form the reading consulted comes back with its name and its
// arm — the decision record the preview reports: which reads it answered,
// which it resolved to their default, and which it kept as written. A form
// the engine never consults (`${NAME#x}`, a script template literal, a bare
// `$NAME`) is not a read and is not named.
func TestToolBodyEnvNamesWhatItCouldNotAnswer(t *testing.T) {
	body := `A=${SET} B=${UNSET} C=${UNSET:-d} D=${SET#x} E=${p.name} F=$BARE`
	lookup := func(name string) (string, bool) {
		if name == "SET" {
			return "v", true
		}
		return "", false
	}
	read, reads := ToolBodyEnv(toolBodyEnvNode(body, "", ""), body, lookup)
	if want := `A=v B=${UNSET} C=d D=${SET#x} E=${p.name} F=$BARE`; read != want {
		t.Errorf("the body reads %q, want %q", read, want)
	}
	want := []ir.BracedEnvRead{
		{Name: "SET", At: 2},
		{Name: "UNSET", At: 11},
		{Name: "UNSET", HasDefault: true, At: 22},
	}
	if !reflect.DeepEqual(reads, want) {
		t.Errorf("the reads came out as %+v, want %+v — the record the preview reports names each form the engine consults, with its arm and where it opens", reads, want)
	}
	// A body with no environment form is handed on as it is, and names
	// nothing: a `command:` that is a bare registered-tool name passes
	// through the same chokepoint.
	plain, none := ToolBodyEnv(toolBodyEnvNode("Read", "", ""), "Read", lookup)
	if plain != "Read" || len(none) != 0 {
		t.Errorf("a body with no braced form came out as %q with %d reads, want it untouched and silent", plain, len(none))
	}
}

// The run and the preview read the same forms and answer them differently,
// and only where the ENGINE's environment decides: the lookup is consulted
// first on both arms, so a `${NAME:-default}` the engine's process carries
// runs as that value while the preview shows the author's default, and a
// plain `${NAME}` the preview keeps as written. Both name the same reads —
// which is what lets the preview REPORT the difference instead of pretending
// there is none.
func TestToolBodyEnvRunAndPreviewDisagreeOnlyWhereTheEngineDecides(t *testing.T) {
	body := "echo ${DIAL:-d} ${PLAIN}"
	engine := func(name string) (string, bool) {
		switch name {
		case "DIAL":
			return "engine-dial", true
		case "PLAIN":
			return "engine-plain", true
		}
		return "", false
	}
	node := toolBodyEnvNode(body, "", "")
	run, runReads := ToolBodyEnv(node, body, engine)
	preview, previewReads := ToolBodyEnv(node, body, nil)
	if want := "echo engine-dial engine-plain"; run != want {
		t.Errorf("the run reads %q, want %q — the lookup answers both arms", run, want)
	}
	if want := "echo d ${PLAIN}"; preview != want {
		t.Errorf("the preview reads %q, want %q — a nil lookup answers nothing: the `:-` arm takes the author's default and the plain read stays as written", preview, want)
	}
	if !reflect.DeepEqual(runReads, previewReads) {
		t.Errorf("the run named %+v and the preview %+v — the two answer the same reads differently, they do not consult different ones", runReads, previewReads)
	}
	if len(runReads) != 2 {
		t.Fatalf("the reads came out as %+v, want the two forms of the body", runReads)
	}
	// A nil lookup answers NOTHING. It is never forwarded to a helper whose
	// nil means the process environment, so a name this process carries is
	// still kept as written.
	t.Setenv("ITERION_TEST_BODY_PREVIEW", "host-value")
	hostBody := "echo ${ITERION_TEST_BODY_PREVIEW} ${ITERION_TEST_BODY_PREVIEW:-d}"
	if got, _ := ToolBodyEnv(node, hostBody, nil); got != "echo ${ITERION_TEST_BODY_PREVIEW} d" {
		t.Errorf("a nil lookup read this process: %q", got)
	}
	// And the engine's own chain does carry it, so the probe proves the two
	// lookups are really two.
	if got, _ := ToolBodyEnv(node, hostBody, lookupToolEnv); !strings.Contains(got, "host-value") {
		t.Errorf("the engine's lookup did not answer a name this process carries (%q): the comparison above proves nothing", got)
	}
}

// runBodies resolves the three bodies the engine hands a reader through the
// production recipes, which is the only path that shows WHICH environment
// each site speaks for: the lookup is a parameter of the chokepoint now, so a
// witness on the chokepoint alone, or on the scanner's own door
// (expandBracedEnv), holds for whatever environment the test picks and says
// nothing about the sites.
func runBodies(t *testing.T, node *ir.ToolNode) map[string]string {
	t.Helper()
	e := &ClawExecutor{}
	ctx := context.Background()
	shell, _ := e.shellRecipe(ctx, node, nil)
	script, _ := e.scriptRecipe(ctx, node, nil)
	return map[string]string{
		"command":       shell(),
		"script":        script(),
		"postcondition": e.postconditionBody(ctx, node, nil),
	}
}

// Each of the three bodies reads the ENGINE's own environment chain — the
// bot-var overlay before this process (ADR-093) — and not an environment that
// merely happens to carry the name. That chain IS #1522's defect: a review
// table published the author's default "high" beside a reviewer that ran at
// the stored "max".
//
// The overlay is the discriminator, and it is the engine's alone: a site that
// reads this process directly answers nothing here, and a site that reads
// nothing at all answers the author's default. Both arms are exercised at
// each body — a `${NAME:-default}` and a plain `${NAME}` — because a witness
// carrying one arm alone stays green with the other blinded.
func TestEveryRunBodyReadsTheEnginesOwnEnvironmentChain(t *testing.T) {
	const dial = "ITERION_TEST_BODY_RUN_DIAL"
	const plain = "ITERION_TEST_BODY_RUN_PLAIN"
	// Only the overlay carries the two names: the process must not, or the
	// witness would also pass for a site that reads the process alone.
	t.Setenv(dial, "")
	t.Setenv(plain, "")
	for _, name := range []string{dial, plain} {
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unsetenv %s: %v", name, err)
		}
	}
	ir.SetEnvOverlay(func(name string) (string, bool) {
		switch name {
		case dial:
			return "stored-dial", true
		case plain:
			return "stored-plain", true
		}
		return "", false
	})
	defer ir.SetEnvOverlay(nil)

	form := "${" + dial + ":-authors-default} ${" + plain + "}"
	node := toolBodyEnvNode("echo "+form, "x = "+form, "test "+form)
	for where, got := range runBodies(t, node) {
		if !strings.Contains(got, "stored-dial") {
			t.Errorf("the %s reads %q — its `${NAME:-default}` did not take the engine's own value: this site hands the chokepoint an environment that is not the engine's, and the reader gets the author's default while the run holds the stored one (#1522)", where, got)
		}
		if !strings.Contains(got, "stored-plain") {
			t.Errorf("the %s reads %q — its plain `${NAME}` was not answered from the engine's own chain, so it travels to the interpreter as written and whatever the child process carries answers it instead", where, got)
		}
	}
}

// And each of the three reads it under the process-env read policy: a name
// the policy refuses stays the author's text at every body. On a cloud
// process the engine's environment holds the platform's credentials, not the
// tenant's, so a site that read the process directly would hand a tool body
// the value of a refused name.
//
// The permitted twin is asserted at the same three bodies: without it a site
// that expanded nothing at all would pass this as a clean run.
func TestEveryRunBodyRefusesANameTheProcessPolicyRefuses(t *testing.T) {
	const refused = "PROBE_BODY_RUN_API_KEY"
	const permitted = "PROBE_BODY_RUN_DIAL"
	t.Setenv(refused, "refused-probe-value")
	t.Setenv(permitted, "permitted-probe-value")
	ir.SetProcessEnvPolicy(func(name string) bool { return !strings.Contains(name, "KEY") })
	t.Cleanup(func() { ir.SetProcessEnvPolicy(nil) })

	form := "${" + refused + "} ${" + permitted + "}"
	node := toolBodyEnvNode("echo "+form, "x = "+form, "test "+form)
	for where, got := range runBodies(t, node) {
		if strings.Contains(got, "refused-probe-value") {
			t.Errorf("the %s carries the value of a name the process-env policy refuses — this site reads the process outside the policy, which on a cloud engine is the platform's own environment", where)
		}
		if !strings.Contains(got, "${"+refused+"}") {
			t.Errorf("the %s did not keep the refused reference as written: %q", where, got)
		}
		if !strings.Contains(got, "permitted-probe-value") {
			t.Errorf("the %s did not read the permitted twin (%q) — the refusal above would hold for a site that reads no environment at all", where, got)
		}
	}
}

// `env_expansion: none` (ADR-130) is the author taking the engine out of the
// way: at all three bodies the authored environment text reaches the reader
// byte for byte, whatever the engine's own chain carries — the `:-` arm the
// engine answers unconditionally included, and a `js` template literal the
// heuristic only happened to spare. The declaration is the node's, so one
// line covers the three bodies.
//
// Mutation that reddens it: make ToolBodyEnv ignore node.EnvExpansion (call
// the expansion whatever the node declares) — every row goes red on its own
// body, naming the text the engine rewrote.
func TestEnvExpansionNoneKeepsTheBodyVerbatim(t *testing.T) {
	const dial = "ITERION_TEST_BODY_VERBATIM_DIAL"
	const plain = "ITERION_TEST_BODY_VERBATIM_PLAIN"
	// Both the overlay and this process carry the names, so under `braced`
	// every form below WOULD be answered: the verbatim text is the
	// property's doing, not an environment that answers nothing.
	t.Setenv(plain, "host-value")
	ir.SetEnvOverlay(func(name string) (string, bool) {
		if name == dial {
			return "stored-dial", true
		}
		return "", false
	})
	defer ir.SetEnvOverlay(nil)

	command := "echo ${" + dial + ":-authors-default} ${" + plain + "}"
	script := "const n = `${count}`; const h = `${" + plain + "}`;"
	postcondition := "test -n \"${" + dial + ":-authors-default}\""
	declined := &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "body"},
		Command:       command,
		CommandRefs:   mustRefs(command),
		Script:        script,
		ScriptRefs:    mustRefs(script),
		Language:      "js",
		Postcondition: postcondition,
		PostcondRefs:  mustRefs(postcondition),
		EnvExpansion:  ir.EnvExpansionNone,
	}
	for where, want := range map[string]string{"command": command, "script": script, "postcondition": postcondition} {
		if got := runBodies(t, declined)[where]; got != want {
			t.Errorf("the %s reads %q, want the authored text %q — the engine rewrote a body the node declared verbatim", where, got, want)
		}
	}
	// The same node WITHOUT the declaration is expanded, so the assertions
	// above are the property's and not a body nothing would have touched.
	expanded := *declined
	expanded.EnvExpansion = ""
	for where, got := range runBodies(t, &expanded) {
		if got == map[string]string{"command": command, "script": script, "postcondition": postcondition}[where] {
			t.Errorf("the %s was untouched without the declaration (%q) — the fixture does not arm the property", where, got)
		}
	}
	// And the chokepoint names no read under `none`: there is no decision to
	// report, because the engine made none.
	if body, reads := ToolBodyEnv(declined, command, lookupToolEnv); body != command || len(reads) != 0 {
		t.Errorf("ToolBodyEnv read %q with %d reads under `none`, want the body untouched and an empty record", body, len(reads))
	}
}

// The chokepoint fails CLOSED on the declaration's CARRIER, not only on its
// value: a call with no node, or with a word the compiler refuses, keeps the
// body as written. Every call site passes a compiled node today and C004
// stops a third word at compile, so neither row is a live path — the sweeps
// of this package pin WHICH functions reach the chokepoint and with which
// lookup, and none of them reads its first argument, so a future
// `ToolBodyEnv(nil, body, lookupToolEnv)` would pass every one of them while
// rewriting a body an author may have declared verbatim.
//
// Mutation that reddens it: read the declaration as `node != nil &&
// node.EnvExpansion == ir.EnvExpansionNone` again, or return true from
// ir.ToolNode.ExpandsBracedEnv's default arm.
func TestToolBodyEnvFailsClosedWithoutADeclaration(t *testing.T) {
	// A name no process carries, with an author's default: under the reading
	// the `:-` arm is resolved and the text changes, which is what makes the
	// rows below the predicate's doing.
	const absent = "ITERION_TEST_BODY_CARRIER_ABSENT"
	body := "echo ${" + absent + ":-authors-default}"
	for name, node := range map[string]*ir.ToolNode{
		"no node at all":    nil,
		"a word off by one": {BaseNode: ir.BaseNode{ID: "body"}, EnvExpansion: ir.EnvExpansionNone + "e"},
	} {
		got, reads := ToolBodyEnv(node, body, lookupToolEnv)
		if got != body || len(reads) != 0 {
			t.Errorf("%s: ToolBodyEnv read %q with %d reads, want the authored text and an empty record — a body the engine rewrote for a declaration it could not read", name, got, len(reads))
		}
	}
	// The same body on a node that declares nothing IS expanded, so the rows
	// above are the floor's doing and not a body nothing would have touched.
	expanded, reads := ToolBodyEnv(&ir.ToolNode{BaseNode: ir.BaseNode{ID: "body"}}, body, lookupToolEnv)
	if expanded != "echo authors-default" || len(reads) != 1 {
		t.Errorf("an undeclared node read %q with %d reads, want \"echo authors-default\" and one read — the fixture does not arm the floor", expanded, len(reads))
	}
}

// The decline is ENV-scoped: `{{…}}` template substitution is the stage
// after the reading and `none` does not touch it. A body that declines the
// environment forms still gets its references resolved — shell-escaped in a
// `command:`, a JSON literal in a `script:` — because the injection boundary
// is the substitution's, not the expansion's.
func TestEnvExpansionNoneStillSubstitutesTemplates(t *testing.T) {
	command := "echo ${KEEP:-d} {{input.msg}}"
	script := "x = `${KEEP}` + {{input.msg}}"
	node := &ir.ToolNode{
		BaseNode:     ir.BaseNode{ID: "body"},
		Command:      command,
		CommandRefs:  mustRefs(command),
		Script:       script,
		ScriptRefs:   mustRefs(script),
		Language:     "js",
		EnvExpansion: ir.EnvExpansionNone,
	}
	e := &ClawExecutor{}
	ctx := context.Background()
	input := map[string]any{"msg": "a b"}
	shell, _ := e.shellRecipe(ctx, node, input)
	script2, _ := e.scriptRecipe(ctx, node, input)
	if got, want := shell(), "echo ${KEEP:-d} 'a b'"; got != want {
		t.Errorf("the command reads %q, want %q — the reference must still resolve, shell-escaped, beside the declined env form", got, want)
	}
	if got, want := script2(), "x = `${KEEP}` + \"a b\""; got != want {
		t.Errorf("the script reads %q, want %q", got, want)
	}
}
