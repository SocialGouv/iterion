package parser

import (
	"bytes"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// enumReaders is one document per enum reader — every property whose value
// is a word from a fixed list — with `%s` where the value goes, and one of
// its listed words. Session, await, router mode, MCP transport and
// interaction each have their own reader; all five take the word through
// enumWord, and this holds each of them to the rule.
var enumReaders = []struct {
	name, tmpl, word string
}{
	{"session", "agent a:\n  session: %s\n", "fresh"},
	{"await", "agent a:\n  await: %s\n", "best_effort"},
	{"router mode", "router r:\n  mode: %s\n", "fan_out_each"},
	{"mcp transport", "mcp_server m:\n  transport: %s\n", "sse"},
	{"interaction", "human h:\n  interaction: %s\n", "human_or_host"},
	{"workflow interaction", "workflow w:\n  interaction: %s\n", "review"},
}

// A listed word reads quoted as it reads bare — `session: "fresh"` is
// `session: fresh` — on every enum reader, and the two spellings are the
// same document. Before, one reader out of five took the quoted form
// (interaction compared values, the others compared token types), so an
// author could not tell from one property what the next one accepted.
func TestAnEnumWordReadsQuotedAsWellAsBare(t *testing.T) {
	for _, r := range enumReaders {
		bare := Parse("b.bot", strings.ReplaceAll(r.tmpl, "%s", r.word))
		quoted := Parse("q.bot", strings.ReplaceAll(r.tmpl, "%s", `"`+r.word+`"`))
		if len(bare.Diagnostics) != 0 || len(quoted.Diagnostics) != 0 {
			t.Errorf("%s: bare %v, quoted %v", r.name, bare.Diagnostics, quoted.Diagnostics)
			continue
		}
		jb, _ := ast.MarshalFile(bare.File)
		jq, _ := ast.MarshalFile(quoted.File)
		if !bytes.Equal(jb, jq) {
			t.Errorf("%s: `%s` and `\"%s\"` read as different documents", r.name, r.word, r.word)
		}
	}
}

// Quoting does not widen the list: a word outside it is refused quoted as
// it is refused bare, by the reader's own diagnostic, which names the
// accepted words — and so is a value that is not a word at all.
func TestAnEnumRefusesAnUnlistedWordQuotedOrBare(t *testing.T) {
	for _, r := range enumReaders {
		for _, v := range []string{"zz_bogus", `"zz_bogus"`, "1", "[" + r.word + "]"} {
			res := Parse("x.bot", strings.ReplaceAll(r.tmpl, "%s", v))
			if len(res.Diagnostics) == 0 {
				t.Errorf("%s: %s accepted", r.name, v)
				continue
			}
			if d := res.Diagnostics[0]; d.Code != DiagInvalidValue || !strings.Contains(d.Message, r.word) {
				t.Errorf("%s: %s drew %s %q — want E-invalid-value naming the accepted words", r.name, v, d.Code, d.Message)
			}
		}
	}
}
