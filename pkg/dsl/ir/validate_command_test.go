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

// An env-ref backend resolves only at run time; the literal text isn't the
// resolved backend, so the validator must defer and not warn.
func TestCommand_EnvRefBackendSkips(t *testing.T) {
	r := compileFile(t, commandSrc("${BACKEND:-claw}", "claude-canary"))
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
