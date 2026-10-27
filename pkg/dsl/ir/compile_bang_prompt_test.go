package ir

import "testing"

// A bang ref inside a prompt body is parsed and then ignored: a prompt has
// no shell to escape, so the value renders whatever the prefix says. The
// compiler names the spelling (C169) per occurrence instead of leaving it
// silent, and a plain ref in a neighbouring prompt draws nothing.
func TestCompileWarnsOnBangRefInPromptBody(t *testing.T) {
	src := `
schema s:
  a: string
  ok: bool

prompt sys:
  value {{!input.a}}

prompt usr:
  plain {{input.a}}

agent a:
  input: s
  output: s
  backend: "claude_code"
  system: sys
  user: usr

workflow test:
  entry: a
  a -> done
`
	r := Compile(parseFile(t, src))
	warned := 0
	for _, d := range r.Diagnostics {
		if d.Code == DiagBangRefInPromptBody {
			if d.Severity != SeverityWarning {
				t.Fatalf("C169 fired as %v, want a warning", d.Severity)
			}
			warned++
		}
	}
	if warned != 1 {
		t.Fatalf("expected exactly one C169 warning for the bang prompt, got %d", warned)
	}
}
