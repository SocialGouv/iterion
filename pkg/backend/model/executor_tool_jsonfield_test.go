package model

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func jsonFieldExecutor(fields ...*ir.SchemaField) *ClawExecutor {
	return &ClawExecutor{schemas: map[string]*ir.Schema{
		"gate_in": {Name: "gate_in", Fields: fields},
	}}
}

func listVarExecutor(vars map[string]*ir.Var, fields ...*ir.SchemaField) *ClawExecutor {
	e := jsonFieldExecutor(fields...)
	e.wfShapes = WorkflowShapes(&ir.Workflow{Vars: vars})
	return e
}

func jsonFieldNode() *ir.ToolNode {
	return &ir.ToolNode{SchemaFields: ir.SchemaFields{InputSchema: "gate_in"}}
}

// ---------------------------------------------------------------------------
// The two declared renderings (#1320)
// ---------------------------------------------------------------------------

// The regression: a `json` field holding an all-string list reached
// shellEscapeValue's scalar-slice arm and space-joined, so
// `KEY={{input.x}} cmd` assigned only the first word and ran the second
// as a command (exit 127).
func TestShapeJSON_StringListIsOneToken(t *testing.T) {
	got := shellEscapeValue([]any{"Go", "Fail-open partout: comma"}, ShapeJSON)
	if want := `'["Go","Fail-open partout: comma"]'`; got != want {
		t.Errorf("json rendering = %s, want %s", got, want)
	}
}

// A `string[]` field keeps one shell word per element — `git add --
// {{input.files}}` depends on it, and an agent's output arrives as []any
// whatever the declaration, so only the declared type separates the two.
func TestShapeWords_ListIsOneWordPerElement(t *testing.T) {
	got := shellEscapeValue([]any{"a.go", "b c.go"}, ShapeWords)
	if want := `'a.go' 'b c.go'`; got != want {
		t.Errorf("string[] rendering = %s, want %s", got, want)
	}
}

// #1320: the empty list. `json` keeps its token so the command's arity
// does not move; `string[]` renders nothing, which is what an empty argv
// list means and what the docs now promise.
func TestEmptyList_JSONKeepsItsToken_WordsRenderNone(t *testing.T) {
	if got := shellEscapeValue([]any{}, ShapeJSON); got != `'[]'` {
		t.Errorf("empty json = %s, want '[]'", got)
	}
	if got := shellEscapeValue([]string{}, ShapeJSON); got != `'[]'` {
		t.Errorf("empty []string json = %s, want '[]'", got)
	}
	if got := shellEscapeValue([]any{}, ShapeWords); got != "" {
		t.Errorf("empty string[] = %q, want no token at all", got)
	}
}

// A `json` value that is not JSON stays the author's own text: the type
// says "document", the value says "string", and adding quotes it never
// carried would change what the program reads.
func TestShapeJSON_TextStaysText(t *testing.T) {
	if got := shellEscapeValue("not json at all", ShapeJSON); got != `'not json at all'` {
		t.Errorf("json text = %s, want the text as one token", got)
	}
	if got := shellEscapeValue(map[string]any{"k": "v"}, ShapeJSON); got != `'{"k":"v"}'` {
		t.Errorf("json object = %s, want a single quoted token", got)
	}
}

// Undeclared sites keep the shape heuristic, unchanged: an `outputs.*`
// field, a key an edge delivers to a node with no `input:` schema.
func TestShapeUndeclared_KeepsTheHeuristic(t *testing.T) {
	if got := shellEscapeValue([]any{"a", "b"}, ShapeUndeclared); got != `'a' 'b'` {
		t.Errorf("undeclared scalar slice = %s, want the space-join", got)
	}
	if got := shellEscapeValue([]any{map[string]any{"k": "v"}}, ShapeUndeclared); got != `'[{"k":"v"}]'` {
		t.Errorf("undeclared complex slice = %s, want one JSON token", got)
	}
}

// ---------------------------------------------------------------------------
// The declaration reaches the renderer (Shapes)
// ---------------------------------------------------------------------------

