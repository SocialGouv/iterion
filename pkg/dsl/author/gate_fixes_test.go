package author

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A mapping key whose plain YAML spelling reads as another type — a
// number, a bool, null, a date — is written quoted: the key node's `!!str`
// tag makes the encoder say the string, the one spelling the document
// reads back as the key; bare, the reader would refuse the document its
// own writer produced (E050, a non-string key). Author-chosen keys reach
// the writer through a JSON object (a port's default here).
func TestWriteQuotesAKeyYAMLWouldReadAsAnotherType(t *testing.T) {
	for _, key := range []string{"123", "1.5", "true", "false", "null", "~", "2026-01-01", "0x1F", "yes", "on", "1e3"} {
		t.Run(key, func(t *testing.T) {
			def, err := json.Marshal(map[string]any{key: 1})
			if err != nil {
				t.Fatal(err)
			}
			file := &ast.File{Contracts: []*ast.ContractDecl{{Name: "c", Inputs: []*ast.PortDecl{{Name: "i", Type: "json", Default: def}}}}}
			out, err := Write(file)
			if err != nil {
				t.Fatal(err)
			}
			res := Parse("k.yaml", out)
			if res.HasErrors() {
				t.Fatalf("the written document is refused: %v\n%s", res.Diagnostics, out)
			}
			var want, got any
			if err := json.Unmarshal(def, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(res.File.Contracts[0].Inputs[0].Default, &got); err != nil {
				t.Fatalf("the default came back as %s: %v", res.File.Contracts[0].Inputs[0].Default, err)
			}
			if !reflect.DeepEqual(want, got) {
				t.Errorf("the default came back as %v, want %v", got, want)
			}
		})
	}
}

// A quoted prompt body's trailing newline is the author's character, not
// a block scalar's clip: it reaches the lexer, which settles it as it
// settles a .bot's trailing blank line — and says so. A block scalar's
// trailing newline is the scalar's, dropped without a word.
func TestAQuotedBodyKeepsItsTrailingNewlineForTheLexerToSettle(t *testing.T) {
	nl, bs := string(rune(10)), string(rune(92))
	tail := strings.Join([]string{"nodes:", "  - agent: a", "    model: m", "    system: p", ""}, nl)
	for _, tc := range []struct {
		name, scalar string
		wantWarning  bool
	}{
		{"a quoted body ending with an escaped newline", `"text` + bs + `n"` + nl, true},
		{"a literal body, whose newline is the clip's", "|" + nl + "    text" + nl, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := Parse("p.yaml", []byte("dsl: 2"+nl+"prompts:"+nl+"  p: "+tc.scalar+tail))
			if res.HasErrors() {
				t.Fatalf("the document is refused: %v", res.Diagnostics)
			}
			if got := res.File.Prompts[0].Body; got != "text" {
				t.Errorf("the body read is %q, want %q", got, "text")
			}
			var warned *parser.Diagnostic
			for i, d := range res.Diagnostics {
				if d.Code == parser.DiagAuthorPromptBody && strings.Contains(d.Message, "trailing") {
					warned = &res.Diagnostics[i]
				}
			}
			if tc.wantWarning && warned == nil {
				t.Fatalf("the dropped newline is not said: %v", res.Diagnostics)
			}
			if !tc.wantWarning && warned != nil {
				t.Fatalf("the clip's newline is reported as the author's: %s", warned.Message)
			}
		})
	}
}
