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
// stands, and ShellScriptRefPositions — a best-effort lexer, the reading the
// renderer takes too — says four things worth a word at authoring time. All
// four are warnings, never refusals: the script runs as written.
//
//   - C343: where the shell reads nothing — single quotes, `$'…'`, a quoted
//     heredoc's body, a comment — the value does not arrive: the text holds
//     the read itself. A local shell that runs that text (`sh -c '…'`,
//     `trap '…'`, a quoted heredoc fed to `sh`, after `timeout`, `env`,
//     `xargs`, `find -exec`, …) reads the variable from the environment it
//     inherits — the safe form, no warning. A shell that does not inherit it
//     (`ssh`, `sudo`, `docker exec`, `kubectl exec`) does not receive it.
//   - C344: where bash evaluates the value — as arithmetic, as a compound
//     array assignment, or as a variable name — a crafted value runs the
//     command substitutions its array subscript holds (`a[$(cmd)]`).
//   - C345: a command's first word runs the value as a program.
//   - C346: where the value is code another shell parses — a double-quoted
//     `sh -c` string, an `eval` argument, an unquoted heredoc fed to a
//     shell, a `mapfile -C` callback, PS4 / BASH_ENV / ENV /
//     PROMPT_COMMAND, an `alias` — a crafted value runs.
//
// Once per reference and code in a body, on the first position that shows
// it; a position the reading is unsure of (ShellRefPosition.Unsure) earns
// none.
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
			case p.Code && !quiet && p.Remote:
				warn(DiagShellScriptDataCode, raw,
					"tool %q script, body line %d: %s is %s, a shell that parses the value as code — a crafted value runs (`$(cmd)`, `; cmd`) — and that does not inherit the script's environment. "+
						"Hand the value on as data, on the command's stdin (`printf '%%s' {{…}} | ssh host 'read -r v; …'`)",
					t.ID, line, raw, p.Where)
			case p.Code && !quiet:
				warn(DiagShellScriptDataCode, raw,
					"tool %q script, body line %d: %s is %s, where another shell parses the value as code — a crafted value runs (`$(cmd)`, `; cmd`). "+
						"Single-quote the text that shell runs (`sh -c 'printf %%s {{…}}'`, `trap '…' EXIT`, a heredoc with a quoted delimiter): it then reads the variable from the environment it inherits, as data — or hand the value as an argument (`sh -c '… \"$1\"' _ {{…}}`)",
					t.ID, line, raw, p.Where)
			case p.ArithEval || p.NameTaking:
				what := "evaluates the value"
				if p.NameTaking {
					what = "takes the value as a variable name"
				}
				warn(DiagShellScriptDataArith, raw,
					"tool %q script, body line %d: %s is %s, where bash %s — a crafted value runs the command substitutions its array subscript holds (`a[$(cmd)]`), or a data-chosen name sets PATH, BASH_ENV or PS4. "+
						"Untrusted data must be checked first (`case $n in ''|*[!0-9]*) exit 1;; esac` for a number, an allow-list for a name), or handled in a py/js script",
					t.ID, line, raw, p.Where, what)
			case p.CommandWord:
				warn(DiagShellScriptDataCommand, raw,
					"tool %q script, body line %d: %s is the command word, so the shell runs the value as a program — a value names any command the script's environment holds, `env` among them. "+
						"If that is the intent, check the value against the programs you allow first",
					t.ID, line, raw)
			case quiet && (!p.Code || p.Remote):
				where := p.Where
				if p.Remote {
					where += " — run by a shell that does not inherit the script's environment, so not even that shell receives it"
				}
				warn(DiagShellScriptDataNotArriving, raw,
					"tool %q script, body line %d: %s stands %s, where the shell reads nothing — the value does NOT arrive: the text holds the read of the variable the engine sets, as written (a v6.2.0 script received the value's JSON literal here). "+
						"For a payload, build it where the shell expands — an unquoted heredoc (`cat <<EOF`, which receives the value's JSON literal) or `jq -n --arg k {{…}}`; for a local shell's code (`sh -c '…'`, `trap '…'`) the single quotes are right — that shell reads the variable from its environment; for `ssh`, hand the value on stdin",
					t.ID, line, raw, where)
			}
		}
	}
}
