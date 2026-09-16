package spec

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPublicCriterionRegistryValidatesParametersAndValues(t *testing.T) {
	for _, test := range []struct {
		kind, params string
		value        any
		valid        bool
	}{
		{"min_length", `{"min":2}`, "éé", true},
		{"min_length", `{"min":2}`, "é", false},
		{"min_length", `{"min":1}`, []string{"x"}, true},
		{"min_length", `{"min":1}`, []any{}, false},
		{"pattern", `{"pattern":"^[a-z]+$"}`, "valid", true},
		{"pattern", `{"pattern":"^[a-z]+$"}`, "42", false},
	} {
		check, err := CompilePublicCriterion(test.kind, json.RawMessage(test.params))
		if err != nil {
			t.Fatal(err)
		}
		if got := check(test.value) == nil; got != test.valid {
			t.Fatalf("%s %s on %#v: valid=%t, want %t", test.kind, test.params, test.value, got, test.valid)
		}
	}
	for _, params := range []string{`{}`, `{"min":"1"}`, `{"min":-1}`, `{"min":1,"typo":2}`, "null", "[]", "{} {}"} {
		if _, err := CompilePublicCriterion("min_length", json.RawMessage(params)); err == nil {
			t.Fatalf("accepted invalid parameters %s", params)
		}
	}
	if _, err := CompilePublicCriterion("pattern", json.RawMessage(`{"pattern":"["}`)); err == nil {
		t.Fatal("invalid regular expression passed compilation")
	}
	if _, err := CompilePublicCriterion("unregistered", nil); err == nil || !strings.Contains(err.Error(), "unregistered") {
		t.Fatal("unknown evaluator was not refused by name")
	}
}

// The evaluator a lookup hands out refuses a missing or mistyped parameter
// instead of panicking: CompilePublicCriterion validates first, a caller
// of Compile alone gets an error too.
func TestPublicCriterionCompileRefusesBadParametersWithoutPanicking(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		params map[string]any
	}{
		{"min_length", map[string]any{}},
		{"min_length", map[string]any{"min": "2"}},
		{"pattern", map[string]any{}},
		{"pattern", map[string]any{"pattern": 2}},
	} {
		c, ok := LookupPublicCriterion(tc.kind)
		if !ok {
			t.Fatalf("%s not registered", tc.kind)
		}
		if _, err := c.Compile(tc.params); err == nil {
			t.Errorf("%s %v: compiled", tc.kind, tc.params)
		}
	}
}
