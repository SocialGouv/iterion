package model

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
	"github.com/SocialGouv/iterion/pkg/store"
)

// bodyTestValue is a declared secret's value: no quote and no shell
// metacharacter, so what a body receives is compared byte for byte.
const bodyTestValue = "bv-9f2c41e7d03a5b68"

// bodyGrantShape is one way a secret is granted, with the reference its
// author writes in a shell body (a command, a postcondition) and in an sh
// script body.
type bodyGrantShape struct {
	name  string
	guard *secretguard.Guard
	vars  map[string]any
	ph    string
	value string
	// command and script are the author's own reference to the secret.
	command, script string
	// defused: the secret is kept off the agent, so a data value holding
	// its placeholder takes the redact-only form (DefuseUngranted) — text
	// either way.
	defused bool
}

// bodyGrantShapes are the four grants: no `sinks:` (every sink), `sinks:`
// with agent, `sinks:` without agent, and the env-only class of a credential
// the server minted for the run, which an author reaches through
// `{{vars.x}}` in a shell body and by naming the variable in a script.
func bodyGrantShapes(t *testing.T) []bodyGrantShape {
	t.Helper()
	declared := func(name string, sinks ...string) bodyGrantShape {
		g := secretguard.New([]secretguard.Secret{{Name: "pw", Value: bodyTestValue, Sinks: sinks}}, secretguard.DefaultConfig())
		return bodyGrantShape{name: name, guard: g, ph: secretguard.PlaceholderForName("pw"), value: bodyTestValue, command: "{{secrets.pw}}", script: "{{secrets.pw}}"}
	}
	noAgent := declared("sinks without agent", secretsink.Command, secretsink.Params)
	noAgent.defused = true
	tok := randomToken(t)
	minted := secretguard.PlaceholderForName(store.ForgePublishTokenVar)
	envOnly := bodyGrantShape{
		name:    "env-only",
		guard:   mintedGrantGuard(tok),
		vars:    map[string]any{store.ForgePublishTokenVar: tok},
		ph:      minted,
		value:   tok,
		command: "{{vars." + store.ForgePublishTokenVar + "}}",
		script:  `"$` + minted + `"`,
	}
	return []bodyGrantShape{
		declared("no sinks"),
		declared("sinks with agent", secretsink.Agent, secretsink.Command, secretsink.Params),
		noAgent,
		envOnly,
	}
}

// mintedGrantGuard is a guard holding a credential the server minted for the
// run, registered as BuildSecretGuard registers the grant the run holds
// (secretguard.go, step 4) when the run carries no endpoint var to scope it:
// redact-only, env-only.
func mintedGrantGuard(tok string) *secretguard.Guard {
	return secretguard.New([]secretguard.Secret{{Name: store.ForgePublishTokenVar, Value: tok, RedactOnly: true, EnvOnly: true}}, secretguard.DefaultConfig())
}

// placeholderDataForm is one way data carries a placeholder into a body:
// the references the body holds, the input that fills them, and what the
// body's process receives — given the placeholder a whole value keeps (the
// placeholder, or its defused form).
type placeholderDataForm struct {
	name  string
	ref   string
	input func(ph string) map[string]any
	// whole: one value holds the whole placeholder, so DefuseUngranted
	// reads it; a placeholder that only forms once rendered is not.
	whole    bool
	received func(kept string) string
}

func placeholderDataForms() []placeholderDataForm {
	same := func(kept string) string { return kept }
	return []placeholderDataForm{
		{"bare", "{{input.d}}", func(ph string) map[string]any { return map[string]any{"d": ph} }, true, same},
		{"single-quoted", "{{input.d}}", func(ph string) map[string]any { return map[string]any{"d": "'" + ph + "'"} }, true,
			func(kept string) string { return "'" + kept + "'" }},
		{"raw single-quoted", "{{!input.d}}", func(ph string) map[string]any { return map[string]any{"d": "'" + ph + "'"} }, true, same},
		{"raw bare", "{{!input.d}}", func(ph string) map[string]any { return map[string]any{"d": ph} }, true, same},
		{"split across two raw values", "{{!input.a}}{{!input.b}}", func(ph string) map[string]any {
			return map[string]any{"a": ph[:12], "b": ph[12:]}
		}, false, same},
		{"after the author's text", "__ITERION_SECRET_{{!input.d}}", func(ph string) map[string]any {
			return map[string]any{"d": strings.TrimPrefix(ph, "__ITERION_SECRET_")}
		}, false, same},
		{"before the author's text", "{{!input.d}}__", func(ph string) map[string]any {
			return map[string]any{"d": strings.TrimSuffix(ph, "__")}
		}, false, same},
	}
}

