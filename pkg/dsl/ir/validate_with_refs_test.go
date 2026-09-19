package ir

import (
	"strings"
	"testing"
)

// C149: `{{input.x}}` in a subbot/emit `with:` warns. The kind has
// no `input:` surface, so the reference resolves against the parent's
// run inputs at run time — a typo lands nil silently, and on a
// subbot that nil suppresses the child's declared default. A warning
// rather than an error because the parent's launch payload may
// legitimately carry an undeclared key (the CLI `--var k=v` and the
// cloud launch path forward every key wholesale) and that key is
// reachable ONLY through `{{input.x}}`. The test mutates toward the
// forbidden alternative (a namespace that could travel: `{{vars.x}}`),
// and requires the diagnostic to disappear — so the check fires
// because it saw an `input.*` ref in a with:, not because ANY
// namespace does.
func TestC149InputRefInSubbotWithIsRefused(t *testing.T) {
	head := `dsl: 2

schema kout:
  ok: bool

subbot child:
  source: "kid.bot"
  with { b: %s }

tool kick:
  command: ` + "`echo hi`" + `
  output: kout

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> child
  child -> done
`
	// Forbidden shape: `{{input.b}}` on a subbot with:.
	got := compileText(t, appendf(head, `"{{input.b}}"`))
	if !anyDiag(got, "C149") {
		t.Fatalf("C149 did not fire for `{{input.b}}` on a subbot with-mapping:\n%v", got.Diagnostics)
	}
	// C149 is a WARNING, not an error — the parent may legitimately be
	// forwarding an undeclared payload key (see the docstring for
	// validateNodeInputRef). Assert the severity so a silent change
	// back to `refErrorf` is caught by the test.
	if !hasDiagAtSeverity(got, "C149", SeverityWarning) {
		t.Fatalf("C149 did not fire as a WARNING on a subbot with-mapping — an error would refuse a legitimate forwarding channel:\n%v", got.Diagnostics)
	}
	// Mutation toward the forbidden alternative: use `{{vars.b}}` (a
	// launch-time namespace that DOES travel). C149 must disappear.
	// Vars must be declared.
	mutated := strings.Replace(head, "workflow w:", "vars:\n  b: string = \"hi\"\n\nworkflow w:", 1)
	ok := compileText(t, appendf(mutated, `"{{vars.b}}"`))
	if anyDiag(ok, "C149") {
		t.Fatalf("C149 fires on `{{vars.b}}` (a namespace that travels) — the check is not specific to input refs:\n%v", ok.Diagnostics)
	}
}

// C149 also fires on emit `with:` — same rule (emit accepts no
// `input:` either). Two separate sites of one class: rule enforced at
// each. Mutation: swap to `{{vars.x}}` and require silence.
func TestC149InputRefInEmitWithIsRefused(t *testing.T) {
	head := `dsl: 2

schema kout:
  ok: bool

wait ev:
  event: "ping"
  timeout: "1m"

emit ev_out:
  event: "ping"
  with { p: %s }

tool kick:
  command: ` + "`echo hi`" + `
  output: kout

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> ev_out
  ev_out -> ev
  ev -> done
`
	got := compileText(t, appendf(head, `"{{input.p}}"`))
	if !anyDiag(got, "C149") {
		t.Fatalf("C149 did not fire on emit with-mapping:\n%v", got.Diagnostics)
	}
	// C149 is a WARNING — lock the severity like on the subbot arm.
	if !hasDiagAtSeverity(got, "C149", SeverityWarning) {
		t.Fatalf("C149 did not fire as a WARNING on emit with-mapping:\n%v", got.Diagnostics)
	}
	mutated := strings.Replace(head, "workflow w:", "vars:\n  p: string = \"hi\"\n\nworkflow w:", 1)
	ok := compileText(t, appendf(mutated, `"{{vars.p}}"`))
	if anyDiag(ok, "C149") {
		t.Fatalf("C149 fires on `{{vars.p}}` — the rule is not specific to input refs:\n%v", ok.Diagnostics)
	}
}

