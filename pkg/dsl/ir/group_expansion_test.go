package ir

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func TestGroupReferenceRewritingPreservesCallerText(t *testing.T) {
	x := &groupExpansion{internal: map[string]bool{"gate": true, "deep.member": true}, prefix: "r1", binds: map[string]string{"fn": "concat", "op": "+", "n": "2", "field": "ok", "value": "outputs.gate", "template": "{{outputs.gate.ok}}"}}
	for _, tc := range []struct{ src, want string }{
		{`outputs . gate.ok && outputs.gatekeeper.ok`, `outputs.r1 . gate.ok && outputs.gatekeeper.ok`},
		{`concat('outputs.gate', outputs.gate.name)`, `concat('outputs.gate', outputs.r1.gate.name)`},
		{`map(outputs.gate.items, gate => gate.value)`, `map(outputs.r1.gate.items, gate => gate.value)`},
		{`outputs.gate.n + {{params.n}}`, `outputs.r1.gate.n + 2`},
		{`{{params.fn}}(outputs.gate.n)`, `concat(outputs.r1.gate.n)`},
		{`1 {{params.op}} outputs.gate.n`, `1 + outputs.r1.gate.n`},
		{`other.outputs.gate + outputs.gate.n`, `other.outputs.gate + outputs.r1.gate.n`},
		{`outputs.gate.{{params.field}}`, `outputs.r1.gate.ok`},
		{`concat('{{params.value}}', outputs.deep.member.name)`, `concat('outputs.gate', outputs.r1.deep.member.name)`},
	} {
		if got := substParams(x.expression(tc.src), x.binds); got != tc.want {
			t.Errorf("%s => %s, want %s", tc.src, got, tc.want)
		}
	}
	src := `outputs.gate {{ outputs.gate.ok }} {{!outputs.gate.ok}} {{outputs.gatekeeper.ok}} {{artifacts.gate.ok}} {{params.template}}`
	want := `outputs.gate {{ outputs.r1.gate.ok }} {{!outputs.r1.gate.ok}} {{outputs.gatekeeper.ok}} {{artifacts.gate.ok}} {{outputs.gate.ok}}`
	if got := substParams(x.templates(src), x.binds); got != want {
		t.Fatalf("templates = %s, want %s", got, want)
	}
}

// The loop-cap string supports the existing template form and the expression
// form added by #1145. Both must bind local members before caller parameters
// are inserted; strings inside an expression remain literal.
func TestGroupLoopCapReferenceRewriting(t *testing.T) {
	x := &groupExpansion{internal: map[string]bool{"gate": true}, prefix: "r1", binds: map[string]string{"cap": "outputs.gate.n"}}
	for _, tc := range []struct{ src, want string }{
		{`{{outputs.gate.n}}`, `{{outputs.r1.gate.n}}`},
		{`outputs.gate.n - 1`, `outputs.r1.gate.n - 1`},
		{`{{params.cap}} + outputs.gate.n`, `outputs.gate.n + outputs.r1.gate.n`},
		{`if('outputs.gate' == '{{outputs.gate.n}}', outputs.gate.n, 1)`, `if('outputs.gate' == '{{outputs.gate.n}}', outputs.r1.gate.n, 1)`},
	} {
		original := &ast.LoopClause{MaxIterationsExpr: tc.src}
		cloned := x.clone(reflect.ValueOf(original), "", "").Interface().(*ast.LoopClause)
		if cloned.MaxIterationsExpr != tc.want || original.MaxIterationsExpr != tc.src {
			t.Errorf("cap %q became %q, want %q; source=%q", tc.src, cloned.MaxIterationsExpr, tc.want, original.MaxIterationsExpr)
		}
	}
}

