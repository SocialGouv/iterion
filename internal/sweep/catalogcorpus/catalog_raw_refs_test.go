package catalogcorpus

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestCatalogHasNoAuthorQuotedRefs(t *testing.T) {
	// The catalog AND the shipped examples: an example is copied by whoever
	// starts a bot, so an injectable shape there propagates further than one
	// in a catalog bot. The whole corpus table, floored per root, carries
	// both — the children and the loose files this guard used to glob for
	// itself included.
	paths := catalogWorkflowFiles(t)
	for _, path := range paths {
		pr := parseBotUnit(path)
		if pr.File == nil {
			t.Logf("%s: not inspected (unparseable — the parse/compile test owns that)", path)
			continue
		}
		cr := ir.Compile(pr.File)
		// C137 is emitted by the compiler; the parser's own diagnostics are a
		// different type and a different concern.
		for _, d := range cr.Diagnostics {
			if d.Code == ir.DiagQuotedCommandRef {
				t.Errorf("%s: %s\n(the runtime shell-quotes refs; author quotes CANCEL that and the value becomes shell syntax)",
					path, d.Message)
			}
		}
	}
}