func shapesFixture() *Shapes {
	gate := &ir.Schema{Name: "gate_in", Fields: []*ir.SchemaField{
		{Name: "payload", Type: ir.FieldTypeJSON},
		{Name: "paths", Type: ir.FieldTypeStringArray},
		{Name: "title", Type: ir.FieldTypeString},
	}}
	produced := &ir.Schema{Name: "scan_out", Fields: []*ir.SchemaField{
		{Name: "langs", Type: ir.FieldTypeJSON},
		{Name: "files", Type: ir.FieldTypeStringArray},
		{Name: "head", Type: ir.FieldTypeString},
	}}
	wf := &ir.Workflow{
		Vars: map[string]*ir.Var{
			"langs":  {Name: "langs", Type: ir.VarJSON},
			"files":  {Name: "files", Type: ir.VarStringArray},
			"branch": {Name: "branch", Type: ir.VarString},
		},
		Schemas: map[string]*ir.Schema{"gate_in": gate, "scan_out": produced,
			// A node "grp" that also declares a field named like the node
			// "grp.review": the two walks must pick the same one.
			"grp_out": {Name: "grp_out", Fields: []*ir.SchemaField{{Name: "review", Type: ir.FieldTypeStringArray}}}},
		Nodes: map[string]ir.Node{
			"scan":       &ir.ToolNode{BaseNode: ir.BaseNode{ID: "scan"}, SchemaFields: ir.SchemaFields{OutputSchema: "scan_out"}},
			"grp":        &ir.ToolNode{BaseNode: ir.BaseNode{ID: "grp"}, SchemaFields: ir.SchemaFields{OutputSchema: "grp_out"}},
			"grp.review": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "grp.review"}, SchemaFields: ir.SchemaFields{OutputSchema: "scan_out"}},
		},
	}
	return WorkflowShapes(wf).WithInputSchema(gate)
}

func TestDeclaredShapes_ReadsVarsInputFieldsAndOutputs(t *testing.T) {
	s := shapesFixture()
	cases := []struct {
		expr string
		want ValueShape
	}{
		{"{{vars.langs}}", ShapeJSON},
		{"{{vars.files}}", ShapeWords},
		{"{{vars.branch}}", ShapeUndeclared},
		{"{{vars.absent}}", ShapeUndeclared},
		{"{{input.payload}}", ShapeJSON},
		{"{{input.paths}}", ShapeWords},
		{"{{input.title}}", ShapeUndeclared},
		// A drilled path is the leaf of a structured value; no schema
		// types it, so it must say undeclared rather than borrow the
		// container's type. The value still RESOLVES to the leaf — this
		// is about its rendering, not its lookup.
		{"{{input.payload.inner}}", ShapeUndeclared},
		{"{{vars.langs.inner}}", ShapeUndeclared},
		// The producer's `output:` schema answers for an outputs
		// reference — including a group instance, whose id is dotted and
		// collides with the reference grammar.
		{"{{outputs.scan.langs}}", ShapeJSON},
		{"{{outputs.scan.files}}", ShapeWords},
		{"{{outputs.scan.head}}", ShapeUndeclared},
		{"{{outputs.scan.langs.inner}}", ShapeUndeclared},
		{"{{outputs.scan}}", ShapeUndeclared},
		// A node whose id IS the dotted prefix wins, as the runtime's own
		// walk decides: `outputs.grp.review` is node "grp.review"'s whole
		// output, not node "grp"'s field "review".
		{"{{outputs.grp.review}}", ShapeUndeclared},
		{"{{outputs.absent.langs}}", ShapeUndeclared},
		{"{{outputs.grp.review.langs}}", ShapeJSON},
		{"{{outputs.grp.review.files}}", ShapeWords},
	}
	for _, c := range cases {
		refs := mustRefs(c.expr)
		if len(refs) != 1 {
			t.Fatalf("%s parsed to %d refs", c.expr, len(refs))
		}
		if got := s.Of(refs[0]); got != c.want {
			t.Errorf("%s: shape %d, want %d", c.expr, got, c.want)
		}
	}
	if got := (*Shapes)(nil).Of(mustRefs("{{vars.langs}}")[0]); got != ShapeUndeclared {
		t.Errorf("nil Shapes answered %d, want ShapeUndeclared", got)
	}
}

// ---------------------------------------------------------------------------
// The wiring — deleting the nodeShapes argument reverts the whole fix, and
// every renderer test above stays green because the renderer is correct in
// isolation. These resolve a real body through the real recipe closures.
// ---------------------------------------------------------------------------

func jsonFieldRecipeNode(command, script string) *ir.ToolNode {
	return &ir.ToolNode{
		BaseNode:     ir.BaseNode{ID: "gate"},
		SchemaFields: ir.SchemaFields{InputSchema: "gate_in"},
		Command:      command,
		CommandRefs:  mustRefs(command),
		Script:       script,
		ScriptRefs:   mustRefs(script),
	}
}

