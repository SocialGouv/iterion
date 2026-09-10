package ast

import (
	"encoding/json"
	"strings"
	"testing"
)

// The cloud queue ships the AST, so a field the JSON codec does not carry
// is silently LOST on a runner pod: the bot's typed refusal would compile
// locally and degrade to the generic FAIL_NODE in production, which is the
// one place an operator cannot look at the source to find out why.
func TestFailDeclSurvivesTheJSONRoundTrip(t *testing.T) {
	src := &File{
		Fails: []*FailDecl{
			{
				Name:        "plan_exhausted",
				Description: "the plan phase outgrew its share of the budget",
				Code:        "PLAN_BUDGET_EXHAUSTED",
				Message:     "planning used {{outputs.gate.pct}}% of the budget",
				Resumable:   true,
			},
			{Name: "not_actionable", Code: "LOT_NOT_ACTIONABLE"},
		},
		Workflows: []*WorkflowDecl{{Name: "w", Entry: "plan_exhausted"}},
	}

	encoded, err := MarshalFile(src)
	if err != nil {
		t.Fatalf("MarshalFile: %v", err)
	}
	if !strings.Contains(string(encoded), "PLAN_BUDGET_EXHAUSTED") {
		t.Fatalf("the encoded AST does not carry the failure code: %s", encoded)
	}

	back, err := UnmarshalFile(encoded)
	if err != nil {
		t.Fatalf("UnmarshalFile: %v", err)
	}
	if len(back.Fails) != len(src.Fails) {
		t.Fatalf("decoded %d fail decls, want %d", len(back.Fails), len(src.Fails))
	}
	for i, want := range src.Fails {
		got := back.Fails[i]
		if got.Name != want.Name || got.Description != want.Description ||
			got.Code != want.Code || got.Message != want.Message || got.Resumable != want.Resumable {
			t.Errorf("fail[%d] = %+v, want %+v", i, *got, *want)
		}
	}

	// Second round trip: the encoding is stable, so a pod that re-encodes
	// what it received hands the same bytes on.
	again, err := MarshalFile(back)
	if err != nil {
		t.Fatalf("re-MarshalFile: %v", err)
	}
	var a, b any
	if err := json.Unmarshal(encoded, &a); err != nil {
		t.Fatalf("unmarshal first: %v", err)
	}
	if err := json.Unmarshal(again, &b); err != nil {
		t.Fatalf("unmarshal second: %v", err)
	}
	if string(encoded) != string(again) {
		t.Errorf("re-encoding drifted:\nfirst:  %s\nsecond: %s", encoded, again)
	}
}
