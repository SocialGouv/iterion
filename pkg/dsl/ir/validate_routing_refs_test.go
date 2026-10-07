package ir

import (
	"fmt"
	"strings"
	"testing"
)

// The routing fields (model, backend, provider, interaction_model — on the
// node and on its fallbacks routes) resolve vars.* at dispatch and nothing
// else, so the compiler says so: an undeclared var is C033 (an error, as
// everywhere), any other `{{…}}` span — a foreign namespace, a misspelt one,
// a dotted var path, the raw `{{!…}}` form, an unterminated one — is C148,
// a warning (the runtime failure at the first delegation stays loud, and a
// bot in the field keeps compiling); C087 no longer calls a templated
// provider "ignored". A supervisor's model holds the same line at its own
// resolution — whole {{vars.name}} only, resolved at spawn from the run's
// resolved vars. examples/clarify shipped `model: "{{vars.model}}"` and died at
// the first delegation with an "invalid spec" from claw, after a clean
// validate.
func TestRoutingFieldRefs(t *testing.T) {
	const head = "vars:\n  m: string = \"anthropic/claude-sonnet-4-6\"\n  b: string = \"claw\"\n  tags: string[] = \"a,b\"\n\nprompt p:\n  Hi.\n\nschema s:\n  ok: bool\n\n"
	agent := func(props string) string { return "agent a:\n  system: p\n" + props }
	type tc struct {
		name string
		body string
		wf   string   // extra properties of the workflow block
		want DiagCode // "" = none of C033, C148, C087
	}
	cases := []tc{
		{name: "workflow default_backend from a declared var", body: agent(""), wf: "  default_backend: \"{{vars.b}}\"\n"},
		{name: "workflow default_backend from an undeclared var", body: agent(""), wf: "  default_backend: \"{{vars.nope}}\"\n", want: DiagUndeclaredVar},
		{name: "workflow default_backend from an output", body: agent(""), wf: "  default_backend: \"{{outputs.a.b}}\"\n", want: DiagRoutingFieldRef},
		{name: "declared vars resolve in model, backend, provider and interaction_model",
			body: agent("  model: \"{{vars.m}}\"\n  backend: \"{{vars.b}}\"\n  provider: \"{{vars.b}}\"\n  interaction_model: \"{{vars.m}}\"\n")},
		{name: "a plain id and an env form are not references",
			body: agent("  model: \"${M:-anthropic/claude-opus-5}\"\n  backend: \"claw\"\n")},
		{name: "undeclared var in model", body: agent("  model: \"{{vars.nope}}\"\n"), want: DiagUndeclaredVar},
		{name: "undeclared var in backend", body: agent("  backend: \"{{vars.nope}}\"\n"), want: DiagUndeclaredVar},
		{name: "undeclared var in interaction_model", body: agent("  interaction_model: \"{{vars.nope}}\"\n"), want: DiagUndeclaredVar},
		{name: "an output in model", body: agent("  model: \"{{outputs.a.m}}\"\n"), want: DiagRoutingFieldRef},
		{name: "an input in provider", body: agent("  provider: \"{{input.p}}\"\n"), want: DiagRoutingFieldRef},
		{name: "a loop counter in backend", body: agent("  backend: \"{{loop.x.iteration}}\"\n"), want: DiagRoutingFieldRef},
		{name: "a secret in model", body: agent("  model: \"{{secrets.token}}\"\n"), want: DiagRoutingFieldRef},
		{name: "the singular misspelling var.m", body: agent("  model: \"{{var.m}}\"\n"), want: DiagRoutingFieldRef},
		{name: "a capitalised namespace", body: agent("  model: \"{{Vars.m}}\"\n"), want: DiagRoutingFieldRef},
		{name: "a namespace with no path", body: agent("  model: \"{{vars}}\"\n"), want: DiagRoutingFieldRef},
		{name: "an env namespace that does not exist", body: agent("  model: \"{{env.HOME}}\"\n"), want: DiagRoutingFieldRef},
		{name: "an unterminated reference", body: agent("  model: \"{{vars.m\"\n"), want: DiagRoutingFieldRef},
		{name: "a dotted path under a declared var", body: agent("  model: \"{{vars.m.id}}\"\n"), want: DiagRoutingFieldRef},
		{name: "the raw form the resolver does not read", body: agent("  model: \"{{!vars.m}}\"\n"), want: DiagRoutingFieldRef},
		{name: "judge model from an undeclared var",
			body: "judge a:\n  model: \"{{vars.nope}}\"\n  system: p\n  output: s\n", want: DiagUndeclaredVar},
		{name: "fallback route model from an undeclared var",
			body: agent("  model: \"anthropic/claude-sonnet-4-6\"\n  fallbacks:\n    alt:\n      backend: \"claw\"\n      model: \"{{vars.nope}}\"\n"), want: DiagUndeclaredVar},
		{name: "fallback route provider from an output",
			body: agent("  model: \"anthropic/claude-sonnet-4-6\"\n  fallbacks:\n    alt:\n      backend: \"claw\"\n      provider: \"{{outputs.a.p}}\"\n"), want: DiagRoutingFieldRef},
		{name: "human companion model from an output",
			body: "human a:\n  instructions: p\n  output: s\n  interaction: llm\n  system: p\n  model: \"{{outputs.a.m}}\"\n", want: DiagRoutingFieldRef},
		{name: "human interaction_model from an output",
			body: "human a:\n  instructions: p\n  output: s\n  interaction: llm\n  system: p\n  interaction_model: \"{{outputs.a.m}}\"\n", want: DiagRoutingFieldRef},
		{name: "supervisor model from a declared var",
			body: agent("  model: \"anthropic/claude-sonnet-4-6\"\n") + "\nsupervisor sup:\n  watches: [a]\n  model: \"{{vars.m}}\"\n"},
		{name: "supervisor model from an undeclared var",
			body: agent("  model: \"anthropic/claude-sonnet-4-6\"\n") + "\nsupervisor sup:\n  watches: [a]\n  model: \"{{vars.nope}}\"\n", want: DiagUndeclaredVar},
		{name: "supervisor model from an output",
			body: agent("  model: \"anthropic/claude-sonnet-4-6\"\n") + "\nsupervisor sup:\n  watches: [a]\n  model: \"{{outputs.a.m}}\"\n", want: DiagRoutingFieldRef},
		{name: "supervisor model from a dotted vars path",
			body: agent("  model: \"anthropic/claude-sonnet-4-6\"\n") + "\nsupervisor sup:\n  watches: [a]\n  model: \"{{vars.m.id}}\"\n", want: DiagRoutingFieldRef},
		{name: "supervisor model from a list var",
			body: agent("  model: \"anthropic/claude-sonnet-4-6\"\n") + "\nsupervisor sup:\n  watches: [a]\n  model: \"{{vars.tags}}\"\n", want: DiagRoutingFieldRef},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, diags := compileSource(t, head+c.body+"\nworkflow w:\n  entry: a\n"+c.wf+"  a -> done\n")
			var seen []DiagCode
			for _, d := range diags {
				switch d.Code {
				case DiagUndeclaredVar:
					seen = append(seen, d.Code)
					if d.Severity != SeverityError {
						t.Errorf("C033 is %v, want an error", d.Severity)
					}
				case DiagRoutingFieldRef:
					seen = append(seen, d.Code)
					if d.Severity != SeverityWarning {
						t.Errorf("C148 is %v, want a warning (a fielded bot keeps compiling; the node fails loud at its first delegation)", d.Severity)
					}
				case DiagUnknownProvider:
					t.Errorf("C087 on a templated provider: %s", d.Error())
				}
			}
			switch {
			case c.want == "" && len(seen) != 0:
				t.Fatalf("unexpected routing diagnostics %v in\n%s", seen, diags)
			case c.want != "" && len(seen) != 1:
				t.Fatalf("want exactly one %s, got %v in\n%s", c.want, seen, diags)
			case c.want != "" && seen[0] != c.want:
				t.Fatalf("want %s, got %s", c.want, seen[0])
			}
		})
	}
}

