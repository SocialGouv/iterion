package author

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// yaml.v3's scanner ends a line at LS (U+2028) and PS (U+2029) — the next
// line's indentation is stripped — but leaves the character in a block
// scalar's value. A literal prompt body is read with the newline the
// scanner meant, and a warning names where the separator sits; a folded
// body, whose break would have folded, is refused with the two ways out;
// an escaped separator in a quoted body is the author's character and
// stays.
func TestABlockBodySeparatorIsReadAsTheScannerReadIt(t *testing.T) {
	nl, bs := string(rune(10)), string(rune(92))
	ls, ps := string(rune(0x2028)), string(rune(0x2029))
	tail := strings.Join([]string{"nodes:", "  - agent: a", "    model: m", "    system: p", ""}, nl)
	for _, tc := range []struct {
		name, scalar, wantBody, where, wantErr string
		wantLine                               int
	}{
		{"LS in a literal", "|" + nl + "    one" + ls + "    two" + nl + "    three" + nl, "one" + nl + "two" + nl + "three", "a line separator (U+2028/U+2029), the first on line 4", "", 4},
		{"PS in a literal", "|" + nl + "    one" + nl + "    two" + ps + "    three" + nl, "one" + nl + "two" + nl + "three", "on line 5", "", 5},
		{"two separators in a literal", "|" + nl + "    one" + ls + "    two" + ps + "    three" + nl, "one" + nl + "two" + nl + "three", "2 line separators (U+2028/U+2029), the first on line 4", "", 4},
		{"LS in a folded", ">" + nl + "    one" + ls + "    two" + nl, "", "", "in a folded block", 3},
		{"an escaped LS in a quoted body", `"one` + bs + `Ltwo"` + nl, "one" + ls + "two", "", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := "dsl: 2" + nl + "prompts:" + nl + "  p: " + tc.scalar + tail
			res := Parse("p.yaml", []byte(doc))
			// The separator's own diagnostic, not another E053 of the body
			// (a settled blank line has one too).
			var found *parser.Diagnostic
			for i, d := range res.Diagnostics {
				if d.Code == parser.DiagAuthorPromptBody && strings.Contains(d.Message, "line separator") {
					found = &res.Diagnostics[i]
				}
			}
			if tc.wantErr != "" {
				if found == nil || found.Severity != parser.SeverityError {
					t.Fatalf("no E053 error among %v", res.Diagnostics)
				}
				if !strings.Contains(found.Message, tc.wantErr) || !strings.Contains(found.Message, "literal block") {
					t.Errorf("the refusal does not name the case and the way out: %s", found.Message)
				}
				if found.Line != tc.wantLine {
					t.Errorf("the refusal is at line %d, want %d", found.Line, tc.wantLine)
				}
				return
			}
			if res.HasErrors() {
				t.Fatalf("the document is refused: %v", res.Diagnostics)
			}
			if got := res.File.Prompts[0].Body; got != tc.wantBody {
				t.Errorf("the body read is %q, want %q", got, tc.wantBody)
			}
			if tc.where == "" {
				if found != nil {
					t.Fatalf("an escaped separator is warned about: %s", found.Message)
				}
				return
			}
			if found == nil {
				t.Fatalf("no E053 warning among %v", res.Diagnostics)
			}
			if !strings.Contains(found.Message, tc.where) {
				t.Errorf("the warning does not say where: %s", found.Message)
			}
			if found.Line != tc.wantLine {
				t.Errorf("the warning is at line %d, want %d", found.Line, tc.wantLine)
			}
		})
	}
}

// A string holding a scanner break — CR, NEL, LS, PS — is written escaped
// in double quotes, the one spelling the document reads back as the
// character: a declared prompt body, an inline one, a tool's command, and
// a mapping key (here a port default's JSON object key).
func TestWriteSpellsAScannerBreakAsAnEscape(t *testing.T) {
	nl := string(rune(10))
	for _, br := range []struct{ name, text, escape string }{
		{"CR", string(rune(13)), `\r`},
		{"NEL", string(rune(0x85)), `\N`},
		{"LS", string(rune(0x2028)), `\L`},
		{"PS", string(rune(0x2029)), `\P`},
	} {
		t.Run(br.name, func(t *testing.T) {
			body := "one" + br.text + "two" + nl + "three"
			inline := "say" + br.text + "so"
			command := "echo a" + br.text + "b"
			def, err := json.Marshal(map[string]any{"a" + br.text + "b": 1})
			if err != nil {
				t.Fatal(err)
			}
			file := &ast.File{
				Prompts:   []*ast.PromptDecl{{Name: "p", Body: body}, {Name: "q", Body: inline, Inline: true}},
				Agents:    []*ast.AgentDecl{{Name: "a", LLMDecl: ast.LLMDecl{Model: "m", System: "p"}}, {Name: "b", LLMDecl: ast.LLMDecl{Model: "m", System: "q"}}},
				Tools:     []*ast.ToolNodeDecl{{Name: "t", Command: command}},
				Contracts: []*ast.ContractDecl{{Name: "c", Inputs: []*ast.PortDecl{{Name: "i", Type: "json", Default: def}}}},
			}
			out, err := Write(file)
			if err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(string(out), br.escape); n != 4 {
				t.Errorf("the document spells the break as %s %d times, want 4:\n%s", br.escape, n, out)
			}
			res := Parse("w.yaml", out)
			if res.HasErrors() {
				t.Fatalf("the written document is refused: %v\n%s", res.Diagnostics, out)
			}
			bodies := map[string]bool{}
			for _, p := range res.File.Prompts {
				bodies[p.Body] = true
			}
			if !bodies[body] {
				t.Errorf("the declared body did not come back: %q not among %v", body, bodies)
			}
			if !bodies[inline] {
				t.Errorf("the inline body did not come back: %q not among %v", inline, bodies)
			}
			if got := res.File.Tools[0].Command; got != command {
				t.Errorf("the command came back as %q, want %q", got, command)
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
