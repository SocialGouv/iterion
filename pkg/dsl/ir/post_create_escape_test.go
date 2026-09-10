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
	// third quoting layer, which is how this class propagates. And every
	// remedy it names must WORK on the value in front of the author —
	// `## strict-escape: on` is inert for a raw string or a block scalar
	// (neither ever processes escapes), so the message qualifies it as the
	// `"…"`-string remedy and leads with the one that always applies.
	for _, want := range []string{"literal quote", "backtick raw string", "single-quote", "strict-escape"} {
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

const blockScalarBotShell = `
tool probe:
  command: ` + "`echo hi`" + `
  output: out

schema out:
  ok: string

workflow w:
  entry: probe

  sandbox:
    image: "example/image:tag"
    post_create: |
      %s
`

// The guard fires on what the SHELL misreads, not on two characters in a Go
// string — because `\"` is only a defect where the shell is not already inside
// a `"…"` region, and because a value can reach the compiler from three lexer
// paths, of which only one processes escapes at all.
//
// `expectString` accepts a TokenString from `scanString` (the `"…"` form, the
// only one that consults `strictEscape`), `scanRawString` (backticks) and
// `scanBlockScalar` (`|`). The last two are verbatim in EITHER escape mode, so
// a `\"` arriving from one of them proves nothing about the mode — and
// `## strict-escape: on`, which the first draft of this diagnostic advertised
// as the way out, provably does nothing for them.
func TestPostCreateEscapeCheckReadsShellQuoting(t *testing.T) {
	rawJSON := "`" + `printf '%s' "{\"a\":1}" > /tmp/x.json` + "`"
	rawUnquoted := "`" + `npm install -g --prefix \"$HOME/.npm-global\" pkg` + "`"

	cases := []struct {
		name string
		// value is the post_create as WRITTEN in the .bot, delimiters included.
		value string
		fires bool
		why   string
	}{
		{
			name:  "raw string, escaped quote inside a shell double-quoted region",
			value: rawJSON,
			fires: false,
			why:   "correct working shell: inside \"…\" the shell decodes \\\" to a nested quote, and a raw string hands it over verbatim by design",
		},
		{
			name:  "raw string, escaped quote in unquoted context",
			value: rawUnquoted,
			fires: true,
			why:   "the raw string changes nothing here — the shell still receives \\\" unquoted and glues a literal quote into the word",
		},
		{
			name:  "escaped quote inside a shell single-quoted region",
			value: `"echo 'a \" b'"`,
			fires: false,
			why:   "inside '…' the shell takes both bytes literally; the author wrote what they get",
		},
		{
			name:  "escaped backslash then an opening quote",
			value: "`" + `printf %s \\"$dir"` + "`",
			fires: false,
			why:   "\\\\ is one escaped backslash and the quote right after it OPENS a region — scanning byte pairs without consuming the escape reads it as \\\" and refuses working shell",
		},
		{
			name:  "escaped backslash then an escaped quote",
			value: "`" + `printf %s \\\"$dir\\\"` + "`",
			fires: true,
			why:   "consuming \\\\ leaves \\\" still unquoted, and the shell does hand printf a literal quote — the mirror image of the case above",
		},
		{
			name:  "command substitution anywhere",
			value: `"tag=$(date +%s); echo \"$tag\""`,
			fires: false,
			why:   "the scanner does not model $( ) quoting and must fail open — a missed diagnostic beats a refused workflow",
		},
		{
			name:  "ANSI-C quoting with an escaped quote of its own",
			value: "`" + `printf %s $'it\'s \"fine\"'` + "`",
			fires: false,
			why:   "$'…' ends at an UNESCAPED quote, so \\' does not close it; reading it as a plain '…' closes the region early and inverts every quote after it",
		},
		{
			name:  "text after an ANSI-C region stays correctly tracked",
			value: "`" + `printf %s $'a\'b'; npm i --prefix \"$p\"` + "`",
			fires: true,
			why:   "the defect after the $'…' is only reachable if the region was closed at the right quote",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := compileSrc(t, strings.Replace(escapeBotShell, "%s", tc.value, 1))
			d := diagFor(cr, DiagEscapedQuoteInShellString)
			if tc.fires && d == nil {
				t.Fatalf("no %s diagnostic, but %s. Got %+v", DiagEscapedQuoteInShellString, tc.why, cr.Diagnostics)
			}
			if !tc.fires {
				if d != nil {
					t.Fatalf("%s refused a legitimate post_create: %s\nit should not, because %s", DiagEscapedQuoteInShellString, d.Message, tc.why)
				}
				if cr.HasErrors() {
					t.Fatalf("the value does not compile: %+v", cr.Diagnostics)
				}
			}
		})
	}
}

// A block scalar is the third lexer path, and the shape an author reaches for
// once a bootstrap outgrows one line. It never processes escapes either, so a
// `\"` it carries inside a shell `"…"` region is correct shell that the first
// draft of this check refused outright.
func TestPostCreateBlockScalarWithNestedQuoteCompiles(t *testing.T) {
	src := strings.Replace(blockScalarBotShell, "%s",
		`printf '%s' "{\"pinned\":true}" > /tmp/cfg.json`, 1)

	cr := compileSrc(t, src)
	if d := diagFor(cr, DiagEscapedQuoteInShellString); d != nil {
		t.Fatalf("a block-scalar post_create writing nested JSON was refused: %s", d.Message)
	}
	if cr.HasErrors() {
		t.Fatalf("the block-scalar form does not compile: %+v", cr.Diagnostics)
	}
	if cr.Workflow == nil || cr.Workflow.Sandbox == nil {
		t.Fatal("no sandbox spec compiled")
	}
	if !strings.Contains(cr.Workflow.Sandbox.PostCreate, `{\"pinned\":true}`) {
		t.Errorf("the block scalar did not survive verbatim: %q", cr.Workflow.Sandbox.PostCreate)
	}
}

// The diagnostic reports a suspect string; it does not invalidate the rest of
// the block. Dropping the whole spec would downgrade a workflow that pinned
// `sandbox: image:` (or `sandbox: none`) to whatever ITERION_SANDBOX_DEFAULT
// says on any Compile surface that renders a result carrying errors — the
// cost preview and the studio's DSL endpoint both do.
func TestPostCreateDiagnosticKeepsTheSandboxSpec(t *testing.T) {
	src := strings.Replace(escapeBotShell, "%s",
		`"npm install -g --prefix \"$HOME/.npm-global\" pkg"`, 1)

	cr := compileSrc(t, src)
	if diagFor(cr, DiagEscapedQuoteInShellString) == nil {
		t.Fatalf("expected %s to fire: %+v", DiagEscapedQuoteInShellString, cr.Diagnostics)
	}
	if cr.Workflow == nil || cr.Workflow.Sandbox == nil {
		t.Fatal("the sandbox spec was dropped, so a pinned image reads as absent to any surface that renders a result with errors")
	}
	if cr.Workflow.Sandbox.Image != "example/image:tag" {
		t.Errorf("image = %q, want the pinned one", cr.Workflow.Sandbox.Image)
	}
}