// bodyRun is what one execution of a tool body left behind.
type bodyRun struct {
	data, own string
	// cmdline is the shell's argv (command and postcondition bodies).
	cmdline string
	// persisted is every rendering the hooks were handed.
	persisted []string
}

// runPlantedBody runs a tool node whose body (kind: command, postcondition,
// script) writes what the data reference renders to data.txt and what the
// author's own reference renders to own.txt, on a real executor under the
// shape's guard.
func runPlantedBody(t *testing.T, kind string, shape bodyGrantShape, form placeholderDataForm) bodyRun {
	t.Helper()
	dir := t.TempDir()
	var persisted []string
	e := newTestClawExecutor(NewRegistry(), &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}},
		WithWorkDir(dir), WithSecretGuard(shape.guard),
		WithEventHooks(EventHooks{OnToolNodeResult: func(_, _ string, input []byte, _ string, _ time.Duration, _ error) {
			persisted = append(persisted, string(input))
		}}))
	e.vars = shape.vars
	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "publish"}}
	switch kind {
	case "command", "postcondition":
		body := "cat /proc/$$/cmdline > cmdline.bin; printf '%s' " + form.ref + " > data.txt; printf '%s' " + shape.command + " > own.txt"
		refs, err := ir.ParseRefs(body)
		if err != nil {
			t.Fatal(err)
		}
		if kind == "command" {
			node.Command, node.CommandRefs = body, refs
		} else {
			// Met at the ladder's first rung: the postcondition is all
			// that runs.
			node.Command, node.Postcondition, node.PostcondRefs = "exit 1", body, refs
		}
	case "script":
		body := "printf '%s' " + form.ref + " > data.txt\nprintf '%s' " + shape.script + " > own.txt\n"
		refs, err := ir.ParseRefs(body)
		if err != nil {
			t.Fatal(err)
		}
		node.Script, node.ScriptRefs, node.Language = body, refs, "sh"
	default:
		t.Fatalf("unknown body kind %q", kind)
	}
	if _, err := e.Execute(context.Background(), node, form.input(shape.ph)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("the body did not write %s: %v", name, err)
		}
		return string(b)
	}
	run := bodyRun{data: read("data.txt"), own: read("own.txt"), persisted: persisted}
	if kind != "script" {
		run.cmdline = strings.ReplaceAll(read("cmdline.bin"), "\x00", " ")
	}
	return run
}

