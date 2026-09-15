package unparse

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

const contractSource = `schema report:
  ok: bool

contract c:
  display_name: "Feature dev"
  responsibility: "Implements a feature and opens a PR"
  version: 2
  inputs:
    goal: string
      description: "What to build"
    depth: int
      required: false
      default: 3
    labels: string[]
      nullable: true
      default: null
      min_items: 1
      max_items: 5
    spec: report[]
    brief: string
      file:
        media_type: "text/markdown"
        min_bytes: 1
        schema: report
  outputs:
    pr_url: string
      from: open_pr.url
    artefact: string
      from: write
      file:

  criteria:
    goal_long_enough:
      kind: min_length
      port: input.goal
      params: {min: 2, nested: {}, "quoted key": [true, 1.5, "x", null]}
    declared_only:

  effects:
    opens_pr:
      description: "Opens a pull request"
      paid: true
    silent:


agent a:
  description: "d"

workflow w:

  entry: a
  contract: c

  a -> done
`

// documentJSON is the span-free document a text reads as, for comparing
// two texts as documents (the compiled program does not carry the
// contract yet: the AST mirror is the oracle).
func documentJSON(t *testing.T, f *ast.File) []byte {
	t.Helper()
	raw, err := ast.MarshalFile(withoutComments(f))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The writer puts every property of a contract back in the text the
// parser reads — in the writer's canonical order, empty entries as bare
// headers — and the text reads as the same document.
func TestContractsRoundTrip(t *testing.T) {
	res := parser.Parse("x.bot", contractSource)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("fixture: %v", res.Diagnostics)
	}
	out := Unparse(res.File)
	if out != contractSource {
		t.Fatalf("written:\n%s\nwant:\n%s", out, contractSource)
	}
	back := parser.Parse("x.bot", out)
	if len(back.Diagnostics) != 0 {
		t.Fatalf("the written text does not parse: %v\n%s", back.Diagnostics, out)
	}
	if a, b := documentJSON(t, res.File), documentJSON(t, back.File); !bytes.Equal(a, b) {
		t.Fatalf("the written text is another document: %s\n%s", firstJSONDifference(a, b), out)
	}
	if err := Verify(res.File, out); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// A JSON string in a default or in parameters goes through the writer's
// quoting: a quote or a newline inside it has no v1 `"…"` form and is
// written as a raw string; a key that is not an identifier is quoted; a
// value from the transport with keys in any order is written in key
// order, which is what the parser gives back.
func TestContractJSONValuesUseTheWritersQuoting(t *testing.T) {
	f := &ast.File{
		Contracts: []*ast.ContractDecl{{Name: "c",
			Inputs: []*ast.PortDecl{
				{Name: "x", Type: "string", Default: json.RawMessage(`"say \"hi\"\nnow"`)},
				{Name: "y", Type: "report", Default: json.RawMessage(`{"z":1,"a key":"v","a":[]}`)},
			},
		}},
		Agents: []*ast.AgentDecl{{Name: "a"}},
	}
	out := Unparse(f)
	for _, want := range []string{
		"      default: `say \"hi\"\nnow`\n",
		"      default: {a: [], \"a key\": \"v\", z: 1}\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the text lacks %q:\n%s", want, out)
		}
	}
	back := parser.Parse("x.bot", out)
	if len(back.Diagnostics) != 0 {
		t.Fatalf("the written text does not parse: %v\n%s", back.Diagnostics, out)
	}
	var s string
	if err := json.Unmarshal(back.File.Contracts[0].Inputs[0].Default, &s); err != nil || s != "say \"hi\"\nnow" {
		t.Errorf("the string default read back as %q (%v)", s, err)
	}
	if got := string(back.File.Contracts[0].Inputs[1].Default); got != `{"a":[],"a key":"v","z":1}` {
		t.Errorf("the object default read back as %s", got)
	}
	if err := Verify(f, out); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// A contract with nothing in it — the canvas's before it is filled in —
// is a bare header the parser reads as an empty contract, and the
// workflow's `contract:` survives a document with no other property.
func TestAnEmptyContractIsWrittenAsABareHeader(t *testing.T) {
	f := &ast.File{
		Contracts: []*ast.ContractDecl{{Name: "c"}},
		Workflows: []*ast.WorkflowDecl{{Name: "w", Contract: "c"}},
	}
	out := Unparse(f)
	back := parser.Parse("x.bot", out)
	for _, d := range back.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("does not parse back: %s\n%s", d.Error(), out)
		}
	}
	if len(back.File.Contracts) != 1 || back.File.Contracts[0].Name != "c" || back.File.Workflows[0].Contract != "c" {
		t.Fatalf("read back as %+v, workflow contract %q\n%s", back.File.Contracts, back.File.Workflows[0].Contract, out)
	}
	if err := Verify(f, out); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// A value that is not an identifier where the grammar wants one — a port