// A dotted vars path is not one shape but three: under a `json` var the
// executor DRILLS the document (drillTemplatePath) and resolves the member
// — the launch screen reads it the same way — so there is nothing to warn
// about; under a scalar or list var it can never resolve (a string holds no
// members) and the text reaches the backend as written, which is C148; an
// undeclared root is C033, as a flat undeclared ref draws.
func TestRoutingFieldRefsDottedVarPath(t *testing.T) {
	const head = "vars:\n  cfg: json = \"{\\\"backend\\\": \\\"claw\\\"}\"\n  m: string = \"x\"\n  tags: string[] = \"a,b\"\n\n" +
		"prompt p:\n  Hi.\n\n"
	agent := func(props string) string { return "agent a:\n  system: p\n" + props }
	cases := []struct {
		name string
		body string
		want DiagCode // "" = no C033/C148
	}{
		{"a member of a json var resolves at dispatch", agent("  backend: \"{{vars.cfg.backend}}\"\n"), ""},
		{"a dotted path under a scalar can never resolve", agent("  backend: \"{{vars.m.id}}\"\n"), DiagRoutingFieldRef},
		{"a dotted path under a list can never resolve", agent("  backend: \"{{vars.tags.0}}\"\n"), DiagRoutingFieldRef},
		{"an undeclared root is C033 like a flat one", agent("  backend: \"{{vars.nope.id}}\"\n"), DiagUndeclaredVar},
		{"workflow default_backend drilling a json var", agent(""), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wf := "workflow w:\n  entry: a\n  a -> done\n"
			if c.name == "workflow default_backend drilling a json var" {
				wf = "workflow w:\n  default_backend: \"{{vars.cfg.backend}}\"\n  entry: a\n  a -> done\n"
			}
			_, diags := compileSource(t, head+c.body+"\n"+wf)
			var seen []DiagCode
			for _, d := range diags {
				if d.Code == DiagUndeclaredVar || d.Code == DiagRoutingFieldRef {
					seen = append(seen, d.Code)
				}
			}
			switch {
			case c.want == "" && len(seen) != 0:
				t.Fatalf("unexpected routing diagnostics %v in\n%s", seen, diags)
			case c.want != "" && (len(seen) != 1 || seen[0] != c.want):
				t.Fatalf("want exactly one %s, got %v in\n%s", c.want, seen, diags)
			}
		})
	}
}

