package ir

import (
	"regexp"
	"strings"

	"github.com/SocialGouv/iterion/pkg/treenoise"
)

// ---------------------------------------------------------------------------
// C157 / C158 — the tree-noise channel in an executable body (#1555)
// ---------------------------------------------------------------------------

// validateTreeNoiseChannels guards the two silent shapes of the tree-noise
// exclusion in a tool's executable body. Both checks are gated on SHELL
// bodies (`command:`, `postcondition:`, and a `script:` whose language is
// sh/bash/empty): a js/py script body renders its refs as JSON literals
// (resolveScriptTemplate, unconditional over languages), so
// `const noise = {{run.tree_noise}};` there is valid code consuming the
// list as data — and both shell fixes break it (the bang form splices raw
// pathspecs, a SyntaxError; a bare $ITERION_TREE_NOISE never env-expands in
// a script, expandBracedEnv answers ${NAME} only).
//
// On a shell body, `{{run.tree_noise}}` is the PROMPT rendering of the
// canonical exclusion list: a command/postcondition shell-escapes it into
// ONE argument, a shell script renders it as ONE JSON-quoted string — both
// a single word that matches no file, so the exclusion vanishes in silence
// (a `git status`-based gate lists the noise anyway, `git add` refuses the
// pathspec; the review-fanout scaffold shipped exactly that defect until
// #1530's verdict caught it). The legitimate shell spellings are the bang
// form `{{!run.tree_noise}}` (substituted verbatim) and
// `$ITERION_TREE_NOISE` unquoted (word-split).
//
// The sibling shape is the QUOTED env var. Double quotes expand the var as
// ONE word — the list collapses into one pathspec that matches nothing, git
// exits 0 and the whole tree stages. Single quotes never expand at all —
// the literal text reaches the command. Both silences are C158, each with
// its own mechanism in the message.
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
			shell bool // the body runs through a shell (a script: only when its language is sh/bash/empty)
		}{
			{"command", t.Command, t.CommandRefs, true},
			{"script", t.Script, t.ScriptRefs, t.Language == "" || t.Language == "sh" || t.Language == "bash"},
			{"postcondition", t.Postcondition, t.PostcondRefs, true},
		}
		for _, b := range bodies {
			if b.text == "" || !b.shell {
				continue
			}
			for _, ref := range b.refs {
				// Only the plain member: a sub-field is already C153's,
				// and the bang form is the remedy, not the defect.
				if ref.Kind != RefRun || ref.Unquoted ||
					len(ref.Path) != 1 || ref.Path[0] != "tree_noise" {
					continue
				}
				// The AST keeps no per-property span (the lot-1b work), so
				// the position travels in the message: the body-relative
				// line of the reference, located by its raw text.
				line := bodyLineOf(b.text, ref.Raw)
				if b.where == "script" {
					c.warnfAt(DiagTreeNoiseRefInExecBody, t.ID, "",
						"tool %q script, body line %d: {{run.tree_noise}} renders as ONE JSON-quoted string (a script body renders its refs as JSON literals), so the pathspec list reaches the shell as a single word that matches no file — the tree-noise exclusion vanishes in silence (a `git status` gate lists the noise anyway; `git add` refuses the pathspec). "+
							"Write {{!run.tree_noise}} for a verbatim substitution, or read $%s unquoted — the engine exports it to every tool process",
						t.ID, line, treenoise.TreeNoiseEnvVar)
					continue
				}
				c.warnfAt(DiagTreeNoiseRefInExecBody, t.ID, "",
					"tool %q %s, body line %d: {{run.tree_noise}} renders shell-escaped as ONE argument that matches no file — the tree-noise exclusion vanishes in silence (a `git status` gate lists the noise anyway; `git add` refuses the pathspec). "+
						"Write {{!run.tree_noise}} for a verbatim substitution, or read $%s unquoted — the engine exports it to every tool process",
					t.ID, b.where, line, treenoise.TreeNoiseEnvVar)
			}
			for _, q := range scanQuotedTreeNoiseEnv(b.text) {
				if q.double {
					c.warnfAt(DiagTreeNoiseEnvQuoted, t.ID, "",
						"tool %q %s, body line %d: %s — inside double quotes $%s expands as ONE word, so the pathspec list collapses into a single argument that matches nothing (git exits 0 and the whole tree stages). "+
							"Drop the quotes so the list word-splits — quoting stays mandatory inside [ … ] tests and on an assignment's right-hand side, which this check leaves alone",
						t.ID, b.where, q.line, q.text, treenoise.TreeNoiseEnvVar)
					continue
				}
				c.warnfAt(DiagTreeNoiseEnvQuoted, t.ID, "",
					"tool %q %s, body line %d: %s — single quotes never expand, so the literal text reaches the command and excludes nothing. "+
						"Read $%s unquoted: the engine exports pre-quoted pathspecs meant to word-split (quoting stays mandatory inside [ … ] tests and on an assignment's right-hand side)",
					t.ID, b.where, q.line, q.text, treenoise.TreeNoiseEnvVar)
			}
		}
	}
}

