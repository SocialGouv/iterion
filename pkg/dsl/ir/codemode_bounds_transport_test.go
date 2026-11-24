package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

const boundsOnlyBot = `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    concurrent_tool_calls: 3

workflow w:
  entry: a
  a -> done
`

// The two source paths must read one declaration as one program.
//
// `jsonenc.UnmarshalFile` BYPASSES the parser — compile.go says so where it
// enforces its own two invariants: "the JSON AST entry point (jsonenc.go
// UnmarshalFile) bypasses the parser entirely. The compiler is the single
// convergence point for both source paths." So every default the parser
// applies has to be applied by the transport too, or the same document is two
// programs.
//
// `codemodeBlockFromJSON` was a verbatim field copy of its struct. The idiom
// it mirrors, `sandboxBlockFromJSON`, carries two rules whose comments name
// exactly this failure — an unfilled block "is read as absent, so the
// document saves and reads back the same", and "a block with fields but no
// mode is the block form … the transport reads it the same way, so a document
// and its re-parse agree on the mode". The copy took the shape and left the
// rules, which is what a port that diverges from the thing it ports looks
// like.
//
// A client that does not repeat the mode the block IMPLIES is the reachable
// case: iterion's own writer emits it, but the implication is the documented
// contract, so the first client to model the block will omit it.
func TestABoundsBlockWithNoModeIsOnThroughBothSourcePaths(t *testing.T) {
	t.Parallel()

	// Path 1: the parser.
	direct := compileFile(t, boundsOnlyBot)
	if len(direct.Diagnostics) > 0 {
		t.Fatalf("the parser path drew diagnostics: %+v", direct.Diagnostics)
	}
	viaParser := direct.Workflow.Nodes["a"].(*AgentNode)

	// Path 2: the transport, with the implied mode NOT repeated — which is
	// what the block form implying `on` means a client may do.
	res, err := parser.Parse("t.bot", boundsOnlyBot)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	raw, err := ast.MarshalFile(res.File)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	stripped := strings.Replace(string(raw), `"mode":"on",`, "", 1)
	if stripped == string(raw) {
		// Spelled with spaces, or the writer stopped emitting it: either
		// way this test would silently stop measuring anything.
		if stripped = strings.Replace(string(raw), `"mode": "on",`, "", 1); stripped == string(raw) {
			t.Fatalf("no %q in the marshalled document, so this test is not exercising the omission it is named for:\n%s", `"mode":"on"`, raw)
		}
	}
	file, err := ast.UnmarshalFile([]byte(stripped))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	viaJSON := Compile(file)
	for _, d := range viaJSON.Diagnostics {
		if d.Severity == SeverityError {
			t.Fatalf("the transport path failed to compile: %+v", viaJSON.Diagnostics)
		}
	}
	node := viaJSON.Workflow.Nodes["a"].(*AgentNode)

	// The mode. "" is INHERIT, which resolves to off — so without the
	// normalisation the transport path runs a node with a bound and no
	// capability, and C348 cannot say so because it keys on "off", not "".
	if node.Codemode != viaParser.Codemode {
		t.Errorf("codemode = %q through the transport and %q through the parser: one declaration, two programs", node.Codemode, viaParser.Codemode)
	}
	if node.Codemode != "on" {
		t.Errorf("codemode = %q, want \"on\": the block form implies it on BOTH paths", node.Codemode)
	}
	// And the bounds must still be there — a normalisation that fixed the
	// mode by dropping the block would trade one silent defect for another.
	if node.CodemodeBounds == nil || node.CodemodeBounds.ConcurrentToolCalls != 3 || viaParser.CodemodeBounds == nil || viaParser.CodemodeBounds.ConcurrentToolCalls != 3 {
		t.Errorf("bounds = %+v, want ConcurrentToolCalls 3 carried through the transport", node.CodemodeBounds)
	}
}

func TestCodemodeStoreWorkflowQuotaTransport(t *testing.T) {
	t.Parallel()
	source := strings.Replace(boundsOnlyBot, "workflow w:\n", "workflow w:\n  codemode:\n    store_entries: 48\n    store_bytes: 262144\n", 1)
	parsed, err := parser.Parse("quota.bot", source)
	if err != nil || len(parsed.Diagnostics) > 0 {
		t.Fatalf("parse: %v, %+v", err, parsed.Diagnostics)
	}
	raw, err := ast.MarshalFile(parsed.File)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ast.UnmarshalFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	compiled := Compile(back)
	if compiled.Workflow == nil || len(compiled.Diagnostics) > 0 {
		t.Fatalf("compile: %+v", compiled.Diagnostics)
	}
	bounds := compiled.Workflow.CodemodeBounds
	if bounds == nil || bounds.StoreEntries != 48 || bounds.StoreBytes != 262144 {
		t.Fatalf("quota bounds lost: %+v", bounds)
	}
}

func TestCodemodeStoreQuotaRefusesNodeAndNegativeDeclarations(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"agent", "judge"} {
		for _, prop := range []string{"store_entries", "store_bytes"} {
			for _, value := range []string{"1", "0"} {
				source := host + " a:\n  model: \"m\"\n  codemode:\n    " + prop + ": " + value + "\nworkflow w:\n  entry: a\n  a -> done\n"
				res, err := parser.Parse("quota.bot", source)
				if err != nil || len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "run-wide quota") {
					t.Fatalf("%s %s=%s: want run-wide scope diagnostic, got %v / %+v", host, prop, value, err, res.Diagnostics)
				}
			}
		}
	}
	parsed, err := parser.Parse("quota.bot", boundsOnlyBot)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := ast.MarshalFile(parsed.File)
	if err != nil {
		t.Fatal(err)
	}
	mutated := strings.Replace(string(raw), `"concurrent_tool_calls": 3`, `"store_entries": 48`, 1)
	if mutated == string(raw) {
		t.Fatal("test fixture does not carry node bounds")
	}
	back, err := ast.UnmarshalFile([]byte(mutated))
	if err != nil {
		t.Fatal(err)
	}
	if got := countCode(Compile(back), DiagInvalidCodemodeStoreQuota); got != 1 {
		t.Fatalf("node quota via AST JSON produced %d scope diagnostics, want 1", got)
	}
	res, err := parser.Parse("quota.bot", "workflow w:\n  codemode:\n    store_entries: -1\n  start -> done\n")
	if err != nil || len(res.Diagnostics) == 0 {
		t.Fatalf("negative workflow quota was accepted: %v / %+v", err, res.Diagnostics)
	}
}
