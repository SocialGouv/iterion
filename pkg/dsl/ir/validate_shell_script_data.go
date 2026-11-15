package ir

import (
	"slices"
	"strings"
)

// ---------------------------------------------------------------------------
// C343 / C344 — a data reference in a shell script the value does not reach,
// or that bash evaluates
// ---------------------------------------------------------------------------
//
// A sh or bash `script:` receives each data reference (ShellScriptData)
// through its interpreter's environment: the reference renders to a read of
// a variable the engine sets, and the value never enters the script's text
// (#3291). Two positions deserve a word at authoring time, both from
// ShellScriptRefPositions — the reading the renderer itself takes:
//
//   - C343: where the shell reads nothing — single quotes, a `$'…'` string,
//     the body of a heredoc whose delimiter is quoted, a comment — the text
//     holds the variable's read as written: the value arrives only if a
//     shell runs that text (`sh -c '…'`, `trap '…'`, a quoted heredoc fed to
//     `bash`), and never in a comment.
//   - C344: where bash evaluates the expanded value as an arithmetic
//     expression — `$(( … ))`, `(( … ))`, a `[[ … -gt … ]]` operand, a
//     `let` argument, a `declare -i` / `local -i` assignment, an array
//     subscript, a `${…}` subscript or offset — a crafted value runs the
//     command substitutions its array subscript holds (`a[$(cmd)]`). That
//     is the shell's evaluation of a variable, not a rendering the engine
//     can quote: the script validates the value as an integer first.
//
// Warnings: the script runs. Once per reference and code in a body, on the
// first position that shows it; a position the reading is unsure of
// (ShellRefPosition.Unsure) earns none.
func (c *compiler) validateShellScriptData(w *Workflow) {
	ids := make([]string, 0, len(w.Nodes))
	for id := range w.Nodes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		t, ok := w.Nodes[id].(*ToolNode)
		if !ok || t.Script == "" || (t.Language != "" && t.Language != "sh" && t.Language != "bash") {
			continue
		}
		data := map[string]bool{}
		for _, ref := range t.ScriptRefs {
			if ShellScriptData(ref) {
				data[ref.Raw] = true
			}
		}
		if len(data) == 0 {
			continue
		}
		positions := ShellScriptRefPositions(t.Script, t.Language == "bash")
		warned := map[string]bool{}
		for _, span := range TemplateSpans(t.Script) {
			raw := t.Script[span[0]:span[1]]
			p := positions[span[0]]
			if !data[raw] || p.Unsure {
				continue
			}
			line := strings.Count(t.Script[:span[0]], "\n") + 1
			switch {
			case p.ArithEval && !warned["C344"+raw]:
				warned["C344"+raw] = true
				c.warnfAtProperty(DiagShellScriptDataArith, "tool", t.ID, "script",
					"tool %q script, body line %d: %s is %s, where bash evaluates the value as an arithmetic expression — a crafted value runs the command substitutions its array subscript holds (`a[$(cmd)]`). "+
						"Check the value is an integer before bash evaluates it (`n={{…}}; case $n in ''|*[!0-9]*) exit 1;; esac`)",
					t.ID, line, raw, p.Where)
			case (p.Context == ShellRefSingle || p.Context == ShellRefLiteralHeredoc || p.Context == ShellRefComment) && !warned["C343"+raw]:
				warned["C343"+raw] = true
				c.warnfAtProperty(DiagShellScriptDataNotArriving, "tool", t.ID, "script",
					"tool %q script, body line %d: %s stands %s, where the shell reads nothing — a shell script receives a data reference as a variable of its environment, so this text holds the variable's read, not the value: the value arrives only if a shell runs the text (`sh -c '…'`, `trap '…'`, a heredoc fed to `bash`). "+
						"Write the reference where the shell expands it — outside the single quotes, in an unquoted heredoc — or assign it first (`v={{…}}`) and hand `\"$v\"` on",
					t.ID, line, raw, p.Where)
			}
		}
	}
}
