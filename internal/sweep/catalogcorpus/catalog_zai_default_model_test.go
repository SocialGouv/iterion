package catalogcorpus

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// minZaiGLMRoutes is how many z.ai GLM routes the shipped catalogue declares
// at least: the rescue route of review-pr's reviewer_alternate, revi-converse's
// converse_agent and gitops-warden's classify. A walk that finds fewer has
// stopped seeing the catalogue, and would pass on anything.
const minZaiGLMRoutes = 3

// zaiGLMID reports whether a model spec is a GLM id served by z.ai, and its
// bare id. z.ai serves GLM on the anthropic wire, bare (`glm-5.4`, as
// claude_code and pi take it) or behind claw's anthropic provider
// (`anthropic/glm-5.4`); the `zai/` spelling reaches it whole. Any other
// leading component is another provider's route to its own GLM build
// (`openai_compatible/scaleway/glm-5.3`, `openrouter/z-ai/glm-4.6`), which
// serves the versions that provider ships, not z.ai's.
func zaiGLMID(spec string) (string, bool) {
	id := strings.TrimSpace(spec)
	for _, prefix := range []string{"anthropic/", "zai/"} {
		if strings.HasPrefix(id, prefix) {
			id = strings.TrimPrefix(id, prefix)
			break
		}
	}
	if strings.Contains(id, "/") || !strings.HasPrefix(strings.ToLower(id), "glm") {
		return "", false
	}
	return id, true
}

// isDefaultGLM accepts the default GLM and its variants of the same
// generation (`glm-5.4-flash`), never another generation (`glm-5.3`,
// `glm-5.40`).
func isDefaultGLM(id string) bool {
	id = strings.ToLower(id)
	return id == secrets.ZAIDefaultModel || strings.HasPrefix(id, secrets.ZAIDefaultModel+"-")
}

// TestCatalogZaiRoutesNameTheDefaultGLM: every route of the shipped catalogue
// that lands on z.ai's GLM names secrets.ZAIDefaultModel — a node's model, a
// fallback's, and the model of a `zai:<model>` provider-chain element, each
// expanded the way dispatch reads it with nothing set. A newer GLM is adopted
// by moving that constant; this test then names every route still on the old
// one, so no bot keeps rescuing onto a superseded model unnoticed.
//
// Mutated: reviewer_alternate's route put back on glm-5.3 (red, on that
// route alone); the review-pr README and sec-audit examples are prose, not
// routes, and are not this test's to read.
func TestCatalogZaiRoutesNameTheDefaultGLM(t *testing.T) {
	targets := catalogWorkflowFiles(t)
	if len(targets) == 0 {
		t.Fatal("no catalog workflows found — discovery glob likely broke")
	}
	nothingSet := func(string) string { return "" }
	seen := 0
	var failures []string
	check := func(path, where, spec string) {
		id, ok := zaiGLMID(ir.ExpandWithDefault(spec, nothingSet))
		if !ok {
			return
		}
		seen++
		if !isDefaultGLM(id) {
			failures = append(failures, fmt.Sprintf("%s: %s routes z.ai to %q, want %s (secrets.ZAIDefaultModel) — "+
				"a GLM route left on a superseded id keeps rescuing onto it", path, where, id, secrets.ZAIDefaultModel))
		}
	}
	for _, path := range targets {
		u := unittest.Whole(unit.LoadDir(path))
		if u.Merged == nil {
			t.Errorf("%s: no merged file — this test could not judge it (see TestCatalogBotsParseAndCompileClean)", path)
			continue
		}
		cr := ir.Compile(u.Merged)
		if cr.Workflow == nil {
			t.Errorf("%s: compile produced no workflow — this test could not judge it (see TestCatalogBotsParseAndCompileClean)", path)
			continue
		}
		ids := make([]string, 0, len(cr.Workflow.Nodes))
		for id := range cr.Workflow.Nodes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, nodeID := range ids {
			node, ok := cr.Workflow.Nodes[nodeID].(ir.LLMNode)
			if !ok {
				continue
			}
			fields := node.GetLLMFields()
			check(path, fmt.Sprintf("node %q model", nodeID), fields.Model)
			for _, token := range strings.Split(ir.ExpandWithDefault(fields.Provider, nothingSet), ",") {
				if hint, model, hasModel := ir.SplitProviderStep(strings.TrimSpace(token)); hasModel && hint == string(secrets.ProviderZAI) {
					check(path, fmt.Sprintf("node %q provider-chain element %q", nodeID, token), model)
				}
			}
			for _, fb := range node.GetFallbacks() {
				check(path, fmt.Sprintf("node %q fallback %q model", nodeID, fb.Name), fb.Model)
			}
		}
	}
	for _, f := range failures {
		t.Error(f)
	}
	if seen < minZaiGLMRoutes {
		t.Errorf("the walk saw %d z.ai GLM routes, want at least %d — it no longer reaches the catalogue's rescue routes "+
			"and would pass on anything", seen, minZaiGLMRoutes)
	}
}