// C149 stays silent on an edge with-mapping (where {{input.x}} DOES
// mean the source node's output and is validated by C032/C034). The
// forbidden alternative here is "refuse on every `with:`" — which
// would fire on the edge below.
func TestC149IsSilentOnEdgeWithMapping(t *testing.T) {
	src := `dsl: 2

schema kout:
  ok: bool
  b: string

schema pin:
  b: string

compute pass:
  input: pin
  output: pin
  expr:
    b: "input.b"

tool kick:
  command: ` + "`echo hi`" + `
  output: kout

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> pass with { b: "{{input.b}}" }
  pass -> done
`
	got := compileText(t, src)
	if anyDiag(got, "C149") {
		t.Fatalf("C149 fires on an edge with-mapping — rule is over-broad:\n%v", got.Diagnostics)
	}
}

// C150: `{{secrets.<name>}}` in a `with:` value (edge, subbot, emit)
// is refused. Mutation toward the forbidden alternative: reference
// the same declared secret from a tool's `command:` (an execution
// sink that materialises secrets) — C150 must disappear, proving the
// check fires because of the mapping site, not because the secret is
// unusable in general.
func TestC150SecretRefInWithIsRefused(t *testing.T) {
	src := `dsl: 2

secrets:
  api_token:
    as: value

schema kout:
  ok: bool

tool kick:
  command: ` + "`echo hi`" + `
  output: kout

subbot child:
  source: "kid.bot"
  with { t: "{{secrets.api_token}}" }

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> child with { s: "{{secrets.api_token}}" }
  child -> done
`
	got := compileText(t, src)
	// Both the edge with-mapping and the subbot's own with-mapping fire.
	if countByCode(got, "C150") < 2 {
		t.Fatalf("C150 did not fire on both the edge and the subbot with-mapping:\n%v", got.Diagnostics)
	}
	// Sink that WORKS: a tool's `command:`. Mutation: keep the same
	// declaration, keep the same reference, MOVE the reference into
	// the tool's command. C150 must disappear.
	sinkOK := `dsl: 2

secrets:
  api_token:
    as: value

schema kout:
  ok: bool

tool kick:
  command: ` + "`echo {{secrets.api_token}}`" + `
  output: kout

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> done
`
	mutated := compileText(t, sinkOK)
	if anyDiag(mutated, "C150") {
		t.Fatalf("C150 fires on a tool command sink — the rule is not specific to `with:`:\n%v", mutated.Diagnostics)
	}
}

// C150 also fires when `{{secrets.<name>}}` sits in a `fail message:` —
// a fail node's operator-facing reason is resolved through
// `resolveMapping` too, same as a with-mapping. The runtime renders
// the reference to nil, and the operator loses the diagnostic they
// would have seen with a real secret at a real sink. Mutation toward
// the forbidden alternative: move the reference to a tool's
// `command:` (a real sink) and require silence.
func TestC150SecretRefInFailMessageIsRefused(t *testing.T) {
	src := `dsl: 2

secrets:
  api_tok:
    as: value

schema kout:
  ok: bool

tool kick:
  command: ` + "`echo hi`" + `
  output: kout

fail refuse:
  code: SECRET_REFUSED
  message: "token was {{secrets.api_tok}}"

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> refuse
`
	got := compileText(t, src)
	if !anyDiag(got, "C150") {
		t.Fatalf("C150 did not fire on `{{secrets.api_tok}}` in a fail message:\n%v", got.Diagnostics)
	}
	// Same declaration, same reference, moved to a tool command (a
	// real sink). C150 must disappear.
	sinkOK := `dsl: 2

secrets:
  api_tok:
    as: value

schema kout:
  ok: bool

tool kick:
  command: ` + "`echo {{secrets.api_tok}}`" + `
  output: kout

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> done
`
	mutated := compileText(t, sinkOK)
	if anyDiag(mutated, "C150") {
		t.Fatalf("C150 fires on a tool command sink — the rule is not specific to data mappings:\n%v", mutated.Diagnostics)
	}
}

