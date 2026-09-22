package spec_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// witnesses are, for one Form, the values its language ACCEPTS beyond the
// sample sampleValues writes, and the values it REFUSES. They are the
// mutants of a Form: a property registered under the wrong Form reads one
// of them the other way — a quoted string where the registry promises a
// bare name, a bare word where it promises an enum — and the sweep names
// the property. Each value is the text after `name: ` (a value starting
// with "\n" is the multi-line form, re-indented under the property; a
// value starting with the property's own spelling, as `with {…}`, is the
// whole line; `%s` is the property's first listed value).
type witnesses struct {
	accepted []string
	refused  []string
}

var formWitnesses = map[spec.Form]witnesses{
	spec.String: {
		accepted: []string{`"two words"`, "`raw text`", "word", "|\n  block\n  scalar"},
		refused:  []string{"1", "1.5", "[a]", "two words", `{ a: "b" }`},
	},
	spec.Ident: {
		accepted: []string{"x", "x_1"},
		refused:  []string{`"x"`, "1", "1.5", "[a]", "a.b"},
	},
	spec.DottedIdent: {
		accepted: []string{"x", "a.b", "a.b.c"},
		refused:  []string{`"x"`, "1", "1.5", "[a]"},
	},
	spec.StringOrIdent: {
		accepted: []string{"x", `"x y"`, "github.com", "`raw`"},
		refused:  []string{"1", "1.5", "[a]"},
	},
	spec.StringOrNumber: {
		// A number followed by its unit is one value (`30s`): the lexer
		// splits them, the reader joins them.
		accepted: []string{`"30s"`, "30s", "3", "1.5", "x"},
		refused:  []string{"[a]", `{ a: "b" }`},
	},
	spec.Int: {
		accepted: []string{"0", "42"},
		refused:  []string{`"1"`, "1.5", "x", "[1]", "true"},
	},
	spec.Number: {
		accepted: []string{"1", "0.5", "42"},
		refused:  []string{`"1"`, "x", "[1]", "true"},
	},
	spec.Bool: {
		accepted: []string{"true", "false"},
		refused:  []string{`"true"`, "1", "yes", "[true]"},
	},
	spec.JSON: {
		// A trailing comma inside a container is tolerated, not written.
		accepted: []string{"null", "true", "false", "1", "1.5", `"s"`, "[]", "{}", `[1, "a", null]`, `{"k": 1}`, "{k: {n: [true]}}", "[1,]", "{k: 1,}"},
		refused:  []string{"-1", "1e3", "x", "{k}", "1 2"},
	},
	spec.Enum: {
		// The listed values are the samples; the quoted spelling of a
		// listed value reads the same, a word outside the list is refused
		// either way.
		accepted: []string{`"%s"`},
		refused:  []string{"zz_bogus", `"zz_bogus"`, "1", "[%s]"},
	},
	spec.EnumOrEnv: {
		accepted: []string{`"%s"`, `"${VIBE_EFFORT:-max}"`, "`${X}`"},
		refused:  []string{"zz_bogus", "1", "[%s]"},
	},
	spec.PromptRef: {
		accepted: []string{"x", `"inline text"`, "|\n  inline\n  block"},
		refused:  []string{"1", "1.5", "[a]"},
	},
	spec.IdentList: {
		accepted: []string{"[a, b]", "[]", "\n  - a\n  - b"},
		refused:  []string{"a", `"a"`, "1", `["a"]`, "[1]"},
	},
	spec.StringList: {
		accepted: []string{`["a", "b"]`, "[]", "[a, b]", "\n  - \"a\"\n  - b"},
		refused:  []string{`"a"`, "a", "1", "[1]"},
	},
	spec.ToolList: {
		accepted: []string{`[a, b.c, mcp.x.*, "lit-name"]`, "[]", "\n  - a\n  - mcp.x.*"},
		refused:  []string{"a", `"a"`, "1", "[1]"},
	},
	spec.SkillList: {
		accepted: []string{`["kebab-name", dotted.ident]`, "[]", "\n  - \"kebab-name\"\n  - x"},
		refused:  []string{"a", `"a"`, "[1]"},
	},
	spec.MixedList: {
		accepted: []string{`["!**.evil.site", github.com]`, "[]", "\n  - \"!x\"\n  - github.com"},
		refused:  []string{"a", "1", "[1]"},
	},
	spec.IdentOrList: {
		accepted: []string{"a", "[a, b]", "[]"},
		refused:  []string{`"a"`, "1", "[1]", `["a"]`},
	},
	spec.Map: {
		accepted: []string{`{ A: "v", B: w }`, "{}", "\n  A: \"v\"\n  B: w"},
		refused:  []string{"1", "[a]", `"x"`, "x"},
	},
	spec.WithMap: {
		accepted: []string{`with { k: "v" }`, "with { n: 3, ok: true }", "with {}"},
		refused:  []string{"with 1", `with: { k: "v" }`, "with { k: [a] }", "with { k }"},
	},
	spec.Block: {
		refused: []string{"1", `"x"`, "[a]", "x"},
	},
	spec.BlockOrIdent: {
		// The parser takes any bare word (the compiler narrows it, C044);
		// the quoted spelling and a list are not a mode.
		accepted: []string{"zz_word", "\n  image: \"img\""},
		refused:  []string{`"%s"`, "1", "[%s]"},
	},
}

