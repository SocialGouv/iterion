package ir_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/internal/dsltest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// TestCompileDoesNotMutateItsInput holds on one fixture; this holds on every
// shipped bot, example and test fixture. detachForCompile copies exactly the
// lists expandGroups appends to, and nothing but this check would notice a
// later pass writing to another list, or mutating a shared declaration, on
// a construct that fixture does not carry. The oracle is a second, untouched
// parse of the same source, compared field for field — no spelling of the
// write to enumerate.
func TestCorpusCompileIsPure(t *testing.T) {
	checked := 0
	for _, path := range dsltest.CorpusFiles(t, filepath.Join("..", "..", "..")) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		pr := parser.Parse(path, string(src))
		broken := pr.File == nil
		for _, d := range pr.Diagnostics {
			if d.Severity == parser.SeverityError {
				broken = true // a deliberately broken fixture
			}
		}
		if broken {
			continue
		}
		fresh := parser.Parse(path, string(src)).File
		ir.Compile(pr.File)
		if !reflect.DeepEqual(pr.File, fresh) {
			t.Errorf("%s: Compile changed its input", path)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no corpus file was checked")
	}
	t.Logf("%d corpus files checked", checked)
}
