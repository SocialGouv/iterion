package parser_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// Every keyword the lexer knows is a valid name for a declaration, a var or
// a schema field — not the dozen the hand-kept list happened to contain when
// it was last extended. The set is derived from the lexer's own table now;
// this test walks that table so a keyword added tomorrow cannot leave the
// class open again (`agent emit:` broke a shipped bot the last time it did).
func TestEveryKeywordIsAValidName(t *testing.T) {
	for _, kw := range parser.Keywords() {
		if kw == "done" || kw == "fail" {
			continue // the reserved terminal targets, refused by name on purpose (E011)
		}
		t.Run(kw, func(t *testing.T) {
			src := "agent " + kw + ":\n  model: \"m\"\nvars:\n  " + kw + ": string\nschema s:\n  " + kw + ": string\n"
			res := parser.Parse("kw.bot", src)
			for _, d := range res.Diagnostics {
				t.Errorf("keyword %q as a name: %v", kw, d)
			}
			if len(res.File.Agents) != 1 || res.File.Agents[0].Name != kw {
				t.Fatalf("agent named %q not parsed", kw)
			}
			if res.File.Vars == nil || len(res.File.Vars.Fields) != 1 || res.File.Vars.Fields[0].Name != kw {
				t.Fatalf("var named %q not parsed", kw)
			}
			if len(res.File.Schemas) != 1 || len(res.File.Schemas[0].Fields) != 1 || res.File.Schemas[0].Fields[0].Name != kw {
				t.Fatalf("schema field named %q not parsed", kw)
			}
		})
	}
}

// Inside a block that matches properties by their spelling, a keyword that is
// not one of its properties is an unknown PROPERTY (E012, with the remedy),
// not an "unexpected token".
func TestKeywordInAValueMatchedBlockIsAnUnknownProperty(t *testing.T) {
	res := parser.Parse("kw.bot", "workflow w:\n  sandbox:\n    needs: 1\n")
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != parser.DiagUnknownProperty {
		t.Fatalf("want one E012, got %v", res.Diagnostics)
	}
}
