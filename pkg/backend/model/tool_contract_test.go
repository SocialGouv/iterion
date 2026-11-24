package model

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// tcWorkflow declares three producers a tool body can read: `typed` with a
// schema, `untyped` with none, and `idle`, typed, which this run has not
// reached.
func tcWorkflow() *ir.Workflow {
	schema := &ir.Schema{Name: "typed_out", Fields: []*ir.SchemaField{
		{Name: "pr_number", Type: ir.FieldTypeInt},
		{Name: "doc", Type: ir.FieldTypeJSON},
		{Name: "tags", Type: ir.FieldTypeStringArray},
	}}
	typed := func(id string) ir.Node {
		return &ir.ToolNode{BaseNode: ir.BaseNode{ID: id}, SchemaFields: ir.SchemaFields{OutputSchema: "typed_out"}}
	}
	return &ir.Workflow{
		Name: "tc",
		Nodes: map[string]ir.Node{
			"typed":   typed("typed"),
			"idle":    typed("idle"),
			"untyped": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "untyped"}},
		},
		Schemas: map[string]*ir.Schema{"typed_out": schema},
	}
}

func tcOutputsRef(path ...string) *ir.Ref {
	return &ir.Ref{Kind: ir.RefOutputs, Path: path, Raw: "{{outputs." + strings.Join(path, ".") + "}}"}
}

// What a producer owes, case by case, and — for every case it owes — that
// both renderings agree the reference is a hole: the refusal is a subset of
// what the rendering leaves unresolved, never a second definition of it.
func TestAnOutputRefIsOwedOnlyForADeclaredFieldAProducedOutputLacks(t *testing.T) {
	t.Parallel()
	wf := tcWorkflow()
	e := NewClawExecutor(NewRegistry(), wf)
	cases := []struct {
		name   string
		output map[string]any // typed's output
		path   []string
		owed   bool
	}{
		{"a declared field the output does not carry", map[string]any{"doc": 1}, []string{"typed", "pr_number"}, true},
		{"a declared field that is null", map[string]any{"pr_number": nil}, []string{"typed", "pr_number"}, true},
		{"a declared string[] that is null", map[string]any{"tags": nil}, []string{"typed", "tags"}, true},
		{"the inside of a declared field that is absent", map[string]any{}, []string{"typed", "doc", "title"}, true},
		{"a declared field that is there", map[string]any{"pr_number": 42}, []string{"typed", "pr_number"}, false},
		{"a zero that is a value", map[string]any{"pr_number": 0}, []string{"typed", "pr_number"}, false},
		{"an empty string that is a value", map[string]any{"pr_number": ""}, []string{"typed", "pr_number"}, false},
		{"a false that is a value", map[string]any{"pr_number": false}, []string{"typed", "pr_number"}, false},
		{"an empty list that is a value", map[string]any{"tags": []any{}}, []string{"typed", "tags"}, false},
		{"a `json` field that is present and null", map[string]any{"doc": nil}, []string{"typed", "doc"}, false},
		{"the inside of a field that is there", map[string]any{"doc": map[string]any{}}, []string{"typed", "doc", "title"}, false},
		{"the whole output", map[string]any{}, []string{"typed"}, false},
		{"a field the schema does not declare", map[string]any{}, []string{"typed", "nope"}, false},
		{"a runtime-injected field", map[string]any{}, []string{"typed", "_drain_cut"}, false},
		{"a producer that has not run", map[string]any{}, []string{"idle", "pr_number"}, false},
		{"a producer without a schema", map[string]any{}, []string{"untyped", "pr_number"}, false},
		{"a node the workflow does not declare", map[string]any{}, []string{"ghost", "pr_number"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			td := &TemplateData{Nodes: wf.Nodes, Outputs: map[string]map[string]any{"typed": c.output, "untyped": {"other": 1}}}
			got, owed := e.outputRefOwed(td, c.path)
			if owed != c.owed {
				t.Fatalf("owed = %v, want %v (%+v)", owed, c.owed, got)
			}
			if !owed {
				return
			}
			want := OwedOutputRef{Ref: "outputs." + strings.Join(c.path, "."), Producer: "typed", Schema: "typed_out", Field: c.path[1]}
			if got != want {
				t.Errorf("owed = %+v, want %+v", got, want)
			}
			ref := tcOutputsRef(c.path...)
			var command, script []string
			RenderCommand("x "+ref.Raw, []*ir.Ref{ref}, nil, nil, td, "run", WorkflowShapes(wf), func(r string) { command = append(command, r) })
			if _, err := RenderScript("x = "+ref.Raw, "py", []*ir.Ref{ref}, nil, nil, td, "run", func(r string) { script = append(script, r) }); err != nil {
				t.Fatal(err)
			}
			for surface, heard := range map[string][]string{"command": command, "script": script} {
				if len(heard) != 1 || heard[0] != want.Ref {
					t.Errorf("the %s rendering does not leave %s as a hole (heard %v): the refusal would fire where the rendering resolves", surface, want.Ref, heard)
				}
			}
		})
	}
}