// witnessLine writes one witness as the property's line(s): a value after
// `name: `, a multi-line value re-indented under the property, or a whole
// line when the witness spells the property itself. `%s` in a witness is
// the property's first listed value.
func witnessLine(p spec.Property, w, indent string) string {
	if strings.Contains(w, "%s") && len(p.Values) > 0 {
		w = strings.ReplaceAll(w, "%s", p.Values[0])
	}
	var line string
	switch {
	case strings.HasPrefix(w, p.Name+" ") || strings.HasPrefix(w, p.Name+":"):
		line = w
	case strings.HasPrefix(w, "\n"):
		line = p.Name + ":" + w
	default:
		line = p.Name + ": " + w
	}
	return strings.ReplaceAll(line, "\n", "\n"+indent)
}

// TestEveryFormIsTheParsersLanguage holds each property's Form to the
// parser on the witnesses of that Form: every accepted witness parses with
// no diagnostic, every refused witness draws one. A property whose Form
// promises more or less than the parser reads is named here, with the
// witness that told them apart.
func TestEveryFormIsTheParsersLanguage(t *testing.T) {
	n := 0
	for kind, tmpl := range probes {
		k, _ := spec.Lookup(kind)
		line := strings.Replace(tmpl, "%s: 1", "%s", 1)
		at := strings.Index(line, "%s")
		indent := line[strings.LastIndex(line[:at], "\n")+1 : at]
		for _, p := range k.Properties {
			w, ok := formWitnesses[p.Form]
			if !ok {
				t.Errorf("%s.%s: form %q has no witnesses", kind, p.Name, p.Form)
				continue
			}
			for _, v := range w.accepted {
				n++
				doc := fmt.Sprintf(line, witnessLine(p, v, indent))
				if res := parser.Parse("probe.bot", doc); len(res.Diagnostics) > 0 {
					t.Errorf("%s.%s (%s): the form accepts %q, the parser refuses it: %v", kind, p.Name, p.Form, v, res.Diagnostics)
				}
			}
			for _, v := range w.refused {
				n++
				doc := fmt.Sprintf(line, witnessLine(p, v, indent))
				if res := parser.Parse("probe.bot", doc); len(res.Diagnostics) == 0 {
					t.Errorf("%s.%s (%s): the form refuses %q, the parser accepts it", kind, p.Name, p.Form, v)
				}
			}
		}
	}
	if n < 1000 {
		t.Fatalf("only %d witnesses probed — the sweep is not covering the registry", n)
	}
}

// TestEveryPropertyFormHasWitnesses: a Form a property uses without a
// witness table would be a language nothing holds to the parser.
func TestEveryPropertyFormHasWitnesses(t *testing.T) {
	for _, k := range spec.Kinds {
		for _, p := range k.Properties {
			if _, ok := formWitnesses[p.Form]; !ok {
				t.Errorf("%s.%s: form %q has no witnesses", k.Name, p.Name, p.Form)
			}
		}
	}
}
