package ir

import (
	"strings"
	"testing"
)

func TestPublicPortTypesResolveShapesRatherThanAliases(t *testing.T) {
	first := &Schema{Name: "Dossier", Fields: []*SchemaField{
		{Name: "status", Type: FieldTypeString, EnumValues: []string{"ready", "draft"}},
		{Name: "title", Type: FieldTypeString},
	}}
	alias := &Schema{Name: "Research", Fields: []*SchemaField{
		{Name: "title", Type: FieldTypeString},
		{Name: "status", Type: FieldTypeString, EnumValues: []string{"draft", "ready", "ready"}},
	}}
	schemas := map[string]*Schema{"Dossier": first, "Research": alias}
	a, err := ResolvePortType("Dossier[]", schemas)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ResolvePortType("Research[]", schemas)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Equivalent(b) {
		t.Fatal("canonical aliases or field/enum order changed the resolved shape")
	}
	element, ok := a.Element()
	if !ok || element.ArrayDepth != 0 || !element.Array().Equivalent(a) {
		t.Fatal("mapping did not remove exactly one array dimension")
	}
	if element.Equivalent(a) {
		t.Fatal("array and scalar types were implicitly equated")
	}
	if _, ok := element.Element(); ok {
		t.Fatal("a scalar was accepted as a map source")
	}
	first.Fields[0].EnumValues = []string{"published"}
	changed, err := ResolvePortType("Dossier[]", schemas)
	if err != nil {
		t.Fatal(err)
	}
	if a.Equivalent(changed) || a.Schema.Fields[0].EnumValues[0] != "ready" {
		t.Fatal("same schema name hid a changed definition, or resolved data aliased its source")
	}
}

func TestPublicPortShapeEncodingCannotConflateEnumSeparators(t *testing.T) {
	// A separator-joined digest would conflate these two different enums.
	a := &Schema{Fields: []*SchemaField{{Name: "value", Type: FieldTypeString, EnumValues: []string{"a", "b"}}}}
	b := &Schema{Fields: []*SchemaField{{Name: "value", Type: FieldTypeString, EnumValues: []string{"a\x01b"}}}}
	if CanonicalPortSchemaFingerprint(a) == CanonicalPortSchemaFingerprint(b) {
		t.Fatal("different enum constraints share a public type identity")
	}
}

func TestPublicPortTypesRejectUnresolvedOrMalformedSchemas(t *testing.T) {
	for _, tc := range []struct {
		ref      string
		schema   *Schema
		fragment string
	}{
		{"Unknown", nil, "unresolved"},
		{"Bad", &Schema{Fields: []*SchemaField{nil}}, "missing field"},
		{"Bad", &Schema{Fields: []*SchemaField{{Name: "x"}, {Name: "x"}}}, "duplicate"},
		{"Bad", &Schema{Fields: []*SchemaField{{Name: "x", Type: FieldType(999)}}}, "unsupported"},
	} {
		_, err := ResolvePortType(tc.ref, map[string]*Schema{"Bad": tc.schema})
		if err == nil || !strings.Contains(err.Error(), tc.fragment) {
			t.Fatalf("%s: expected %s, got %v", tc.ref, tc.fragment, err)
		}
	}
}
