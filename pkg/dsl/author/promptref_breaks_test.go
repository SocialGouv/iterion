package author

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A prompt written in place of its reference — `system:`, `user:`,
// `instructions:` — is a text of the document like any other: read from a
// block scalar, it is read as the scanner read it (blockBreaks). It was the
// one text reader that took the scalar raw: two authored lines reached the
// model joined by an invisible separator, with no warning, and a folded
// block holding one was not refused as every other folded text is.
func TestAPromptInPlaceIsReadAsTheScannerReadIt(t *testing.T) {
	nl, ls := string(rune(10)), string(rune(0x2028))
	head := strings.Join([]string{"dsl: 2", "nodes:", "  - agent: a", "    model: m", ""}, nl)
	tail := strings.Join([]string{"workflow:", "  name: w", "  entry: a", "  edges:", "    - a -> done", ""}, nl)
	t.Run("a literal user prompt", func(t *testing.T) {
		doc := head + "    user: |" + nl + "      first" + ls + "      second" + nl + tail
		res := Parse("p.yaml", []byte(doc))
		if res.HasErrors() {
			t.Fatalf("the document is refused: %v", res.Diagnostics)
		}
		body := inlineBody(t, res, res.File.Agents[0].User)
		if want := "first" + nl + "second"; body != want {
			t.Errorf("the prompt body read is %q, want %q", body, want)
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
		if warned.Line != 6 || !strings.Contains(warned.Message, "`user`") {
			t.Errorf("the warning is at line %d and says %q; want line 6 naming `user`", warned.Line, warned.Message)
		}
	})
	t.Run("a folded system prompt", func(t *testing.T) {
		doc := head + "    system: >" + nl + "      first" + ls + "      second" + nl + tail
		res := Parse("p.yaml", []byte(doc))
		for _, d := range res.Diagnostics {
			if d.Code == parser.DiagAuthorPromptBody && d.Severity == parser.SeverityError && strings.Contains(d.Message, "in a folded block") {
				return
			}
		}
		t.Fatalf("the folded prompt's separator is not refused: %v", res.Diagnostics)
	})
}

// inlineBody is the body of the inline prompt a property refers to by name.
func inlineBody(t *testing.T, res *Result, name string) string {
	t.Helper()
	for _, p := range res.File.Prompts {
		if p.Name == name {
			return p.Body
		}
	}
	t.Fatalf("no prompt named %q among %d prompts", name, len(res.File.Prompts))
	return ""
}