func mustRefs(s string) []*ir.Ref {
	if s == "" {
		return nil
	}
	refs, err := ir.ParseRefs(s)
	if err != nil {
		panic(err)
	}
	return refs
}

func TestShellRecipe_RendersEachFieldByItsDeclaration(t *testing.T) {
	e := jsonFieldExecutor(
		&ir.SchemaField{Name: "quick_replies", Type: ir.FieldTypeJSON},
		&ir.SchemaField{Name: "files", Type: ir.FieldTypeStringArray},
	)
	node := jsonFieldRecipeNode(`QUICK={{input.quick_replies}} FILES={{input.files}} run`, "")
	input := map[string]any{
		"quick_replies": []any{"Go", "Fail-open partout"},
		"files":         []any{"a.go", "b.go"},
	}

	resolve, _ := e.shellRecipe(context.Background(), node, input)
	got := resolve()

	if want := `QUICK='["Go","Fail-open partout"]'`; !strings.Contains(got, want) {
		t.Errorf("shellRecipe resolved to %q, want it to contain %q", got, want)
	}
	if want := `FILES='a.go' 'b.go'`; !strings.Contains(got, want) {
		t.Errorf("shellRecipe resolved to %q, want it to contain %q", got, want)
	}
}

// The gap the previous fix named and could not close: a `json`-declared
// VAR. jsonFieldsAsText only ever saw the input map, so `{{vars.langs}}`
// space-joined into the command whatever the declaration said.
func TestShellRecipe_RendersVarsByTheirDeclaration(t *testing.T) {
	e := listVarExecutor(map[string]*ir.Var{
		"langs": {Name: "langs", Type: ir.VarJSON},
		"files": {Name: "files", Type: ir.VarStringArray},
		"empty": {Name: "empty", Type: ir.VarJSON},
	})
	e.vars = map[string]any{
		"langs": []any{"go", "ts"},
		"files": []any{"a.go", "b.go"},
		"empty": []any{},
	}
	node := jsonFieldRecipeNode(`LANGS={{vars.langs}} run {{vars.files}} -- {{vars.empty}} tail`, "")

	resolve, _ := e.shellRecipe(context.Background(), node, nil)
	got := resolve()

	if want := `LANGS='["go","ts"]'`; !strings.Contains(got, want) {
		t.Errorf("shellRecipe resolved to %q, want it to contain %q", got, want)
	}
	if want := `run 'a.go' 'b.go' --`; !strings.Contains(got, want) {
		t.Errorf("shellRecipe resolved to %q, want it to contain %q", got, want)
	}
	// #1320: the empty `json` var keeps a token, so `tail` stays the
	// argument the author put after it instead of shifting one place left.
	if want := `-- '[]' tail`; !strings.Contains(got, want) {
		t.Errorf("shellRecipe resolved to %q, want it to contain %q", got, want)
	}
}

func TestPostconditionRecipe_RendersByDeclaration(t *testing.T) {
	e := jsonFieldExecutor(&ir.SchemaField{Name: "ids", Type: ir.FieldTypeJSON})
	node := &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "gate"},
		SchemaFields:  ir.SchemaFields{InputSchema: "gate_in"},
		Postcondition: `IDS={{input.ids}} check`,
	}
	node.PostcondRefs = mustRefs(node.Postcondition)

	got := e.postconditionBody(context.Background(), node, map[string]any{"ids": []any{"T-1", "T-2"}})

	if want := `IDS='["T-1","T-2"]'`; !strings.Contains(got, want) {
		t.Errorf("postcondition resolved to %q, want it to contain %q", got, want)
	}
}

// A drilled path must still resolve. Pre-encoding the json field to text
// one frame up turned `{{input.payload.inner}}` into a drill against a
// string, which found nothing and left the reference in the command as
// written.
func TestShellRecipe_DrilledPathIntoAJSONFieldStillResolves(t *testing.T) {
	e := jsonFieldExecutor(&ir.SchemaField{Name: "payload", Type: ir.FieldTypeJSON})
	node := jsonFieldRecipeNode(`INNER={{input.payload.inner}} run`, "")
	input := map[string]any{"payload": map[string]any{"inner": "leaf"}}

	resolve, _ := e.shellRecipe(context.Background(), node, input)
	got := resolve()

	if want := `INNER='leaf'`; !strings.Contains(got, want) {
		t.Errorf("shellRecipe resolved to %q, want it to contain %q", got, want)
	}
	if strings.Contains(got, "{{input.payload.inner}}") {
		t.Errorf("the drilled reference was left as written: %q", got)
	}
}