func TestGroupExpansionOwnsNestedFieldsAndEveryEdgeClause(t *testing.T) {
	span := ast.Span{Start: ast.Pos{File: "{{params.label}}.bot", Line: 3, Column: 2}}
	g := &ast.GroupDecl{Name: "g", Params: []string{"label"}, Agents: []*ast.AgentDecl{{Name: "a", LLMDecl: ast.LLMDecl{
		Description: "{{params.label}}", Images: []string{"{{params.label}}.png"},
		Fallbacks: []*ast.FallbackDecl{{Model: "{{params.label}}", On: []string{"{{params.label}}"}, When: "outputs.t.ok"}},
		Sandbox:   &ast.SandboxBlock{Build: &ast.SandboxBuildBlock{Args: map[string]string{"name": "{{params.label}}"}}},
	}, Span: span}}, Tools: []*ast.ToolNodeDecl{{Name: "t", Description: "{{params.label}}", Params: []ast.ActionParam{{Key: "name", Value: "{{params.label}}"}}}},
		Edges: []*ast.Edge{{From: "t", To: "a", IsElse: true, With: []*ast.WithEntry{{Key: "value", Value: "{{outputs.t.ok}} {{params.label}}"}}},
			{From: "a", To: "t", Foreach: &ast.ForeachClause{Name: "scan", Item: "item", Collection: "{{outputs.t.items}}"}}}}
	f := &ast.File{Groups: []*ast.GroupDecl{g}, Workflows: []*ast.WorkflowDecl{{Name: "w"}}, Uses: []*ast.UseDecl{
		{Group: "g", Prefix: "r1", With: []*ast.WithEntry{{Key: "label", Value: "A"}}},
		{Group: "g", Prefix: "r2", With: []*ast.WithEntry{{Key: "label", Value: "B"}}},
	}}
	before, err := ast.MarshalFile(f)
	if err != nil {
		t.Fatal(err)
	}
	c := &compiler{file: detachForCompile(f)}
	c.expandGroups()
	a, b := c.file.Agents[0], c.file.Agents[1]
	if a.Description != "A" || b.Description != "B" || a.Images[0] != "A.png" || b.Fallbacks[0].Model != "B" || a.Fallbacks[0].When != "outputs.r1.t.ok" || b.Sandbox.Build.Args["name"] != "B" {
		t.Fatalf("instances not bound: %+v / %+v", a, b)
	}
	if a.Span != span || b.Span != span {
		t.Fatal("source span was substituted")
	}
	edges := c.file.Workflows[0].Edges
	if !edges[0].IsElse || edges[0].With[0].Value != "{{outputs.r1.t.ok}} A" || edges[1].Foreach.Collection != "{{outputs.r1.t.items}}" || edges[3].Foreach.Collection != "{{outputs.r2.t.items}}" {
		t.Fatalf("edge clauses lost: %+v", edges)
	}
	a.Images[0] = "changed"
	a.Fallbacks[0].On[0] = "changed"
	a.Sandbox.Build.Args["name"] = "changed"
	edges[0].With[0].Value = "changed"
	if b.Images[0] != "B.png" || b.Fallbacks[0].On[0] != "B" || b.Sandbox.Build.Args["name"] != "B" {
		t.Fatal("instances alias nested data")
	}
	after, err := ast.MarshalFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("group expansion mutated the source AST")
	}
}

const sharedGroupPrompts = `schema out:
  value: string
prompt shared:
  {{outputs.gate.value}}
compute gate:
  output: out
  expr:
    value: "'external'"
agent outside:
  model: "test-model"
  system: shared
  output: out
group g(label):
  compute gate:
    output: out
    expr:
      value: "'{{params.label}}'"
  agent work:
    model: "test-model"
    system: "{{params.label}} {{outputs.gate.value}}"
    user: shared
    output: out
  gate -> work
use g as r1 with {label: "A"}
use g as r2 with {label: "B"}
workflow w:
  entry: gate
  gate -> outside
  outside -> r1.gate
  r1.work -> r2.gate
  r2.work -> done
`

