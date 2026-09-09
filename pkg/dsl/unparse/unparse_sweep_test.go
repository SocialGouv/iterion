package unparse_test

import (
	goast "go/ast"
	goparser "go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The unparser is the studio's save path: a declaration field it does not
// write is deleted from the .bot the next time anyone touches an unrelated
// property. Nothing in the round-trip tests notices a field the corpus
// never sets, so this sweep holds the writer's SOURCE to the AST's: every
// exported field of every declaration the unparser renders must be read
// somewhere in unparse.go (`.Field`), unless listed as deliberately
// unwritten. It is a drift alarm — a spelling check on the source, the
// value guarantee being the round-trip tests — but it is what catches
// "a field was added to ast.WorkflowDecl on main and nobody threaded it".
func TestEveryDeclarationFieldIsWrittenByTheUnparser(t *testing.T) {
	unwritten := map[string]string{
		"Span":                    "source positions never travel",
		"CursorDecl.Span":         "",
		"AgentDecl.LLMDecl":       "embedded; its fields are read through llmFields",
		"JudgeDecl.LLMDecl":       "embedded; its fields are read through llmFields",
		"RouterDecl.Multi":        "written (rendered as `multi: true`)",
		"SchemaField.Span":        "",
		"Literal.Raw":             "the literal is re-rendered from its typed value",
		"MCPServerDecl.Name":      "written as the declaration header",
		"WorkflowDecl.Name":       "written as the declaration header",
		"HumanDecl.Await":         "written",
		"Preset.Name":             "written as the entry header",
		"PresetValue.Key":         "written",
		"SecretsBlock.Fields":     "iterated",
		"VarsBlock.Fields":        "iterated",
		"PresetsBlock.Entries":    "iterated",
		"AttachmentsBlock.Fields": "iterated",
		"GroupDecl.Name":          "written as the declaration header",
		"UseDecl.Group":           "written",
		"UseDecl.Prefix":          "written",
		"CursorBlock.Settings":    "iterated",
		"SchemaDecl.Fields":       "iterated",
		"SchemaDecl.Name":         "written as the declaration header",
	}
	types := []string{
		"WorkflowDecl", "LLMDecl", "AgentDecl", "JudgeDecl", "RouterDecl", "HumanDecl",
		"ToolNodeDecl", "ComputeDecl", "ComputeExpr", "SubbotDecl", "EmitDecl", "WaitDecl",
		"AwaitAnswersDecl", "FailDecl", "GroupDecl", "UseDecl", "Edge", "LoopClause",
		"ForeachClause", "WhenClause", "WithEntry", "BudgetBlock", "ResourcesBlock",
		"SandboxBlock", "SandboxBuildBlock", "SandboxNetworkBlock", "MemoryBlock",
		"CompactionBlock", "FallbackDecl", "CursorDecl", "CursorEnumValue", "CursorBand",
		"CursorBlock", "CursorSetting", "SupervisorDecl", "MCPServerDecl", "MCPAuthDecl",
		"MCPConfigDecl", "RecoveryBlock", "VarField", "SecretField", "AttachmentField",
		"PromptDecl", "SchemaDecl", "SchemaField", "Preset", "PresetValue", "Literal",
	}
	writer, err := os.ReadFile("unparse.go")
	if err != nil {
		t.Fatal(err)
	}
	fields := structFields(t, filepath.Join("..", "ast", "ast.go"))
	for _, typ := range types {
		fs, ok := fields[typ]
		if !ok {
			t.Errorf("ast.%s not found in ast.go — the sweep's type list is stale", typ)
			continue
		}
		for _, f := range fs {
			if f == "Span" {
				continue
			}
			if _, ok := unwritten[typ+"."+f]; ok {
				continue
			}
			if !strings.Contains(string(writer), "."+f) {
				t.Errorf("ast.%s.%s is never read by the unparser — a document saved through the studio loses it", typ, f)
			}
		}
	}
}

// structFields maps each exported struct type of a Go source file to its
// exported field names (embedded structs listed by their type name).
func structFields(t *testing.T, path string) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := goparser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string][]string{}
	for _, decl := range parsed.Decls {
		gd, ok := decl.(*goast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*goast.TypeSpec)
			if !ok || !ts.Name.IsExported() {
				continue
			}
			st, ok := ts.Type.(*goast.StructType)
			if !ok {
				continue
			}
			var names []string
			for _, field := range st.Fields.List {
				if len(field.Names) == 0 { // embedded
					if id, ok := field.Type.(*goast.Ident); ok && id.IsExported() {
						names = append(names, id.Name)
					}
					continue
				}
				for _, n := range field.Names {
					if n.IsExported() {
						names = append(names, n.Name)
					}
				}
			}
			out[ts.Name.Name] = names
		}
	}
	return out
}