// C151: `{{attachments.<name>}}` in a `with:` value is refused —
// same rule as C150, one file per site. Mutation toward the
// forbidden alternative: move the reference to a tool's `command:`
// (a real execution sink the runtime resolves) — C151 must disappear.
// (A compute expression is NOT a sink for attachments either;
// `TestC151AttachmentRefInComputeExprIsRefused` guards that arm.)
func TestC151AttachmentRefInWithIsRefused(t *testing.T) {
	src := `dsl: 2

attachments:
  spec: file

schema kout:
  ok: bool
  p: string

schema pin:
  p: string

compute pass:
  input: pin
  output: pin
  expr:
    p: "input.p"

tool kick:
  command: ` + "`echo hi`" + `
  output: kout

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> pass with { p: "{{attachments.spec}}" }
  pass -> done
`
	got := compileText(t, src)
	if !anyDiag(got, "C151") {
		t.Fatalf("C151 did not fire on `{{attachments.spec}}` in a with-mapping:\n%v", got.Diagnostics)
	}
	// Mutation: move the reference to a tool `command:` — a real
	// execution sink; C151 must disappear.
	sinkOK := `dsl: 2

attachments:
  spec: file

schema kout:
  ok: bool

tool kick:
  command: ` + "`echo {{attachments.spec}}`" + `
  output: kout

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> done
`
	mutated := compileText(t, sinkOK)
	if anyDiag(mutated, "C151") {
		t.Fatalf("C151 fires on a tool command sink — the rule is over-broad:\n%v", mutated.Diagnostics)
	}
}

// C150/C151 also fire when the reference sits inside a compute node's
// `expr:` — `pkg/dsl/expr` has no arm for `secrets` / `attachments`,
// so the reference renders to nil at evaluation. Same silent-nil class
// as the mapping site. Mutation: move the reference to a tool
// `command:` (a real sink) and require silence.
func TestC150SecretRefInComputeExprIsRefused(t *testing.T) {
	src := `dsl: 2

secrets:
  api_tok:
    as: value

schema kout:
  ok: bool

schema pin:
  s: string

compute pass:
  input: pin
  output: pin
  expr:
    s: "secrets.api_tok"

tool kick:
  command: ` + "`echo hi`" + `
  output: kout

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> pass with { s: "hi" }
  pass -> done
`
	got := compileText(t, src)
	if !anyDiag(got, "C150") {
		t.Fatalf("C150 did not fire on `secrets.api_tok` in a compute expr:\n%v", got.Diagnostics)
	}
	sinkOK := `dsl: 2

secrets:
  api_tok:
    as: value

schema kout:
  ok: bool

tool kick:
  command: ` + "`echo {{secrets.api_tok}}`" + `
  output: kout

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> done
`
	mutated := compileText(t, sinkOK)
	if anyDiag(mutated, "C150") {
		t.Fatalf("C150 fires on a tool command sink — the rule is over-broad:\n%v", mutated.Diagnostics)
	}
}

// Symmetric coverage for attachments in a compute `expr:`.
func TestC151AttachmentRefInComputeExprIsRefused(t *testing.T) {
	src := `dsl: 2

attachments:
  spec: file

schema kout:
  ok: bool

schema pin:
  p: string

compute pass:
  input: pin
  output: pin
  expr:
    p: "attachments.spec"

tool kick:
  command: ` + "`echo hi`" + `
  output: kout

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> pass with { p: "hi" }
  pass -> done
`
	got := compileText(t, src)
	if !anyDiag(got, "C151") {
		t.Fatalf("C151 did not fire on `attachments.spec` in a compute expr:\n%v", got.Diagnostics)
	}
	sinkOK := `dsl: 2

attachments:
  spec: file

schema kout:
  ok: bool

tool kick:
  command: ` + "`echo {{attachments.spec}}`" + `
  output: kout

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> done
`
	mutated := compileText(t, sinkOK)
	if anyDiag(mutated, "C151") {
		t.Fatalf("C151 fires on a tool command sink — the rule is over-broad:\n%v", mutated.Diagnostics)
	}
}