// The contract is the host's to state: without it on the context, nothing is
// refused, on any body.
func TestOwedOutputRefsAsksTheContextAndNamesEveryReferenceOnce(t *testing.T) {
	t.Parallel()
	wf := tcWorkflow()
	e := NewClawExecutor(NewRegistry(), wf)
	td := &TemplateData{Nodes: wf.Nodes, Outputs: map[string]map[string]any{"typed": {"doc": "CANARY-value"}}}
	refs := []*ir.Ref{
		tcOutputsRef("typed", "tags"), tcOutputsRef("typed", "pr_number"), tcOutputsRef("typed", "pr_number"),
		tcOutputsRef("typed", "doc"), {Kind: ir.RefInput, Path: []string{"pr_number"}, Raw: "{{input.pr_number}}"},
	}
	ctx := WithTemplateData(context.Background(), td)
	if err := e.owedOutputRefs(ctx, "command", refs); err != nil {
		t.Fatalf("a host that states no contract was held to one: %v", err)
	}
	if err := e.owedOutputRefs(WithToolContract(context.Background(), ToolContract{OutputRefs: true}), "command", refs); err != nil {
		t.Fatalf("no snapshot, and a reference was judged against nothing: %v", err)
	}
	err := e.owedOutputRefs(WithToolContract(ctx, ToolContract{OutputRefs: true}), "command", refs)
	var owed *UnresolvedOutputRefError
	if !errors.As(err, &owed) {
		t.Fatalf("err = %v, want an UnresolvedOutputRefError", err)
	}
	var names []string
	for _, o := range owed.Owed {
		names = append(names, o.Ref)
	}
	if got := strings.Join(names, " "); got != "outputs.typed.pr_number outputs.typed.tags" {
		t.Errorf("owed = %q, want each owed reference once, sorted", got)
	}
	var violation ToolContractViolation
	if !errors.As(err, &violation) {
		t.Error("the refusal is not a ToolContractViolation: a host could not classify it")
	}
	for _, want := range []string{"command not run", "{{outputs.typed.pr_number}}", `node "typed"`, "typed_out", EnvStrictOutputRefs + "=0",
		"iterion rewind --run-id <run> --node typed", "a resume re-executes only this node", "operator"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "CANARY-value") {
		t.Errorf("the refusal quotes the producer's output: %v", err)
	}
}