func TestGroupPromptsAreSpecializedWithoutChangingSharedSources(t *testing.T) {
	f := parseFile(t, sharedGroupPrompts)
	before, _ := ast.MarshalFile(f)
	first := Compile(f)
	for _, d := range first.Diagnostics {
		if d.Severity == SeverityError {
			t.Fatal(d.Error())
		}
	}
	w := first.Workflow
	for prefix, label := range map[string]string{"r1": "A", "r2": "B"} {
		a := w.Nodes[prefix+".work"].(*AgentNode)
		if got := w.Prompts[a.SystemPrompt].Body; got != label+" {{outputs."+prefix+".gate.value}}" {
			t.Errorf("system = %s", got)
		}
		if got := w.Prompts[a.UserPrompt].Body; got != "{{outputs."+prefix+".gate.value}}" {
			t.Errorf("user = %s", got)
		}
	}
	if w.Prompts["shared"].Body != "{{outputs.gate.value}}" {
		t.Fatal("shared top-level prompt changed")
	}
	after, _ := ast.MarshalFile(f)
	if !bytes.Equal(before, after) {
		t.Fatal("source changed")
	}
	if !reflect.DeepEqual(first, Compile(f)) {
		t.Fatal("repeat compile differs")
	}
	// A concrete consumer of a parameterized prompt still gets a diagnostic.
	bad := strings.Replace(sharedGroupPrompts, "{{outputs.gate.value}}\ncompute gate:", "{{params.label}}\ncompute gate:", 1)
	if !hasDiag(compileFile(t, bad).Diagnostics, DiagBadTemplateRef) {
		t.Fatal("concrete unbound params were silently accepted")
	}
}

func TestGroupReferenceValidationKeepsFieldAndTypeDiagnostics(t *testing.T) {
	src := `schema out:
  n: int
group g():
  compute seed:
    output: out
    expr:
      n: "1"
  compute calc:
    output: out
    expr:
      n: "outputs.seed.n"
  seed -> calc
use g as r1
workflow w:
  entry: r1.seed
  r1.calc -> done
`
	if r := compileFile(t, src); r.HasErrors() {
		t.Fatal(r.Diagnostics)
	}
	if r := compileFile(t, strings.Replace(src, "outputs.seed.n", "outputs.seed.missing", 1)); !hasDiag(r.Diagnostics, DiagRefFieldNotInSchema) {
		t.Fatalf("missing member field not caught: %+v", r.Diagnostics)
	}
	if r := compileFile(t, strings.Replace(src, "outputs.seed.n", "if(outputs.seed.n == 'x', 1, 0)", 1)); !hasDiag(r.Diagnostics, DiagExprOperandTypeMismatch) {
		t.Fatalf("member field type not checked: %+v", r.Diagnostics)
	}
}

func TestGroupMemberNamedHistoryIsNotHistoryAccess(t *testing.T) {
	src := `schema out:
  value: string
group g():
  compute history:
    output: out
    expr:
      value: "'ok'"
use g as r1
agent collect:
  model: "test-model"
  output: out
workflow w:
  entry: r1.history
  r1.history -> collect with { value: "{{outputs.r1.history}}" }
  collect -> done
`
	if result := compileFile(t, src); result.HasErrors() {
		t.Fatal(result.Diagnostics)
	}
	bad := strings.Replace(src, "{{outputs.r1.history}}", "{{outputs.r1.history.history}}", 1)
	if result := compileFile(t, bad); !hasDiag(result.Diagnostics, DiagHistoryRefNotInLoop) {
		t.Fatalf("actual history access lost its diagnostic: %v", result.Diagnostics)
	}
}

