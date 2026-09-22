package spec

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every inline list of the grammar is read by one parser routine, which
// closes the list on a trailing comma: the grammar says so on each of its
// inline-list productions, or the machine grammar contradicts the parser
// for the one it forgot. Held to the file, so a sixth list form cannot be
// added without the allowance.
func TestEveryInlineListProductionAllowsTheTrailingComma(t *testing.T) {
	path := filepath.Join("..", "..", "..", "docs", "grammar", "iterion_v1.ebnf")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	inline := regexp.MustCompile(`^([a-z_]+_list) = "\[" `)
	seen := 0
	for i, line := range strings.Split(string(src), "\n") {
		m := inline.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		seen++
		if !strings.Contains(line, `[ "," ] ] "]"`) {
			t.Errorf("%s:%d: the inline production %s does not allow the trailing comma the parser accepts", path, i+1, m[1])
		}
	}
	if seen < 5 {
		t.Fatalf("only %d inline-list productions found; the grammar has at least five (ident, string, string-or-ident, tool ref, skill ref)", seen)
	}
}
