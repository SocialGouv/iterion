package ast

import (
	"strings"
	"testing"
)

// The JSON seam is where a declared-empty list dies if it is carried as a
// plain slice: `omitempty` erases nil-from-empty, and the studio round-trips
// every document through it (server_files.go: MarshalFile → UnmarshalFile →
// Unparse). Reddens on the mutation that puts `[]string` back on the wire.
func TestDeclaredEmptyToolsSurviveTheJSONRoundTripThatTheStudioSavesThrough(t *testing.T) {
	f := &File{
		Judges: []*JudgeDecl{
			{Name: "declared", LLMDecl: LLMDecl{Tools: []string{}}},
			{Name: "unset"},
			{Name: "named", LLMDecl: LLMDecl{Tools: []string{"read_file"}}},
		},
	}
	blob, err := MarshalFile(f)
	if err != nil {
		t.Fatal(err)
	}
	// The wire tells the two apart: the declaration is a present empty
	// array, the absence is a missing key. Both judges are in the same
	// document, so one assertion cannot pass by accident of the other.
	if !strings.Contains(string(blob), `"tools": []`) {
		t.Fatalf("a declared-empty list must reach the wire as an empty array, got %s", blob)
	}
	back, err := UnmarshalFile(blob)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, j := range back.Judges {
		got[j.Name] = j.Tools
	}
	if got["declared"] == nil {
		t.Errorf("`tools: []` came back undeclared (nil) — the seam erased the declaration")
	} else if len(got["declared"]) != 0 {
		t.Errorf("`tools: []` came back as %#v", got["declared"])
	}
	if got["unset"] != nil {
		t.Errorf("an undeclared list came back declared: %#v — the seam invented a bound", got["unset"])
	}
	if len(got["named"]) != 1 || got["named"][0] != "read_file" {
		t.Errorf("a named list must survive unchanged, got %#v", got["named"])
	}
}
