package model

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestValidateOutput_StringArrayEnumEnforced guards the data-integrity
// fix that pushed the string[] [enum: ...] check into ValidateOutput.
// Before the fix, the generated JSON schema advertised the constraint
// to the LLM but server-side validation accepted any string value, so
// a stray entry flowed downstream unchecked.
func TestValidateOutput_StringArrayEnumEnforced(t *testing.T) {
	schema := &ir.Schema{
		Name: "out",
		Fields: []*ir.SchemaField{
			{
				Name:       "tags",
				Type:       ir.FieldTypeStringArray,
				EnumValues: []string{"red", "green", "blue"},
			},
		},
	}

	cases := []struct {
		name    string
		val     any
		wantErr string
	}{
		{
			name:    "rejects out-of-enum",
			val:     []any{"red", "purple"},
			wantErr: "not in enum",
		},
		{
			name: "accepts all-valid",
			val:  []any{"red", "green", "blue"},
		},
		{
			name: "accepts empty array",
			val:  []any{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateOutput(map[string]any{"tags": c.val}, schema)
			if c.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), c.wantErr)
			}
		})
	}
}
