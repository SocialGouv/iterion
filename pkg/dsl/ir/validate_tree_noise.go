package ir

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/treenoise"
)

// ---------------------------------------------------------------------------
// C157 / C158 — the tree-noise channel in an executable body (#1555)
// ---------------------------------------------------------------------------
//
// PRECISION OVER RECALL (round-3 requalification): a lint that misses is
// acceptable, a lint that lies is not. C158 fires ONLY on the unambiguous
// shape — a standalone, whitespace/separator-delimited word that is exactly
// one quoted span holding exactly the var (`"$ITERION_TREE_NOISE"`,
// `'…'`, `"${ITERION_TREE_NOISE}"`, or a braced parameter-expansion form
// like `"${ITERION_TREE_NOISE:-}"`). Everything embedded — a concatenation
// (`"prefix $VAR"`), a `name="$V"` argument (git would refuse it LOUD, which
// is not the silent-vanishing class), a comment, a heredoc body, an escaped
// `\$VAR` — stays silent BY DESIGN, and the catalogue entries say so.
//
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
// a single word that, read as a git pathspec list, matches no file, so the
// exclusion vanishes in silence (a `git status`-based gate lists the noise
// anyway, `git add` refuses the pathspec; the review-fanout scaffold
// shipped exactly that defect until #1530's verdict caught it). Inside a
// QUOTED span of a script the ref is interpreter-bound data, not a shell
// word (the python3 -c shape), and the check leaves it alone. The
// legitimate shell spellings are the bang form `{{!run.tree_noise}}`
// (substituted verbatim) and `$ITERION_TREE_NOISE` unquoted (word-split).
//
// The sibling shape is the QUOTED env var as a standalone word. Double
// quotes expand the var as ONE word — the list collapses into one pathspec
// that matches nothing, git exits 0 and the whole tree stages. Single
// quotes never expand at all — the literal text reaches the command. Both
// silences are C158, each with its own mechanism in the message.
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
			// One quote/comment/heredoc-aware pass per body: C158 reads the
			// hits, C157's script arm the quoted/heredoc intervals.
			sc := scanShellBody(b.text)
			seenRaw := map[string]bool{} // one C157 per body: repeats of the same ref are the same finding, at the same (first) line
			for _, ref := range b.refs {
				// Only the plain member: a sub-field is already C153's,
				// and the bang form is the remedy, not the defect.
				if ref.Kind != RefRun || ref.Unquoted ||
					len(ref.Path) != 1 || ref.Path[0] != "tree_noise" ||
					seenRaw[ref.Raw] {
					continue
				}
				seenRaw[ref.Raw] = true
				// The AST keeps no per-property span (the lot-1b work), so
				// the position travels in the message: the body-relative
				// line of the reference, located by its raw text.
				line := bodyLineOf(b.text, ref.Raw)
				if b.where == "script" {
					// A ref inside a quoted span or a heredoc of a script
					// MAY be interpreter-bound data rather than a shell
					// word (the python3 -c shape) — the check cannot tell
					// data from a collapsed word there, so it stays silent:
					// a miss is the honest direction, the lie is not.
					freeLine, free := firstFreeOccurrence(b.text, ref.Raw, sc)
					if !free {
						continue
					}
					c.warnfAt(DiagTreeNoiseRefInExecBody, t.ID, "",
						"tool %q script, body line %d: {{run.tree_noise}} renders as ONE JSON-quoted string (a script body renders its refs as JSON literals) — as a git pathspec list that word matches no file, so an exclusion meant for this line silently stops applying (inside quotes the check cannot tell data from a collapsed word, and stays silent). "+
							"Write {{!run.tree_noise}} for a verbatim substitution, or read $%s unquoted — the engine exports it to every tool process",
						t.ID, freeLine, treenoise.TreeNoiseEnvVar)
					continue
				}
				c.warnfAt(DiagTreeNoiseRefInExecBody, t.ID, "",
					"tool %q %s, body line %d: {{run.tree_noise}} renders shell-escaped as ONE word — as a git pathspec list that word matches no file, so an exclusion meant for this line silently stops applying. "+
						"Write {{!run.tree_noise}} for a verbatim substitution, or read $%s unquoted — the engine exports it to every tool process",
					t.ID, b.where, line, treenoise.TreeNoiseEnvVar)
			}
			for _, q := range sc.hits {
				if q.double {
					c.warnfAt(DiagTreeNoiseEnvQuoted, t.ID, "",
						"tool %q %s, body line %d: %s — inside double quotes $%s expands as ONE word, so the pathspec list collapses into a single argument that matches nothing (git exits 0 and the whole tree stages). "+
							"Drop the quotes so the list word-splits — inside [ … ] tests and assignments the quotes are mandatory and left alone",
						t.ID, b.where, q.line, q.text, treenoise.TreeNoiseEnvVar)
					continue
				}
				c.warnfAt(DiagTreeNoiseEnvQuoted, t.ID, "",
					"tool %q %s, body line %d: %s — single quotes never expand, so the literal text reaches the command and excludes nothing. "+
						"Read $%s unquoted: the engine exports pre-quoted pathspecs meant to word-split",
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

// treeNoiseQuote is one offending quoted word in a shell body: the quoted
// span as written (quotes included), its quote style, and the 1-based line
// of the body it starts on.
type treeNoiseQuote struct {
	text   string
	line   int
	double bool // double quotes expand (to ONE word); single quotes never expand
}

// shellScan is what one quote/comment/heredoc-aware pass over a shell body
// learned: the C158 hits, and the byte intervals that are inside a quoted
// span or a heredoc body — C157's "the ref is data there" test for scripts.
type shellScan struct {
	hits   []treeNoiseQuote
	quoted [][2]int // [open, close) of every quoted span, close exclusive
	data   [][2]int // heredoc bodies
}

// inRanges reports whether off falls inside any [start, end) interval.
func inRanges(ranges [][2]int, off int) bool {
	for _, r := range ranges {
		if off >= r[0] && off < r[1] {
			return true
		}
	}
	return false
}

// firstFreeOccurrence returns the body line of the first occurrence of raw
// that is NOT inside a quoted span or a heredoc body, or false when every
// occurrence is data-bound.
func firstFreeOccurrence(body, raw string, sc shellScan) (int, bool) {
	for off := 0; off <= len(body); {
		i := strings.Index(body[off:], raw)
		if i < 0 {
			return 0, false
		}
		i += off
		if !inRanges(sc.quoted, i) && !inRanges(sc.data, i) {
			return strings.Count(body[:i], "\n") + 1, true
		}
		off = i + 1
	}
	return 0, false
}

// treeNoiseEnvOnlyVar reports whether a quoted span's content is EXACTLY the
// tree-noise env var: `$ITERION_TREE_NOISE`, `${ITERION_TREE_NOISE}`, or a
// braced parameter-expansion form (`${ITERION_TREE_NOISE:-}`, `${…#…}`,
// `${…%…}`, …) — "optional default/prefix-suffix inside the quotes". A `\$`
// escape (literal dollar, never expands) and any surrounding text fail the
// anchor, which is what keeps the escaped and concatenated shapes silent.
func treeNoiseEnvOnlyVar(content string) bool {
	v := treenoise.TreeNoiseEnvVar
	if content == "$"+v {
		return true
	}
	prefix := "${" + v
	if !strings.HasPrefix(content, prefix) || !strings.HasSuffix(content, "}") {
		return false
	}
	rest := content[len(prefix) : len(content)-1]
	if rest == "" {
		return true
	}
	return strings.ContainsRune("-+?=#%/:", rune(rest[0]))
}

// shellBoundaryOpen separates shell words on the OPENING side of a quoted
// span: whitespace and the command separators. `=` is deliberately NOT one —
// `X="$V"` (an assignment, where quoting is mandatory, or an argument, which
// fails LOUD rather than silently) is not the standalone-word shape C158
// names; neither is a redirect target (`2>"$V"` — one word by design) or a
// `(`-glued span (a case pattern never word-splits).
func isShellBoundaryOpen(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == ';' || b == '&' || b == '|'
}

// shellBoundaryClose is the closing side: the open set plus the redirection
// and subshell closers — `git add "$V">/dev/null`, `( git add "$V")` and
// `OUT=$(git status -- "$V")` all hand the collapsed word to argv exactly
// like a blank did (measured: bash argc=1).
func isShellBoundaryClose(b byte) bool {
	return isShellBoundaryOpen(b) || b == '>' || b == '<' || b == ')'
}

// shellKeyword keeps command position across the shell's structural words,
// so `if test -n "$V"; then` still reads `test` as a command.
func shellKeyword(w string) bool {
	switch w {
	case "if", "then", "elif", "else", "while", "until", "do", "in", "!", "{", "}", "time", "coproc",
		"case", "esac":
		return true
	}
	return false
}

// scanShellBody walks a shell body once, tracking quote state ACROSS lines
// (a multi-line quoted string's interior lines are never re-paired from
// scratch — the inverted-mechanism false positives of per-line pairing),
// comments (a `#` at a word boundary outside quotes cuts the line — a
// comment's apostrophe is not a quote), escapes (`\` outside quotes and
// inside double quotes skips the next byte — the close-escape-reopen idiom
// for embedding a single quote, and `\$`),
// test regions, and heredoc bodies.
//
// Suppression regions are BOUNDED: a `test …` command ends at the first
// `&&`/`||`/`;`/`|` or newline; a bracket form (`[ … ]` / `[[ … ]]`) ends at
// its closing word `]` / `]]`, found with the same quote/comment awareness
// — a stray `]` inside an echo'd string or a comment closes nothing. A
// `case` compound is tracked with a depth counter (nesting survives): the
// case word and the pattern positions never word-split, the pattern region
// opens only from the "case word consumed, awaiting in" state, and `esac`
// closes only at command position. Arithmetic `$(( … ))` is a paren-depth
// region: no splitting, and `<<` is a shift, never a heredoc. `(` and `)`
// open a fresh command context (a `case` inside `$( … )` is still read as
// one).
//
// A heredoc body is data, never argv: its lines are consumed until the
// delimiter line and nothing inside is scanned. Command substitution
// `$(…)` is not modelled: the quotes inside one pair themselves in every
// shape measured, and a miss is the acceptable direction.
func scanShellBody(body string) shellScan {
	var (
		sc        shellScan
		quote     byte // '\'' or '"' while inside a quoted span
		spanStart int  // opening quote's offset
		spanLine  int  // its 1-based body line
		line      = 1

		wordStart = -1   // offset of the unquoted word under construction (-1: none)
		cmdStart  = true // at command position

		testCloser string // "]" / "]]" while inside a bracket test
		testCmd    bool   // inside a `test …` command
		// case tracking: a `case` at command position arms the compound
		// (depth, for nesting); the NEXT span is the case word, which never
		// word-splits; the pattern region opens ONLY from the
		// "case word consumed, awaiting in" state — a bare `in` inside an
		// arm (`for f in …`, `grep -w in …`) arms nothing.
		caseDepth   int
		casePending bool
		awaitingIn  bool
		casePattern bool   // in a case PATTERN position (between `in` and the arm's `)`, and after each `;;`/`;&`) — patterns never word-split either
		cmdName     string // first word of the current command (the echo|xargs suppression reads it)

		arithDepth int // inside $(( … )) / (( … )): no word-splitting, no heredocs

		heredocDelim string // pending: opens at the next newline
		heredocStrip bool
		heredocFrom  = -1 // active heredoc body's start offset
	)

	// flushWord evaluates the word ending at end (outside any quote).
	flushWord := func(end int) {
		if wordStart < 0 {
			return
		}
		w := body[wordStart:end]
		wordStart = -1
		// A bare case word consumed the pending flag before this word.
		casePending = false
		// Command-name tracking: the first non-keyword, non-assignment
		// word of a command (`FOO=bar echo …` leaves the position open).
		if cmdStart && cmdName == "" && !shellKeyword(w) && !isShellAssignWord(w) {
			cmdName = w
		}
		switch w {
		case "[":
			testCloser = "]"
		case "[[":
			testCloser = "]]"
		case "]":
			if testCloser == "]" {
				testCloser = ""
			}
		case "]]":
			if testCloser == "]]" {
				testCloser = ""
			}
		case "test":
			if cmdStart {
				testCmd = true
			}
		case "case":
			if cmdStart {
				casePending = true
				awaitingIn = true
				caseDepth++
			}
		case "in":
			// `case <word> in` opens the pattern list — and ONLY that
			// shape: a `for f in …` list DOES word-split, and a bare `in`
			// inside an arm is just an argument.
			if awaitingIn {
				casePattern = true
				awaitingIn = false
			}
		case "esac":
			// A closer only at command position: `echo esac` in an arm is
			// an argument and closes nothing.
			if cmdStart && caseDepth > 0 {
				caseDepth--
				if caseDepth == 0 {
					casePattern = false
				}
			}
		}
		// Command-position tracking: a keyword or an assignment prefix
		// keeps it, any other word ends it; the separators set it again
		// below.
		cmdStart = shellKeyword(w) || isShellAssignWord(w)
	}

	// closeSpan records the span ending at the closing quote i and judges
	// it for C158.
	closeSpan := func(close int) {
		sc.quoted = append(sc.quoted, [2]int{spanStart, close + 1})
		standalone := (spanStart == 0 || isShellBoundaryOpen(body[spanStart-1])) &&
			(close+1 >= len(body) || isShellBoundaryClose(body[close+1]))
		if casePending {
			// The case word: `case "$V" in` — a case word never
			// word-splits, so the collapse mechanism is false here.
			casePending = false
			return
		}
		if !standalone || testCloser != "" || testCmd || arithDepth > 0 || casePattern {
			return
		}
		content := body[spanStart+1 : close]
		if !treeNoiseEnvOnlyVar(content) {
			return
		}
		if quote == '"' && (cmdName == "echo" || cmdName == "printf") && pipedToXargs(body, close+1) {
			// echo "$V" | xargs git add — xargs re-splits the one word
			// downstream, so the exclusion survives end to end; naming the
			// collapse here would be a false mechanism. Bounded on purpose:
			// only a DOUBLE-quoted span (a single-quoted one is literal
			// text, argc=1), only a producer whose stdout IS the var
			// (echo/printf — `git add "$V" | xargs echo` collapses at git
			// add, upstream of the pipe), only a flag-free xargs (-I/-0/-d
			// do not re-split).
			return
		}
		sc.hits = append(sc.hits, treeNoiseQuote{body[spanStart : close+1], spanLine, quote == '"'})
	}

	for i := 0; i < len(body); {
		ch := body[i]

		// A heredoc body is consumed whole lines at a time until the
		// delimiter line; nothing inside is scanned (it is data).
		if heredocFrom >= 0 {
			eol := strings.IndexByte(body[i:], '\n')
			lineText, next := body[i:], len(body)
			if eol >= 0 {
				lineText, next = body[i:i+eol], i+eol
			}
			cmp := lineText
			if heredocStrip {
				cmp = strings.TrimLeft(cmp, "\t")
			}
			if cmp == heredocDelim {
				sc.data = append(sc.data, [2]int{heredocFrom, i})
				heredocFrom, heredocDelim = -1, ""
			}
			line++
			i = next + 1 // past the newline (or len(body)+1, ending the loop)
			continue
		}

		switch quote {
		case '\'':
			if ch == '\'' {
				closeSpan(i)
				quote = 0
			} else if ch == '\n' {
				line++
			}
			i++
		case '"':
			switch {
			case ch == '\\': // \" \$ \\ \` — the escaped byte is inert
				if i+1 < len(body) && body[i+1] == '\n' {
					line++
				}
				i += 2
			case ch == '"':
				closeSpan(i)
				quote = 0
				i++
			default:
				if ch == '\n' {
					line++
				}
				i++
			}
		default:
			// Arithmetic, $(( … )) or (( … )): nothing inside word-splits
			// and `<<` is a shift, not a heredoc (one shift line used to
			// open a FAKE pending heredoc that swallowed the rest of the
			// body). Only parens, quotes and escapes mean anything here.
			if arithDepth > 0 {
				switch {
				case ch == '\\':
					if i+1 < len(body) && body[i+1] == '\n' {
						line++
					}
					i += 2
				case ch == '\'' || ch == '"':
					quote = ch
					spanStart = i
					spanLine = line
					i++
				case ch == '(':
					arithDepth++
					i++
				case ch == ')':
					arithDepth--
					i++
				default:
					if ch == '\n' {
						line++
					}
					i++
				}
				continue
			}
			switch {
			case ch == '\\': // an escape outside quotes: \' \$ \\ — never a word char
				if i+1 < len(body) && body[i+1] == '\n' {
					line++
				}
				i += 2
			case ch == '\n':
				flushWord(i)
				line++
				cmdStart = true
				testCmd = false
				cmdName = ""
				i++
				if heredocDelim != "" {
					heredocFrom = i
				}
			case ch == ' ' || ch == '\t':
				flushWord(i)
				i++
			case ch == ';' || ch == '&' || ch == '|':
				flushWord(i)
				cmdStart = true
				testCmd = false
				cmdName = ""
				i++
				// A case arm terminator (`;;` or `;&`) returns to pattern
				// position. `||` is not one.
				if caseDepth > 0 && ch == ';' && i < len(body) && (body[i] == ';' || body[i] == '&') {
					casePattern = true
					i++
				}
			case ch == '(' && i+1 < len(body) && body[i+1] == '(':
				// `(( … ))` or `$(( … ))` — bash reads `$((` as arithmetic
				// first, so the adjacency rule matches the shell's own.
				flushWord(i)
				arithDepth = 2
				i += 2
			case ch == '(':
				// A subshell or a $( opens a FRESH command context:
				// `OUT=$(case "$V" in …` reads `case` at command position.
				flushWord(i)
				cmdStart = true
				cmdName = ""
				i++
			case ch == ')':
				flushWord(i)
				// The `)` after a pattern list: the arm body begins, at
				// command position.
				casePattern = false
				cmdStart = true
				cmdName = ""
				i++
			case ch == '<' && i+1 < len(body) && body[i+1] == '<' && (i+2 >= len(body) || body[i+2] != '<'):
				flushWord(i)
				if delim, strip, n := parseHeredocDelim(body[i+2:]); delim != "" {
					heredocDelim, heredocStrip = delim, strip
					i += 2 + n
				} else {
					i += 2
				}
			case ch == '#' && wordStart < 0:
				// A # at a word boundary opens a comment: the rest of the
				// line is text, and its apostrophes are not quotes.
				for i < len(body) && body[i] != '\n' {
					i++
				}
			case ch == '\'' || ch == '"':
				quote = ch
				spanStart = i
				spanLine = line
				i++
			default:
				if wordStart < 0 {
					wordStart = i
				}
				i++
			}
		}
	}
	flushWord(len(body))
	return sc
}

// isShellAssignWord reports whether w starts as an assignment word
// (`NAME=` prefix) — `FOO=bar echo …` leaves command position open for the
// word that follows.
func isShellAssignWord(w string) bool {
	i := 0
	for i < len(w) && (w[i] == '_' ||
		(w[i] >= 'a' && w[i] <= 'z') || (w[i] >= 'A' && w[i] <= 'Z') ||
		(i > 0 && w[i] >= '0' && w[i] <= '9')) {
		i++
	}
	return i > 0 && i < len(w) && w[i] == '='
}

// pipedToXargs reports whether the span ending at off feeds a pipe whose
// next command is a FLAG-FREE xargs: `echo "$V" | xargs git add` (the pipe
// may end its line — `| \n xargs …` re-splits the same). xargs -I, -0 and
// -d do NOT re-split on blanks, so a flagged form is not a suppression.
func pipedToXargs(body string, off int) bool {
	j := off
	for j < len(body) && (body[j] == ' ' || body[j] == '\t') {
		j++
	}
	if j >= len(body) || body[j] != '|' {
		return false
	}
	if j+1 < len(body) && body[j+1] == '|' {
		return false // || is not a pipe
	}
	j++
	for j < len(body) && (body[j] == ' ' || body[j] == '\t' || body[j] == '\n') {
		j++
	}
	start := j
	for j < len(body) && !isShellBoundaryOpen(body[j]) && body[j] != '(' && body[j] != ')' {
		j++
	}
	if body[start:j] != "xargs" {
		return false
	}
	for j < len(body) && (body[j] == ' ' || body[j] == '\t') {
		j++
	}
	start = j
	for j < len(body) && !isShellBoundaryOpen(body[j]) && body[j] != '(' && body[j] != ')' {
		j++
	}
	flag := body[start:j]
	return !strings.HasPrefix(flag, "-I") && !strings.HasPrefix(flag, "-0") && !strings.HasPrefix(flag, "-d")
}

// parseHeredocDelim reads the delimiter of a `<<` operator: an optional `-`
// (strip leading tabs on the body), blanks, then the word, bare or quoted.
// Returns the delimiter text (quotes removed), the strip flag, and the
// number of bytes consumed after `<<`; an empty delimiter means the shape
// was not a heredoc the scanner trusts.
func parseHeredocDelim(s string) (delim string, strip bool, n int) {
	if n < len(s) && s[n] == '-' {
		strip = true
		n++
	}
	for n < len(s) && (s[n] == ' ' || s[n] == '\t') {
		n++
	}
	if n >= len(s) {
		return "", false, 0
	}
	if s[n] == '\'' || s[n] == '"' {
		q := s[n]
		end := strings.IndexByte(s[n+1:], q)
		if end < 0 {
			return "", false, 0
		}
		return s[n+1 : n+1+end], strip, n + end + 2
	}
	start := n
	for n < len(s) && !isShellBoundaryOpen(s[n]) && s[n] != '(' && s[n] != ')' && s[n] != '<' && s[n] != '>' {
		n++
	}
	return s[start:n], strip, n
}
