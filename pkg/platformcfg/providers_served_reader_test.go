package platformcfg

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/sweepwalk"
)

// providersServedField is the registry field this guard holds a claim about.
const providersServedField = "ProvidersServed"

// RunnerPool.ProvidersServed is a boundary field an operator fills in the
// registry, and its own doc comment used to say the publisher treated a
// provider the pool serves as funded for the pool's teams. Nothing did: the
// funding walk answers "is this route env-funded?" from the route's own
// provider, over the whole provider vocabulary (model.EffectiveProviders /
// knownPoolProviders), and no production file names this field at all.
// docs/sovereign-pools.md carried the same claim in its residual list.
//
// A false claim about a boundary field is worse than a documented gap: a
// reader planning the pool's credential work believes a consumer exists, and
// an operator believes the value they typed decides something. So the claim
// is now "recorded, read by nothing" — and this guard is what keeps that
// sentence true, because prose cannot notice the day a reader lands.
//
// What it locks: every selector spelling `.ProvidersServed` in the module's
// own non-test Go. A reader there must land WITH the sentence it makes false
// rewritten, here and in docs/sovereign-pools.md. The field's declaration is
// an *ast.Field, not a selector, so the declaration itself is never an
// offender; the coverage check below is what distinguishes "no reader" from
// "this walk no longer matches the field".
//
// Writes are out of scope on purpose: the super-admin registry route decodes
// the operator's JSON into the struct, so the field is written without any
// file naming it. "Recorded" is exactly that write.
func TestProvidersServedHasNoProductionReader(t *testing.T) {
	var readers []string
	declarations := 0
	sweepwalk.GoFiles(t, sweepwalk.ModuleRoot(t), func(rel, path string) {
		// Test files may read it as a fixture; the claim is about production.
		if strings.HasSuffix(rel, "_test.go") {
			return
		}
		// Another tree's sources: vendored dependencies and the SDKs under
		// third_party/ belong to their upstreams, and sweepwalk's set prunes
		// vendor/ by name only.
		if strings.HasPrefix(rel, "vendor/") || strings.HasPrefix(rel, "third_party/") {
			return
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				if node.Sel.Name == providersServedField {
					readers = append(readers, fmt.Sprintf("%s:%d", rel, fset.Position(node.Sel.Pos()).Line))
				}
			case *ast.Field:
				for _, name := range node.Names {
					if name.Name == providersServedField {
						declarations++
					}
				}
			}
			return true
		})
	})

	sort.Strings(readers)
	if len(readers) > 0 {
		t.Errorf("%d production site(s) now read RunnerPool.%s:\n  %s\n"+
			"The field's doc comment and the residual list of docs/sovereign-pools.md both say it is recorded and read by nothing. Say what the reader does in those two places, then this guard's allowance belongs here.",
			len(readers), providersServedField, strings.Join(readers, "\n  "))
	}
	if declarations == 0 {
		t.Fatalf("no struct field named %s in the module's non-test Go: the field was renamed or removed, so this guard is UNLOCKED rather than clean — it reports readers only", providersServedField)
	}
}
