package spec

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestPublicJSONRejectsAmbiguousObjects(t *testing.T) {
	for _, raw := range []string{`{"x":1,"x":2}`, `{"x":[{"min":1,"min":2}]}`, `{"min":1,"min":2}`, `[] null`, `{"x":`} {
		if _, err := DecodePublicJSON([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	value, err := DecodePublicJSON([]byte(`{"large":9007199254740993,"empty":[],"null":null}`))
	want := map[string]any{"large": json.Number("9007199254740993"), "empty": []any{}, "null": nil}
	if err != nil || !reflect.DeepEqual(value, want) {
		t.Fatalf("decoded %#v, %v", value, err)
	}
	if _, err := CompilePublicCriterion("min_length", json.RawMessage(`{"min":1,"min":2}`)); err == nil {
		t.Fatal("ambiguous criterion accepted")
	}
}
