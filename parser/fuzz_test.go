package parser_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/parser"
)

// ---------------------------------------------------------------------------
// FuzzParse — detect panics in the parser on malformed inputs
// ---------------------------------------------------------------------------

// loadFixtureSeeds adds all .iter fixtures from examples/ as seed corpus entries.
func loadFixtureSeeds(f *testing.F) {
	f.Helper()
	for _, glob := range []string{"../examples/*.iter", "../examples/skill/*.iter"} {
		paths, err := filepath.Glob(glob)
		if err != nil {
			f.Fatal(err)
		}
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				f.Fatal(err)
			}
			f.Add(string(data))
		}
	}
}

func FuzzParse(f *testing.F) {
	loadFixtureSeeds(f)
	f.Add("")
	f.Add("\n\n\n")
	f.Add("agent a:\n  model: x\n")
	f.Add(strings.Repeat("  ", 200) + "deep\n")
	f.Add("workflow w:\n" + strings.Repeat("  a -> b\n", 100))
	f.Add("schema s:\n  field: string\n  field: int\n")

	f.Fuzz(func(t *testing.T, src string) {
		res := parser.Parse("fuzz.iter", src)
		if res == nil {
			t.Fatal("Parse returned nil")
		}
	})
}

// ---------------------------------------------------------------------------
// FuzzLexer — detect panics in the lexer on malformed inputs
// ---------------------------------------------------------------------------

func FuzzLexer(f *testing.F) {
	loadFixtureSeeds(f)
	f.Add("")
	f.Add("\t\t\t")
	f.Add(strings.Repeat("  ", 200) + "x\n")
	f.Add("\"unterminated string\n")

	f.Fuzz(func(t *testing.T, src string) {
		lex := parser.NewLexer("fuzz.iter", src)
		for {
			tok := lex.Next()
			if tok.Type == parser.TokenEOF || tok.Type == parser.TokenError {
				break
			}
		}
	})
}
