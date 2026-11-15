package ir

import (
	"slices"
	"strings"
)

// ---------------------------------------------------------------------------
// C343 – C346 — what a data reference's position in a shell script does
// ---------------------------------------------------------------------------
//
// A sh or bash `script:` receives each data reference (ShellScriptData)
// through its interpreter's environment: the reference renders to a read of
// a variable the engine sets, and the value never enters the script's text
// (#3291). What the shell then does with the read depends on where it
// stands, and ShellScriptRefPositions — the reading the renderer takes — says
// four things worth a word at authoring time:
//
//   - C343: where the shell reads nothing — single quotes, `$'…'`, a quoted
//     heredoc's body, a comment — the value does not arrive: the text holds
//     the read itself. A shell that runs that text (`sh -c '…'`, `trap '…'`,
//     a quoted heredoc fed to `sh`) reads the variable from its environment,
//     the safe form: no warning there (Code).
//   - C344: where bash evaluates the value — as arithmetic, or as a
//     variable name, whose array subscript it evaluates the same way — a
//     crafted value runs the command substitutions it holds (`a[$(cmd)]`).
//     The run refuses a value that is not an integer (an identifier, for a
//     name) at such a position; the warning says so ahead of the run.
//   - C345: a command's first word runs the value as a program.
//   - C346: where the value is code another shell parses — a double-quoted
//     `sh -c` string, an `eval` argument, an unquoted heredoc fed to a
//     shell, PS4 / BASH_ENV / ENV / PROMPT_COMMAND, an `alias` — a crafted
//     value runs.
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
		warn := func(code DiagCode, raw string, format string, args ...any) {
			if warned[string(code)+raw] {
				return
			}
			warned[string(code)+raw] = true
			c.warnfAtProperty(code, "tool", t.ID, "script", format, args...)
		}
		for _, span := range TemplateSpans(t.Script) {
			raw := t.Script[span[0]:span[1]]
			p := positions[span[0]]
			if !data[raw] || p.Unsure {
				continue
			}
			line := strings.Count(t.Script[:span[0]], "\n") + 1
			quiet := p.Context == ShellRefSingle || p.Context == ShellRefLiteralHeredoc || p.Context == ShellRefComment
			switch {
			case p.Code && !quiet:
				warn(DiagShellScriptDataCode, raw,
					"tool %q script, body line %d: %s is %s, where another shell parses the value as code — a crafted value runs (`$(cmd)`, `; cmd`). "+
						"Single-quote the text that shell runs (`sh -c 'printf %%s {{…}}'`, `trap '…' EXIT`, a heredoc with a quoted delimiter): it then reads the variable from the environment it inherits, as data — or hand the value as an argument (`sh -c '… \"$1\"' _ {{…}}`)",
					t.ID, line, raw, p.Where)
			case p.ArithEval || p.NameTaking:
				what := "as an arithmetic expression"
				refuse := "a value that is not an integer"
				if p.NameTaking {
					what, refuse = "as a variable name", "a value that is not a plain identifier"
				}
				warn(DiagShellScriptDataArith, raw,
					"tool %q script, body line %d: %s is %s, where bash evaluates the value %s — the array subscript a crafted value holds runs its command substitutions (`a[$(cmd)]`). The run refuses %s at this position",
					t.ID, line, raw, p.Where, what, refuse)
			case p.CommandWord:
				warn(DiagShellScriptDataCommand, raw,
					"tool %q script, body line %d: %s is the command word, so the shell runs the value as a program — a value names any command the script's environment holds, `env` among them. "+
						"If that is the intent, check the value against the programs you allow first",
					t.ID, line, raw)
			case quiet && !p.Code:
				warn(DiagShellScriptDataNotArriving, raw,
					"tool %q script, body line %d: %s stands %s, where the shell reads nothing — the value does NOT arrive: the text holds the read of the variable the engine sets, as written (a v6.2.0 script received the value's JSON literal here). "+
						"For a payload, build it where the shell expands — an unquoted heredoc (`cat <<EOF`, which receives the value's JSON literal) or `jq -n --arg k {{…}}` — and for another shell's code keep the single quotes: that shell reads the variable from its environment",
					t.ID, line, raw, p.Where)
			}
		}
	}
}
