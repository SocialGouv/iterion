package ir

import (
	"slices"
	"strings"
)

// ---------------------------------------------------------------------------
// C342 — a secret reference a shell script expands nothing at
// ---------------------------------------------------------------------------
//
// A shell `script:` receives a value secret's `{{secrets.NAME}}` through its
// environment: the reference renders a read of a variable the engine sets
// for the interpreter (`"$…"`), so the value never enters the script's text
// and no quoting the author put around the reference can be closed by it
// (ADR-146, amendment of 2026-11-14). Where the shell expands nothing — a
// single-quoted span, the body of a heredoc whose delimiter is quoted — the
// script receives that variable's name instead of the value, with no error.
// C342 names the position at authoring time.
//
// Silent by design: the raw `{{!secrets.NAME}}` form (the author's own text,
// escaped for a single-quoted word), a file secret (its reference renders
// the mounted path), a js or py script, and every position the shell
// expands — unquoted, double-quoted, an unquoted heredoc.
//
// A warning: the script runs, and the author may want the name.
func (c *compiler) validateQuotedSecretInShellScripts(w *Workflow) {
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
		var sc *shellScan
		seen := map[string]bool{}
		for _, ref := range t.ScriptRefs {
			if ref == nil || ref.Kind != RefSecrets || ref.Unquoted || len(ref.Path) == 0 || seen[ref.Raw] {
				continue
			}
			seen[ref.Raw] = true
			decl, declared := w.Secrets[ref.Path[0]]
			if !declared || decl.IsFile() {
				continue
			}
			if sc == nil {
				scan := scanShellBody(t.Script)
				sc = &scan
			}
			at := literalShellOccurrence(t.Script, ref.Raw, *sc)
			if at < 0 {
				continue
			}
			c.warnfAtProperty(DiagSecretInShellLiteral, "tool", t.ID, "script",
				"tool %q script, body line %d: %s stands where the shell expands nothing (single quotes, or a heredoc whose delimiter is quoted) — a shell script receives the secret's value through a variable the engine sets, so the script reads the variable's name there, not the value. "+
					"Write the reference where the shell expands it (outside the single quotes, or in an unquoted heredoc), or use the raw {{!secrets.%s}} form, which writes the value escaped for single quotes",
				t.ID, strings.Count(t.Script[:at], "\n")+1, ref.Raw, ref.Path[0])
		}
	}
}

// literalShellOccurrence is the offset of the first occurrence of raw in
// body that stands in a single-quoted span or a quoted heredoc's body, -1
// when none does.
func literalShellOccurrence(body, raw string, sc shellScan) int {
	for off := 0; off < len(body); {
		i := strings.Index(body[off:], raw)
		if i < 0 {
			return -1
		}
		i += off
		if inRanges(sc.single, i) || inRanges(sc.literal, i) {
			return i
		}
		off = i + 1
	}
	return -1
}