// The guarantee of ADR-146's amendment (2026-11-14, #3268): a placeholder
// that arrives through data is TEXT in every authored body — command,
// postcondition, script — whatever its secret grants, however the data
// spells it (whole, quoted, split across two raw values, completed by the
// author's own text), while the author's own reference beside it still
// receives the value. Observed in the process the body runs: a real shell
// writes what it was handed.
func TestADataPlaceholderIsTextInEveryAuthoredBodyWhateverItsSecretGrants(t *testing.T) {
	t.Parallel()
	for _, shape := range bodyGrantShapes(t) {
		for _, kind := range []string{"command", "postcondition", "script"} {
			for _, form := range placeholderDataForms() {
				t.Run(shape.name+"/"+kind+"/"+form.name, func(t *testing.T) {
					t.Parallel()
					run := runPlantedBody(t, kind, shape, form)
					kept := shape.ph
					if shape.defused && form.whole {
						kept = strings.TrimSuffix(shape.ph, "__") + "_redacted__"
					}
					if want := form.received(kept); run.data != want {
						t.Errorf("the body received %q for the data, want the text %q", run.data, want)
					}
					if strings.Contains(run.data, shape.value) {
						t.Errorf("a placeholder carried by data was materialised: the body received the value")
					}
					if run.own != shape.value {
						t.Errorf("the author's own reference received %q, want the value", run.own)
					}
					if run.cmdline != "" {
						if strings.Contains(run.cmdline, shape.value) {
							t.Errorf("the value entered the argv: %q", run.cmdline)
						}
						if !strings.Contains(run.cmdline, `"$`+shape.ph+`"`) {
							t.Errorf("argv = %q, want the author's reference through the child's environment", run.cmdline)
						}
					}
					if len(run.persisted) == 0 {
						t.Fatal("no rendering reached the hooks")
					}
					for _, p := range run.persisted {
						if strings.Contains(p, shape.value) || strings.Contains(p, authoredMarkerPrefix) {
							t.Errorf("the hooks persisted a value or a marker: %q", p)
						}
						if !strings.Contains(p, shape.ph) {
							t.Errorf("the hooks persisted %q, want the placeholder form", p)
						}
					}
				})
			}
		}
	}
}

// The raw `{{!secrets.X}}` form is the author's own text: it keeps its
// inline fallback, the value written where the reference stands, beside a
// placeholder data carries — which stays text.
func TestTheRawAuthoredReferenceStillInlinesBesideDataText(t *testing.T) {
	t.Parallel()
	g := secretguard.New([]secretguard.Secret{{Name: "pw", Value: bodyTestValue}}, secretguard.DefaultConfig())
	ph := secretguard.PlaceholderForName("pw")
	body := "printf '%s' {{!secrets.pw}} > own.txt; printf '%s' {{!input.d}} > data.txt"
	refs, err := ir.ParseRefs(body)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(dir), WithSecretGuard(g))
	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "raw"}, Command: body, CommandRefs: refs}
	if _, err := e.Execute(context.Background(), node, map[string]any{"d": ph}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for name, want := range map[string]string{"own.txt": bodyTestValue, "data.txt": ph} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// A script body receives the env-only credential its AUTHOR names — the
// placeholder is the variable — and no other: a placeholder that data
// carries into the body names no variable, so a script whose author names
// none runs without the grant in its environment.
func TestAScriptReceivesOnlyTheMintedGrantItsAuthorNames(t *testing.T) {
	t.Parallel()
	tok := randomToken(t)
	ph := secretguard.PlaceholderForName(store.ForgePublishTokenVar)
	body := "env > env.txt\nprintf '%s' {{input.d}} > data.txt\n"
	refs, err := ir.ParseRefs(body)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}}
	e := newTestClawExecutor(NewRegistry(), wf, WithWorkDir(dir), WithSecretGuard(mintedGrantGuard(tok)))
	e.vars = map[string]any{store.ForgePublishTokenVar: tok}
	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "s"}, Script: body, ScriptRefs: refs, Language: "sh"}
	if _, err := e.Execute(context.Background(), node, map[string]any{"d": ph}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	env, err := os.ReadFile(filepath.Join(dir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), tok) {
		t.Error("data naming the minted grant's placeholder handed the grant to a script whose author names none")
	}
	data, err := os.ReadFile(filepath.Join(dir, "data.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != ph {
		t.Errorf("data.txt = %q, want the placeholder text", data)
	}
}

// The agent sink is another path, untouched: a placeholder a MODEL emits in
// its own tool input still materialises where the secret grants `agent` —
// the claw tool loop's and claude_code's PreToolUse materialiser, and the
// self-repair rung's corrected command (plain MaterializeShellEnv).
func TestTheAgentSinkStillMaterialisesAModelsOwnPlaceholder(t *testing.T) {
	t.Parallel()
	g := secretguard.New([]secretguard.Secret{{Name: "pw", Value: bodyTestValue}}, secretguard.DefaultConfig())
	ph := secretguard.PlaceholderForName("pw")
	e := &ClawExecutor{secretGuard: g}
	if m := e.secretMaterializer(); m == nil || m(`{"command":"echo `+ph+`"}`) != `{"command":"echo `+bodyTestValue+`"}` {
		t.Fatal("the agent sink's materialiser no longer resolves a model's own placeholder")
	}

	dir := t.TempDir()
	corrected := fmt.Sprintf("printf '%%s' '%s' > repaired.txt", ph)
	reg := NewRegistry()
	mock := newMockClient(toolUseEvents("t1", "structured_output", fmt.Sprintf(`{"corrected_command":%q}`, corrected), 50, 10))
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	exec := newTestClawExecutor(reg, &ir.Workflow{}, WithWorkDir(dir), WithSecretGuard(g))
	node := &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "repair"},
		Command:       "exit 1",
		Goal:          "write repaired.txt",
		Postcondition: "test -f repaired.txt",
		Policy:        ir.PolicyRecover,
		// A recovery model named, so the rung does not ask the default's
		// funding of the environment: the registry is a mock.
		Recovery: &ir.RecoverySpec{MaxRepairAttempts: 1, Model: "anthropic/claude-test"},
	}
	if _, err := exec.Execute(context.Background(), node, map[string]any{}); err != nil {
		t.Fatalf("Execute: %v — the corrected command did not run", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "repaired.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != bodyTestValue {
		t.Errorf("the model's corrected command received %q, want the value its placeholder names", got)
	}
}

