package spec_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// A prompt ref and a string read the same tokens — a quoted text, or one
// bare word — so no witness of the form sweep tells them apart: what
// differs is what the parser DOES with the text. A quoted text on a prompt
// ref becomes an inline prompt of the file, named after its body, and the
// property refers to it; on a string it is the value itself and no prompt
// appears. Held here on every property of either form, so a string
// registered as a prompt ref (or the reverse) is named.
func TestAPromptRefMakesAnInlinePromptAndAStringDoesNot(t *testing.T) {
	const text = "zz inline text"
	refs, strs := 0, 0
	for kind, tmpl := range probes {
		k, _ := spec.Lookup(kind)
		line := strings.Replace(tmpl, "%s: 1", "%s", 1)
		for _, p := range k.Properties {
			if p.Form != spec.PromptRef && p.Form != spec.String {
				continue
			}
			res := parser.Parse("probe.bot", fmt.Sprintf(line, p.Name+`: "`+text+`"`))
			if len(res.Diagnostics) != 0 {
				t.Errorf("%s.%s: %v", kind, p.Name, res.Diagnostics)
				continue
			}
			var inline []string
			for _, pr := range res.File.Prompts {
				if pr.Inline && pr.Body == text {
					inline = append(inline, pr.Name)
				}
			}
			switch p.Form {
			case spec.PromptRef:
				refs++
				if len(inline) != 1 || inline[0] != parser.InlinePromptName(text) {
					t.Errorf("%s.%s (prompt ref): a quoted text did not become the inline prompt %q: %v", kind, p.Name, parser.InlinePromptName(text), inline)
				}
			case spec.String:
				strs++
				if len(inline) != 0 {
					t.Errorf("%s.%s (string): a quoted text became an inline prompt %v", kind, p.Name, inline)
				}
			}
		}
	}
	if refs < 5 || strs < 30 {
		t.Fatalf("only %d prompt refs and %d strings probed — the sweep is not reading the registry", refs, strs)
	}
}
