// Package dsltest holds the equivalence oracle the DSL packages' tests
// share: "the program that comes out of this transport, serialiser or
// editor is the program that went in", asked of the compiler.
package dsltest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// AssertSameProgram fails the test when two compilations do not describe the
// same program (ir.SameProgram).
func AssertSameProgram(t testing.TB, name string, direct, via *ir.CompileResult) {
	t.Helper()
	if why := ir.SameProgram(direct, via); why != "" {
		t.Errorf("%s: not the same program after the round-trip: %s", name, why)
	}
}

// CorpusFiles lists every .bot file under the repository's bots/, examples/
// and e2e/testdata — the corpus a round-trip guarantee is proven on. rel is
// the package directory's path to the repository root.
func CorpusFiles(t testing.TB, rel string) []string {
	t.Helper()
	var files []string
	for _, dir := range []string{"bots", "examples", filepath.Join("e2e", "testdata")} {
		root := filepath.Join(rel, dir)
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(path, ".bot") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(files) < 50 {
		t.Fatalf("only %d corpus files found under %s — the walk is broken", len(files), rel)
	}
	return files
}
