package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A `"…"` DSL string is lexed in legacy escape mode unless the file opts into
// `## strict-escape: on`, and legacy mode keeps every \X verbatim. A
// backslash-escaped quote in sandbox.post_create therefore reaches the shell,
// which reads \" as a LITERAL quote character: the command runs with quotes
// inside its arguments instead of around them.
//
// post_create is best-effort by design, so nothing surfaces. Measured
// 2026-09-10 on a bootstrap installing a pinned CLI:
//
//	npm error code EINVALIDPACKAGENAME
//	Invalid package name """ of package ""@openai/codex@0.154.0""
//
// It had never run once, and the sandbox silently kept an older binary — the
// no-op-confirmed-as-success class. Hence an error rather than a warning: the
// string provably cannot do what it says, and a warning scrolls past in a
// validate run that already prints other warnings.
func compileSrc(t *testing.T, src string) *CompileResult {
	t.Helper()
	pr := parser.Parse("test.bot", src)
	if pr.File == nil {
		t.Fatalf("parse produced no File for:\n%s", src)
	}
	return Compile(pr.File)
}

func diagFor(cr *CompileResult, code DiagCode) *Diagnostic {
	for i := range cr.Diagnostics {
		if cr.Diagnostics[i].Code == code {
			return &cr.Diagnostics[i]
		}
	}
	return nil
}

const escapeBotShell = `
tool probe:
  command: ` + "`echo hi`" + `
  output: out

schema out:
  ok: string

workflow w:
  entry: probe

  sandbox:
    image: "example/image:tag"
    post_create: %s
`

func TestPostCreateRefusesAnEscapedQuote(t *testing.T) {
	// The exact shape that shipped and never ran.
	src := strings.Replace(escapeBotShell, "%s",
		`"npm install -g --prefix \"$HOME/.npm-global\" \"@openai/codex@0.154.0\""`, 1)

	cr := compileSrc(t, src)
	d := diagFor(cr, DiagEscapedQuoteInShellString)
	if d == nil {
		t.Fatalf("no %s diagnostic: an escaped quote in post_create compiles clean, so the next author writes it again. Got %+v",
			DiagEscapedQuoteInShellString, cr.Diagnostics)
	}
	if d.Severity != SeverityError {
		t.Errorf("severity = %v, want error — a warning scrolls past a validate run that already prints other warnings", d.Severity)
	}
	// The message has to name the fix, not just the sin: an author who reads
	// "escaped quote" without "drop them / use single quotes" reaches for a
	// third quoting layer, which is how this class propagates.
	for _, want := range []string{"literal quote", "single quotes", "strict-escape"} {
		if !strings.Contains(strings.ToLower(d.Message), want) {
			t.Errorf("message does not mention %q, so it names the defect without naming the way out: %s", want, d.Message)
		}
	}
}

func TestPostCreateAcceptsTheFormThatWorks(t *testing.T) {
	// Same command, corrected: nothing here has a space, and the one message
	// that does is in single quotes. Verified in a real sandbox on 2026-09-10 —
	// NPM_RC=0 and `codex-cli 0.154.0`, where the escaped form died.
	src := strings.Replace(escapeBotShell, "%s",
		`"pfx=$HOME/.npm-global; npm install -g --prefix $pfx @openai/codex@0.154.0 || echo 'install failed' >&2"`, 1)

	cr := compileSrc(t, src)
	if d := diagFor(cr, DiagEscapedQuoteInShellString); d != nil {
		t.Errorf("the working form is refused (%s) — a gate that reddens on the sane version discriminates nothing", d.Message)
	}
	if cr.HasErrors() {
		t.Errorf("the working form does not compile: %+v", cr.Diagnostics)
	}
	if cr.Workflow == nil || cr.Workflow.Sandbox == nil {
		t.Fatal("no sandbox spec compiled")
	}
	if !strings.Contains(cr.Workflow.Sandbox.PostCreate, "@openai/codex@0.154.0") {
		t.Errorf("post_create did not survive compilation: %q", cr.Workflow.Sandbox.PostCreate)
	}
}

func TestPostCreateWithoutQuotesIsUntouched(t *testing.T) {
	// The other sandbox post_create in the catalogue uses none, and must stay
	// compiling: the guard is about ESCAPED quotes, not about quoting at all.
	src := strings.Replace(escapeBotShell, "%s",
		`"set -e; sudo corepack enable 2>/dev/null || true; mkdir -p /tmp/x 2>/dev/null || true"`, 1)

	cr := compileSrc(t, src)
	if d := diagFor(cr, DiagEscapedQuoteInShellString); d != nil {
		t.Errorf("a post_create with no escaped quote was refused: %s", d.Message)
	}
}