// C152: a ref-less literal in a `with:` mapping value whose text
// cannot be the target field's type — the runtime returns dm.Raw as
// a string, so `bool`/`int`/`float`/`json`-shaped fields carry the
// author's intent as a string, not the decoded value. Warning at
// every consumer (a runtime that tolerates via `truthy()` or an
// LLM's prompt is not a runtime that rejects). Mutation toward the
// forbidden alternative: replace the literal by a reference to a
// compute that emits the typed constant — C152 must disappear.
func TestC152WithLiteralTypeMismatch(t *testing.T) {
	// The type-mismatch case: literal `"false"` reaches a bool field.
	src := `dsl: 2

schema kout:
  ok: bool
  flag: bool
  count: int

schema pin:
  ok: bool
  flag: bool
  count: int

compute pass:
  input: pin
  output: pin
  expr:
    ok: "input.ok"
    flag: "input.flag"
    count: "input.count"

tool kick:
  command: ` + "`echo hi`" + `
  output: kout

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> pass with {
    ok: "true"
    flag: "false"
    count: "42"
  }
  pass -> done
`
	got := compileText(t, src)
	if countByCode(got, "C152") < 3 {
		t.Fatalf("C152 did not fire on all three ref-less literals to typed fields:\n%v", got.Diagnostics)
	}
	// Mutation toward the forbidden alternative: use a producer whose
	// output carries the typed constants. C152 must disappear.
	sinkOK := `dsl: 2

schema kout:
  ok: bool
  flag: bool
  count: int

schema pin:
  ok: bool
  flag: bool
  count: int

compute typed_constants:
  output: pin
  expr:
    ok: "true"
    flag: "false"
    count: "42"

compute pass:
  input: pin
  output: pin
  expr:
    ok: "input.ok"
    flag: "input.flag"
    count: "input.count"

tool kick:
  command: ` + "`echo hi`" + `
  output: kout

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> typed_constants
  typed_constants -> pass with {
    ok: "{{outputs.typed_constants.ok}}"
    flag: "{{outputs.typed_constants.flag}}"
    count: "{{outputs.typed_constants.count}}"
  }
  pass -> done
`
	mutated := compileText(t, sinkOK)
	if anyDiag(mutated, "C152") {
		t.Fatalf("C152 fires on a typed-constant reference — the rule is not specific to literals:\n%v", mutated.Diagnostics)
	}
}

// C152 stays silent on a `string` target field: any string literal
// is a legal value for `string`. The forbidden alternative here is
// "warn on every ref-less literal" — which would fire on this
// fixture.
func TestC152SilentOnStringLiteral(t *testing.T) {
	src := `dsl: 2

schema kout:
  ok: bool
  greeting: string

schema pin:
  greeting: string

compute pass:
  input: pin
  output: pin
  expr:
    greeting: "input.greeting"

tool kick:
  command: ` + "`echo hi`" + `
  output: kout

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> pass with { greeting: "hello world" }
  pass -> done
`
	got := compileText(t, src)
	if anyDiag(got, "C152") {
		t.Fatalf("C152 fires on a legitimate string literal — the rule is over-broad:\n%v", got.Diagnostics)
	}
}

