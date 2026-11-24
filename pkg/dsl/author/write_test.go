package author

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// An AST the document cannot say is refused by Write, never written
// otherwise: a name used twice in a mapping keyed by the author's names
// (YAML names a key once, and the reader would refuse the whole document),
// two workflows, a declared prompt body with no written form, a prompt
// name YAML cannot write plain.
func TestWriteRefusesWhatTheDocumentCannotSay(t *testing.T) {
	for _, tc := range []struct {
		name string
		file *ast.File
		want string
	}{
		{"two vars of one name", &ast.File{Vars: &ast.VarsBlock{Fields: []*ast.VarField{{Name: "x", Type: ast.TypeString}, {Name: "x", Type: ast.TypeInt}}}}, `vars names "x" twice`},
		{"two ports of one name", &ast.File{Contracts: []*ast.ContractDecl{{Name: "c", Inputs: []*ast.PortDecl{{Name: "p", Type: "string"}, {Name: "p", Type: "int"}}}}}, `ports names "p" twice`},
		{"two secrets of one name", &ast.File{Secrets: &ast.SecretsBlock{Fields: []*ast.SecretField{{Name: "s"}, {Name: "s", Value: "v"}}}}, `secrets names "s" twice`},
		{"two prompts of one name", &ast.File{Prompts: []*ast.PromptDecl{{Name: "p", Body: "a"}, {Name: "p", Body: "b"}}}, `prompts names "p" twice`},
		{"two cursor settings of one name", &ast.File{Agents: []*ast.AgentDecl{{Name: "a", LLMDecl: ast.LLMDecl{Cursors: &ast.CursorBlock{Enabled: true, Settings: []*ast.CursorSetting{{Key: "depth", Value: "deep"}, {Key: "depth", Value: "shallow"}}}}}}}, `entries names "depth" twice`},
		{"two params of one key", &ast.File{Tools: []*ast.ToolNodeDecl{{Name: "t", Action: "x.y.z", Params: []ast.ActionParam{{Key: "k", Value: "1"}, {Key: "k", Value: "2"}}}}}, `params names "k" twice`},
		{"two expr fields of one name", &ast.File{Computes: []*ast.ComputeDecl{{Name: "c", Expr: []*ast.ComputeExpr{{Key: "ok", Expr: "1"}, {Key: "ok", Expr: "2"}}}}}, `expr names "ok" twice`},
		{"two with entries of one key", &ast.File{Uses: []*ast.UseDecl{{Group: "g", Prefix: "p", With: []*ast.WithEntry{{Key: "k", Value: "1"}, {Key: "k", Value: "2"}}}}}, `with map names "k" twice`},
		{"two schema fields of one name", &ast.File{Schemas: []*ast.SchemaDecl{{Name: "s", Fields: []*ast.SchemaField{{Name: "f", Type: ast.FieldTypeString}, {Name: "f", Type: ast.FieldTypeInt}}}}}, `schema s names "f" twice`},
		{"two workflows", &ast.File{Workflows: []*ast.WorkflowDecl{{Name: "a"}, {Name: "b"}}}, "one workflow"},
		{"a prompt body with no written form", &ast.File{Prompts: []*ast.PromptDecl{{Name: "p", Body: "    deeper first line\n  shallower second"}}}, "no written form"},
		// `on`/`yes`/`no` are strings to YAML 1.2 and written plain; `true`
		// and `null` are not, and a reference YAML writes quoted would be
		// read as an inline prompt's text.
		{"a prompt named like a YAML bool", &ast.File{Prompts: []*ast.PromptDecl{{Name: "true", Body: "x"}}, Agents: []*ast.AgentDecl{{Name: "a", LLMDecl: ast.LLMDecl{System: "true"}}}}, "no plain YAML spelling"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Write(tc.file)
			if err == nil {
				t.Fatalf("written without error:\n%s", out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not say %q", err, tc.want)
			}
		})
	}
}

// A document built in memory — no positions — is written kind by kind, in
// the order of ast.File's fields; a document from a parse follows the
// text's order. Both read back as the same program.
func TestWriteOrdersNodesByPositionOrByKind(t *testing.T) {
	inMemory := &ast.File{
		Tools:  []*ast.ToolNodeDecl{{Name: "t", Command: "true"}},
		Agents: []*ast.AgentDecl{{Name: "a", LLMDecl: ast.LLMDecl{Model: "m"}}},
	}
	out, err := Write(inMemory)
	if err != nil {
		t.Fatal(err)
	}
	if ai, ti := strings.Index(string(out), "- agent: a"), strings.Index(string(out), "- tool: t"); ai < 0 || ti < 0 || ai > ti {
		t.Fatalf("an in-memory document writes agents before tools:\n%s", out)
	}
	res := Parse("m.yaml", out)
	if errs := errorsOf(res); len(errs) > 0 {
		t.Fatalf("refused: %v", errs)
	}
	positioned := res.File // the tool now sits after the agent, positioned
	positioned.Tools[0].Span.Start.Line = 1
	positioned.Agents[0].Span.Start.Line = 2
	out2, err := Write(positioned)
	if err != nil {
		t.Fatal(err)
	}
	if ai, ti := strings.Index(string(out2), "- agent: a"), strings.Index(string(out2), "- tool: t"); ai < 0 || ti < 0 || ti > ai {
		t.Fatalf("a positioned document writes the nodes in the text's order (tool first here):\n%s", out2)
	}
}

// The loop clause's suffixes ride the document as .bot edge text: a
// workflow written to x.bot.yaml and read back keeps `guard:` and
// `sized_by` (#2677) — a dropped suffix would silently re-arm the guard
// or restore the unscaled pricing on a studio save.
func TestWriteKeepsTheLoopClauseSuffixes(t *testing.T) {
	f := &ast.File{Workflows: []*ast.WorkflowDecl{{
		Name:  "w",
		Entry: "a",
		Edges: []*ast.Edge{
			{From: "a", To: "b", Loop: &ast.LoopClause{Name: "reask", MaxIterations: 2, Guard: "off", SizedBy: "outputs.probe.pending"}},
			{From: "b", To: "done"},
		},
	}}}
	out, err := Write(f)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(string(out), `as reask(2) guard: off sized_by "outputs.probe.pending"`) {
		t.Fatalf("the clause suffixes did not come out:\n%s", out)
	}
	back := Parse("w.x.bot.yaml", out)
	if back.HasErrors() {
		t.Fatalf("the written document does not read back: %v", back.Diagnostics)
	}
	loop := back.File.Workflows[0].Edges[0].Loop
	if loop == nil || loop.Guard != "off" || loop.SizedBy != "outputs.probe.pending" {
		t.Fatalf("read back %+v, want guard off + sized_by outputs.probe.pending", loop)
	}
}