// A body that owes fields of two producers names ONE node to rewind to: the
// producer of the first reference owed, in the order the refusal lists them.
func TestTheRewindHintNamesTheProducerOfTheFirstReferenceOwed(t *testing.T) {
	t.Parallel()
	wf := tcWorkflow()
	wf.Nodes["alpha"] = &ir.ToolNode{BaseNode: ir.BaseNode{ID: "alpha"}, SchemaFields: ir.SchemaFields{OutputSchema: "typed_out"}}
	e := NewClawExecutor(NewRegistry(), wf)
	td := &TemplateData{Nodes: wf.Nodes, Outputs: map[string]map[string]any{"typed": {"doc": 1}, "alpha": {"doc": 1}}}
	ctx := WithToolContract(WithTemplateData(context.Background(), td), ToolContract{OutputRefs: true})
	// Written last-producer-first: the refusal sorts what it lists.
	for _, refs := range [][]*ir.Ref{
		{tcOutputsRef("typed", "pr_number"), tcOutputsRef("alpha", "pr_number")},
		{tcOutputsRef("alpha", "pr_number"), tcOutputsRef("typed", "pr_number")},
	} {
		err := e.owedOutputRefs(ctx, "command", refs)
		var owed *UnresolvedOutputRefError
		if !errors.As(err, &owed) || len(owed.Owed) != 2 || owed.Owed[0].Producer != "alpha" {
			t.Fatalf("err = %v, want both references owed, alpha's first", err)
		}
		if msg := err.Error(); !strings.Contains(msg, "iterion rewind --run-id <run> --node alpha") || strings.Contains(msg, "--node typed") {
			t.Errorf("the hint does not name the first producer owed, and only it: %s", msg)
		}
	}
}

// An action's `params:` is the fourth body: refused before any argument is
// rendered, with the same error, and rendered as before when the host states
// no contract.
func TestAnActionParamReadingAnOwedFieldIsRefusedBeforeItIsRendered(t *testing.T) {
	t.Parallel()
	wf := tcWorkflow()
	ref := tcOutputsRef("typed", "pr_number")
	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "comment"}, Action: "forge.issue.comment", Params: []ir.ActionParam{
		{Key: "index", Value: ref.Raw, Refs: []*ir.Ref{ref}},
	}}
	e := NewClawExecutor(NewRegistry(), wf)
	td := &TemplateData{Nodes: wf.Nodes, Outputs: map[string]map[string]any{"typed": {"doc": 1}}}
	ctx := WithTemplateData(context.Background(), td)

	params, err := e.renderActionParams(ctx, node, spec.Operation{}, nil, secretsAsPlaceholders)
	if err != nil {
		t.Fatalf("no contract stated, and the parameter was refused: %v", err)
	}
	if _, present := params["index"]; !present {
		t.Errorf("params = %#v, want the parameter rendered", params)
	}

	_, err = e.renderActionParams(WithToolContract(ctx, ToolContract{OutputRefs: true}), node, spec.Operation{}, nil, secretsAsPlaceholders)
	var owed *UnresolvedOutputRefError
	if !errors.As(err, &owed) || owed.Surface != "params" || len(owed.Owed) != 1 || owed.Owed[0].Ref != "outputs.typed.pr_number" {
		t.Fatalf("err = %v, want the parameter refused for outputs.typed.pr_number", err)
	}
}