// A routing field is a scalar by nature — its text becomes a name at
// dispatch — so a `{{vars.x}}` span that DOES resolve is still unroutable
// when the var it names is declared `string[]`/`json` (#1605): the
// resolved text is the list's JSON spelling (`["claw","claude_code"]`),
// which the backend registry rejects after the workspace and the sandbox
// have been paid for. It is C148, the failure the family exists to
// describe, and a warning like the rest of it: a launch override can
// still hand the var a scalar.
func TestRoutingFieldListTypedVar(t *testing.T) {
	// The json defaults are written as the catalogue writes them — a
	// backtick document, or a bare word/number — because a profile-1 `\"`
	// escapes nothing and the compiler keeps the backslashes (the default
	// is then not JSON at all, and the run reads it as the string it is).
	const head = `vars:
  m: string = "anthropic/claude-sonnet-4-6"
  b: string = "claw"
  bs: string[] = "claw,claude_code"
  cfg: json = ` + "`" + `{"backend": "claw"}` + "`" + `
  js: json = ` + "`" + `"claude_code"` + "`" + `
  jn: json = "3"
  jw: json = "claude_code"
  ja: json = ` + "`" + `["a", "b"]` + "`" + `
  j0: json = "null"
  je: json = "${BACKEND_JSON}"
  jcost: json = ` + "`" + `"gpt $1"` + "`" + `
  jawk: json = ` + "`" + `{"awk": "{print $1}"}` + "`" + `
  jx: json

prompt p:
  Hi.

schema s:
  ok: bool

`
	agent := func(props string) string { return "agent a:\n  system: p\n" + props }
	cases := []struct {
		name string
		body string
		wf   string // extra properties of the workflow block
		want int    // C148 count
	}{
		{name: "the ticket's probe: a string[] var as backend", body: agent("  backend: \"{{vars.bs}}\"\n"), want: 1},
		{name: "a json var as backend", body: agent("  backend: \"{{vars.cfg}}\"\n"), want: 1},
		{name: "a string[] var as model", body: agent("  model: \"{{vars.bs}}\"\n"), want: 1},
		{name: "a string[] var as provider", body: agent("  provider: \"{{vars.bs}}\"\n"), want: 1},
		{name: "a string[] var as interaction_model", body: agent("  interaction_model: \"{{vars.bs}}\"\n"), want: 1},
		{name: "a string[] var on a fallback route",
			body: agent("  model: \"anthropic/claude-sonnet-4-6\"\n  fallbacks:\n    alt:\n      backend: \"claw\"\n      model: \"{{vars.bs}}\"\n"), want: 1},
		{name: "a string[] var as the workflow default_backend", body: agent(""), wf: "  default_backend: \"{{vars.bs}}\"\n", want: 1},
		{name: "two list-typed fields fire once each",
			body: agent("  model: \"{{vars.bs}}\"\n  backend: \"{{vars.cfg}}\"\n"), want: 2},
		{name: "scalar vars stay silent", body: agent("  model: \"{{vars.m}}\"\n  backend: \"{{vars.b}}\"\n"), want: 0},
		{name: "a scalar workflow default_backend stays silent", body: agent(""), wf: "  default_backend: \"{{vars.b}}\"\n", want: 0},
		// The MEDIUM review finding: a `json` var is not a list by
		// declaration — its default DOCUMENT decides. A scalar document
		// resolves to the routable scalar with no override involved.
		{name: "a json var whose default document is a string scalar stays silent",
			body: agent("  backend: \"{{vars.js}}\"\n"), want: 0},
		{name: "a json var whose default document is a number stays silent",
			body: agent("  backend: \"{{vars.jn}}\"\n"), want: 0},
		{name: "a json var whose default is a non-JSON word stays silent (it resolves to that word)",
			body: agent("  backend: \"{{vars.jw}}\"\n"), want: 0},
		{name: "a json var whose default document is a list warns",
			body: agent("  backend: \"{{vars.ja}}\"\n"), want: 1},
		{name: "a json var whose default document is null warns",
			body: agent("  backend: \"{{vars.j0}}\"\n"), want: 1},
		{name: "a json var with no default warns (the launch supplies the document)",
			body: agent("  backend: \"{{vars.jx}}\"\n"), want: 1},
		// A `${...}` never changes a document's shape: the run parses the
		// default first (not JSON → a string) and only then expands it, so
		// `"${BACKEND_JSON}"` is the routable string it expands to.
		{name: "a json var whose default is a ${...} reference stays silent (it resolves to a string)",
			body: agent("  backend: \"{{vars.je}}\"\n"), want: 0},
		// The round-2 MEDIUM: a json leaf's `$1` is DATA under the run's
		// braced-only reading — never expanded — and the document is the
		// scalar string. Warn here was a doubly-false mechanism.
		{name: "a json scalar whose text carries $1 stays silent (the braced-only reading never touches it)",
			body: agent("  model: \"{{vars.jcost}}\"\n"), want: 0},
		{name: "a json object whose leaf carries $-data still warns for the document's shape, not its text",
			body: agent("  backend: \"{{vars.jawk}}\"\n"), want: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, diags := compileSource(t, head+c.body+"\nworkflow w:\n  entry: a\n"+c.wf+"  a -> done\n")
			got := 0
			for _, d := range diags {
				if d.Code == DiagRoutingFieldRef {
					got++
					if d.Severity != SeverityWarning {
						t.Errorf("C148 is %v, want a warning — a launch override can still hand the var a scalar", d.Severity)
					}
				}
			}
			if got != c.want {
				t.Fatalf("C148 count = %d, want %d in\n%s", got, c.want, diags)
			}
		})
	}
}

