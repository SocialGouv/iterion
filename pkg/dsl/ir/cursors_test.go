package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

const cursorsBaseWorkflow = `
cursor ambition:
  description: "How aggressively to expand scope"
  values:
    cautious: "Stick strictly to the stated request."
    balanced: "Address the request; suggest one improvement."
    ambitious: "Proactively identify 2-3 improvements."

cursor depth:
  description: "Investigation thoroughness"
  bands:
    "0.0..0.33": "Skim the surface; cite primary sources only."
    "0.34..0.66": "Examine main code paths."
    "0.67..1.0": "Trace all call sites and edge cases."

agent reviewer:
  model: "anthropic/claude-opus-4-7"
  system: review_prompt
  cursors:
    enabled: true
    ambition: ambitious
    depth: 0.7

judge gate:
  model: "anthropic/claude-sonnet-4-6"
  system: review_prompt
  cursors:
    ambition: cautious

prompt review_prompt:
  Review carefully.

workflow w:
  entry: reviewer
  reviewer -> gate
  gate -> done
`

func TestCursorsParseAndCompile(t *testing.T) {
	pr := parser.Parse("test.iter", cursorsBaseWorkflow)
	if len(pr.Diagnostics) > 0 {
		for _, d := range pr.Diagnostics {
			t.Logf("parser diag: %+v", d)
		}
		t.Fatalf("parser produced diagnostics for valid cursors workflow")
	}
	cr := Compile(pr.File)
	if cr.HasErrors() {
		for _, d := range cr.Diagnostics {
			t.Logf("ir diag: %+v", d)
		}
		t.Fatalf("compile returned errors")
	}
	wf := cr.Workflow
	if wf == nil {
		t.Fatal("compile returned nil workflow")
	}
	if len(wf.Cursors) != 2 {
		t.Fatalf("expected 2 cursors, got %d", len(wf.Cursors))
	}
	amb := wf.Cursors["ambition"]
	if amb == nil || len(amb.Values) != 3 {
		t.Fatalf("ambition cursor not properly compiled: %+v", amb)
	}
	if amb.Values[2].Name != "ambitious" || !strings.Contains(amb.Values[2].Prompt, "Proactively") {
		t.Fatalf("unexpected ambition[2]: %+v", amb.Values[2])
	}
	dep := wf.Cursors["depth"]
	if dep == nil || len(dep.Bands) != 3 {
		t.Fatalf("depth cursor not properly compiled: %+v", dep)
	}
	if dep.Bands[0].Lo != 0.0 || dep.Bands[0].Hi != 0.33 {
		t.Fatalf("depth band 0: got [%g..%g], want [0..0.33]", dep.Bands[0].Lo, dep.Bands[0].Hi)
	}
	rev, ok := wf.Nodes["reviewer"].(*AgentNode)
	if !ok || rev.Cursors == nil {
		t.Fatalf("reviewer cursors missing on agent")
	}
	if !rev.Cursors.Enabled || len(rev.Cursors.Settings) != 2 {
		t.Fatalf("reviewer cursors block unexpected: %+v", rev.Cursors)
	}
	gate, ok := wf.Nodes["gate"].(*JudgeNode)
	if !ok || gate.Cursors == nil || len(gate.Cursors.Settings) != 1 {
		t.Fatalf("gate cursor invocation missing on judge")
	}
}

func TestCursorMalformedDecl(t *testing.T) {
	src := `
cursor broken:
  values:
    a: "ok"
  bands:
    "0.0..0.5": "nope"

agent x:
  model: "anthropic/c"
  system: p

prompt p:
  hi

workflow w:
  entry: x
  x -> done
`
	pr := parser.Parse("t.iter", src)
	if len(pr.Diagnostics) > 0 {
		t.Fatalf("parser should accept syntactically valid form: %+v", pr.Diagnostics)
	}
	cr := Compile(pr.File)
	if !cr.HasErrors() {
		t.Fatal("expected C085 for cursor with both values and bands")
	}
	found := false
	for _, d := range cr.Diagnostics {
		if d.Code == DiagMalformedCursor {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected DiagMalformedCursor (C085), got %+v", cr.Diagnostics)
	}
}

func TestCursorUnknownInvocation(t *testing.T) {
	src := `
cursor ambition:
  values:
    a: "ok"

agent x:
  model: "anthropic/c"
  system: p
  cursors:
    nonexistent: a

prompt p:
  hi

workflow w:
  entry: x
  x -> done
`
	pr := parser.Parse("t.iter", src)
	if len(pr.Diagnostics) > 0 {
		t.Fatalf("parser diagnostics: %+v", pr.Diagnostics)
	}
	cr := Compile(pr.File)
	foundUnknown := false
	for _, d := range cr.Diagnostics {
		if d.Code == DiagUnknownCursor {
			foundUnknown = true
		}
	}
	if !foundUnknown {
		t.Fatalf("expected DiagUnknownCursor (C083), got %+v", cr.Diagnostics)
	}
}

func TestCursorInvalidEnumValue(t *testing.T) {
	src := `
cursor ambition:
  values:
    cautious: "stay focused"
    ambitious: "go wide"

agent x:
  model: "anthropic/c"
  system: p
  cursors:
    ambition: nuclear

prompt p:
  hi

workflow w:
  entry: x
  x -> done
`
	pr := parser.Parse("t.iter", src)
	if len(pr.Diagnostics) > 0 {
		t.Fatalf("parser diagnostics: %+v", pr.Diagnostics)
	}
	cr := Compile(pr.File)
	found := false
	for _, d := range cr.Diagnostics {
		if d.Code == DiagInvalidCursorVal {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected DiagInvalidCursorVal (C084), got %+v", cr.Diagnostics)
	}
}

func TestCursorEnvVarAcceptedStatically(t *testing.T) {
	src := `
cursor ambition:
  values:
    cautious: "stay focused"
    ambitious: "go wide"

agent x:
  model: "anthropic/c"
  system: p
  cursors:
    ambition: "${ITERION_AMBITION:-ambitious}"

prompt p:
  hi

workflow w:
  entry: x
  x -> done
`
	pr := parser.Parse("t.iter", src)
	if len(pr.Diagnostics) > 0 {
		t.Fatalf("parser diagnostics: %+v", pr.Diagnostics)
	}
	cr := Compile(pr.File)
	for _, d := range cr.Diagnostics {
		if d.Code == DiagInvalidCursorVal {
			t.Fatalf("env-substituted value should be deferred to runtime, got %+v", d)
		}
	}
}