// No guard, or a guard that holds no secret: nothing is marked, the body
// runs exactly as rendered, and nothing joins the child's environment.
func TestABodyWithNoSecretRendersAndRunsAsWritten(t *testing.T) {
	t.Parallel()
	const body = "printf '%s' {{input.d}} {{!input.d}} {{secrets.pw}}"
	refs, err := ir.ParseRefs(body)
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"d": secretguard.PlaceholderForName("pw")}
	plain := renderCommand(body, refs, input, nil, nil, "run", nil, nil, nil, nil)
	for name, guard := range map[string]*secretguard.Guard{
		"nil guard":         nil,
		"guard, no secrets": secretguard.New(nil, secretguard.DefaultConfig()),
	} {
		for kind, rendered := range map[string]authoredBody{
			"command": renderCommandBody(body, refs, input, nil, nil, "run", guard, nil),
			"script":  renderScriptBody(body, refs, input, nil, nil, "run", guard),
		} {
			want := plain
			if kind == "script" {
				want = renderScript(body, refs, input, nil, nil, "run", nil, nil, nil)
			}
			if rendered.text != want || rendered.shown() != want {
				t.Errorf("%s, %s: rendered %q (shown %q), want %q", name, kind, rendered.text, rendered.shown(), want)
			}
			if kind == "script" {
				if out, err := rendered.scriptFor(rendered.shown()); err != nil || out != want {
					t.Errorf("%s, script: runs %q (%v), want %q", name, out, err, want)
				}
				continue
			}
			out, env, err := rendered.shellFor(rendered.shown())
			if err != nil || out != want || env != nil {
				t.Errorf("%s, command: runs %q env %v (%v), want %q and no env", name, out, env, err, want)
			}
		}
	}
}

// A recipe runs the body it rendered and reported, and nothing else: handed
// another text, the build refuses instead of running a command the hooks
// never saw.
func TestABodyRefusesToRunTextItDidNotRender(t *testing.T) {
	t.Parallel()
	g := secretguard.New([]secretguard.Secret{{Name: "pw", Value: bodyTestValue}}, secretguard.DefaultConfig())
	const body = "printf '%s' {{secrets.pw}}"
	refs, err := ir.ParseRefs(body)
	if err != nil {
		t.Fatal(err)
	}
	rendered := renderCommandBody(body, refs, nil, nil, nil, "run", g, nil)
	if _, _, err := rendered.shellFor(rendered.shown() + " extra"); err == nil {
		t.Error("a shell body ran text it did not render")
	}
	if _, err := rendered.scriptFor("other"); err == nil {
		t.Error("a script body wrote text it did not render")
	}
}
