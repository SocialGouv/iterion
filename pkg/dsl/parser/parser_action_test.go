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