// A recipe's stdout, judged: only a declared `output:`, non-empty bytes that
// are not a JSON object, and a host that states the contract fail it.
func TestStdoutContractJudgesOnlyNonEmptyNonObjectStdoutUnderADeclaredOutput(t *testing.T) {
	t.Parallel()
	on := WithToolContract(context.Background(), ToolContract{JSONStdout: true})
	declared := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "n"}, SchemaFields: ir.SchemaFields{OutputSchema: "typed_out"}}
	bare := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "n"}}
	cases := []struct {
		name   string
		ctx    context.Context
		node   *ir.ToolNode
		stdout string
		fails  bool
	}{
		{"text beside the object", on, declared, "[main abc] chore\n{\"pr_number\": 1}", true},
		{"plain text", on, declared, "done\n", true},
		{"a JSON array", on, declared, "[1, 2]", true},
		{"a JSON null", on, declared, "null", true},
		{"two objects", on, declared, "{\"a\": 1}\n{\"b\": 2}", true},
		{"one object", on, declared, " {\"pr_number\": 1}\n", false},
		{"an empty object", on, declared, "{}", false},
		{"nothing", on, declared, "", false},
		{"whitespace alone", on, declared, " \n", false},
		{"text without a declared output", on, bare, "done\n", false},
		{"text, and a host that states no contract", context.Background(), declared, "done\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			output, wrapped := parseToolNodeOutputWrapped(c.stdout, strings.TrimSpace(c.stdout))
			answer, err := declaredAnswer(c.ctx, c.node, recipeResult{output: output, stdout: c.stdout, notObject: wrapped || output == nil})
			if (err != nil) != c.fails {
				t.Fatalf("err = %v, want a failure: %v", err, c.fails)
			}
			if err == nil {
				if !reflect.DeepEqual(answer, output) {
					t.Errorf("answer = %v, want the output as parsed: %v", answer, output)
				}
				return
			}
			var refused *ToolStdoutNotJSONError
			if !errors.As(err, &refused) || refused.Schema != "typed_out" || refused.Bytes != len(c.stdout) {
				t.Errorf("err = %#v, want the schema and the size", err)
			}
			if body := strings.TrimSpace(c.stdout); strings.Contains(err.Error(), body) {
				t.Errorf("the refusal quotes the stdout: %v", err)
			}
		})
	}
	// A drain the bound cut short travels into the refusal: those bytes may
	// be a prefix of the answer, and the message is the one place that says
	// so once the wrapper and its `_drain_cut` mark are not published.
	_, err := declaredAnswer(on, declared, recipeResult{
		output: map[string]any{"result": "{\"pr_nu", toolNodeDrainCutKey: true}, stdout: "{\"pr_nu", notObject: true, drainCut: true,
	})
	var cut *ToolStdoutNotJSONError
	if !errors.As(err, &cut) || !cut.DrainCut || !strings.Contains(err.Error(), "cut short") {
		t.Errorf("a cut drain is not carried into the refusal: %v", err)
	}
	if _, err := declaredAnswer(on, declared, recipeResult{output: map[string]any{"result": "x"}, stdout: "x", notObject: true}); err == nil || strings.Contains(err.Error(), "cut short") {
		t.Errorf("a whole stdout is reported as cut: %v", err)
	}
	// Both dials, and whose they are: the first alone does not bring the
	// run of before back.
	for _, want := range []string{EnvStrictOutputValidation + "=0", EnvStrictOutputRefs + "=0", "operator", "no bot variable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
}

