package ir

import "testing"

// commandSrc builds a minimal one-agent workflow with the given backend
// and command field so the command validator can be exercised in
// isolation.
func commandSrc(backend, command string) string {
	return `
schema empty:
  ok: bool

prompt sys:
  body
  hello

agent writer:
  model: "gpt-4"
  backend: "` + backend + `"
  command: "` + command + `"
  system: sys
  output: empty

workflow w:
  entry: writer
  writer -> done
`
}

// claw makes a direct API call (no CLI), so a `command:` there is inert.
func TestCommand_IgnoredOnClawWarns(t *testing.T) {
	r := compileFile(t, commandSrc("claw", "claude-canary"))
	expectDiag(t, r, DiagCommandIgnored)
}

// codex resolves its own binary, so a `command:` there is inert too.
func TestCommand_IgnoredOnCodexWarns(t *testing.T) {
	r := compileFile(t, commandSrc("codex", "claude-canary"))
	expectDiag(t, r, DiagCommandIgnored)
}

// claude_code honors the override, so no C174.
func TestCommand_OnClaudeCodeNoWarning(t *testing.T) {
	r := compileFile(t, commandSrc("claude_code", "claude-canary"))
	expectNoDiag(t, r, DiagCommandIgnored)
}

// A backend written as a dial is read by what it RESOLVES to: with no env
// set, `${BACKEND:-claw}` IS claw, and claw ignores `command:` exactly as
// the literal spelling does. Deferring on the spelling meant an author
// learned it at run time instead (#1389).
func TestCommand_EnvRefBackendIsReadByItsDefault(t *testing.T) {
	r := compileFile(t, commandSrc("${C174_UNSET:-claw}", "claude-canary"))
	expectDiag(t, r, DiagCommandIgnored)

	// claude_code honours the override, dialled or not.
	r = compileFile(t, commandSrc("${C174_UNSET:-claude_code}", "claude-canary"))
	expectNoDiag(t, r, DiagCommandIgnored)

	// A reference nothing answers has no compile-time value: the run
	// decides it, and the validator defers as it always did.
	r = compileFile(t, commandSrc("${C174_UNSET}", "claude-canary"))
	expectNoDiag(t, r, DiagCommandIgnored)
}

// commandSrcDefaultBackend builds a one-agent workflow whose node leaves
// `backend:` unset, so the effective backend is the workflow-level
// `default_backend:` — the compile-time-knowable fallback the validator
// must honor.
func commandSrcDefaultBackend(defaultBackend, command string) string {
	return `
schema empty:
  ok: bool

prompt sys:
  body
  hello

agent writer:
  model: "gpt-4"
  command: "` + command + `"
  system: sys
  output: empty

workflow w:
  default_backend: "` + defaultBackend + `"
  entry: writer
  writer -> done
`
}

// A node with no explicit backend inherits the workflow `default_backend:`;
// when that resolves to claw the command is inert, so C174 must fire even
// though the node itself names no backend.
func TestCommand_InheritsDefaultBackendClawWarns(t *testing.T) {
	r := compileFile(t, commandSrcDefaultBackend("claw", "claude-canary"))
	expectDiag(t, r, DiagCommandIgnored)
}

// Same inheritance, but default_backend is claude_code, which honors the
// override — no warning.
func TestCommand_InheritsDefaultBackendClaudeCodeNoWarning(t *testing.T) {
	r := compileFile(t, commandSrcDefaultBackend("claude_code", "claude-canary"))
	expectNoDiag(t, r, DiagCommandIgnored)
}

// `command:` also works on judge nodes and warns on a hint-ignoring backend.
func TestCommand_JudgeIgnoredOnClawWarns(t *testing.T) {
	src := `
schema empty:
  ok: bool

prompt sys:
  body
  hello

judge reviewer:
  model: "gpt-4"
  backend: "claw"
  command: "claude-canary"
  system: sys
  output: empty

workflow w:
  entry: reviewer
  reviewer -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagCommandIgnored)
}
