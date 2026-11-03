package bots

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
)

// C331 warns where a `{{vars.<server-minted credential>}}` stands in a
// `script:` body — the reference the run refuses at that node. The shipped
// corpus must be silent: every call site that spends the forge publish grant
// writes `NAME={{vars.forge_publish_token}} <program>` on a `command:` line
// and reads NAME from its environment, and the one python body that needs the
// bearer reads the placeholder itself. A row here is a bot that would die
// mid-run after the spend upstream.
//
// It is also the check's upper bound, measured on real text rather than on a
// fixture: the corpus holds the grant on command lines, ordinary vars in
// script bodies and raw `{{!vars.x}}` forms, so a condition that drops any of
// the three terms the corpus exercises — the script-body discriminator, the
// minted-name predicate, the raw-form skip — reddens here. The fourth term,
// the one-segment path, has no shape in the corpus and is pinned by the unit
// table's own row instead.
func TestNoCatalogBotReferencesAMintedCredentialInAScriptBody(t *testing.T) {
	targets := catalogWorkflowFiles(t)
	if len(targets) == 0 {
		t.Fatal("no catalog workflows found — discovery glob likely broke")
	}
	checked := 0
	for _, path := range targets {
		u := unittest.Whole(unit.LoadDir(path))
		if u.Merged == nil {
			// Named, not silent: a bot that stops merging drops out of this
			// lint, and the count below alone does not say which one left.
			t.Logf("%s: not inspected (does not merge — TestCatalogBotsParseAndCompileClean owns the parse floor)", path)
			continue
		}
		cr := ir.Compile(u.Merged)
		if cr.Workflow == nil {
			// A compile that produces no workflow returns before the ref
			// pass C331 rides (no workflow declaration, unresolved imports),
			// so its diagnostics can hold no C331 whatever the body says:
			// counting it as inspected would read silence as a verdict.
			t.Logf("%s: not inspected (does not compile to a workflow — the ref pass never ran)", path)
			continue
		}
		for _, d := range cr.Diagnostics {
			if d.Code == ir.DiagMintedVarInScriptBody {
				t.Errorf("%s: %s", path, d.Error())
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no catalog workflow compiled — discovery glob likely broke")
	}
	t.Logf("%d catalog workflow(s) compiled with no C331", checked)
}