// The lines the engine consumes are not part of what a node answers: with
// them read out, a declared node that printed its object answers with the
// object, and one that printed directives alone answered nothing.
func TestDeclaredAnswerReadsTheEnginesDirectivesOutOfIt(t *testing.T) {
	t.Parallel()
	on := WithToolContract(context.Background(), ToolContract{JSONStdout: true})
	declared := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "n"}, SchemaFields: ir.SchemaFields{OutputSchema: "typed_out"}}
	object := `{"pr_number": 1}`
	want := map[string]any{"pr_number": float64(1)}
	for _, directive := range toolOutputDirectives {
		line := directive + "out/report.md name=report"
		for name, stdout := range map[string]string{
			"before the object":      line + "\n" + object + "\n",
			"after the object":       object + "\n" + line + "\n",
			"around the object":      line + "\n" + object + "\n  " + line + "  \n",
			"with a carriage return": line + "\r\n" + object + "\r\n",
			"indented, before":       "   " + line + "\n" + object,
		} {
			t.Run(directive+" "+name, func(t *testing.T) {
				raw, wrapped := parseToolNodeOutputWrapped(stdout, strings.TrimSpace(stdout))
				if !wrapped {
					t.Fatal("the fixture parses as it stands: it proves nothing")
				}
				answer, err := declaredAnswer(on, declared, recipeResult{output: raw, stdout: stdout, notObject: true})
				if err != nil || !reflect.DeepEqual(answer, want) {
					t.Errorf("answer = %v, err = %v, want the object", answer, err)
				}
				// The host that states no contract keeps the wrapper of the
				// stdout as printed.
				if answer, err := declaredAnswer(context.Background(), declared, recipeResult{output: raw, stdout: stdout, notObject: true}); err != nil || !reflect.DeepEqual(answer, raw) {
					t.Errorf("no contract stated: answer = %v, err = %v, want the wrapper", answer, err)
				}
			})
		}
		t.Run(directive+" alone", func(t *testing.T) {
			raw, _ := parseToolNodeOutputWrapped(line+"\n", line)
			answer, err := declaredAnswer(on, declared, recipeResult{output: raw, stdout: line + "\n", notObject: true})
			if err != nil || !reflect.DeepEqual(answer, raw) {
				t.Errorf("answer = %v, err = %v, want the wrapper of an answer that is empty", answer, err)
			}
		})
		t.Run(directive+" beside text", func(t *testing.T) {
			stdout := line + "\ndone\n"
			raw, _ := parseToolNodeOutputWrapped(stdout, strings.TrimSpace(stdout))
			if _, err := declaredAnswer(on, declared, recipeResult{output: raw, stdout: stdout, notObject: true}); err == nil {
				t.Error("text beside a directive passed as an answer")
			}
		})
		// Read out, what is left is judged like any stdout: valid JSON that
		// is not an object is still not the answer.
		for name, rest := range map[string]string{"a JSON null": "null", "a JSON array": `[{"pr_number": 1}]`} {
			t.Run(directive+" beside "+name, func(t *testing.T) {
				stdout := line + "\n" + rest + "\n"
				raw, _ := parseToolNodeOutputWrapped(stdout, strings.TrimSpace(stdout))
				answer, err := declaredAnswer(on, declared, recipeResult{output: raw, stdout: stdout, notObject: true})
				var refused *ToolStdoutNotJSONError
				if !errors.As(err, &refused) {
					t.Errorf("answer = %v, err = %v, want the node refused", answer, err)
				}
			})
		}
		// A directive with nothing after its `=` is not one the engine
		// consumes, so it is not read out either.
		t.Run(directive+" with no value", func(t *testing.T) {
			stdout := directive + "\n" + object
			raw, _ := parseToolNodeOutputWrapped(stdout, strings.TrimSpace(stdout))
			if _, err := declaredAnswer(on, declared, recipeResult{output: raw, stdout: stdout, notObject: true}); err == nil {
				t.Error("a line the engine does not consume was read out of the answer")
			}
		})
	}
}

// Every directive a scanner of this package asks for is one the answer is
// read without: a fourth directive cannot be consumed by the engine and
// still fail the node that printed it.
func TestEveryDirectiveTheEngineScansIsOutOfTheAnswer(t *testing.T) {
	t.Parallel()
	listed := map[string]bool{}
	for _, d := range toolOutputDirectives {
		listed[d] = true
	}
	consts := map[string]string{}
	var asked []string
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if perr != nil {
			t.Errorf("%s could not be parsed: %v", name, perr)
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ValueSpec:
				for i, id := range x.Names {
					if i < len(x.Values) {
						if lit, ok := x.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							consts[id.Name] = strings.Trim(lit.Value, "\"`")
						}
					}
				}
			case *ast.CallExpr:
				if fn, ok := x.Fun.(*ast.Ident); ok && fn.Name == "scanDirectiveLines" && len(x.Args) == 2 {
					id, isIdent := x.Args[1].(*ast.Ident)
					if !isIdent {
						t.Errorf("%s: scanDirectiveLines is asked for a directive that is not a named constant", name)
						return true
					}
					asked = append(asked, id.Name)
				}
			}
			return true
		})
	}
	if len(asked) < len(toolOutputDirectives) {
		t.Fatalf("found %d scanDirectiveLines call(s) for %d listed directives — the walk lost them", len(asked), len(toolOutputDirectives))
	}
	for _, name := range asked {
		value, known := consts[name]
		if !known || !listed[value] {
			t.Errorf("scanDirectiveLines is asked for %s (%q), which toolOutputDirectives does not list: the engine would consume that line and still refuse the node that printed it beside its JSON", name, value)
		}
	}
}

