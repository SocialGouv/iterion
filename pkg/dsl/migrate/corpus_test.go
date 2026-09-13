package migrate

import (
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/internal/dsltest"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// Every shipped bot, example and fixture migrates without a refusal, and
// the rewrite touches nothing but the header and the literals that hold a
// backslash: the corpus is the proof the migrator is safe to run on the
// catalogue, without migrating it here (that is its own reviewed lot).
func TestMigrateDryRunOverTheCorpus(t *testing.T) {
	files := dsltest.CorpusFiles(t, "../../..")
	literals := 0
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		res, err := Bytes(path, src, Options{})
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if !res.Changed {
			t.Errorf("%s: a profile-1 file that did not change", path)
			continue
		}
		for _, c := range res.Changes {
			switch c.Kind {
			case "header":
			case "literal":
				literals++
				if !strings.Contains(c.From, `\`) {
					t.Errorf("%s:%d: a literal without a backslash was rewritten: %s", path, c.Line, c.From)
				}
			default:
				t.Errorf("%s:%d: unexpected change %s (the corpus has no directive)", path, c.Line, c.Kind)
			}
		}
		after := parser.Parse(path, string(res.Migrated))
		if after.File.EffectiveProfile() != 2 || len(parseErrors(after.Diagnostics)) > 0 {
			t.Errorf("%s: migrated text reads as profile %d with %v", path, after.File.EffectiveProfile(), after.Diagnostics)
		}
	}
	if literals == 0 {
		t.Fatalf("no literal was re-spelled over the corpus — the corpus measurement found nine")
	}
}
