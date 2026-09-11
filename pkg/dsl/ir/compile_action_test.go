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

compute pick:
  output: picked
  expr:
    number: "1"

schema picked:
  number: int

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
  entry: pick
  pick -> comment
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

// TestOrphanTimeoutWarns is the same guard for the property most likely to be
// written by mistake.
//
// `timeout:` is newly accepted on ANY tool node, and only the action path
// reads it: `command: go test ./...` with `timeout: 30s` compiled clean, said
// nothing, and ran with no bound whatsoever. Before the connector recipe
// existed the property was refused as unknown, so the author was told; an
// inert one reads as configured, which is strictly worse than an absent
// control because it is documented.
func TestOrphanTimeoutWarns(t *testing.T) {
	for _, recipe := range []string{
		"command: `go test ./...`",
		"script: `echo hi`\n  language: sh",
	} {
		_, diags := compileSource(t, `
tool t:
  `+recipe+`
  timeout: 30s
workflow main:
  entry: t
  t -> done
`)
		if !hasCode(diags, DiagActionOnlyProperty) {
			t.Errorf("%s: want %s, got %v", recipe, DiagActionOnlyProperty, codesOf(diags))
		}
		if errs := errorsOnly(diags); len(errs) > 0 {
			t.Errorf("%s: an inert property must warn, not fail the compile: %v", recipe, codesOf(errs))
		}
	}
}

// The mirror: on a node that DOES declare an action, `timeout:` is read, so it
// must draw nothing. A warning that fires on the configured case would teach
// authors to ignore C266.
func TestTimeoutOnAnActionNodeIsSilent(t *testing.T) {
	_, diags := compileSource(t, `
tool t:
  action: forgejo.issue.comment
  connection: forge_main
  timeout: 30s
workflow main:
  entry: t
  t -> done
`)
	if hasCode(diags, DiagActionOnlyProperty) {
		t.Errorf("`timeout:` is read on an action node and must draw no C266: %v", codesOf(diags))
	}
}

// TestActionParamRefsAreValidated closes the same hole `script:` had before
// it — and the comment in validate_refs.go records that history, which is the
// point: a third recipe was added without walking the passes that read the
// other two.
//
// An unvalidated reference is not a cosmetic miss. `{{outputs.typo.field}}`
// renders to empty at run time and is SENT, so the vendor receives a silently
// wrong argument — a comment on the wrong issue, a release cut from the wrong
// ref — where the author should have seen C029 at compile time.
func TestActionParamRefsAreValidated(t *testing.T) {
	_, diags := compileSource(t, `
tool comment:
  action: forgejo.issue.comment
  connection: forge_main
  params:
    index: "{{outputs.nowhere.number}}"
workflow main:
  entry: comment
  comment -> done
`)
	if !hasCode(diags, DiagUnknownRefNode) {
		t.Fatalf("a reference to an unknown node inside an action param must be refused, got %v", codesOf(diags))
	}
}

// TestGroupExpansionReachesActionParams covers the other pass that reads a
// recipe's fields.
//
// A group exists to be instantiated per target, so its whole value is that
// `{{params.X}}` becomes the caller's argument. Tool nodes had every scalar
// field substituted and the action params left alone: they would arrive at
// the vendor as the literal text `{{params.repo}}`.
//
// Instantiating the SAME group twice is what proves the copy is DEEP. A
// shallow copy shares the params slice with the template, so the first
// expansion writes its values into it and the second inherits them — two
// calls against the same repository, one of which the author never asked for.
func TestGroupExpansionReachesActionParams(t *testing.T) {
	wf, diags := compileSource(t, `
group commenter(repo):
  tool say:
    action: forgejo.issue.comment
    connection: forge_main
    params:
      repo: "{{params.repo}}"
      body: "hello"

use commenter as first with { repo: "widgets" }
use commenter as second with { repo: "gadgets" }

workflow main:
  entry: first.say
  first.say -> second.say
  second.say -> done
`)
	if errs := errorsOnly(diags); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", codesOf(errs))
	}
	for _, tc := range []struct{ node, want string }{
		{"first.say", "widgets"},
		{"second.say", "gadgets"},
	} {
		n, ok := wf.Nodes[tc.node].(*ToolNode)
		if !ok {
			t.Fatalf("%s is not a tool node: %T", tc.node, wf.Nodes[tc.node])
		}
		var got string
		for _, p := range n.Params {
			if p.Key == "repo" {
				got = p.Value
			}
		}
		if got != tc.want {
			t.Errorf("%s repo = %q, want %q — the group's own value leaked across instantiations", tc.node, got, tc.want)
		}
	}
}

// TestTheRetryRemedyDoesNotTeachTheFormItRefuses.
//
// `retry:` takes a count of EXTRA attempts and nothing else — the delay
// between them is the vendor's Retry-After to name, not the workflow's to
// choose. Three surfaces render that remedy (the property registry, this
// catalogue, docs/references/diagnostics.md) and they drifted one at a time:
// the registry was corrected while the catalogue still read "an attempt count
// or a duration", so an author writing `retry: 1m` was refused by C265 and
// then told by HintFor — which `iterion validate`, the studio badge and the
// MCP result all render — to write exactly that.
//
// A remedy that cannot be followed is worse than none: it sends the author
// round the loop a second time with the compiler's own instructions.
func TestTheRetryRemedyDoesNotTeachTheFormItRefuses(t *testing.T) {
	_, diags := compileSource(t, `
tool t:
  action: forgejo.issue.comment
  connection: c
  retry: 1m
workflow main:
  entry: t
  t -> done
`)
	if !hasCode(diags, DiagActionBadTimeout) {
		t.Fatalf("a duration in `retry:` must be refused, got %v", codesOf(diags))
	}
	hint := HintFor(DiagActionBadTimeout)
	if hint == "" {
		t.Fatal("the code must carry a fix line")
	}
	// The remedy must not offer the form the compiler just refused.
	for _, forbidden := range []string{"or a duration", "or `retry: 1m`", "attempt count or"} {
		if strings.Contains(hint, forbidden) {
			t.Errorf("the remedy offers what C265 refuses (%q): %s", forbidden, hint)
		}
	}
	if !strings.Contains(hint, "attempts") {
		t.Errorf("the remedy must say what `retry:` does take: %s", hint)
	}
}
