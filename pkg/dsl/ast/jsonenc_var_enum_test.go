package ast_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// Marshal → Unmarshal must round-trip a var declaration's enum
// constraint, and the wire field must be `"enum"` — the key the studio's
// VarField type and pkg/botregistry's schema surface read.
func TestJSONRoundtripVarEnum(t *testing.T) {
	f := &ast.File{
		Vars: &ast.VarsBlock{
			Fields: []*ast.VarField{
				{
					Name:       "mode",
					Type:       ast.TypeString,
					EnumValues: []string{"autonomous", "interview"},
					Default:    &ast.Literal{Kind: ast.LitString, Raw: `"autonomous"`, StrVal: "autonomous"},
				},
				{Name: "plain", Type: ast.TypeString},
			},
		},
	}

	data, err := ast.MarshalFile(f)
	if err != nil {
		t.Fatalf("MarshalFile: %v", err)
	}
	if !strings.Contains(string(data), `"enum"`) {
		t.Fatalf("wire payload missing \"enum\" key:\n%s", data)
	}

	// Pin the exact wire shape (not just the Go round-trip): the studio
	// is built against `"enum": ["...", ...]` on each vars field.
	var doc struct {
		Vars struct {
			Fields []struct {
				Name string   `json:"name"`
				Enum []string `json:"enum"`
			} `json:"fields"`
		} `json:"vars"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode wire payload: %v", err)
	}
	if len(doc.Vars.Fields) != 2 {
		t.Fatalf("wire vars fields = %d, want 2", len(doc.Vars.Fields))
	}
	if got := doc.Vars.Fields[0].Enum; len(got) != 2 || got[0] != "autonomous" || got[1] != "interview" {
		t.Errorf("wire enum = %v, want [autonomous interview]", got)
	}
	if doc.Vars.Fields[1].Enum != nil {
		t.Errorf("plain var must omit enum, got %v", doc.Vars.Fields[1].Enum)
	}

	got, err := ast.UnmarshalFile(data)
	if err != nil {
		t.Fatalf("UnmarshalFile: %v", err)
	}
	vf := got.Vars.Fields[0]
	if len(vf.EnumValues) != 2 || vf.EnumValues[0] != "autonomous" || vf.EnumValues[1] != "interview" {
		t.Errorf("EnumValues after round-trip = %v", vf.EnumValues)
	}
	if got.Vars.Fields[1].EnumValues != nil {
		t.Errorf("plain var EnumValues after round-trip = %v, want nil", got.Vars.Fields[1].EnumValues)
	}
}