// script: bodies are JSON literals for every namespace — the shape does
// not enter there, and a `json` value must not gain a second encoding.
func TestScriptRecipe_IsUnchangedByTheDeclaration(t *testing.T) {
	e := jsonFieldExecutor(&ir.SchemaField{Name: "ids", Type: ir.FieldTypeJSON})
	node := jsonFieldRecipeNode("", `const ids = {{input.ids}};`)
	node.Language = "js"

	resolve, _ := e.scriptRecipe(context.Background(), node, map[string]any{
		"ids": []any{"T-1", "T-2"},
	})
	got := resolve()

	if want := `const ids = ["T-1","T-2"];`; !strings.Contains(got, want) {
		t.Errorf("scriptRecipe resolved to %q, want it to contain %q (no double encoding)", got, want)
	}
	if strings.Contains(got, `"[\"T-1\"`) || strings.Contains(got, `'["T-1"`) {
		t.Errorf("scriptRecipe double-encoded the json field: %q", got)
	}
}

// No schema, no declared field: the reference renders by the heuristic
// rather than by a declaration nobody made.
func TestNodeShapes_NoSchemaIsUndeclared(t *testing.T) {
	e := &ClawExecutor{schemas: map[string]*ir.Schema{}}
	if s := e.nodeShapes(jsonFieldNode()); s != nil {
		t.Errorf("unknown schema produced shapes: %#v", s)
	}
	if s := e.nodeShapes(&ir.ToolNode{}); s != nil {
		t.Errorf("schemaless node produced shapes: %#v", s)
	}
	if s := e.nodeShapes(nil); s != nil {
		t.Errorf("nil node produced shapes: %#v", s)
	}
}

// The construction, not the fixture: NewClawExecutor is where the
// workflow's declarations reach the executor, and where an unoverridden
// default is read. A test that sets wfVars by hand proves the renderer and
// nothing about how it ever gets its answer.
func TestNewClawExecutor_SeedsAndTypesTheDeclaredVars(t *testing.T) {
	wf := &ir.Workflow{
		Name:    "wf",
		Schemas: map[string]*ir.Schema{"gate_in": {Name: "gate_in"}},
		Vars: map[string]*ir.Var{
			"langs": {Name: "langs", Type: ir.VarJSON, HasDefault: true, Default: "[1, 2]"},
			"files": {Name: "files", Type: ir.VarStringArray, HasDefault: true, Default: "a.go,b.go"},
			"mode":  {Name: "mode", Type: ir.VarString, HasDefault: true, Default: "${JSONFIELD_UNSET:-fast}"},
		},
	}
	e := NewClawExecutor(NewRegistry(), wf)

	// The seed takes the run's reading of the text, not the compiler's.
	if got, want := e.vars["mode"], "fast"; got != want {
		t.Errorf("seeded mode = %#v, want %#v (the ${VAR:-default} form)", got, want)
	}
	if _, isText := e.vars["files"].(string); isText {
		t.Errorf("seeded files = %#v, want the list reading an override gets", e.vars["files"])
	}

	node := jsonFieldRecipeNode(`LANGS={{vars.langs}} run {{vars.files}}`, "")
	resolve, _ := e.shellRecipe(context.Background(), node, nil)
	got := resolve()

	if want := `LANGS='[1,2]'`; !strings.Contains(got, want) {
		t.Errorf("shellRecipe resolved to %q, want it to contain %q", got, want)
	}
	if want := `run 'a.go' 'b.go'`; !strings.Contains(got, want) {
		t.Errorf("shellRecipe resolved to %q, want it to contain %q", got, want)
	}
}

