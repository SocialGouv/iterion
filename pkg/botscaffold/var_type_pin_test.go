package botscaffold

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestShapeVarsKeepTheTypeTheirShapeDeclares: the form lets a var row's
// type be edited, and a shape's expressions are typed against its var
// declarations (`vars.max_passes >= 1` compares a number; `!!vars.x` on a
// string reads its emptiness). EVERY var a shape references keeps the type
// its template declares — refused by name at the form, not met by the
// entry compute at runtime — while a var the operator added is theirs to
// type, and an unknown type is still reported as such.
func TestShapeVarsKeepTheTypeTheirShapeDeclares(t *testing.T) {
	other := map[string]string{"string": "int", "int": "string", "bool": "string", "float": "int"}
	pin := func(spec Spec, slug string) Spec {
		spec.Slug = slug
		spec.Model, spec.Backend = "anthropic/claude-opus-4-8", "claude_code"
		spec.Vars = append([]VarSpec(nil), spec.Vars...)
		return spec
	}
	pairs := 0
	for _, shape := range Shapes() {
		tpl := templateForShape(t, shape)
		for _, name := range shapeVarRefs(shape) {
			retyped := pin(tpl.Spec, "pin")
			var want string
			for i := range retyped.Vars {
				if retyped.Vars[i].Name == name {
					want = retyped.Vars[i].Type
					retyped.Vars[i].Type = other[want]
					// A default the NEW type accepts, so the per-row default
					// check (which runs first) is not what refuses the row.
					if other[want] == "int" {
						retyped.Vars[i].Default = "1"
					}
				}
			}
			if want == "" {
				t.Fatalf("%s: the referenced var %s is not declared by its template", shape, name)
			}
			pairs++
			_, err := Scaffold(filepath.Join(t.TempDir(), "pin"), retyped)
			if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), " as "+want) {
				t.Errorf("%s: %s retyped %s→%s: Scaffold = %v, want the var refused by name with the shape's type", shape, name, want, other[want], err)
			}
		}
	}
	if pairs == 0 {
		t.Fatal("no (shape, referenced var) pair; the pin guards nothing")
	}

	tpl, ok := TemplateByID("campaign-loop")
	if !ok {
		t.Fatal("no campaign-loop template")
	}
	added := pin(tpl.Spec, "camp2")
	added.Vars = append(added.Vars, VarSpec{Name: "extra", Type: "string", Default: "x", Description: "the operator's own"})
	if _, err := Scaffold(filepath.Join(t.TempDir(), "camp2"), added); err != nil {
		t.Fatalf("a var the operator added was refused: %v", err)
	}
	unknown := pin(tpl.Spec, "camp3")
	for i := range unknown.Vars {
		if unknown.Vars[i].Name == "max_passes" {
			unknown.Vars[i].Type = "number"
		}
	}
	if _, err := Scaffold(filepath.Join(t.TempDir(), "camp3"), unknown); err == nil || !strings.Contains(err.Error(), "invalid type") {
		t.Fatalf("an unknown type reported as %v, want the per-row invalid-type message before the pin", err)
	}
}
