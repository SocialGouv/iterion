package model

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestSchemaFeedbackBoundsOffendingValue(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	schema := &ir.Schema{Name: "s", Fields: []*ir.SchemaField{{Name: "verdict", Type: ir.FieldTypeString, EnumValues: []string{"pass", "fail"}}}}
	for _, value := range []string{strings.Repeat("the diff looks fine because ", 4000), strings.Repeat("é", 60000)} {
		err := ValidateOutput(map[string]any{"verdict": value}, schema)
		feedback := formatSchemaRetryFeedback(err)
		if len(feedback) > 2500 || !strings.Contains(feedback, "verdict") || !strings.Contains(feedback, "truncated") || !utf8.ValidString(feedback) {
			t.Fatalf("unbounded feedback: value=%d feedback=%d", len(value), len(feedback))
		}
	}
}
