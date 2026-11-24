package author

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// docDiag is the door the document's own refusals are answered through —
// they join no report, so the bound sits in the constructor (#2801). A YAML
// decode refusal carries the anchor an alias names whole: 1 MiB of anchor
// name must come back under the bound, not as a 1 MiB E050.
func TestADocDiagIsBoundedAtTheDoor(t *testing.T) {
	anchor := strings.Repeat("a", 1<<20)
	res := Parse("probe.bot.yaml", []byte("dsl: 2\nname: *"+anchor+"\n"))
	var d *parser.Diagnostic
	for i := range res.Diagnostics {
		if res.Diagnostics[i].Code == parser.DiagAuthorDocument {
			d = &res.Diagnostics[i]
			break
		}
	}
	if d == nil {
		t.Fatalf("no E050 for an alias naming no anchor: %+v", res.Diagnostics)
	}
	if !strings.Contains(d.Message, "anchor") {
		t.Fatalf("E050 does not name the cause: %.200s", d.Message)
	}
	if len(d.Message) > expr.MaxDiagnosticText+len("…") || len(d.Hint) > expr.MaxDiagnosticText+len("…") {
		t.Fatalf("the anchor name rode the message whole: %d bytes of message, %d of hint, want each bounded to %d of content", len(d.Message), len(d.Hint), expr.MaxDiagnosticText)
	}
}
