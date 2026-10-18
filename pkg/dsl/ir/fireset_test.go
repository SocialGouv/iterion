package ir_test

// The C167 fire set over the shipped corpus, as a CI fact. The docs say
// "measured 0 fires"; a number no test guards is a snapshot, and the walk
// that measured it compiled each file ALONE, which C030-aborts on the bots
// keeping their nodes in lib/ fragments — their nodes were invisible. This
// walk compiles each file AS A UNIT (unit.LoadDir merges the fragments,
// what `iterion validate` does), so the count covers every node, and it
// fails when a shipped workflow starts firing C167: the author either
// bounds the shell tools the cell names, or accepts the fire and states the
// new count here.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/internal/dsltest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

func TestShippedCorpusStaysSilentOnClassCoverage(t *testing.T) {
	files := dsltest.CorpusFiles(t, filepath.Join("..", "..", ".."))
	fires := 0
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		pr := parser.Parse(path, string(src))
		// A deliberately broken fixture is someone else's diagnostic; the
		// fire set is a claim about workflows that compile.
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
		// Import-units compile AS A UNIT, or their fragment nodes are
		// invisible to every pass (C030 aborts the compile).
		unresolved := false
		for _, d := range res.Diagnostics {
			if d.Code == ir.DiagUnresolvedImports {
				unresolved = true
			}
		}
		if unresolved {
			u := unit.LoadDir(path)
			if u.Merged != nil {
				res = ir.Compile(u.Merged)
			}
		}
		for _, d := range res.Diagnostics {
			if d.Code == ir.DiagPermissionClassRisk {
				fires++
				t.Errorf("%s: C167 fired on %q: %s", path, d.NodeID, strings.ToValidUTF8(d.Message, "?"))
			}
		}
	}
	if fires != 0 {
		t.Fatalf("%d C167 fires over %d corpus files — update the docs' measured count or bound the shells", fires, len(files))
	}
	t.Logf("%d corpus files walked, 0 C167 fires", len(files))
}
