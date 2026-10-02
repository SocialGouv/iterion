package botscaffold

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The gallery ships what `iterion bots create` renders: a template that
// reads {{run.tree_noise}} in a tool's command, script, postcondition or
// action param hands the runtime a rendering it shell-escapes into one
// inert argument (C153) — a scope gate built on it reports an empty scope
// on every run. Executable bodies read $ITERION_TREE_NOISE instead (or
// the bang form {{!run.tree_noise}}, substituted verbatim); the prompt
// rendering is for prompts. Checked on the compiled IR of every template,
// not on the template text.
func TestNoTemplateRendersTheTreeNoiseMemberInAnExecutableBody(t *testing.T) {
	for _, tpl := range Templates() {
		spec := tpl.Spec
		spec.Slug = "channel-" + strings.ReplaceAll(tpl.ID, "_", "-")
		_, w, _ := scaffoldAndCompile(t, spec)
		for _, n := range nodesOf[*ir.ToolNode](w) {
			refs := append([]*ir.Ref{}, n.CommandRefs...)
			refs = append(refs, n.ScriptRefs...)
			refs = append(refs, n.PostcondRefs...)
			for _, p := range n.Params {
				refs = append(refs, p.Refs...)
			}
			for _, r := range refs {
				if r.Kind == ir.RefRun && len(r.Path) == 1 && r.Path[0] == "tree_noise" && !r.Unquoted {
					t.Errorf("template %s: tool %s renders %s in an executable body — read $ITERION_TREE_NOISE there instead, or write {{!run.tree_noise}} for the verbatim list", tpl.ID, n.ID, r.Raw)
				}
			}
		}
	}
}