// The message names the field, the var, its declared type and the failure
// — the author has to find one line in a file of them, and the remedy (a
// `string` var, or a launch-time scalar override) is on it.
func TestRoutingFieldListTypedVarMessage(t *testing.T) {
	const src = "vars:\n  bs: string[] = \"claw,claude_code\"\n\nprompt p:\n  Hi.\n\nagent a:\n  system: p\n  backend: \"{{vars.bs}}\"\n\nworkflow w:\n  entry: a\n  a -> done\n"
	_, diags := compileSource(t, src)
	var msg string
	for _, d := range diags {
		if d.Code == DiagRoutingFieldRef {
			msg = d.Message
		}
	}
	if msg == "" {
		t.Fatalf("no C148 raised\ndiagnostics: %v", diags)
	}
	for _, want := range []string{`agent "a" backend`, "{{vars.bs}}", "`string[]`", "first delegation"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not carry %q", msg, want)
		}
	}
}

// TestRoutingFieldNullJsonVarMessage: a null document does not resolve to
// a JSON spelling and does not fail a delegation — it renders as the
// empty string (formatValue), and an empty routing field is an unset one
// that falls back to its default (resolveBackend reads "" as "not
// named"). The message says that, never the list mechanism.
func TestRoutingFieldNullJsonVarMessage(t *testing.T) {
	const src = "vars:\n  j0: json = \"null\"\n\nprompt p:\n  Hi.\n\nagent a:\n  system: p\n  backend: \"{{vars.j0}}\"\n\nworkflow w:\n  entry: a\n  a -> done\n"
	_, diags := compileSource(t, src)
	var msg string
	for _, d := range diags {
		if d.Code == DiagRoutingFieldRef {
			msg = d.Message
		}
	}
	if msg == "" {
		t.Fatalf("no C148 raised\ndiagnostics: %v", diags)
	}
	for _, want := range []string{"{{vars.j0}}", "null", "empty string", "UNSET", "falls back"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not carry %q", msg, want)
		}
	}
	for _, bad := range []string{"JSON spelling", "first delegation"} {
		if strings.Contains(msg, bad) {
			t.Errorf("message %q claims %q — a null document renders empty and fails nothing", msg, bad)
		}
	}
}

