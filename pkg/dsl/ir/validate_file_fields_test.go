package ir

import (
	"strings"
	"testing"
)

// humanFileSrc is a minimal workflow whose human gate collects an
// operator-uploaded binary alongside an ordinary verdict field.
const humanFileSrc = `
schema gate_out:
  approved: bool
  music: file
  notes: string

human gate:
  output: gate_out

workflow main:
  entry: gate
  gate -> done
`

func TestFileFieldOnHumanNodeCompiles(t *testing.T) {
	w := mustCompile(t, humanFileSrc)

	schema := w.Schemas["gate_out"]
	if schema == nil {
		t.Fatal("schema gate_out missing from compiled workflow")
	}
	var got *SchemaField
	for _, f := range schema.Fields {
		if f.Name == "music" {
			got = f
		}
	}
	if got == nil {
		t.Fatalf("field 'music' missing; fields = %+v", schema.Fields)
	}
	if got.Type != FieldTypeFile {
		t.Errorf("music type = %v, want %v", got.Type, FieldTypeFile)
	}
	if got.Type.String() != "file" {
		t.Errorf("music type string = %q, want %q", got.Type.String(), "file")
	}
}

// A `file` field can only ever be produced by an operator upload at a
// human pause. On any other node it silently arrives empty, so the
// compiler must reject it rather than let the run discover it.
func TestFileFieldOnAgentNodeIsC129(t *testing.T) {
	src := `
schema agent_out:
  summary: string
  diagram: file

prompt sys:
  You are an agent.

prompt usr:
  Do something.

agent work:
  model: "anthropic/claude-sonnet-4-6"
  system: sys
  user: usr
  output: agent_out

workflow main:
  entry: work
  work -> done
`
	res := compileFile(t, src)

	var found bool
	for _, d := range res.Diagnostics {
		if d.Code == DiagFileFieldNotHuman {
			found = true
			if d.Severity != SeverityError {
				t.Errorf("C129 severity = %v, want error", d.Severity)
			}
			if !strings.Contains(d.Message, "diagram") {
				t.Errorf("C129 message should name the offending field, got: %s", d.Message)
			}
		}
	}
	if !found {
		t.Fatalf("expected C129 for a file field on an agent node; diagnostics = %v", res.Diagnostics)
	}
}

// A human node with `interaction: llm` is auto-answered by a model: no
// operator, no pause, no upload. Because the JSON schema handed to that
// model marks the descriptor's `path` required, it will invent one and
// the downstream node reads a file that does not exist — the same
// silent-nothing failure C129 exists to catch, just one node over.
func TestFileFieldOnLLMAnsweredHumanIsC129(t *testing.T) {
	src := `
schema gate_out:
  approved: bool
  music: file

human gate:
  interaction: llm
  model: "anthropic/claude-sonnet-4-6"
  output: gate_out

workflow main:
  entry: gate
  gate -> done
`
	res := compileFile(t, src)

	var found bool
	for _, d := range res.Diagnostics {
		if d.Code == DiagFileFieldNotHuman {
			found = true
			if d.Severity != SeverityError {
				t.Errorf("C129 severity = %v, want error", d.Severity)
			}
			if !strings.Contains(d.Message, "music") {
				t.Errorf("C129 message should name the offending field, got: %s", d.Message)
			}
		}
	}
	if !found {
		t.Fatalf("expected C129 for a file field on an interaction: llm human node; diagnostics = %v", res.Diagnostics)
	}
}

// `llm_or_human` keeps a real escalation path — the model can decline
// and hand the gate to an operator, which is exactly how the bytes
// arrive. Rejecting it would remove the only shape that lets a bot ask
// for a file ONLY when it cannot proceed without one.
func TestFileFieldOnLLMOrHumanGateCompiles(t *testing.T) {
	src := `
schema gate_out:
  approved: bool
  music: file

human gate:
  interaction: llm_or_human
  model: "anthropic/claude-sonnet-4-6"
  output: gate_out

workflow main:
  entry: gate
  gate -> done
`
	res := compileFile(t, src)
	for _, d := range res.Diagnostics {
		if d.Code == DiagFileFieldNotHuman {
			t.Fatalf("llm_or_human must not be rejected: %s", d.Message)
		}
	}
}

// `file` is recognised as a type only in type position — it is NOT a
// reserved word. A schema field literally NAMED `file` must keep
// parsing (bots/sec-audit-source/main.bot declares one), including the
// pathological `file: file`.
func TestFileIsNotAReservedIdentifier(t *testing.T) {
	src := `
schema finding:
  file: string
  line: int

human gate:
  output: finding

workflow main:
  entry: gate
  gate -> done
`
	w := mustCompile(t, src)
	if got := w.Schemas["finding"].Fields[0]; got.Name != "file" || got.Type != FieldTypeString {
		t.Errorf("field 0 = %q:%v, want file:string", got.Name, got.Type)
	}

	// And the degenerate case: a file-typed field named `file`.
	w2 := mustCompile(t, `
schema upload:
  file: file

human gate:
  output: upload

workflow main:
  entry: gate
  gate -> done
`)
	if got := w2.Schemas["upload"].Fields[0]; got.Name != "file" || got.Type != FieldTypeFile {
		t.Errorf("field 0 = %q:%v, want file:file", got.Name, got.Type)
	}
}
