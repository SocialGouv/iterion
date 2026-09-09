package ast

import (
	goast "go/ast"
	goparser "go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"
)

// Every exported field of an AST declaration must have a counterpart in its
// JSON mirror, or the transport silently drops it: `group`/`use`, the
// `foreach` clause and named resource pools were lost this way for months
// (#1012). The mirror uses the same field names, with the renames listed
// here; `Span` never travels.
func TestEveryASTFieldHasAJSONCounterpart(t *testing.T) {
	pairs := []struct {
		ast, json any
		renames   map[string]string // AST field → JSON field, when the names differ
	}{
		{File{}, jsonFile{}, nil},
		{GroupDecl{}, jsonGroupDecl{}, nil},
		{UseDecl{}, jsonUseDecl{}, nil},
		{WorkflowDecl{}, jsonWorkflowDecl{}, nil},
		{Edge{}, jsonEdge{}, nil},
		{ForeachClause{}, jsonForeachClause{}, nil},
		{LoopClause{}, jsonLoopClause{}, nil},
		{WhenClause{}, jsonWhenClause{}, nil},
		{WithEntry{}, jsonWithEntry{}, nil},
		{BudgetBlock{}, jsonBudgetBlock{}, nil},
		{AgentDecl{}, jsonAgentDecl{}, nil},
		{JudgeDecl{}, jsonJudgeDecl{}, nil},
		{RouterDecl{}, jsonRouterDecl{}, nil},
		{HumanDecl{}, jsonHumanDecl{}, nil},
		{ToolNodeDecl{}, jsonToolNodeDecl{}, nil},
		{ComputeDecl{}, jsonComputeDecl{}, nil},
		{ComputeExpr{}, jsonComputeExpr{}, nil},
		{SubbotDecl{}, jsonSubbotDecl{}, nil},
		{EmitDecl{}, jsonEmitDecl{}, nil},
		{WaitDecl{}, jsonWaitDecl{}, nil},
		{AwaitAnswersDecl{}, jsonAwaitAnswersDecl{}, nil},
		{FailDecl{}, jsonFailDecl{}, nil},
		{PromptDecl{}, jsonPromptDecl{}, nil},
		{SchemaDecl{}, jsonSchemaDecl{}, nil},
		{SchemaField{}, jsonSchemaField{}, nil},
		{SupervisorDecl{}, jsonSupervisorDecl{}, nil},
		{CursorDecl{}, jsonCursorDecl{}, nil},
		{CursorBlock{}, jsonCursorBlock{}, nil},
		{FallbackDecl{}, jsonFallbackDecl{}, nil},
		{MemoryBlock{}, jsonMemoryBlock{}, nil},
		{CompactionBlock{}, jsonCompactionBlock{}, nil},
		{RecoveryBlock{}, jsonRecoveryBlock{}, nil},
		{SandboxBlock{}, jsonSandboxBlock{}, nil},
		{SandboxBuildBlock{}, jsonSandboxBuildBlock{}, nil},
		{SandboxNetworkBlock{}, jsonSandboxNetworkBlock{}, nil},
		{MCPServerDecl{}, jsonMCPServerDecl{}, nil},
		{MCPAuthDecl{}, jsonMCPAuthDecl{}, nil},
		{MCPConfigDecl{}, jsonMCPConfigDecl{}, nil},
		{VarsBlock{}, jsonVarsBlock{}, nil},
		{VarField{}, jsonVarField{}, map[string]string{"EnumValues": "Enum"}},
		{SecretsBlock{}, jsonSecretsBlock{}, nil},
		{SecretField{}, jsonSecretField{}, nil},
		{PresetsBlock{}, jsonPresetsBlock{}, nil},
		{AttachmentsBlock{}, jsonAttachmentsBlock{}, nil},
		{AttachmentField{}, jsonAttachmentField{}, nil},
		{Literal{}, jsonLiteral{}, nil},
		{Comment{}, jsonComment{}, nil},
		{Preset{}, jsonPreset{}, nil},
		{PresetValue{}, jsonPresetValue{}, nil},
		{CursorEnumValue{}, jsonCursorEnumValue{}, nil},
		{CursorBand{}, jsonCursorBand{}, nil},
		{CursorSetting{}, jsonCursorSetting{}, nil},
	}
	// The list above is kept complete by construction: every exported
	// struct type declared in ast.go must appear in it, or in the explicit
	// exclusions — an AST type nobody paired is exactly the hole the sweep
	// exists to close (a mutant field on an unlisted type passed unseen).
	listed := map[string]bool{}
	for _, p := range pairs {
		listed[reflect.TypeOf(p.ast).Name()] = true
	}
	excluded := map[string]string{
		"LLMDecl":        "embedded in AgentDecl and JudgeDecl, whose mirrors are flat",
		"ResourcesBlock": "mirrored across two workflow fields (TestResourcesBlockIsFullyMirrored)",
		"Span":           "source positions never travel",
		"Pos":            "source positions never travel",
	}
	for _, name := range exportedStructTypes(t, "ast.go") {
		if !listed[name] {
			if _, ok := excluded[name]; !ok {
				t.Errorf("ast.%s has no entry in the sweep's pair list (nor an exclusion) — a field added to it can be dropped by the transport unseen", name)
			}
		}
	}
	for _, p := range pairs {
		at, jt := reflect.TypeOf(p.ast), reflect.TypeOf(p.json)
		jsonFields := map[string]bool{}
		for i := 0; i < jt.NumField(); i++ {
			jsonFields[jt.Field(i).Name] = true
		}
		for _, f := range exportedFields(at) {
			want := f.Name
			if r, ok := p.renames[f.Name]; ok {
				want = r
			}
			if !jsonFields[want] {
				t.Errorf("%s.%s has no counterpart in %s — the JSON transport drops it", at.Name(), f.Name, jt.Name())
			}
		}
	}
}

