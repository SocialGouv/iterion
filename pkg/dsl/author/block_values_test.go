package author

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// Every text of the document read from a block scalar — not only a prompt
// body — is read as the scanner read it: a line separator it left in a
// literal block is the line break it meant, said by a warning at its line;
// a folded block holding one is refused. Here a tool's command, whose two
// authored lines would otherwise reach the shell as one word.
func TestABlockValueSeparatorIsReadAsTheScannerReadIt(t *testing.T) {
	nl, ls := string(rune(10)), string(rune(0x2028))
	tail := strings.Join([]string{"workflow:", "  name: w", "  entry: t", "  edges:", "    - t -> done", ""}, nl)
	t.Run("a literal command", func(t *testing.T) {
		doc := strings.Join([]string{"dsl: 2", "nodes:", "  - tool: t", "    command: |", "      echo a" + ls + "      echo b", ""}, nl) + tail
		res := Parse("c.yaml", []byte(doc))
		if res.HasErrors() {
			t.Fatalf("the document is refused: %v", res.Diagnostics)
		}
		if got, want := res.File.Tools[0].Command, "echo a"+nl+"echo b"+nl; got != want {
			t.Errorf("the command read is %q, want %q", got, want)
		}
		var warned *parser.Diagnostic
		for i, d := range res.Diagnostics {
			if d.Code == parser.DiagAuthorPromptBody && strings.Contains(d.Message, "line separator") {
				warned = &res.Diagnostics[i]
			}
		}
		if warned == nil {
			t.Fatalf("the separator is not said: %v", res.Diagnostics)
		}
		if warned.Line != 5 || !strings.Contains(warned.Message, "`command`") {
			t.Errorf("the warning is at line %d and says %q; want line 5 naming `command`", warned.Line, warned.Message)
		}
	})
	t.Run("a folded description", func(t *testing.T) {
		doc := strings.Join([]string{"dsl: 2", "nodes:", "  - tool: t", "    command: echo", "    description: >", "      one" + ls + "      two", ""}, nl) + tail
		res := Parse("d.yaml", []byte(doc))
		for _, d := range res.Diagnostics {
			if d.Code == parser.DiagAuthorPromptBody && d.Severity == parser.SeverityError && strings.Contains(d.Message, "in a folded block") {
				return
			}
		}
		t.Fatalf("the folded block's separator is not refused: %v", res.Diagnostics)
	})
}
