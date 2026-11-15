package unparse_test

import (
	goast "go/ast"
	goparser "go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
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
		"Properties":              "source positions never travel (the per-property span table, ast.PropertySpans)",
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
		"ContractDecl.Name":       "written as the declaration header",
		"PortDecl.Name":           "written as the entry header",
		"CriterionDecl.Name":      "written as the entry header",
		"PublicEffect.Name":       "written as the entry header",
		"File.Profile":            "written through f.EffectiveProfile(), which a `.Field` regex cannot see",
	}
	types := []string{
		"WorkflowDecl", "LLMDecl", "AgentDecl", "JudgeDecl", "RouterDecl", "HumanDecl",
		"ToolNodeDecl", "ComputeDecl", "ComputeExpr", "SubbotDecl", "EmitDecl", "WaitDecl",
		"AwaitAnswersDecl", "FailDecl", "GroupDecl", "UseDecl", "Edge", "LoopClause",
		"ForeachClause", "WhenClause", "WithEntry", "BudgetBlock", "ResourcesBlock",
		"CodemodeBlock",
		"SandboxBlock", "SandboxBuildBlock", "SandboxNetworkBlock", "MemoryBlock",
		"CompactionBlock", "FallbackDecl", "CursorDecl", "CursorEnumValue", "CursorBand",
		"CursorBlock", "CursorSetting", "SupervisorDecl", "MCPServerDecl", "MCPAuthDecl",
		"MCPConfigDecl", "RecoveryBlock", "VarField", "SecretField", "AttachmentField",
		"PromptDecl", "SchemaDecl", "SchemaField", "Preset", "PresetValue", "Literal",
		"ContractDecl", "PortDecl", "PortFileDecl", "CriterionDecl", "PublicEffect",
		"File", "Comment", "ImportDecl", "ActionParam",
		"VarsBlock", "SecretsBlock", "AttachmentsBlock", "PresetsBlock",
	}
	var writer []byte
	for _, file := range []string{"unparse.go", "contracts.go", "comments.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		writer = append(writer, src...)
	}
	fields := structFields(t, filepath.Join("..", "ast", "ast.go"))
	for typ, fs := range structFields(t, filepath.Join("..", "ast", "contract.go")) {
		fields[typ] = fs
	}
	// KEPT COMPLETE BY CONSTRUCTION. Until this check existed the sweep only
	// erred when a LISTED type was missing from ast.go, never the reverse —
	// so a new declaration type was outside the alarm entirely, and
	// `CodemodeBlock` was: a mutant that stopped writing a workflow-level
	// `codemode:` block survived `./pkg/dsl/...` and every importer of this
	// package, while `iterion fmt` silently deleted the declaration. Since
	// the block form implies `on`, that does not merely lose seven numbers,
	// it turns codemode OFF for every node that inherited it. The jsonenc
	// sweep has had this half since it shipped; this one owed it.
	// No exclusion list. Measured: seven of the eight names an earlier draft
	// excluded pass the sweep's own assertion if they are simply LISTED, so
	// the exclusion bought nothing and set a precedent for the next type. The
	// eighth, ast.File, hid exactly one real field — Profile, written through
	// f.EffectiveProfile() and so invisible to a `.Field` regex — at the cost
	// of putting every top-level declaration list on ast.File (Agents,
	// Workflows, Contracts, …) outside the alarm, which is the field class
	// the alarm exists for. That one field is named in `unwritten` instead.
	listed := map[string]bool{}
	for _, name := range types {
		listed[name] = true
	}
	for typ := range fields {
		if listed[typ] {
			continue
		}
		t.Errorf("ast.%s is not in the sweep's type list — a declaration outside this alarm loses fields silently, which is how CodemodeBlock got here", typ)
	}

	for _, typ := range types {
		fs, ok := fields[typ]
		if !ok {
			t.Errorf("ast.%s not found in ast.go — the sweep's type list is stale", typ)
			continue
		}
		for _, f := range fs {
			if f == "Span" || f == "Properties" {
				continue
			}
			if _, ok := unwritten[typ+"."+f]; ok {
				continue
			}
			if !regexp.MustCompile(`\.` + regexp.QuoteMeta(f) + `\b`).Match(writer) {
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
