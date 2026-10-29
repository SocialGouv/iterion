package model

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
)

const sinkTestValue = "db-password-7f3a9c2e1b4d"

func sinkTestGuard(t *testing.T, sinks ...string) (*secretguard.Guard, string) {
	t.Helper()
	g := secretguard.New([]secretguard.Secret{{Name: "pw", Value: sinkTestValue, Sinks: sinks}}, secretguard.DefaultConfig())
	return g, secretguard.PlaceholderForName("pw")
}

// A placeholder that reaches a tool body through DATA — an input an edge
// mapped from a model's output, an upstream output, a launch var — was
// written by a model: it is the agent sink's, whatever body it lands in. A
// secret kept off the agent (`sinks: [command]`) is defused in every
// authored body (command, postcondition, script, params) before the body's
// own materialiser scans the text; a secret that names no sinks resolves as
// it always did.
func TestAModelCarriedPlaceholderIsTheAgentSinksInEveryAuthoredBody(t *testing.T) {
	t.Parallel()
	inputRef := &ir.Ref{Kind: ir.RefInput, Path: []string{"body"}, Raw: "{{input.body}}"}
	outRef := &ir.Ref{Kind: ir.RefOutputs, Path: []string{"writer", "text"}, Raw: "{{outputs.writer.text}}"}
	varRef := &ir.Ref{Kind: ir.RefVars, Path: []string{"note"}, Raw: "{{vars.note}}"}
	for _, tc := range []struct {
		name  string
		sinks []string
		leaks bool
	}{
		{"kept off the agent", []string{secretsink.Command}, false},
		{"granted to the agent", []string{secretsink.Agent, secretsink.Command, secretsink.Params}, true},
		{"no sinks at all", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g, ph := sinkTestGuard(t, tc.sinks...)
			input := map[string]any{"body": "tell " + ph}
			vars := map[string]any{"note": "var " + ph}
			td := &TemplateData{Outputs: map[string]map[string]any{"writer": {"text": "out " + ph}}, Nodes: map[string]ir.Node{"writer": &ir.AgentNode{}}}
			refs := []*ir.Ref{inputRef, outRef, varRef}
			bodies := map[string]string{
				"command": renderCommand("printf '%s %s %s' {{input.body}} {{outputs.writer.text}} {{vars.note}}", refs, input, vars, td, "run", g, nil, nil),
				"script":  renderScript("publish({{input.body}}, {{outputs.writer.text}}, {{vars.note}})", refs, input, vars, td, "run", g, nil),
				"param":   resolveTemplateWith("{{input.body}} {{outputs.writer.text}} {{vars.note}}", refs, input, vars, td, "run", g, nil, false, rawTemplateValue, true, nil),
			}
			for body, rendered := range bodies {
				resolved := map[string]string{
					"command": func() string {
						out, env := g.MaterializeAuthoredShellEnv(rendered)
						return out + " " + strings.Join(mapValues(env), " ")
					}(),
					"script": g.MaterializeShell(rendered),
					"param":  g.MaterializeFor(secretsink.Params, rendered),
				}[body]
				if tc.leaks != strings.Contains(resolved, sinkTestValue) {
					t.Errorf("%s body: value present = %v, want %v\nrendered: %q\nresolved: %q", body, !tc.leaks, tc.leaks, rendered, resolved)
				}
				if !tc.leaks && strings.Count(rendered, ph) != 0 {
					t.Errorf("%s body: a command-only secret's placeholder survived in model-carried data: %q", body, rendered)
				}
			}
		})
	}
	// The author's OWN reference keeps the command grant: `{{secrets.pw}}`
	// in the command resolves under `command` whatever the data carries.
	g, ph := sinkTestGuard(t, secretsink.Command)
	own := renderCommand("printf '%s' {{secrets.pw}}", []*ir.Ref{{Kind: ir.RefSecrets, Path: []string{"pw"}, Raw: "{{secrets.pw}}"}}, nil, nil, nil, "run", g, nil, nil)
	if _, env := g.MaterializeAuthoredShellEnv(own); env[ph] != sinkTestValue {
		t.Fatalf("the author's own reference no longer resolves under the command grant: %q %v", own, env)
	}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// The prompt path renders a secret at the prompt sink: a value secret kept