// exportedStructTypes reads the exported struct type names declared in a
// source file of this package, through go/parser — the one enumeration a
// running test cannot get from reflection.
func exportedStructTypes(t *testing.T, file string) []string {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := goparser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var names []string
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
			if _, isStruct := ts.Type.(*goast.StructType); isStruct {
				names = append(names, ts.Name.Name)
			}
		}
	}
	return names
}

// exportedFields lists a struct's exported fields, flattening embedded
// structs (AgentDecl and JudgeDecl embed LLMDecl; their mirrors are flat)
// and skipping Span.
func exportedFields(t reflect.Type) []reflect.StructField {
	var out []reflect.StructField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Name == "Span" || !f.IsExported() {
			continue
		}
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			out = append(out, exportedFields(f.Type)...)
			continue
		}
		out = append(out, f)
	}
	return out
}

// ResourcesBlock is the one declaration mirrored across two fields of the
// workflow mirror (capacities + members) rather than by a struct of its own.
func TestResourcesBlockIsFullyMirrored(t *testing.T) {
	at := reflect.TypeOf(ResourcesBlock{})
	jt := reflect.TypeOf(jsonWorkflowDecl{})
	for _, want := range []string{"Resources", "ResourceMembers"} {
		if _, ok := jt.FieldByName(want); !ok {
			t.Errorf("jsonWorkflowDecl lacks %s", want)
		}
	}
	var names []string
	for i := 0; i < at.NumField(); i++ {
		if at.Field(i).Name != "Span" {
			names = append(names, at.Field(i).Name)
		}
	}
	if got := strings.Join(names, ","); got != "Capacities,Members" {
		t.Errorf("ResourcesBlock grew a field the workflow mirror does not know: %s", got)
	}
	// Field names on both sides prove nothing about what the converters
	// actually assign: each half has to travel on its OWN, or one field's
	// survival hangs on an unrelated one and a pool disappears in silence.
	for _, tc := range []struct {
		name string
		res  *ResourcesBlock
	}{
		{"both", &ResourcesBlock{Capacities: map[string]int{"godot": 2}, Members: map[string][]string{"godot": {"s1", "s2"}}}},
		{"capacities only", &ResourcesBlock{Capacities: map[string]int{"cpu": 4}}},
		{"members only", &ResourcesBlock{Members: map[string][]string{"godot": {"s1", "s2"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := &File{Workflows: []*WorkflowDecl{{Name: "w", Resources: tc.res}}}
			b, err := MarshalFile(in)
			if err != nil {
				t.Fatal(err)
			}
			out, err := UnmarshalFile(b)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(out.Workflows[0].Resources, tc.res) {
				t.Errorf("resources did not survive: %+v became %+v\n%s", tc.res, out.Workflows[0].Resources, b)
			}
		})
	}
}