// JSON null is a VALUE. A declared slot holding it must render the value,
// not the `{{…}}` placeholder a shell body keeps for a reference nobody
// wired — which is what the nil short-circuit did, past the declaration.
func TestPresentNull_RendersThroughTheDeclaration(t *testing.T) {
	e := listVarExecutor(
		map[string]*ir.Var{
			"doc": {Name: "doc", Type: ir.VarJSON},
			// Declared, and the run never seeded it: absent, not null.
			"unseeded": {Name: "unseeded", Type: ir.VarJSON},
		},
		&ir.SchemaField{Name: "payload", Type: ir.FieldTypeJSON},
		&ir.SchemaField{Name: "paths", Type: ir.FieldTypeStringArray},
	)
	e.vars = map[string]any{"doc": nil}
	node := jsonFieldRecipeNode(`D={{vars.doc}} P={{input.payload}} run {{input.paths}} tail {{input.absent}} {{vars.gone}} {{vars.unseeded}}`, "")

	// `paths` is DECLARED and absent from the input; `absent` and
	// `vars.gone` are not declared at all.
	resolve, _ := e.shellRecipe(context.Background(), node, map[string]any{"payload": nil})
	got := resolve()

	if want := `D='null' P='null' run`; !strings.Contains(got, want) {
		t.Errorf("resolved to %q, want it to contain %q", got, want)
	}
	// An ABSENT reference keeps its placeholder whether or not its slot is
	// declared: a missing wiring must stay visible, and `null` is a value
	// only when the value is there.
	for _, ref := range []string{"{{input.paths}}", "{{input.absent}}", "{{vars.gone}}", "{{vars.unseeded}}"} {
		if !strings.Contains(got, ref) {
			t.Errorf("absent reference %s was substituted away: %q", ref, got)
		}
	}
}

// #1320 on the namespace that most often carries an agent's structured
// output: the producer's `output:` schema is a declaration too.
func TestShellRecipe_RendersOutputsByTheProducersSchema(t *testing.T) {
	produced := &ir.Schema{Name: "scan_out", Fields: []*ir.SchemaField{
		{Name: "langs", Type: ir.FieldTypeJSON},
		{Name: "files", Type: ir.FieldTypeStringArray},
	}}
	wf := &ir.Workflow{
		Schemas: map[string]*ir.Schema{"scan_out": produced},
		Nodes: map[string]ir.Node{
			"scan": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "scan"}, SchemaFields: ir.SchemaFields{OutputSchema: "scan_out"}},
		},
	}
	e := NewClawExecutor(NewRegistry(), wf)
	node := jsonFieldRecipeNode(`L={{outputs.scan.langs}} run {{outputs.scan.files}}`, "")
	td := &TemplateData{
		Nodes:   map[string]ir.Node{"scan": wf.Nodes["scan"]},
		Outputs: map[string]map[string]any{"scan": {"langs": []any{"go", "ts"}, "files": []any{"a.go", "b.go"}}},
	}

	resolve, _ := e.shellRecipe(WithTemplateData(context.Background(), td), node, nil)
	got := resolve()

	if want := `L='["go","ts"]'`; !strings.Contains(got, want) {
		t.Errorf("resolved to %q, want it to contain %q", got, want)
	}
	if want := `run 'a.go' 'b.go'`; !strings.Contains(got, want) {
		t.Errorf("resolved to %q, want it to contain %q", got, want)
	}
}

// A map that lands in a `string[]` slot anyway keeps its JSON. Go's
// `map[k:v]` debug syntax is what the undeclared arm exists to avoid, and
// a declared slot must not reintroduce it.
func TestShapeWords_MapKeepsItsJSON(t *testing.T) {
	m := map[string]any{"k": "v"}
	if got, want := shellEscapeValue(m, ShapeWords), `'{"k":"v"}'`; got != want {
		t.Errorf("map in a string[] slot = %s, want %s", got, want)
	}
	if got, want := shellEscapeValue(m, ShapeUndeclared), `'{"k":"v"}'`; got != want {
		t.Errorf("undeclared map = %s, want %s", got, want)
	}
}

// A producer that had nothing to say is NOT an empty argv list. Rendering
// a present null as no token deleted the argument and shifted the next one
// into its place — in the one configuration where the author had DECLARED
// the slot, which is the opposite of what declaring it should buy.
func TestPresentNull_InAStringArraySlotKeepsThePlaceholder(t *testing.T) {
	e := jsonFieldExecutor(&ir.SchemaField{Name: "files", Type: ir.FieldTypeStringArray})
	node := jsonFieldRecipeNode(`run {{input.files}} --flag SENTINEL`, "")

	resolve, _ := e.shellRecipe(context.Background(), node, map[string]any{"files": nil})
	got := resolve()

	if !strings.Contains(got, "{{input.files}}") {
		t.Errorf("a null string[] deleted the argument: %q", got)
	}
	// An EMPTY list still renders no token — that is the arbitrated rule.
	resolve2, _ := e.shellRecipe(context.Background(), node, map[string]any{"files": []any{}})
	if got2 := resolve2(); !strings.Contains(got2, "run  --flag SENTINEL") {
		t.Errorf("an empty string[] = %q, want no token at all", got2)
	}
}