// C152 warns on a ref-less literal reaching a `json` field when the
// text visibly ATTEMPTS an encoding — the copilot bot's `host_event`
// mapping holding two apostrophes (an attempted and wrong spelling of
// the empty string). The runtime hands the destination the raw text,
// and no consumer decodes two apostrophes as an empty string. A numeric literal stays
// silent — a decision this test locks: it reads as the JSON string the
// author wrote, and although arithmetic on it would concatenate
// (`input.j + 1` on "42" yields "421"), the diagnostic claims the
// encodings that visibly carry structure, not every scalar the
// destination might have wanted typed.
func TestC152AttemptedEncodingOnJSONWarns(t *testing.T) {
	src := `dsl: 2

schema kout:
  ok: bool
  host_event: json

schema pin:
  host_event: json

compute pass:
  input: pin
  output: pin
  expr:
    host_event: "input.host_event"

tool kick:
  command: ` + "`echo hi`" + `
  output: kout

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> pass with { host_event: "''" }
  pass -> done
`
	got := compileText(t, src)
	if !hasDiagAtSeverity(got, "C152", SeverityWarning) {
		t.Fatalf("C152 did not fire on `host_event: \"''\"` (a ref-less literal reaching a `json` field, not valid JSON):\n%v", got.Diagnostics)
	}
	// Mutation toward the forbidden alternative: bare primitives that
	// ARE valid JSON stay silent. `42` for a json field would parse as
	// the JSON number and round-trip through a permissive consumer.
	silent := `dsl: 2

schema kout:
  ok: bool
  host_event: json

schema pin:
  host_event: json

compute pass:
  input: pin
  output: pin
  expr:
    host_event: "input.host_event"

tool kick:
  command: ` + "`echo hi`" + `
  output: kout

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> pass with { host_event: "42" }
  pass -> done
`
	got2 := compileText(t, silent)
	for _, d := range got2.Diagnostics {
		if d.Code == DiagWithLiteralTypeMismatch {
			t.Fatalf("C152 fires on `host_event: \"42\"` (a bare valid-JSON primitive) — the arm is over-broad:\n%s\n%v", d.Error(), got2.Diagnostics)
		}
	}
}

// C152 warns on a JSON-shaped literal reaching a `json` field — the
// author's intent (an encoded array/object) diverges from the
// runtime's rendering (a two-character string). Warning, not error:
// a permissive downstream consumer may tolerate the text today.
func TestC152JSONShapedLiteralWarns(t *testing.T) {
	src := `dsl: 2

schema kout:
  ok: bool
  items: json

schema pin:
  items: json

compute pass:
  input: pin
  output: pin
  expr:
    items: "input.items"

tool kick:
  command: ` + "`echo hi`" + `
  output: kout

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> pass with { items: "[]" }
  pass -> done
`
	got := compileText(t, src)
	// A warning, not an error.
	found := false
	for _, d := range got.Diagnostics {
		if d.Code == DiagWithLiteralTypeMismatch && d.Severity == SeverityWarning {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("C152 did not fire as a warning on a JSON-shaped literal reaching a json field:\n%v", got.Diagnostics)
	}
}

// anyDiag reports whether a compile result carries a diagnostic with
// the given code, whatever the severity.
func anyDiag(r *CompileResult, code string) bool {
	if r == nil {
		return false
	}
	for _, d := range r.Diagnostics {
		if string(d.Code) == code {
			return true
		}
	}
	return false
}

// hasDiagAtSeverity reports whether a compile result carries a
// diagnostic with the given code AT the given severity. Locking the
// severity is the difference between "this check fires" and "this
// check fires as a warning, not an error" — the C149 tests use it to
// catch a silent switch from `refWarnf` back to `refErrorf`, which
// would refuse a legitimate forwarding channel.
func hasDiagAtSeverity(r *CompileResult, code string, sev Severity) bool {
	if r == nil {
		return false
	}
	for _, d := range r.Diagnostics {
		if string(d.Code) == code && d.Severity == sev {
			return true
		}
	}
	return false
}

// countByCode returns the number of diagnostics with the given code
// (string form, so a caller can test a code the compiler emits
// without importing the constant name).
func countByCode(r *CompileResult, code string) int {
	if r == nil {
		return 0
	}
	n := 0
	for _, d := range r.Diagnostics {
		if string(d.Code) == code {
			n++
		}
	}
	return n
}

// appendf sprintfs the given src pattern (`%s` in one place) with the
// value string, so the fixture can vary the `with:` value while the
// surrounding shape stays fixed.
func appendf(src, value string) string {
	return strings.Replace(src, "%s", value, 1)
}