// type, a producer, a criterion's port — is quoted, so the text is refused
// by the parser at the field instead of by the lexer somewhere else.
func TestContractIdentifiersThatAreNotOneAreQuoted(t *testing.T) {
	f := &ast.File{
		Contracts: []*ast.ContractDecl{{Name: "c",
			Outputs:  []*ast.PortDecl{{Name: "x", Type: "not a type", From: "no way"}},
			Criteria: []*ast.CriterionDecl{{Name: "k", Kind: "min_length", Port: "output x"}},
		}},
	}
	out := Unparse(f)
	for _, want := range []string{`    x: "not a type"`, `      from: "no way"`, `      port: "output x"`} {
		if !strings.Contains(out, want) {
			t.Errorf("the text lacks %q:\n%s", want, out)
		}
	}
	if err := Verify(f, out); err == nil {
		t.Fatalf("a document the text cannot carry passed the guard:\n%s", out)
	}
}

// A name the transport can carry but the grammar cannot — a newline in it,
// written bare, would read as more declarations than the document has —
// is quoted by the writer, so the text is refused by the parser at the
// name, and named by Verify first. The four name sites, one by one, on a
// document that compiles: the compiled program would not have told.
func TestContractNamesThatAreNotIdentifiersAreRefusedAtTheName(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(c *ast.ContractDecl)
		bare   string
	}{
		"contract":  {func(c *ast.ContractDecl) { c.Name = "c:\n\ncontract injected" }, "\ncontract injected:"},
		"port":      {func(c *ast.ContractDecl) { c.Inputs[0].Name = "goal: string\n    injected" }, "\n    injected: string"},
		"criterion": {func(c *ast.ContractDecl) { c.Criteria[0].Name = "k:\n      kind: x\n    injected" }, "\n    injected:\n"},
		"effect":    {func(c *ast.ContractDecl) { c.Effects[0].Name = "e:\n    injected" }, "\n    injected:\n"},
	} {
		t.Run(name, func(t *testing.T) {
			res := parser.Parse("x.bot", contractSource)
			if len(res.Diagnostics) != 0 {
				t.Fatalf("fixture: %v", res.Diagnostics)
			}
			tc.mutate(res.File.Contracts[0])
			out := Unparse(res.File)
			if strings.Contains(out, tc.bare) {
				t.Fatalf("the name was written bare:\n%s", out)
			}
			back := parser.Parse("x.bot", out)
			var refused bool
			for _, d := range back.Diagnostics {
				refused = refused || d.Severity == parser.SeverityError
			}
			if !refused {
				t.Fatalf("the written text parses, as %d contracts:\n%s", len(back.File.Contracts), out)
			}
			if err := Verify(res.File, out); err == nil || !strings.Contains(err.Error(), "not an identifier") {
				t.Fatalf("Verify: %v", err)
			}
		})
	}
}

// The guard holds the contract to the document even when the program
// compiles: a text that lost the contract, a property of it, a criterion's
// parameter or the workflow's `contract:` is refused, named.
func TestVerifyHoldsTheContractToTheDocument(t *testing.T) {
	res := parser.Parse("x.bot", contractSource)
	out := Unparse(res.File)
	for name, edit := range map[string]func(string) string{
		"no contract": func(s string) string {
			return s[:strings.Index(s, "contract c:")] + s[strings.Index(s, "agent a:"):]
		},
		"no workflow contract": func(s string) string { return strings.Replace(s, "  contract: c\n", "", 1) },
		"a changed parameter":  func(s string) string { return strings.Replace(s, "params: {min: 2,", "params: {min: 3,", 1) },
		"a dropped version":    func(s string) string { return strings.Replace(s, "  version: 2\n", "", 1) },
		"a dropped required":   func(s string) string { return strings.Replace(s, "      required: false\n", "", 1) },
		"a dropped producer":   func(s string) string { return strings.Replace(s, "      from: write\n", "", 1) },
	} {
		t.Run(name, func(t *testing.T) {
			text := edit(out)
			if text == out {
				t.Fatal("the edit changed nothing")
			}
			if err := Verify(res.File, text); err == nil || !strings.Contains(err.Error(), "not the same document") {
				t.Fatalf("Verify: %v\n%s", err, text)
			}
		})
	}
	if err := Verify(res.File, out); err != nil {
		t.Fatalf("the unedited text is refused: %v", err)
	}
}

// A JSON value the text cannot write — a number with a sign or an
// exponent, which the transport accepts — and a nil entry are refused by
// Verify by name, before a text is written.
func TestVerifyRefusesWhatTheTextCannotCarry(t *testing.T) {
	for name, tc := range map[string]struct {
		f    *ast.File
		want string
	}{
		"default":      {&ast.File{Contracts: []*ast.ContractDecl{{Name: "c", Inputs: []*ast.PortDecl{{Name: "x", Type: "int", Default: json.RawMessage(`1e2`)}}}}}, `input "x" default: the number 1e2`},
		"params":       {&ast.File{Contracts: []*ast.ContractDecl{{Name: "c", Criteria: []*ast.CriterionDecl{{Name: "k", Kind: "min_length", Params: json.RawMessage(`{"min": -1}`)}}}}}, `criterion "k" params: the number -1`},
		"nil port":     {&ast.File{Contracts: []*ast.ContractDecl{{Name: "c", Outputs: []*ast.PortDecl{nil}}}}, "output 1 is nil"},
		"nil contract": {&ast.File{Contracts: []*ast.ContractDecl{nil}}, "a nil contract"},
	} {
		t.Run(name, func(t *testing.T) {
			err := Verify(tc.f, "")
			if err == nil || !strings.Contains(err.Error(), "cannot be written as .bot source") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Verify: %v", err)
			}
		})
	}
}

