package unparse_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/internal/dsltest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// matchingSrc is a program whose vars carry a pattern alone, a pattern
// beside an enum, and neither — so a writer or a transport that drops the
// pattern changes the program rather than merely losing a decoration.
const matchingSrc = "dsl: 2\n\n" +
	"vars:\n" +
	"  agent: string [matching: \"^[A-Za-z0-9._][A-Za-z0-9._-]*$|^$\"] = \"\"\n" +
	"  mode: string [enum: \"fast\", \"slow\"] [matching: \"^[a-z]+$\"] = \"fast\"\n" +
	"  free: string = \"anything\"\n\n" +
	"schema label:\n  kind: string\n\n" +
	"compute show:\n  output: label\n  expr:\n    kind: \"vars.agent\"\n\n" +
	"workflow w:\n  entry: show\n  show -> done\n"

// TestVarMatchingSurvivesTheWriterAndTheJSONTransport executes both round
// trips a var declaration has to survive and asks ir.SameProgram — the
// repo's own oracle for "this text or document IS the program the author
// wrote" — about each.
//
// It is executed rather than asserted on the text because that is the
// whole lesson of the #1222 cluster: a field can be spelled correctly in
// the output and still be absent from the program, or present in the
// program and dropped by one of the transport's three touch points. Only
// compiling both sides and comparing the workflows catches all of it.
//
// Mutations that redden it: delete the writeMatchingConstraint call in
// writeVarsBlock (the writer), drop Matching from jsonVarField, from
// varsBlockToJSON, or from varsBlockFromJSON (the transport).
func TestVarMatchingSurvivesTheWriterAndTheJSONTransport(t *testing.T) {
	pr := parser.Parse("matching.bot", matchingSrc)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("fixture does not parse: %s", d.Error())
		}
	}
	direct := ir.Compile(pr.File)
	if direct.HasErrors() {
		t.Fatalf("fixture does not compile: %v", direct.Diagnostics)
	}
	// The fixture has to be able to redden: if the compiler never carried
	// the pattern, every comparison below would pass on two empty fields.
	if got := direct.Workflow.Vars["agent"].Matching; got == "" {
		t.Fatal("fixture is inert: the compiled var carries no pattern, so nothing here can fail")
	}

	t.Run("the writer", func(t *testing.T) {
		text := unparse.Unparse(pr.File)
		if !strings.Contains(text, `[matching: "^[A-Za-z0-9._][A-Za-z0-9._-]*$|^$"]`) {
			t.Fatalf("iterion fmt dropped the constraint:\n%s", text)
		}
		pr2 := parser.Parse("matching.bot", text)
		for _, d := range pr2.Diagnostics {
			if d.Severity == parser.SeverityError {
				t.Fatalf("the written text does not parse back: %s\n%s", d.Error(), text)
			}
		}
		dsltest.AssertSameProgram(t, "matching.bot", direct, ir.Compile(pr2.File))
		if err := unparse.Verify(pr.File, text); err != nil {
			t.Errorf("Verify: %v", err)
		}
	})

	t.Run("the JSON transport", func(t *testing.T) {
		data, err := ast.MarshalFile(pr.File)
		if err != nil {
			t.Fatalf("MarshalFile: %v", err)
		}
		// The wire key is pinned: the studio's VarField type and
		// pkg/botregistry's schema surface are built against it, and both
		// decode into structs of their own that drop what they do not name.
		if !strings.Contains(string(data), `"matching"`) {
			t.Fatalf("the wire payload carries no \"matching\" key:\n%s", data)
		}
		back, err := ast.UnmarshalFile(data)
		if err != nil {
			t.Fatalf("UnmarshalFile: %v", err)
		}
		dsltest.AssertSameProgram(t, "matching.bot", direct, ir.Compile(back))
	})

	t.Run("a var with no pattern keeps none", func(t *testing.T) {
		data, err := ast.MarshalFile(pr.File)
		if err != nil {
			t.Fatalf("MarshalFile: %v", err)
		}
		back, err := ast.UnmarshalFile(data)
		if err != nil {
			t.Fatalf("UnmarshalFile: %v", err)
		}
		for _, f := range back.Vars.Fields {
			if f.Name == "free" && f.Matching != "" {
				t.Errorf("an unconstrained var came back with Matching = %q", f.Matching)
			}
		}
	})
}
