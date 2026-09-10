package ir

import (
	"strings"
	"testing"
)

// The connector-action recipe (ADR-098). Every refusal below protects ONE
// promise: an action node reaches a third-party API with no LLM deciding the
// operation, building the arguments or reading the answer.

const actionBot = `
vars:
  owner: string = "acme"

tool comment:
  action: forgejo.issue.comment
  connection: forge_main
  params:
    owner: "{{vars.owner}}"
    repo: "widgets"
    index: "{{outputs.pick.number}}"
    body: "done"
  timeout: 30s
  output: comment_result

schema comment_result:
  status: int

workflow main:
  entry: comment
  comment -> done
`

func compileSource(t *testing.T, src string) (*Workflow, []Diagnostic) {
	t.Helper()
	r := compileFile(t, src)
	return r.Workflow, r.Diagnostics
}

func errorsOnly(diags []Diagnostic) []Diagnostic {
	var out []Diagnostic
	for _, d := range diags {
		if d.Severity == SeverityError {
			out = append(out, d)
		}
	}
	return out
}

func hasCode(diags []Diagnostic, code DiagCode) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

func codesOf(diags []Diagnostic) []string {
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		out = append(out, string(d.Code)+": "+d.Message)
	}
	return out
}

// TestActionCompiles is the happy path: the recipe parses, compiles, and
// every part reaches the IR — including the template refs inside each param,
// without which a value would travel to the vendor as literal `{{...}}`.
func TestActionCompiles(t *testing.T) {
	wf, diags := compileSource(t, actionBot)
	if errs := errorsOnly(diags); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", codesOf(errs))
	}
	node, ok := wf.Nodes["comment"].(*ToolNode)
	if !ok {
		t.Fatalf("comment is not a tool node: %T", wf.Nodes["comment"])
	}
	if node.Action != "forgejo.issue.comment" {
		t.Errorf("action = %q", node.Action)
	}
	if node.Connection != "forge_main" {
		t.Errorf("connection = %q", node.Connection)
	}
	if node.CallTimeout != "30s" {
		t.Errorf("timeout = %q", node.CallTimeout)
	}
	if len(node.Params) != 4 {
		t.Fatalf("params = %d, want 4: %+v", len(node.Params), node.Params)
	}
	// ORDER is the author's, not sorted: a `.bot` is read and diffed.
	wantOrder := []string{"owner", "repo", "index", "body"}
	for i, want := range wantOrder {
		if node.Params[i].Key != want {
			t.Errorf("params[%d] = %q, want %q — the authored order must survive", i, node.Params[i].Key, want)
		}
	}
	// The refs inside a value must be PARSED, or the value reaches the vendor
	// as the literal template text.
	byKey := map[string]ActionParam{}
	for _, p := range node.Params {
		byKey[p.Key] = p
	}
	if len(byKey["owner"].Refs) != 1 {
		t.Errorf("owner refs = %+v, want the {{vars.owner}} ref parsed", byKey["owner"].Refs)
	}
	if len(byKey["repo"].Refs) != 0 {
		t.Errorf("repo is a literal and must carry no refs, got %+v", byKey["repo"].Refs)
	}
}

