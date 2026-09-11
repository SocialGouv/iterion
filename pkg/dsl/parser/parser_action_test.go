package parser_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The connector-action recipe (ADR-098) had no parser test of its own, which
// is how a scalar reader that silently concatenated two bare words shipped:
// nothing here read a `params:` value back.

func TestParseActionNode(t *testing.T) {
	src := `tool comment:
  action: forgejo.issue.comment
  connection: forge_main
  params:
    owner: "acme"
    repo: "widgets"
    index: "{{outputs.pick.number}}"
    body: "hello world"
    state: open
  retry: 3
  timeout: 30s
`
	res := parser.Parse("test.bot", src)
	assertNoDiags(t, res)
	if len(res.File.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(res.File.Tools))
	}
	tn := res.File.Tools[0]
	assertEq(t, "Action", tn.Action, "forgejo.issue.comment")
	assertEq(t, "Connection", tn.Connection, "forge_main")
	assertEq(t, "Retry", tn.Retry, "3")
	assertEq(t, "Timeout", tn.Timeout, "30s")

	want := []struct{ key, value string }{
		{"owner", "acme"},
		{"repo", "widgets"},
		{"index", "{{outputs.pick.number}}"},
		// A QUOTED two-word value must survive intact — the string branch
		// never went through the join, and it must stay that way.
		{"body", "hello world"},
		{"state", "open"},
	}
	if len(tn.Params) != len(want) {
		t.Fatalf("Params = %d entries, want %d: %+v", len(tn.Params), len(want), tn.Params)
	}
	for i, w := range want {
		// ORDER is part of the contract: a `.bot` is read and diffed by
		// humans, so the author's own argument order is kept.
		if tn.Params[i].Key != w.key || tn.Params[i].Value != w.value {
			t.Errorf("param %d = %q: %q, want %q: %q", i, tn.Params[i].Key, tn.Params[i].Value, w.key, w.value)
		}
	}
}

// The defect: two bare words were joined with no separator, so `body: hello
// world` reached the vendor as "helloworld" with no diagnostic at all — a
// value the author never wrote, on the one recipe whose whole promise is that
// the request is what was declared.
func TestParseActionParamRefusesAnUnquotedMultiWordValue(t *testing.T) {
	res := parser.Parse("test.bot", `tool comment:
  action: forgejo.issue.comment
  connection: forge_main
  params:
    body: hello world
`)
	if len(res.Diagnostics) == 0 {
		t.Fatal("an unquoted two-word value must be diagnosed, not silently joined")
	}
	joined := diagText(res)
	if !strings.Contains(joined, "body") {
		t.Errorf("the diagnostic must name the parameter, got: %s", joined)
	}
	if !strings.Contains(joined, "quoted") {
		t.Errorf("the diagnostic must say the remedy, got: %s", joined)
	}
	// The hint echoes the value as the author wrote it.
	if !strings.Contains(joined, `"hello world"`) {
		t.Errorf("the hint must show the value to write, got: %s", joined)
	}
	// The corrupted form must never be what the AST carries.
	if len(res.File.Tools) == 1 {
		for _, p := range res.File.Tools[0].Params {
			if p.Value == "helloworld" {
				t.Error("the two words were concatenated into the value anyway")
			}
		}
	}
	// LOAD-BEARING: an ERROR, not a warning. The AST keeps the partial head
	// (`hello`) so tooling has something to show, and only the severity stops
	// that truncated value from being sent — a warning here would be a worse
	// bug than the join, since the vendor would receive half the argument.
	var fatal bool
	for _, d := range res.Diagnostics {
		if d.Severity == parser.SeverityError {
			fatal = true
		}
	}
	if !fatal {
		t.Error("an unquotable value must be an error: a warning would let the truncated value be sent")
	}
}

// The third word used to be read as the NEXT parameter name, so the author got
// a diagnostic about something they never wrote.
func TestParseActionParamMultiWordDoesNotInventAParameter(t *testing.T) {
	res := parser.Parse("test.bot", `tool comment:
  action: forgejo.issue.comment
  connection: forge_main
  params:
    title: fix the parser
    repo: "widgets"
`)
	if len(res.File.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(res.File.Tools))
	}
	for _, p := range res.File.Tools[0].Params {
		switch p.Key {
		case "title", "repo":
		default:
			t.Errorf("parser invented parameter %q from a multi-word value", p.Key)
		}
	}
	// The remedy shows the author their own value, written the way it has to
	// be written.
	if txt := diagText(res); !strings.Contains(txt, "fix the parser") {
		t.Errorf("the hint must echo the whole value, got: %s", txt)
	}
}