// TestJsonDefaultShapeIsFixedByItsText pins the run behaviour the json
// arm's reading (jsonDefaultDocument) rests on: ResolveVarText parses a
// json default BEFORE it expands anything, and expands only the string
// leaves, braced-only. So a reference never changes the document's shape
// — whatever the environment holds, even a list-looking text — and a
// json leaf's bare `$NAME` is data the run never touches (the round-2
// MEDIUM, executed: a json default "gpt $1" resolved to "gpt $1"). If
// the run ever re-parsed an expansion, `"${DOC}"` could become a list and
// C148/C180 would read the wrong shape.
func TestJsonDefaultShapeIsFixedByItsText(t *testing.T) {
	listLooking := func(string) string { return `["a","b"]` }
	cases := []struct {
		def  string
		want string // the shape both readings must agree on
	}{
		{`${DOC}`, "string"},
		{`"${DOC}"`, "string"},
		{`{"a": ${DOC}}`, "string"}, // not JSON as written: the text stays a string
		{`["${DOC}"]`, "list"},
		{`{"dir": "${PROJECT_DIR}/x"}`, "object"},
		{`"gpt $1"`, "string"},
		{`{"awk": "{print $1}"}`, "object"},
	}
	shape := func(v any) string {
		switch v.(type) {
		case string:
			return "string"
		case []any:
			return "list"
		case map[string]any:
			return "object"
		}
		return fmt.Sprintf("%T", v)
	}
	for _, c := range cases {
		static, _ := jsonDefaultDocument(&Var{Type: VarJSON, HasDefault: true, Default: c.def})
		run, err := ResolveVarText(c.def, VarJSON, listLooking)
		if err != nil {
			t.Fatalf("ResolveVarText(%q): %v", c.def, err)
		}
		if shape(static) != c.want || shape(run) != c.want {
			t.Errorf("%s: static shape %s, run shape %s, want %s — the run's reading moved the document's shape", c.def, shape(static), shape(run), c.want)
		}
	}
	// A bare `$NAME` in a json leaf is data: the run leaves it as written.
	for _, def := range []string{`"gpt $1"`, `"$HOME of the brave"`, `"100$"`} {
		v, err := ResolveVarText(def, VarJSON, func(string) string { return "SET" })
		if s, ok := v.(string); err != nil || !ok || strings.Contains(s, "SET") || !strings.Contains(s, "$") {
			t.Errorf("the run rewrote the json leaf of %s into %#v (err %v) — a bare $ is data", def, v, err)
		}
	}
}
