package spec_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// Every string-valued property of every kind reads one plain bare word as
// the string it spells — `backend: claw` and `backend: "claw"` are the same
// document — derived from the registry, so a string property added later
// is covered; and a property whose value is a NAME (an ident form) is left
// untouched: a bare word there is still a reference, never a string.
func TestEveryStringPropertyReadsABareWord(t *testing.T) {
	n := 0
	for kind, tmpl := range probes {
		k, ok := spec.Lookup(kind)
		if !ok {
			continue
		}
		line := strings.Replace(tmpl, "%s: 1", "%s", 1)
		for _, p := range k.Properties {
			if p.Form != spec.String {
				continue
			}
			word := "claw"
			if len(p.Values) > 0 {
				word = p.Values[0] // a quoted enum: its own words, bare
			}
			n++
			quoted := parser.Parse("q.bot", fmt.Sprintf(line, p.Name+`: "`+word+`"`))
			bare := parser.Parse("b.bot", fmt.Sprintf(line, p.Name+": "+word))
			if len(quoted.Diagnostics) != 0 || len(bare.Diagnostics) != 0 {
				t.Errorf("%s.%s: quoted %v, bare %v", kind, p.Name, quoted.Diagnostics, bare.Diagnostics)
				continue
			}
			jq, _ := ast.MarshalFile(quoted.File)
			jb, _ := ast.MarshalFile(bare.File)
			if !bytes.Equal(jq, jb) {
				t.Errorf("%s.%s: `%s` and `\"%s\"` read as different documents", kind, p.Name, word, word)
			}
		}
	}
	if n < 20 {
		t.Fatalf("only %d string properties probed — the sweep is not reading the registry", n)
	}
}

// A value that is not one plain word still wants its quotes: the bare form
// is a relaxation for the word, not a new lexical form.
func TestABareValueMustBeOnePlainWord(t *testing.T) {
	for _, src := range []string{
		"agent a:\n  timeout: 20m\n",
		"agent a:\n  model: gpt-5.5\n",
		"agent a:\n  description: two words\n",
		// One token that is not a word: a number alone, a float alone —
		// nothing after it to draw a diagnostic on its own.
		"agent a:\n  timeout: 20\n",
		"agent a:\n  description: 3.5\n",
	} {
		if res := parser.Parse("x.bot", src); len(res.Diagnostics) == 0 {
			t.Errorf("accepted without quotes: %q", src)
		}
	}
}
