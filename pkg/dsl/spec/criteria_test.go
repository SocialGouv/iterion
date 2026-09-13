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
		t.Fatal("unknown predicate was not rejected by name")
	}
}
