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
// no-op-confirmed-as-success class.
//
// A WARNING, not an error. The tempting argument — "under strict escape the
// lexer would have decoded \", so seeing it here proves no unescaping
// happened" — is false: expectString accepts a TokenString from three
// scanners and only scanString consults strictEscape. A backtick raw string
// and a `|` block scalar keep \" verbatim BY DESIGN, where it can be a
// perfectly correct shell escape inside a double-quoted region. Refusing
// would break a working bundle at launch, including ones stored outside this
// tree (Revi R1d1a9f).
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
	if d.Severity != SeverityWarning {
		t.Errorf("severity = %v, want warning — the same shape is legitimate in a backtick or block-scalar value, "+
			"so the compiler cannot prove a defect here and must not refuse the workflow", d.Severity)
	}
	if cr.HasErrors() {
		t.Errorf("the workflow was REFUSED: %+v — a warning must leave it compiling", cr.Diagnostics)
	}
	if cr.Workflow == nil || cr.Workflow.Sandbox == nil {
		t.Fatal("the SandboxSpec was dropped — a warning must not cost the block, or a consumer reads the workflow as having no sandbox at all")
	}
	// The message has to name the fix, not just the sin: an author who reads
	// "escaped quote" without "drop them / use single quotes" reaches for a
	// third quoting layer, which is how this class propagates.
	for _, want := range []string{"literal quote", "single quotes", "backtick"} {
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

// TestPostCreateBacktickValueKeepsCompiling is the false positive Revi named
// (R1d1a9f): a backtick raw string never goes through escape processing in
// EITHER mode, so a \" inside it is verbatim by design — and inside a shell
// double-quoted region it is the correct way to write a literal quote.
// bots/wiki-gen already writes JSON that way. The warning may still fire (the
// compiler cannot tell the string kinds apart at this point), but the
// workflow MUST still compile and keep its sandbox.
func TestPostCreateBacktickValueKeepsCompiling(t *testing.T) {
	src := strings.Replace(escapeBotShell, "%s",
		"`printf '{\"k\":1}' > /tmp/c.json && echo \"done\"`", 1)

	cr := compileSrc(t, src)
	if cr.HasErrors() {
		t.Errorf("a backtick post_create carrying \\\" was refused: %+v — that shape is correct shell today, "+
			"and refusing it breaks a working bundle at launch", cr.Diagnostics)
	}
	if cr.Workflow == nil || cr.Workflow.Sandbox == nil {
		t.Fatal("the SandboxSpec was dropped on a legitimate value")
	}
	if !strings.Contains(cr.Workflow.Sandbox.PostCreate, "/tmp/c.json") {
		t.Errorf("post_create did not survive: %q", cr.Workflow.Sandbox.PostCreate)
	}
}