// A duration is the one value the lexer splits in two, and the join exists for
// it alone.
func TestParseActionDurationsAndCounts(t *testing.T) {
	for _, tc := range []struct{ src, timeout, retry string }{
		{"  timeout: 30s\n  retry: 3\n", "30s", "3"},
		{"  timeout: 2m\n  retry: 10\n", "2m", "10"},
		{"  timeout: 1500ms\n  retry: 0\n", "1500ms", "0"},
		{"  timeout: \"45s\"\n  retry: \"2\"\n", "45s", "2"},
	} {
		res := parser.Parse("test.bot", "tool comment:\n  action: forgejo.issue.comment\n  connection: c\n"+tc.src)
		assertNoDiags(t, res)
		if len(res.File.Tools) != 1 {
			t.Fatalf("expected 1 tool for %q", tc.src)
		}
		assertEq(t, "Timeout", res.File.Tools[0].Timeout, tc.timeout)
		assertEq(t, "Retry", res.File.Tools[0].Retry, tc.retry)
	}
}

// A dotted id is reassembled from the tokens the lexer hands back around the
// dots; a quoted one is accepted as written.
func TestParseActionID(t *testing.T) {
	for _, tc := range []struct{ written, want string }{
		{"forgejo.issue.comment", "forgejo.issue.comment"},
		{`"forgejo.issue.comment"`, "forgejo.issue.comment"},
		{"forgejo.pull_request.merge", "forgejo.pull_request.merge"},
	} {
		res := parser.Parse("test.bot", "tool t:\n  action: "+tc.written+"\n  connection: c\n")
		assertNoDiags(t, res)
		assertEq(t, "Action", res.File.Tools[0].Action, tc.want)
	}
}

// A trailing dot produces a diagnostic where the mistake is, rather than a
// truncated id that fails far away as "unknown operation".
func TestParseActionIDTrailingDotIsDiagnosed(t *testing.T) {
	res := parser.Parse("test.bot", "tool t:\n  action: forgejo.issue.\n  connection: c\n")
	if len(res.Diagnostics) == 0 {
		t.Fatal("a trailing dot in an operation id must be diagnosed")
	}
}

func diagText(res *parser.ParseResult) string {
	var b strings.Builder
	for _, d := range res.Diagnostics {
		b.WriteString(d.Message)
		b.WriteByte(' ')
		b.WriteString(d.Hint)
		b.WriteByte('\n')
	}
	return b.String()
}

// A value the lexer split on PUNCTUATION is one word to its author. The
// remedy must hand back what they wrote — gluing the pieces with spaces would
// change the value they were trying to send, which is the same defect as the
// join this replaced.
func TestParseActionParamHintKeepsPunctuatedValuesIntact(t *testing.T) {
	for _, written := range []string{"refs/heads/main", "my-repo", "a.b.c", "1.2.3-rc1"} {
		res := parser.Parse("test.bot", "tool t:\n  action: p.r.v\n  connection: c\n  params:\n    k: "+written+"\n")
		if len(res.Diagnostics) == 0 {
			t.Errorf("%q: an unquoted punctuated value must be diagnosed", written)
			continue
		}
		txt := diagText(res)
		if !strings.Contains(txt, `"`+written+`"`) {
			t.Errorf("%q: the hint must echo it verbatim, got: %s", written, txt)
		}
		// The wording must not accuse them of writing several words.
		if strings.Contains(txt, "more than one word") {
			t.Errorf("%q: the message describes a mistake the author did not make: %s", written, txt)
		}
	}
}