// Who can owe a field: a tool node, whichever recipe or rung answered — and
// no other kind of producer, each of which may leave a declared field out by
// design or is validated where it answers.
func TestOnlyAToolProducerOwesADeclaredField(t *testing.T) {
	t.Parallel()
	schema := &ir.Schema{Name: "out", Fields: []*ir.SchemaField{{Name: "note", Type: ir.FieldTypeString}}}
	fields := ir.SchemaFields{OutputSchema: "out"}
	producers := []struct {
		kind string
		node ir.Node
		owes bool
	}{
		{"a tool node's command", &ir.ToolNode{BaseNode: ir.BaseNode{ID: "p"}, SchemaFields: fields, Command: "printf x"}, true},
		{"a tool node's script", &ir.ToolNode{BaseNode: ir.BaseNode{ID: "p"}, SchemaFields: fields, Script: "x", Language: "py"}, true},
		{"a Verified Action, whichever rung answered", &ir.ToolNode{BaseNode: ir.BaseNode{ID: "p"}, SchemaFields: fields, Command: "printf x", Postcondition: "true"}, true},
		{"a tool node's action", &ir.ToolNode{BaseNode: ir.BaseNode{ID: "p"}, SchemaFields: fields, Action: "forge.issue.get"}, true},
		{"a human gate answered in part", &ir.HumanNode{BaseNode: ir.BaseNode{ID: "p"}, SchemaFields: fields}, false},
		{"a compute node whose expression is an absence", &ir.ComputeNode{BaseNode: ir.BaseNode{ID: "p"}, SchemaFields: fields}, false},
		{"a subbot, answering for another workflow", &ir.SubbotNode{BaseNode: ir.BaseNode{ID: "p"}, OutputSchema: "out"}, false},
		{"an agent, validated where it answers", &ir.AgentNode{BaseNode: ir.BaseNode{ID: "p"}, SchemaFields: fields}, false},
		{"a judge, validated where it answers", &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "p"}, SchemaFields: fields}, false},
		{"a router, whose output is its input and a fan-out item", &ir.RouterNode{BaseNode: ir.BaseNode{ID: "p"}}, false},
		{"a wait node", &ir.WaitNode{BaseNode: ir.BaseNode{ID: "p"}, SchemaFields: fields}, false},
	}
	for _, c := range producers {
		t.Run(c.kind, func(t *testing.T) {
			wf := &ir.Workflow{Name: "kinds", Nodes: map[string]ir.Node{"p": c.node}, Schemas: map[string]*ir.Schema{"out": schema}}
			if got := ir.NodeOutputSchema(c.node); got != "out" && c.owes {
				t.Fatalf("the fixture declares no schema (%q): it proves nothing", got)
			}
			e := NewClawExecutor(NewRegistry(), wf)
			td := &TemplateData{Nodes: wf.Nodes, Outputs: map[string]map[string]any{"p": {"other": true}}}
			if _, owed := e.outputRefOwed(td, []string{"p", "note"}); owed != c.owes {
				t.Errorf("owed = %v, want %v", owed, c.owes)
			}
		})
	}
}

// toolBodyRenderers are the functions through which a tool node's body — or
// a preview of it — reaches the shared rendering core.
var toolBodyRenderers = map[string]bool{
	"resolveTemplateWith": true, "renderCommand": true, "renderScript": true, "renderScriptIn": true,
	"renderShellScript": true, "renderCommandBody": true, "renderScriptBody": true, "renderScriptBodyIn": true,
	"resolveCommandTemplate": true, "resolveScriptTemplate": true,
}

// asksOwedOutputRefs marks an inventory entry that must itself call
// owedOutputRefs; any other value is the reason the entry does not.
const asksOwedOutputRefs = "asks"