// bodyLineOf returns the 1-based line of needle inside body — the position
// the message carries where the AST keeps no per-property span. 0 when the
// needle is absent (a ref reconstructed from an expression, say), so the
// message degrades to "body line 0" rather than naming a wrong one.
func bodyLineOf(body, needle string) int {
	i := strings.Index(body, needle)
	if i < 0 {
		return 0
	}
	return strings.Count(body[:i], "\n") + 1
}

// treeNoiseQuote is one quoted occurrence of the tree-noise env var in a
// shell body: the quoted span as written (quotes included), its quote style,
// and the 1-based line of the body it sits on.
type treeNoiseQuote struct {
	text   string
	line   int
	double bool // double quotes expand (to ONE word); single quotes never expand
}

// treeNoiseEnvBare and treeNoiseEnvBraced are the two spellings of the var
// the scanner recognises — the braced one by PREFIX, so a defaulted form
// ("${ITERION_TREE_NOISE:-}") counts: it collapses exactly the same.
func treeNoiseEnvReferenced(s string) bool {
	v := treenoise.TreeNoiseEnvVar
	return strings.Contains(s, "$"+v) || strings.Contains(s, "${"+v)
}

// scanQuotedTreeNoiseEnv walks a shell body line by line and returns every
// quoted span that references the tree-noise env var, minus the shapes where
// quoting is MANDATORY and the remedy would break the line:
//
//   - inside a `[ … ]` / `[[ … ]]` / `test …` test — word-splitting corrupts
//     the test (`[ -n $VAR ]` unquoted is TRUE on an empty var), so quoting
//     is the correct form there;
//   - on an assignment's right-hand side (`NOISE="$VAR"`, optionally behind
//     local/export/declare/readonly/typeset) — an assignment never
//     word-splits, and the quoted form preserves the list as ONE value the
//     later unquoted `$NOISE` then splits.
//
// The scan is LINE-anchored. A `#` at a word boundary opens a comment whose
// apostrophes are not quotes (a comment's `tool's` used to unbalance the
// pairing and misframe a sound `'…' $VAR '…'` line as one quoted span —
// measured on the catalogue), and a quote left open at end of line (a
// multi-line string, or prose) ends the scan of that line: a miss is cheap,
// a mispaired span corrupts every line after it.
func scanQuotedTreeNoiseEnv(body string) []treeNoiseQuote {
	var hits []treeNoiseQuote
	for ln, line := range strings.Split(body, "\n") {
		hits = append(hits, scanQuotedTreeNoiseLine(line, ln+1)...)
	}
	return hits
}

func scanQuotedTreeNoiseLine(line string, lineno int) []treeNoiseQuote {
	var hits []treeNoiseQuote
	for i := 0; i < len(line); {
		ch := line[i]
		switch {
		case ch == '#' && (i == 0 || strings.ContainsRune(" \t;&|(", rune(line[i-1]))):
			return hits // comment: the rest of the line is text
		case ch == '"' || ch == '\'':
			// Find the closing quote: \" closes nothing inside double
			// quotes; a single quote escapes nothing at all.
			j := i + 1
			for j < len(line) {
				if ch == '"' && line[j] == '\\' {
					j += 2
					continue
				}
				if line[j] == ch {
					break
				}
				j++
			}
			if j >= len(line) {
				return hits // unclosed on this line: stop scanning it
			}
			if content := line[i+1 : j]; treeNoiseEnvReferenced(content) && !noiseQuoteSuppressed(line, i, j) {
				hits = append(hits, treeNoiseQuote{line[i : j+1], lineno, ch == '"'})
			}
			i = j + 1
		default:
			i++
		}
	}
	return hits
}

// noiseQuoteAssignRHS matches the text before a quoted span that is an
// assignment's right-hand side: `NAME=`, optionally behind a declaration
// keyword, in command position (line start or after ; & | or a keyword).
var noiseQuoteAssignRHS = regexp.MustCompile(`(?:^|[\s;&|])(?:(?:local|export|declare|readonly|typeset)\s+)?[A-Za-z_][A-Za-z0-9_]*=$`)

// noiseQuoteTestOpener matches a `[`, `[[` or `test` in command position —
// the openers of the test syntaxes where quoting the var is mandatory.
var noiseQuoteTestOpener = regexp.MustCompile(`(?:^|[\s;&|!(]|\b(?:if|while|until|then|elif)\s+)(\[\[?|test)(?:\s|$)`)

// noiseQuoteSuppressed reports whether the quoted span at [open, close] on
// its line is one of the mandatory-quoting shapes: an assignment RHS, or an
// operand of a test (for the bracket forms a closing `]` must follow on
// that line, so `echo "[x] $VAR"` is not mistaken for a test).
func noiseQuoteSuppressed(line string, open, close int) bool {
	before := line[:open]
	after := line[close+1:]
	if noiseQuoteAssignRHS.MatchString(before) {
		return true
	}
	// The LAST opener before the span decides: an operand of a test, not of
	// whatever command line precedes it.
	matches := noiseQuoteTestOpener.FindAllStringSubmatchIndex(before, -1)
	if matches == nil {
		return false
	}
	last := matches[len(matches)-1]
	opener := before[last[2]:last[3]]
	if opener == "test" {
		return true
	}
	return strings.Contains(after, "]")
}