// TestActionRefusals covers every way the deterministic promise could be
// quietly broken. Each case states what the refusal PREVENTS, because a
// diagnostic nobody can explain gets deleted the first time it is
// inconvenient.
func TestActionRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		bot  string
		want DiagCode
	}{
		{
			// Prevents: an LLM repairing a call the node promised no LLM would
			// touch.
			name: "recovery on an action",
			bot: `
tool t:
  action: forgejo.issue.comment
  connection: c
  policy: recover
  recovery:
    max_agent_attempts: 1
workflow main:
  entry: t
  t -> done
`,
			want: DiagActionRecovery,
		},
		{
			// Prevents: a shell exit code overruling the vendor's own typed
			// answer, so a node reports success on a call that failed.
			name: "postcondition on an action",
			bot: `
tool t:
  action: forgejo.issue.comment
  connection: c
  postcondition: ` + "`test -f /tmp/x`" + `
workflow main:
  entry: t
  t -> done
`,
			want: DiagActionPostcond,
		},
		{
			// Prevents: a call with no credential, which is not a call.
			name: "action with no connection",
			bot: `
tool t:
  action: forgejo.issue.comment
workflow main:
  entry: t
  t -> done
`,
			want: DiagActionNoConnection,
		},
		{
			// Prevents: an id that addresses nothing.
			name: "an action id that is not connector.resource.verb",
			bot: `
tool t:
  action: comment
  connection: c
workflow main:
  entry: t
  t -> done
`,
			want: DiagActionMalformedID,
		},
		{
			// Prevents: sending a value the author did not write, with nothing
			// to notice it.
			name: "a duplicate params key",
			bot: `
tool t:
  action: forgejo.issue.comment
  connection: c
  params:
    body: "one"
    body: "two"
workflow main:
  entry: t
  t -> done
`,
			want: DiagActionBadParam,
		},
		{
			// Prevents: a call with no bound, because the value looked like one.
			name: "a timeout that is not a duration",
			bot: `
tool t:
  action: forgejo.issue.comment
  connection: c
  timeout: soon
workflow main:
  entry: t
  t -> done
`,
			want: DiagActionBadTimeout,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := compileSource(t, tc.bot)
			if !hasCode(diags, tc.want) {
				t.Fatalf("want %s, got %v", tc.want, codesOf(diags))
			}
		})
	}
}

// TestRecipesAreExclusive pins that a tool node has exactly one answer to
// "how does this node do its work". Two answers is not a choice the runtime
// may make on the author's behalf.
func TestRecipesAreExclusive(t *testing.T) {
	_, diags := compileSource(t, `
tool t:
  action: forgejo.issue.comment
  connection: c
  command: `+"`echo hi`"+`
workflow main:
  entry: t
  t -> done
`)
	errs := errorsOnly(diags)
	if len(errs) == 0 {
		t.Fatal("declaring both an action and a command must be refused")
	}
	joined := strings.Join(codesOf(errs), " ")
	if !strings.Contains(joined, "mutually exclusive") {
		t.Errorf("diagnostics = %v, want the exclusivity refusal", codesOf(errs))
	}
}

// TestAToolWithNoRecipeIsStillRefused pins that adding a third recipe did not
// weaken the original requirement: a tool node that declares none is an error,
// and the message must now name all three.
func TestAToolWithNoRecipeIsStillRefused(t *testing.T) {
	_, diags := compileSource(t, `
tool t:
  output: r
schema r:
  ok: bool
workflow main:
  entry: t
  t -> done
`)
	errs := errorsOnly(diags)
	if len(errs) == 0 {
		t.Fatal("a tool node with no recipe must be refused")
	}
	joined := strings.Join(codesOf(errs), " ")
	if !strings.Contains(joined, "action:") {
		t.Errorf("the refusal must name every recipe an author may use, got %v", codesOf(errs))
	}
}

// TestOrphanConnectionWarns pins that a property which only means something
// alongside `action:` is reported when it appears without one. An inert
// property reads as configured, which is how a node ends up looking bound to
// a connection it never uses.
func TestOrphanConnectionWarns(t *testing.T) {
	_, diags := compileSource(t, `
tool t:
  command: `+"`echo hi`"+`
  connection: forge_main
workflow main:
  entry: t
  t -> done
`)
	if !hasCode(diags, DiagActionOnlyProperty) {
		t.Fatalf("want %s, got %v", DiagActionOnlyProperty, codesOf(diags))
	}
	// A warning, not an error: the node is still perfectly runnable.
	if errs := errorsOnly(diags); len(errs) > 0 {
		t.Errorf("an inert property must warn, not fail the compile: %v", codesOf(errs))
	}
}