// A trailing comment must never change whether a line parses.
//
// `scanComment` consumes the newline and emits TokenComment in its place, so
// there is no TokenNewline behind `timeout: 30s # keep it short`. The scalar
// reader did not know that: it read a valid line as a value that is not a
// single bare word (an E020 error on correct source), and then consumed the
// comment and kept going into the NEXT line — so `output:` was deleted from
// the node too. Every property of this recipe reads through that path.
func TestParseActionPropertiesAcceptATrailingComment(t *testing.T) {
	res := parser.Parse("test.bot", `tool comment:
  action: forgejo.issue.comment   # the operation
  connection: forge_main  ## the connection
  params:
    owner: acme     # the org
    index: 42       # the issue
  retry: 3      # extra attempts
  timeout: 30s   # keep the call short
  output: comment_result
`)
	assertNoDiags(t, res)
	if len(res.File.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(res.File.Tools))
	}
	tn := res.File.Tools[0]
	assertEq(t, "Action", tn.Action, "forgejo.issue.comment")
	assertEq(t, "Connection", tn.Connection, "forge_main")
	assertEq(t, "Retry", tn.Retry, "3")
	assertEq(t, "Timeout", tn.Timeout, "30s")
	// The property AFTER the commented one must survive: the reader used to
	// run past the comment and swallow it.
	assertEq(t, "Output", tn.Output, "comment_result")
	if len(tn.Params) != 2 {
		t.Fatalf("Params = %d, want 2: %+v", len(tn.Params), tn.Params)
	}
	for _, want := range []struct{ key, value string }{{"owner", "acme"}, {"index", "42"}} {
		var got string
		for _, p := range tn.Params {
			if p.Key == want.key {
				got = p.Value
			}
		}
		if got != want.value {
			t.Errorf("param %s = %q, want %q — the comment must not reach the value", want.key, got, want.value)
		}
	}
}

// The unit join is licensed by ADJACENCY, not by the head being a number.
//
// `30s` is one value the lexer split at a boundary with no space in it. The
// first fix restricted the join to a numeric head, which left `body: 2
// failures` joined into `2failures` — and silently, because after the join
// the line IS ended, so the refusal below never fires. A vendor receiving a
// value the author never wrote is this recipe's whole failure mode.
func TestParseActionParamRefusesANumberFollowedByAWord(t *testing.T) {
	res := parser.Parse("test.bot", `tool comment:
  action: forgejo.issue.comment
  connection: forge_main
  params:
    body: 2 failures
`)
	if len(res.Diagnostics) == 0 {
		t.Fatal("a number followed by a word must be diagnosed, not joined")
	}
	if txt := diagText(res); !strings.Contains(txt, `"2 failures"`) {
		t.Errorf("the hint must echo the value as written, got: %s", txt)
	}
	if len(res.File.Tools) == 1 {
		for _, p := range res.File.Tools[0].Params {
			if p.Value == "2failures" {
				t.Error("the number and the word were concatenated into the value anyway")
			}
		}
	}
	var fatal bool
	for _, d := range res.Diagnostics {
		if d.Severity == parser.SeverityError {
			fatal = true
		}
	}
	if !fatal {
		t.Error("it must be an error: a warning would let the truncated value be sent")
	}
}

// A `params:` body that is not an indented block is DIAGNOSED, never dropped.
//
// The hand-rolled reader consumed the rest of the line whenever it did not
// find an INDENT, which covered two very different inputs with silence: a
// bare `params:` followed by a sibling property swallowed that property's
// line, and an inline `params: { owner: "acme" }` dropped every argument. An
// action then called the vendor with no filters, or ran unbounded while the
// source read as capped.
func TestParseActionParamsBlockDiagnosesAMalformedBody(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"a sibling property at the same indent", `tool comment:
  action: forgejo.issue.comment
  connection: forge_main
  params:
  timeout: 30s
`},
		{"an inline body", `tool comment:
  action: forgejo.issue.comment
  connection: forge_main
  params: { owner: "acme" }
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := parser.Parse("test.bot", tc.src)
			if len(res.Diagnostics) == 0 {
				t.Fatal("a body that is not an indented block must be diagnosed")
			}
			// ONE diagnostic, at the mistake. The offending token is consumed
			// by `expect`, so what follows must go with it — otherwise the
			// property loop reads a `:` or an `owner` as a property name and
			// the author is told about text they never wrote.
			if len(res.Diagnostics) != 1 {
				t.Errorf("want one diagnostic at the mistake, got %d: %s", len(res.Diagnostics), diagText(res))
			}
		})
	}
}

// An EMPTY `params:` stays legal in the two shapes the language already uses
// to write one — the studio saves a declaration the moment it is created.
func TestParseActionParamsBlockAcceptsAnEmptyBody(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"last property of the node", `tool comment:
  action: forgejo.issue.comment
  connection: forge_main
  params:
`},
		{"separated by a blank line", `tool comment:
  action: forgejo.issue.comment
  connection: forge_main
  params:

workflow w:
  entry: comment
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := parser.Parse("test.bot", tc.src)
			assertNoDiags(t, res)
			if len(res.File.Tools) != 1 {
				t.Fatalf("expected 1 tool, got %d", len(res.File.Tools))
			}
			if n := len(res.File.Tools[0].Params); n != 0 {
				t.Errorf("Params = %d, want an empty block", n)
			}
		})
	}
}

