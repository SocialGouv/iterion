package bots

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// concatStringArg matches a `concat(` call whose FIRST argument is a string
// literal — the one shape that is always wrong.
var concatStringArg = regexp.MustCompile(`concat\(\s*['"]`)

// TestCatalogExprConcatIsArrayOnly pins a defect that only appears at RUNTIME,
// and only on the branch nobody exercises.
//
// `concat` is the ARRAY builtin (pkg/dsl/expr: "concat() argument 1 is %T,
// want array"); string concatenation is `+`. A compute node written as
// concat('prefix: ', outputs.x.log) parses, compiles and VALIDATES clean —
// then dies with a type error the first time it evaluates.
//
// Which is precisely when it hurts: these expressions live in `notice` and
// `fail_log` fields, inside if(converged, ..., <the concat>) — the failure
// branch. The node crashes only when something else has already gone wrong,
// turning a reported failure into a dead run. Shipped once in a bundled
// subbot's refusal path, found by a sibling bot hitting the same shape on its
// first real run.
func TestCatalogExprConcatIsArrayOnly(t *testing.T) {
	var targets []string
	for _, root := range []string{".", "../examples"} {
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".bot") {
				return nil
			}
			targets = append(targets, path)
			return nil
		}); err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(targets) == 0 {
		t.Fatal("no catalog workflows found — discovery likely broke")
	}

	for _, path := range targets {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: read: %v", path, err)
			continue
		}
		for i, line := range strings.Split(string(src), "\n") {
			if concatStringArg.MatchString(line) {
				t.Errorf("%s:%d: concat() is called on a string literal; it is the ARRAY builtin and "+
					"fails at evaluation time with \"want array\". Use `+` for string concatenation.\n  %s",
					path, i+1, strings.TrimSpace(line))
			}
		}
	}
}