// `{{vars.cfg.on}}` is the member `on` of a `json` var, the same thing an
// expression reads from the same text. Answering the whole document here
// made one reference mean three things across the three surfaces that
// resolve it.
func TestShellRecipe_DrillsIntoAJSONVar(t *testing.T) {
	e := listVarExecutor(map[string]*ir.Var{"cfg": {Name: "cfg", Type: ir.VarJSON}})
	e.vars = map[string]any{"cfg": map[string]any{"name": "alpha", "on": true}}
	node := jsonFieldRecipeNode(`NAME={{vars.cfg.name}} ON={{vars.cfg.on}} ABSENT={{vars.cfg.nope}}`, "")

	resolve, _ := e.shellRecipe(context.Background(), node, nil)
	got := resolve()

	if want := `NAME='alpha' ON='true'`; !strings.Contains(got, want) {
		t.Errorf("resolved to %q, want it to contain %q", got, want)
	}
	// A member the document has not keeps the placeholder, like any other
	// reference nobody wired.
	if !strings.Contains(got, "{{vars.cfg.nope}}") {
		t.Errorf("an absent member was substituted away: %q", got)
	}
	// The prompt surface reads the same text the same way.
	r := &TemplateResolver{Vars: e.vars}
	if v, ok := r.ResolveValue("vars.cfg.name", nil, nil); !ok || v != "alpha" {
		t.Errorf("prompt reading of vars.cfg.name = %#v (ok=%v), want %q", v, ok, "alpha")
	}
}

// One *Shapes serves every node of a run: WithInputSchema must not write
// the maps it shares. Two nodes with different input schemas, rendered
// concurrently off the same workflow shapes, under -race.
func TestShapes_AreSafeForConcurrentNodes(t *testing.T) {
	wf := &ir.Workflow{
		Vars: map[string]*ir.Var{"langs": {Name: "langs", Type: ir.VarJSON}},
		Schemas: map[string]*ir.Schema{
			"in_a": {Name: "in_a", Fields: []*ir.SchemaField{{Name: "x", Type: ir.FieldTypeJSON}}},
			"in_b": {Name: "in_b", Fields: []*ir.SchemaField{{Name: "x", Type: ir.FieldTypeStringArray}}},
		},
	}
	e := NewClawExecutor(NewRegistry(), wf)
	e.vars = map[string]any{"langs": []any{"go", "ts"}}
	nodes := map[string]string{"in_a": `'["p","q"]'`, "in_b": `'p' 'q'`}

	done := make(chan string, 2*8)
	for i := 0; i < 8; i++ {
		for schema := range nodes {
			go func(schema string) {
				n := &ir.ToolNode{
					BaseNode:     ir.BaseNode{ID: "n"},
					SchemaFields: ir.SchemaFields{InputSchema: schema},
					Command:      `X={{input.x}} L={{vars.langs}}`,
				}
				n.CommandRefs = mustRefs(n.Command)
				resolve, _ := e.shellRecipe(context.Background(), n, map[string]any{"x": []any{"p", "q"}})
				done <- schema + "\x00" + resolve()
			}(schema)
		}
	}
	for i := 0; i < 2*8; i++ {
		got := <-done
		schema, rendered, _ := strings.Cut(got, "\x00")
		if want := "X=" + nodes[schema]; !strings.Contains(rendered, want) {
			t.Errorf("%s rendered %q, want it to contain %q", schema, rendered, want)
		}
		if !strings.Contains(rendered, `L='["go","ts"]'`) {
			t.Errorf("%s lost the workflow-wide declaration: %q", schema, rendered)
		}
	}
}

// The two shapes' answer for nil, stated once: the renderer is what the
// presence rule above hands a null to.
func TestShellEscapeValue_NilByShape(t *testing.T) {
	if got := shellEscapeValue(nil, ShapeJSON); got != `'null'` {
		t.Errorf("nil json = %s, want 'null'", got)
	}
	if got := shellEscapeValue(nil, ShapeWords); got != "" {
		t.Errorf("nil string[] = %q, want no token", got)
	}
	if got := shellEscapeValue(nil, ShapeUndeclared); got != "" {
		t.Errorf("nil undeclared = %q, want no token", got)
	}
}