// toolBodyRenderSites is the INVENTORY of every function of this package that
// renders a tool body, keyed by file:function: the ones that render a body a
// RUN is about to execute ask owedOutputRefs before it runs, and every other
// one says why it does not. A fifth body cannot start rendering
// `{{outputs.*}}` without stating which it is.
var toolBodyRenderSites = map[string]string{
	"executor_tool.go:shellRecipe":                  asksOwedOutputRefs,
	"executor_tool.go:scriptRecipe":                 asksOwedOutputRefs,
	"executor_tool_action.go:renderActionParams":    asksOwedOutputRefs,
	"executor_verified_action.go:postconditionBody": "renders the postcondition for its one caller, runPostcondition, which asks before the check runs (toolBodyRenderedFor)",

	"template_resolver.go:RenderCommand": "a preview for the dry run: it executes nothing, and reports every hole to its listener",
	"template_resolver.go:RenderScript":  "a preview for the dry run: it executes nothing, and reports every hole to its listener",

	"executor_tool.go:resolveCommandTemplate": "a rendering primitive: the shown form of renderCommandBody",
	"executor_tool.go:resolveScriptTemplate":  "a rendering primitive: the shown form of renderScriptBody",
	"executor_tool.go:renderCommandBody":      "a rendering primitive, called by the sites above",
	"executor_tool.go:renderScriptBody":       "a rendering primitive, called by the sites above",
	"executor_tool.go:renderScriptBodyIn":     "a rendering primitive, called by the sites above",
	"executor_tool.go:renderCommand":          "a rendering primitive, called by the sites above",
	"executor_tool.go:renderScript":           "a rendering primitive, called by the sites above",
	"script_shell_data.go:renderScriptIn":     "a rendering primitive, called by the sites above",
	"script_shell_data.go:renderShellScript":  "a rendering primitive, called by the sites above",
}

// toolBodyRenderedFor names the inventory entries that render a body for
// ANOTHER function to run: every caller of the entry asks in its place.
var toolBodyRenderedFor = map[string]bool{"postconditionBody": true}

func TestEveryRunBodyThatRendersOutputsAsksWhatItsProducersOwe(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	calls := func(body ast.Node, names map[string]bool) bool {
		found := false
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				found = found || names[fn.Name]
			case *ast.SelectorExpr:
				found = found || names[fn.Sel.Name]
			}
			return !found
		})
		return found
	}
	seen := map[string]bool{}
	files, handedOver := 0, 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		f, perr := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if perr != nil {
			t.Errorf("%s could not be parsed, so it was NOT inventoried: %v", name, perr)
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if calls(fn.Body, toolBodyRenderedFor) {
				handedOver++
				if !calls(fn.Body, map[string]bool{"owedOutputRefs": true}) {
					t.Errorf("%s:%s runs a body another function rendered for it and does not call owedOutputRefs", name, fn.Name.Name)
				}
			}
			if !calls(fn.Body, toolBodyRenderers) {
				continue
			}
			key := name + ":" + fn.Name.Name
			seen[key] = true
			verdict, listed := toolBodyRenderSites[key]
			switch {
			case !listed:
				t.Errorf("%s renders a tool body and is not in toolBodyRenderSites: if a run executes what it renders, it asks owedOutputRefs before the body runs; otherwise list it with the reason it does not", key)
			case verdict == asksOwedOutputRefs && !calls(fn.Body, map[string]bool{"owedOutputRefs": true}):
				t.Errorf("%s is listed as asking owedOutputRefs and does not call it: a body it renders runs on an output field its producer did not supply", key)
			}
		}
	}
	var stale []string
	for key := range toolBodyRenderSites {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		t.Errorf("toolBodyRenderSites lists %s, which renders no tool body any more — drop it", key)
	}
	if handedOver == 0 {
		t.Error("no function runs a body rendered for it: toolBodyRenderedFor names nothing the package still calls")
	}
	if files < 50 {
		t.Fatalf("read %d production files of this package — the walk lost the package", files)
	}
}
