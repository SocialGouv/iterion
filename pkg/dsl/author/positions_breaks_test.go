package author

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The source lines edgeAt reads back are cut where yaml.v3 ends a line — a
// lone CR, NEL, LS and PS count, not LF alone — so after such a character
// the line a node's Line names is the line read: a verbatim edge keeps its
// exact column, and a neighbouring line that carries the same text cannot
// confirm a column the edge does not have.
func TestEdgePositionsFollowYAMLsOwnLineBreaks(t *testing.T) {
	nl, cr := string(rune(10)), string(rune(13))
	backslash := string(rune(92))
	rest := strings.Join([]string{"nodes:", "  - agent: a", "    model: m", "  - agent: b", "    model: m", "workflow:", "  name: w", "  entry: a", "  edges:", ""}, nl)
	plain := "    - a -> b when x && y"
	// A double-quoted edge holding an escape, followed by a comment line that
	// carries the decoded text at the column the escape would have.
	decoy := `    - "a -> ` + backslash + `x41 when x && y"` + nl + `#     Xa -> A when x && y`
	for _, br := range []struct{ name, text string }{
		{"CRLF", cr + nl},
		{"lone CR", cr},
		{"NEL", string(rune(0x85))},
		{"LS", string(rune(0x2028))},
		{"PS", string(rune(0x2029))},
	} {
		head := "dsl: 2" + br.text + rest
		for _, tc := range []struct {
			name    string
			lines   string
			wantCol int
		}{
			{"a verbatim edge after the break keeps its exact column", plain, strings.Index(plain, "&") + 1},
			{"a decoy line cannot confirm a column the edge does not have", decoy, 7},
		} {
			t.Run(br.name+"/"+tc.name, func(t *testing.T) {
				res := Parse("e.yaml", []byte(head+tc.lines+nl))
				for _, d := range res.Diagnostics {
					if d.Code != parser.DiagUnexpectedToken {
						continue
					}
					if d.Line != 11 || d.Column != tc.wantCol {
						t.Errorf("E001 at %d:%d, want 11:%d", d.Line, d.Column, tc.wantCol)
					}
					return
				}
				t.Fatalf("no E001 among %v", res.Diagnostics)
			})
		}
	}
}