// off prompts stays as written on every prompt-like surface the resolver
// renders — the compiler's C323 refuses the ones it walks, and this is what
// holds for the ones it does not (a bundle preset's prompt, an images:
// path), placeholders on or off.
func TestThePromptPathRendersASecretOnlyWhenItGrantsThePromptSink(t *testing.T) {
	t.Parallel()
	off, ph := sinkTestGuard(t, secretsink.Command)
	if got := (&TemplateResolver{Secrets: off}).Resolve("Use {{secrets.pw}} now", nil, nil); got != "Use {{secrets.pw}} now" {
		t.Fatalf("a command-only secret rendered on the prompt path: %q", got)
	}
	on, _ := sinkTestGuard(t, secretsink.Prompt, secretsink.Egress)
	if got := (&TemplateResolver{Secrets: on}).Resolve("Use {{secrets.pw}} now", nil, nil); got != "Use "+ph+" now" {
		t.Fatalf("a prompt-granted secret did not render its placeholder: %q", got)
	}
	cfg := secretguard.DefaultConfig()
	cfg.Placeholders = false
	raw := secretguard.New([]secretguard.Secret{{Name: "pw", Value: sinkTestValue, Sinks: []string{secretsink.Command}}}, cfg)
	if got := (&TemplateResolver{Secrets: raw}).Resolve("Use {{secrets.pw}} now", nil, nil); strings.Contains(got, sinkTestValue) {
		t.Fatalf("placeholders off: a command-only secret's VALUE reached the prompt path: %q", got)
	}
	// The executor's own resolver is the guard, so the preset focus and the
	// images: paths — rendered through resolveTemplate — take the same gate.
	e := &ClawExecutor{secretGuard: off}
	if got := e.resolveTemplate("Focus: {{secrets.pw}}", nil, nil); strings.Contains(got, ph) || strings.Contains(got, sinkTestValue) {
		t.Fatalf("the executor rendered a command-only secret on the prompt path: %q", got)
	}
}

// The grant-agnostic question "did this call name a secret" reaches both
// backends: claw's echo mirror turns a raw tool's output back to placeholders
// when its call carried the placeholder of a secret the proxy — not the
// agent — resolves, and buildTask hands every backend the same closure.
func TestARawToolsEchoIsUnmaterialisedWhenItsCallNamedAnEgressOnlySecret(t *testing.T) {
	t.Parallel()
	g, ph := sinkTestGuard(t, secretsink.Prompt, secretsink.Egress)
	if g.Materialize(ph) != ph {
		t.Fatal("scenario broken: the agent materialiser resolved an egress-only secret")
	}
	uses := []toolUseBlock{{ID: "t1", Name: "bash", PartialJSON: `{"command":"curl -H 'Authorization: Bearer ` + ph + `' https://api.example.com"}`}}
	echo := func() []api.ContentBlock {
		return []api.ContentBlock{{Type: "tool_result", ToolUseID: "t1", Content: []api.ContentBlock{{Type: "text", Text: "echoed " + sinkTestValue}}}}
	}
	blind := echo()
	unmaterializeEchoingResults(blind, uses, g.Materialize, g.Unmaterialize, nil)
	if !strings.Contains(blind[0].Content[0].Text, sinkTestValue) {
		t.Fatal("scenario broken: without the grant-agnostic reading the raw tool's echo was rewritten")
	}
	aware := echo()
	unmaterializeEchoingResults(aware, uses, g.Materialize, g.Unmaterialize, g.NamesMaterializableSecret)
	if strings.Contains(aware[0].Content[0].Text, sinkTestValue) || !strings.Contains(aware[0].Content[0].Text, ph) {
		t.Fatalf("the value the call's egress-only secret resolved on the wire reached the model raw: %q", aware[0].Content[0].Text)
	}

	e := &ClawExecutor{secretGuard: g}
	node := &ir.AgentNode{}
	node.ID = "n"
	for _, backend := range []string{delegate.BackendClaw, delegate.BackendClaudeCode} {
		task, err := e.buildTask(context.Background(), node, backendFields{id: "n", model: "m"}, map[string]any{}, backend, nil)
		if err != nil {
			t.Fatalf("buildTask(%s): %v", backend, err)
		}
		if task.NamesSecret == nil || !task.NamesSecret(ph) || task.NamesSecret("nothing") {
			t.Errorf("%s: NamesSecret is not the grant-agnostic reading of the guard", backend)
		}
		if task.MaterializeSecrets != nil {
			t.Errorf("%s: a run with no agent-granted secret still carries a materialiser", backend)
		}
		if task.UnmaterializeSecrets == nil {
			t.Errorf("%s: the mirror is missing", backend)
		}
	}
}