func TestGroupPromptIncludesAreSpecializedBeforeValidation(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"sub/outer.md": `{{include "inner.md"}}`,
		"sub/inner.md": `{{params.label}} {{outputs.gate.value}}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	src := strings.Replace(sharedGroupPrompts, "prompt shared:\n  {{outputs.gate.value}}", "prompt shared:\n  {{include \"sub/outer.md\"}}", 1)
	src = strings.Replace(src, "system: shared", `system: "external"`, 1)
	src = strings.Replace(src, `system: "{{params.label}} {{outputs.gate.value}}"`, "system: `prefix {{include \"sub/outer.md\"}}`", 1)
	path := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed := parser.Parse(path, src)
	for _, d := range parsed.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatal(d)
		}
	}
	before, err := ast.MarshalFile(parsed.File)
	if err != nil {
		t.Fatal(err)
	}
	result := Compile(parsed.File)
	if result.HasErrors() {
		t.Fatal(result.Diagnostics)
	}
	for prefix, label := range map[string]string{"r1": "A", "r2": "B"} {
		n := result.Workflow.Nodes[prefix+".work"].(*AgentNode)
		want := label + " {{outputs." + prefix + ".gate.value}}"
		if body := result.Workflow.Prompts[n.UserPrompt].Body; body != want {
			t.Errorf("user=%q, want %q", body, want)
		}
		if body := result.Workflow.Prompts[n.SystemPrompt].Body; body != "prefix "+want {
			t.Errorf("system=%q, want %q", body, "prefix "+want)
		}
	}
	after, err := ast.MarshalFile(parsed.File)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !reflect.DeepEqual(result, Compile(parsed.File)) {
		t.Fatal("include specialization changed the source or repeat compilation")
	}
}

// TestAnUnresolvableIncludeIsRefusedOncePerDeclaration.
//
// A prompt whose source cannot be resolved — a transported prompt with no
// file, or a relative path — refuses its {{include}} for a reason that
// belongs to the DECLARATION and cannot differ between the group instances
// binding it. The budget refusals beside it already stop at the first
// (`blown` is sticky: "thousands more would name nothing new"); this one
// must too, or a group of N members lands N identical diagnostics on one
// span and buries whatever else the compile found.
func TestAnUnresolvableIncludeIsRefusedOncePerDeclaration(t *testing.T) {
	const instances = 5
	var src strings.Builder
	src.WriteString("prompt p:\n  {{include \"rules.md\"}} {{params.label}}\ngroup g(label):\n  agent a:\n    model: \"test-model\"\n    system: p\n")
	for i := 0; i < instances; i++ {
		fmt.Fprintf(&src, "use g as g%d with {label: \"%d\"}\n", i, i)
	}
	src.WriteString("workflow w:\n  entry: g0.a\n  g0.a -> done\n")
	// A RELATIVE source path: no include resolves beside it.
	parsed := parser.Parse("t.bot", src.String())
	for _, d := range parsed.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("parse error: %s", d.Error())
		}
	}
	refusals := 0
	for _, d := range Compile(parsed.File).Diagnostics {
		if d.Code == DiagBadPromptInclude {
			refusals++
		}
	}
	if refusals != 1 {
		t.Fatalf("DiagBadPromptInclude × %d, want 1 — one declaration whose source cannot resolve, bound by %d instances", refusals, instances)
	}
}

func TestGroupPromptIncludesShareTheFileBudget(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rules.md"), []byte("label {{params.label}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var src strings.Builder
	src.WriteString("prompt p:\n  {{include \"rules.md\"}}\ngroup g(label):\n  agent a:\n    model: \"test-model\"\n    system: p\n")
	for i := 0; i <= maxPromptIncludeExpansions; i++ {
		fmt.Fprintf(&src, "use g as g%d with {label: \"%d\"}\n", i, i)
	}
	src.WriteString("workflow w:\n  entry: g0.a\n  g0.a -> done\n")
	result := compileAt(t, filepath.Join(dir, "main.bot"), src.String())
	if !hasDiag(result.Diagnostics, DiagBadPromptInclude) {
		t.Fatal("group copies bypassed the file include budget")
	}
}
