package ir_test

// The C316 fire set over the shipped corpus, as a CI fact (#2427): no shipped
// bot routes a node to codex with a `tools:` list that buys the workspace-write
// sandbox. A shipped workflow that starts firing C316 either locks the node
// with `readonly: true`, declares a read-only list, or accepts the fire and
// states the new count here. Each file compiles AS A UNIT, as `iterion
// validate` does, so fragment nodes count.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/internal/dsltest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
)

func TestShippedCorpusRoutesNoCodexNodeWithAWriteBuyingList(t *testing.T) {
	files := dsltest.CorpusFiles(t, filepath.Join("..", "..", ".."))
	fires := 0
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		pr := parsertest.Parse(path, string(src))
		broken := pr.File == nil
		for _, d := range pr.Diagnostics {
			if d.Severity == parser.SeverityError {
				broken = true
			}
		}
		if broken {
			continue
		}
		res := ir.Compile(pr.File)
		unresolved := false
		for _, d := range res.Diagnostics {
			if d.Code == ir.DiagUnresolvedImports {
				unresolved = true
			}
		}
		if unresolved {
			u := unittest.Whole(unit.LoadDir(path))
			if u.Merged != nil {
				res = ir.Compile(u.Merged)
			}
		}
		for _, d := range res.Diagnostics {
			if d.Code == ir.DiagCodexToolsListBoundsNothing {
				fires++
				t.Errorf("%s: C316 fired on %q: %s", path, d.NodeID, strings.ToValidUTF8(d.Message, "?"))
			}
		}
	}
	if fires != 0 {
		t.Fatalf("%d C316 fires over %d corpus files", fires, len(files))
	}
	t.Logf("%d corpus files walked, 0 C316 fires", len(files))
}