// An alias is chosen by the OPERATOR at `iterion connections add --alias`,
// where a dash is an ordinary thing to write. Read as a bare identifier only,
// `forge-main` — a name that command stores without a word — could not be
// named from any workflow, and the quoted form is what lets the unparser hand
// back a programmatically-built AST without corrupting it.
// TestParseActionParamAcceptsAQuotedWireKey.
//
// A parameter's key is the VENDOR's wire name (spec.Param.Key, carried
// through unchanged by the generator and looked up by it in exec), and a
// vendor names what it likes. The shipped Forgejo package has 22 keys that
// are not Go identifiers — `activity-id` and `user-id` are REQUIRED path
// parameters of forgejo.activitypub.*, so with no written form for them those
// operations could not be called from any workflow at all, and
// notification/repository silently lost their optional arguments.
func TestParseActionParamAcceptsAQuotedWireKey(t *testing.T) {
	src := `tool a:
  action: forgejo.activitypub.person_activity
  connection: forge_main
  params:
    "user-id": 1
    "activity-id": 2
    plain: 3
`
	res := parser.Parse("test.bot", src)
	assertNoDiags(t, res)
	if len(res.File.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(res.File.Tools))
	}
	want := []struct{ key, value string }{
		{"user-id", "1"},
		{"activity-id", "2"},
		{"plain", "3"},
	}
	got := res.File.Tools[0].Params
	if len(got) != len(want) {
		t.Fatalf("Params = %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Key != w.key || got[i].Value != w.value {
			t.Errorf("param %d = %q: %q, want %q: %q", i, got[i].Key, got[i].Value, w.key, w.value)
		}
	}

	// A key that is neither an identifier nor a quoted string is still
	// refused, naming the position: the escape hatch is the quoted form, not
	// "anything goes here now".
	bad := parser.Parse("test.bot", "tool a:\n  action: p.r.v\n  connection: c\n  params:\n    42: x\n")
	if !strings.Contains(diagText(bad), "expected a parameter name") {
		t.Errorf("a non-name key must still be diagnosed, got %s", diagText(bad))
	}
}

// TestParseActionParamsBlocksAccumulate.
//
// A second `params:` block used to REPLACE the first, with no diagnostic —
// so the arguments of the first block simply left the call. Appending keeps
// them and hands a genuine collision to C264, which already refuses a
// duplicate key written inside one block.
func TestParseActionParamsBlocksAccumulate(t *testing.T) {
	src := `tool a:
  action: p.r.v
  connection: c
  params:
    owner: "acme"
  params:
    repo: "widgets"
`
	res := parser.Parse("test.bot", src)
	assertNoDiags(t, res)
	got := res.File.Tools[0].Params
	if len(got) != 2 || got[0].Key != "owner" || got[1].Key != "repo" {
		t.Errorf("params = %+v, want both blocks kept in order", got)
	}
}

func TestParseActionConnectionAcceptsAQuotedAlias(t *testing.T) {
	for _, tc := range []struct{ written, want string }{
		{"forge_main", "forge_main"},
		{`"forge-main"`, "forge-main"},
		{`"main 2"`, "main 2"},
	} {
		res := parser.Parse("test.bot", "tool t:\n  action: p.r.v\n  connection: "+tc.written+"\n")
		assertNoDiags(t, res)
		if len(res.File.Tools) != 1 {
			t.Fatalf("%s: expected 1 tool", tc.written)
		}
		assertEq(t, "Connection", res.File.Tools[0].Connection, tc.want)
	}
}
