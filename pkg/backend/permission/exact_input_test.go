package permission

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestCanonicalApprovalInput(t *testing.T) {
	original := json.RawMessage(`{"options":[true,{"name":"cyan","weight":9007199254740993}],"count":1}`)
	got, err := CanonicalApprovalInput(original)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, input string
		matches     bool
	}{
		{"object order and whitespace", ` { "count":1, "options":[ true,{"weight":9007199254740993,"name":"cyan"} ] } `, true},
		{"precise number", `{"count":1,"options":[true,{"name":"cyan","weight":9007199254740992}]}`, false},
		{"value type", `{"count":"1","options":[true,{"name":"cyan","weight":9007199254740993}]}`, false},
		{"array order", `{"count":1,"options":[{"name":"cyan","weight":9007199254740993},true]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other, err := CanonicalApprovalInput(json.RawMessage(tc.input))
			if err != nil || bytes.Equal(got, other) != tc.matches {
				t.Fatalf("match = %t, want %t (error: %v)", bytes.Equal(got, other), tc.matches, err)
			}
		})
	}
	for _, raw := range []string{``, `null`, `[]`, `{} {}`, `{"field":1,"field":2}`, `{"x":[{"name":"a","name":"b"}]}`} {
		if _, err := CanonicalApprovalInput(json.RawMessage(raw)); err == nil {
			t.Errorf("expected invalid approval input to be refused: %q", raw)
		}
	}
}
