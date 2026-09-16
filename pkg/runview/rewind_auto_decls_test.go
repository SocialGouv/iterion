package runview

import (
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// declFingerprints lists the declaration kinds by hand: a kind added to
// ast.File and not listed is invisible to rewind --auto — an edit of one
// answers "the workflow source is unchanged", the dangerous direction.
// Every named list of ast.File must yield a key for a declaration in it.
func TestDeclFingerprintsCoverEveryDeclarationKind(t *testing.T) {
	ft := reflect.TypeOf(ast.File{})
	var covered int
	for i := 0; i < ft.NumField(); i++ {
		field := ft.Field(i)
		if field.Type.Kind() != reflect.Slice || field.Type.Elem().Kind() != reflect.Pointer {
			continue
		}
		elem := field.Type.Elem().Elem()
		nameField, ok := elem.FieldByName("Name")
		if !ok || nameField.Type.Kind() != reflect.String {
			continue // comments, imports, uses: keyed otherwise, or not declarations
		}
		decl := reflect.New(elem)
		decl.Elem().FieldByName("Name").SetString("probe")
		f := &ast.File{}
		fv := reflect.ValueOf(f).Elem().Field(i)
		fv.Set(reflect.Append(fv, decl))
		var found bool
		for key := range declFingerprints(f) {
			if strings.Contains(key, "probe") {
				found = true
			}
		}
		if !found {
			t.Errorf("ast.File.%s: declFingerprints yields no key for a %s — an edit of one is invisible to rewind --auto", field.Name, elem.Name())
		}
		covered++
	}
	if covered < 15 {
		t.Fatalf("only %d named lists probed: the walk over ast.File is broken", covered)
	}
}

// A contract edit is a source change rewind --auto sees, as an edit of the
// contract, not of the workflow that names it.
func TestRewindAutoSeesAContractEdit(t *testing.T) {
	const before = "contract c:\n  inputs:\n    goal: string\n      default: \"x\"\n\nagent worker:\n  model: \"m\"\n  description: \"d\"\n\nworkflow w:\n  contract: c\n  entry: worker\n  worker -> done\n"
	after := strings.Replace(before, `default: "x"`, `default: "y"`, 1)
	a, b := parser.Parse("a.bot", before), parser.Parse("b.bot", after)
	if len(a.Diagnostics) != 0 || len(b.Diagnostics) != 0 {
		t.Fatalf("fixture: %v %v", a.Diagnostics, b.Diagnostics)
	}
	changes := diffDecls(declFingerprints(a.File), declFingerprints(b.File))
	if len(changes) != 1 || changes[0].Kind != "contract" || changes[0].Name != "c" {
		t.Fatalf("changes: %+v", changes)
	}
	if changes := diffDecls(declFingerprints(a.File), declFingerprints(a.File)); len(changes) != 0 {
		t.Fatalf("an unchanged contract reads as changed: %+v", changes)
	}
}
