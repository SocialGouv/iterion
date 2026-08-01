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

func jsonFieldNode() *ir.ToolNode {
	return &ir.ToolNode{SchemaFields: ir.SchemaFields{InputSchema: "gate_in"}}
}

// The regression: a `json` field holding an all-string list reached
// shellEscapeValue's scalar-slice arm and space-joined, so
// `KEY={{input.x}} cmd` assigned only the first word and ran the second
// as a command (exit 127).
func TestJSONFieldsAsText_StringListBecomesOneToken(t *testing.T) {
	e := jsonFieldExecutor(&ir.SchemaField{Name: "quick_replies", Type: ir.FieldTypeJSON})
	in := map[string]any{"quick_replies": []any{"Go", "Fail-open partout: comma"}}

	out := e.jsonFieldsAsText(jsonFieldNode(), in)

	got, ok := out["quick_replies"].(string)
	if !ok {
		t.Fatalf("quick_replies = %T, want string", out["quick_replies"])
	}
	if want := `["Go","Fail-open partout: comma"]`; got != want {
		t.Errorf("quick_replies = %q, want %q", got, want)
	}
	if resolved := shellEscapeValue(out["quick_replies"]); resolved != `'["Go","Fail-open partout: comma"]'` {
		t.Errorf("shell rendering = %s, want a single quoted token", resolved)
	}
	// The caller's map must not be mutated.
	if _, still := in["quick_replies"].([]any); !still {
		t.Error("jsonFieldsAsText mutated the caller's input map")
	}
}

// A `string[]` field keeps the space-join that `git add -- {{input.files}}`
// sites depend on — the declared type is the only signal separating the
// two intents, since an agent's output arrives as []any either way.
func TestJSONFieldsAsText_LeavesNonJSONFieldsAlone(t *testing.T) {
	e := jsonFieldExecutor(&ir.SchemaField{Name: "files", Type: ir.FieldTypeStringArray})
	in := map[string]any{"files": []any{"a.go", "b.go"}}

	out := e.jsonFieldsAsText(jsonFieldNode(), in)

	if _, isString := out["files"].(string); isString {
		t.Fatalf("files was JSON-encoded, want the slice preserved")
	}
	if resolved := shellEscapeValue(out["files"]); resolved != `'a.go' 'b.go'` {
		t.Errorf("shell rendering = %s, want two space-joined tokens", resolved)
	}
}

// Values that are already a single token stay untouched: re-encoding a
// string would add quotes the author never wrote.
func TestJSONFieldsAsText_TextualValuesUntouched(t *testing.T) {
	e := jsonFieldExecutor(
		&ir.SchemaField{Name: "payload", Type: ir.FieldTypeJSON},
		&ir.SchemaField{Name: "absent", Type: ir.FieldTypeJSON},
	)
	in := map[string]any{"payload": `{"already":"json"}`}

	out := e.jsonFieldsAsText(jsonFieldNode(), in)

	if got := out["payload"]; got != `{"already":"json"}` {
		t.Errorf("payload = %#v, want it unchanged", got)
	}
	if _, present := out["absent"]; present {
		t.Error("a schema field missing from the input was invented")
	}
}

// Object-valued json fields already JSON-encoded downstream; the result
// must stay a single token and must not double-encode.
func TestJSONFieldsAsText_ObjectStaysSingleToken(t *testing.T) {
	e := jsonFieldExecutor(&ir.SchemaField{Name: "cfg", Type: ir.FieldTypeJSON})
	in := map[string]any{"cfg": map[string]any{"k": "v"}}

	out := e.jsonFieldsAsText(jsonFieldNode(), in)

	if got := out["cfg"]; got != `{"k":"v"}` {
		t.Errorf("cfg = %#v, want %q", got, `{"k":"v"}`)
	}
	if resolved := shellEscapeValue(out["cfg"]); resolved != `'{"k":"v"}'` {
		t.Errorf("shell rendering = %s, want a single quoted token", resolved)
	}
}

// No schema, no declared field, or an empty input: the map passes
// through untouched rather than being rebuilt.
func TestJSONFieldsAsText_NoSchemaPassesThrough(t *testing.T) {
	e := &ClawExecutor{schemas: map[string]*ir.Schema{}}
	in := map[string]any{"x": []any{"a", "b"}}

	if out := e.jsonFieldsAsText(jsonFieldNode(), in); len(out) != 1 {
		t.Errorf("unknown schema changed the input map: %#v", out)
	}
	if out := e.jsonFieldsAsText(&ir.ToolNode{}, in); len(out) != 1 {
		t.Errorf("schemaless node changed the input map: %#v", out)
	}
}

// The wiring, not just the helper: deleting the jsonFieldsAsText call in
// shellRecipe reverts the whole fix, and every test above stays green
// because the helper remains correct in isolation. These resolve a real
// command template through the real recipe closures.
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

func TestShellRecipe_PreEncodesJSONFields(t *testing.T) {
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

	wantJSON := `QUICK='["Go","Fail-open partout"]'`
	if !strings.Contains(got, wantJSON) {
		t.Errorf("shellRecipe resolved to %q, want it to contain %q", got, wantJSON)
	}
	// The string[] field must still space-join: `git add -- {{input.files}}`
	// depends on one shell word per element.
	if wantJoin := `FILES='a.go' 'b.go'`; !strings.Contains(got, wantJoin) {
		t.Errorf("shellRecipe resolved to %q, want it to contain %q", got, wantJoin)
	}
}

func TestPostconditionRecipe_PreEncodesJSONFields(t *testing.T) {
	e := jsonFieldExecutor(&ir.SchemaField{Name: "ids", Type: ir.FieldTypeJSON})
	node := &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "gate"},
		SchemaFields:  ir.SchemaFields{InputSchema: "gate_in"},
		Postcondition: `IDS={{input.ids}} check`,
	}
	node.PostcondRefs = mustRefs(node.Postcondition)

	expanded := expandBracedEnv(node.Postcondition)
	got := resolveCommandTemplate(expanded, node.PostcondRefs,
		e.jsonFieldsAsText(node, map[string]any{"ids": []any{"T-1", "T-2"}}), nil)

	if want := `IDS='["T-1","T-2"]'`; !strings.Contains(got, want) {
		t.Errorf("postcondition resolved to %q, want it to contain %q", got, want)
	}
}

// script: bodies JSON-encode values themselves, so pre-encoding there
// would double-encode. This pins the scoping claim the doc comment makes.
func TestScriptRecipe_DoesNotPreEncodeJSONFields(t *testing.T) {
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
