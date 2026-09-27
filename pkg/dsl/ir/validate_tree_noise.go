package ir

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/treenoise"
)

// ---------------------------------------------------------------------------
// C157 / C158 — the tree-noise channel in an executable body (#1555)
// ---------------------------------------------------------------------------

// validateTreeNoiseChannels guards the two silent shapes of the tree-noise
// exclusion in a tool's executable body. `{{run.tree_noise}}` is the PROMPT
// rendering of the canonical exclusion list: in a `command:`, `script:` or
// `postcondition:` the runtime shell-escapes the rendering into ONE argument
// that matches no file, so the exclusion vanishes in silence — a `git
// status`-based gate lists the noise anyway, `git add` refuses the pathspec
// (C153's catalogue entry documents the shape; the review-fanout scaffold
// shipped exactly that defect until #1530's verdict caught it). The
// legitimate executable-body spellings are the bang form `{{!run.tree_noise}}`
// (substituted verbatim) and `$ITERION_TREE_NOISE` unquoted (word-split).
//
// The sibling shape is the QUOTED env var: "$ITERION_TREE_NOISE" collapses
// the list into one pathspec that matches nothing — git exits 0, the whole
// tree stages, and `validate --exec` reports the gate "clean".
//
// Both are warnings, like C153: yesterday's bot keeps compiling, and the
// diagnostic names the fix. Neither walks a prompt body — there the rendered
// member is exactly what an agent pastes, the channel #1464 built.
func (c *compiler) validateTreeNoiseChannels(w *Workflow) {
	for _, n := range w.Nodes {
		t, ok := n.(*ToolNode)
		if !ok {
			continue
		}
		bodies := []struct {
			where string
			text  string
			refs  []*Ref
			shell bool // the body runs through bash -c (a script: only when its language is a shell)
		}{
			{"command", t.Command, t.CommandRefs, true},
			{"script", t.Script, t.ScriptRefs, t.Language == "" || t.Language == "sh" || t.Language == "bash"},
			{"postcondition", t.Postcondition, t.PostcondRefs, true},
		}
		for _, b := range bodies {
			if b.text == "" {
				continue
			}
			for _, ref := range b.refs {
				// Only the plain member: a sub-field is already C153's,
				// and the bang form is the remedy, not the defect.
				if ref.Kind != RefRun || ref.Unquoted ||
					len(ref.Path) != 1 || ref.Path[0] != "tree_noise" {
					continue
				}
				c.warnfAt(DiagTreeNoiseRefInExecBody, t.ID, "",
					"tool %q %s: {{run.tree_noise}} in an executable body renders shell-escaped as ONE argument that matches no file — the tree-noise exclusion vanishes in silence (a `git status` gate lists the noise anyway; `git add` refuses the pathspec). "+
						"Write {{!run.tree_noise}} for a verbatim substitution, or read $%s unquoted — the engine exports it to every tool process",
					t.ID, b.where, treenoise.TreeNoiseEnvVar)
			}
			if b.shell {
				if q := quotedTreeNoiseEnv(b.text); q != "" {
					c.warnfAt(DiagTreeNoiseEnvQuoted, t.ID, "",
						"tool %q %s: %s is quoted — the list collapses into ONE pathspec that matches nothing, so the exclusion silently stops applying (git exits 0 and the whole tree stages). "+
							"Drop the quotes: $%s is pre-quoted pathspecs meant to word-split",
						t.ID, b.where, q, treenoise.TreeNoiseEnvVar)
				}
			}
		}
	}
}

// quotedTreeNoiseEnv returns the quoted form of the tree-noise env var the
// body carries — "$ITERION_TREE_NOISE", '$ITERION_TREE_NOISE' or the braced
// spellings — or "" when the var appears unquoted or not at all. Quoting is
// the whole defect: it is what stops the word-splitting the list needs.
func quotedTreeNoiseEnv(body string) string {
	v := treenoise.TreeNoiseEnvVar
	for _, q := range []string{`"$` + v + `"`, `'$` + v + `'`, `"${` + v + `}"`, `'${` + v + `}'`} {
		if strings.Contains(body, q) {
			return fmt.Sprintf("%q", q)
		}
	}
	return ""
}
