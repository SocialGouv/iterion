package parser

import "testing"

// The `defer:` property parses as written, in every position of the block,
// beside every other property. The parser's job is to read the word the
// author wrote — like `mode:`'s, the VALUE is the compiler's to narrow
// (C354), so an unknown word parses here and errors at compile.
func TestTheDeferPropertyParsesAsWritten(t *testing.T) {
	t.Parallel()
	const src = `agent a:
  model: "m"
  backend: "claw"
  codemode:
    defer: on
    tool_calls: 128

judge j:
  model: "m"
  backend: "claw"
  codemode:
    mode: off
    defer: off

workflow w:
  entry: a
  a -> j
  j -> done
`
	res, err := Parse("defer.bot", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Diagnostics) > 0 {
		t.Fatalf("the defer property drew diagnostics: %v", res.Diagnostics)
	}
	if got := res.File.Agents[0].Codemode; got == nil || got.Defer != "on" || got.ToolCalls != 128 {
		t.Errorf("agent codemode = %+v, want defer on beside its bounds", got)
	}
	if got := res.File.Judges[0].Codemode; got == nil || got.Defer != "off" || got.Mode != "off" {
		t.Errorf("judge codemode = %+v, want mode off and defer off as written", got)
	}
	if got := res.File.Agents[0].Codemode; got != nil && got.Mode != "on" {
		t.Errorf("agent codemode mode = %q, want \"on\": the block form implies the capability its defer bounds", got.Mode)
	}
}