// `kind` may be dotted (a plugin's evaluator); a type and a schema are
// names. Written bare, read back as they were; a type that is not a name is
// quoted and refused at the port.
func TestContractDottedKindAndNamedTypesRoundTrip(t *testing.T) {
	src := "contract c:\n  inputs:\n    x: string\n      file:\n        schema: report\n  criteria:\n    k:\n      kind: plugin.check\n      port: input.x\n"
	res := parser.Parse("x.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("fixture: %v", res.Diagnostics)
	}
	if out := Unparse(res.File); out != src {
		t.Fatalf("written:\n%s\nwant:\n%s", out, src)
	}
	if err := Verify(res.File, Unparse(res.File)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	res.File.Contracts[0].Inputs[0].Type = "a.b"
	out := Unparse(res.File)
	if !strings.Contains(out, `    x: "a.b"`) {
		t.Fatalf("a dotted type was written bare:\n%s", out)
	}
	if err := Verify(res.File, out); err == nil {
		t.Fatalf("a document with a type that is not a name passed the guard:\n%s", out)
	}
}

// An explicit version is a value the author wrote — 0 included, which the
// compiler refuses: kept by the parser, written back, held by the guard.
func TestAnExplicitContractVersionSurvives(t *testing.T) {
	src := "contract c:\n  version: 0\n"
	res := parser.Parse("x.bot", src)
	if len(res.Diagnostics) != 0 || res.File.Contracts[0].Version == nil || *res.File.Contracts[0].Version != 0 {
		t.Fatalf("version read as %v (%v)", res.File.Contracts[0].Version, res.Diagnostics)
	}
	if out := Unparse(res.File); out != src {
		t.Fatalf("written:\n%s", out)
	}
	if err := Verify(res.File, "contract c:\n"); err == nil {
		t.Fatal("a text that dropped the version passed the guard")
	}
}

// Every value the writer renders bare — a type, a producer, a kind, a
// port, a schema, an integer — is held by Verify to what the grammar
// reads back, and a port without a type is named: never a lexer error at
// a column the author never typed.
func TestVerifyNamesEveryValueTheTextCannotCarry(t *testing.T) {
	minus := -3
	port := func(p ast.PortDecl) *ast.File {
		return &ast.File{Contracts: []*ast.ContractDecl{{Name: "c", Inputs: []*ast.PortDecl{&p}}}}
	}
	for name, tc := range map[string]struct {
		f    *ast.File
		want string
	}{
		"no type":           {port(ast.PortDecl{Name: "goal"}), `input "goal" has no type`},
		"type not a name":   {port(ast.PortDecl{Name: "goal", Type: "a b[]"}), `input goal type "a b" is not an identifier`},
		"from not a ref":    {port(ast.PortDecl{Name: "goal", Type: "string", From: "a b"}), `input goal from "a b" is not an identifier`},
		"negative items":    {port(ast.PortDecl{Name: "goal", Type: "string[]", MinItems: &minus}), "input goal min_items: the number -3"},
		"schema not a name": {port(ast.PortDecl{Name: "goal", Type: "string", FileSpec: &ast.PortFileDecl{Schema: "a.b"}}), `input goal file schema "a.b" is not an identifier`},
		"negative bytes":    {port(ast.PortDecl{Name: "goal", Type: "string", FileSpec: &ast.PortFileDecl{MinBytes: -1}}), "input goal file min_bytes: the number -1"},
		"negative version":  {&ast.File{Contracts: []*ast.ContractDecl{{Name: "c", Version: &minus}}}, "version: the number -3"},
		"kind not a name":   {&ast.File{Contracts: []*ast.ContractDecl{{Name: "c", Criteria: []*ast.CriterionDecl{{Name: "k", Kind: "a b"}}}}}, `criterion k kind "a b" is not an identifier`},
		"port not a ref":    {&ast.File{Contracts: []*ast.ContractDecl{{Name: "c", Criteria: []*ast.CriterionDecl{{Name: "k", Kind: "min_length", Port: "input goal"}}}}}, `criterion k port "input goal" is not an identifier`},
	} {
		t.Run(name, func(t *testing.T) {
			err := Verify(tc.f, "")
			if err == nil || !strings.Contains(err.Error(), "cannot be written as .bot source") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Verify: %v", err)
			}
		})
	}
}
