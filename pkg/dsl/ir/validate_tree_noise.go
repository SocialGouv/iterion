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
// learned: the C158 hits, the byte intervals that are inside a quoted
// span or a heredoc body — C157's "the ref is data there" test for scripts —
// and, for the C188 braced-env lint, the names the body assigns and where
// its unquoted and double-quoted positions open a braced `${` form.
type shellScan struct {
	hits    []treeNoiseQuote
	quoted  [][2]int // [open, close) of every quoted span, close exclusive
	data    [][2]int // heredoc bodies
	assigns []shellAssign
	// reads holds the offset of every `${` the SHELL reads — unquoted or
	// double-quoted, so never one inside a comment, a single-quoted span
	// or a heredoc body. The scanner answers where the shell looks and
	// nothing else: whether a form there is an env ref the engine
	// consults is BracedEnvReads' word, intersected on these offsets.
	reads []int
}

// shellAssign is one name a body assigns, at a position bash accepts
// assignments at, with the offset of the TERMINATOR of the command that
// assigns it — where the value it sets starts to exist for the rest of the
// body. The terminator, not the assigning word's end: a command reads its
// own text before it assigns — bash expands a command's words and
// redirections before its prefix assignments take effect, a declaration
// utility's operands before it assigns them, a `for`'s word list before the
// loop binds its variable, a `read`'s redirection before read assigns — so
// a braced read there (a right-hand side read through the command's own
// text, `PATH="${PATH}:/opt/bin"`, `X=$(tr : , <<< "${X}")`; a `for`'s word
// list; a `read`'s redirection target; a second operand of the same
// `export`) reads the ambient value by design. A read before the offset is
// one of those, or simply precedes the assignment; both answer the ambient
// value, and calling either dead would be false. An assignment-only command
// is the exception, twice: it assigns left to right, each assignment seeing
// the previous one (`A=1 B="${A}"` reads 1), and under bash it assigns
// before its own redirections (`X=new >"f.${X}"` creates f.new; dash
// expands the redirection first). The engine defeats both reads and C188
// reports neither — documented misses, the direction a lint may take.
type shellAssign struct {
	name string
	end  int
}

// pendingAssign is a name an assignment has been seen for, waiting for its
// command's terminator to say where its value starts to exist. depth is the
// `(` nesting it was read at, so a subshell's or a command substitution's
// own separators discharge its assignments and not the enclosing command's.
// prefix marks an entry of the command-position arm: under bash such an
// assignment reaches the rest of the body only when its command turns out
// to have no verb (`X=1`), never when one follows (`HOME=/tmp/x git …` sets
// HOME in git's environment alone), so the entry is dropped the moment a
// verb arrives. sh keeps more: dash and `bash --posix` keep a prefix before
// a special builtin (`X=1 :`, `X=1 export Q=2`, `X=1 eval …`,
// `X=1 set -- …`, `X=1 exec`), so the drop is a documented miss there.
type pendingAssign struct {
	name   string
	depth  int
	prefix bool
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
		// case tracking, one state PER DEPTH (a stack): a `case` at command
		// position pushes the enclosing state; `esac` at command position
		// pops it back — an inner case's `in`/`esac` touches nothing of the
		// outer compound. The NEXT span after `case` is the case word, which
		// never word-splits; the pattern region opens ONLY from the
		// "case word consumed, awaiting in" state — a bare `in` inside an
		// arm (`for f in …`, `grep -w in …`) arms nothing.
		caseStack   []caseState
		casePending bool
		awaitingIn  bool
		casePattern bool // in a case PATTERN position (between `in` and the arm's `)`, and after each `;;`/`;&`) — patterns never word-split either

		arithDepth int // inside $(( … )) / (( … )): no word-splitting, no heredocs

		heredocDelim string // pending: opens at the next newline
		heredocStrip bool
		heredocFrom  = -1 // active heredoc body's start offset

		// Per-command buffering for the ONE pipe rule: a span that passes
		// every other check waits for its command's end; flushed at the
		// separator, a DOUBLE-quoted span produced by echo/printf whose
		// command ends at a pipe is DROPPED — piped output: the downstream
		// may re-split, and the check cannot tell. Nothing downstream is
		// parsed at all (no xargs detection, no flags, no continuations —
		// that machinery provably did not converge).
		pending []treeNoiseQuote
		cmdVerb string // first word of the current command (after keywords, assignment prefixes and command modifiers)

		// The C188 writer arms' position state: the word just flushed was a
		// declaration utility that writes the global (export/readonly), a
		// `read`, or a `for` awaiting its loop variable — each opens the
		// one window where bash takes the next word as a name it assigns.
		// declareFlagged records that the open declaration window has
		// already seen a flag word, past which a bare word closes it.
		declareNext    bool
		declareFlagged bool
		readNext       bool
		forNext        bool

		// The names the writer arms have seen, waiting for their command's
		// terminator (see shellAssign), and the `(` nesting of the scan:
		// a subshell or a command substitution discharges its own.
		deferred []pendingAssign
		subDepth int

		// The narrowings that keep the writer arms on what bash itself
		// assigns for the rest of the body. Each one silences a shape where
		// a recorded name would be false, and pays for it with a documented
		// miss rather than a deeper model of the shell:
		//   - verbSeen: the current command has a verb, and bash takes a
		//     `NAME=value` word as an assignment only BEFORE it — past it the
		//     word is an operand (`make build COMMIT=abc`, `for kv in
		//     FOO=1`, `"$MAKE" PREFIX=/opt`). A quote or an escape opening a
		//     word at command position is a verb too, and so is a command
		//     modifier (`env X=1 cmd`). A `(` saves it on verbStack
		//     and opens a fresh context; its `)` restores it, so a nesting
		//     never makes the scan forget the verb of the command around it
		//     (`echo $(date) A=1`, `local X=$(date) Y=2`).
		//   - hereWord: the operand of `<<<` is the command's stdin, never
		//     an argv word, and bash does not word-split it.
		//   - pipeElem: the command after a single `|` runs in a subshell
		//     (bash without lastpipe, sh always), so a `read` there assigns
		//     nothing the rest of the body sees.
		verbSeen  bool
		verbStack []bool
		hereWord  bool
		pipeElem  bool

		// Three more narrowings of the same kind:
		//   - bqOpen: the scan sits inside an unquoted backquote
		//     substitution, which nests like `$(`: what it assigns dies
		//     with it (curDepth).
		//   - declareFunc: the open export/readonly window has seen a flag
		//     carrying `f`, past which its operands name functions —
		//     `export -f X=1` assigns nothing.
		//   - forPending: a `for` variable, with the depth it was read at,
		//     recorded only once an `in` follows: `for NAME do` iterates
		//     the positional parameters, and a tool body runs without any.
		bqOpen          bool
		declareFunc     bool
		forPending      string
		forPendingDepth int
	)

	// dischargeAssigns ends the deferral of every pending assignment read at
	// this depth or deeper, its command having ended at end, and records the
	// depth-0 ones only: an assignment inside a subshell or a command
	// substitution dies with it (`( export X=1 )` and `OUT=$(X=1; …)` leave
	// X as it was for the rest of the body), so recording it would arm a
	// read bash answers from the ambient value. The cost is a documented
	// miss: a name assigned and read inside the same substitution.
	dischargeAssigns := func(end, depth int) {
		kept := deferred[:0]
		for _, a := range deferred {
			if a.depth >= depth {
				if a.depth == 0 {
					sc.assigns = append(sc.assigns, shellAssign{a.name, end})
				}
				continue
			}
			kept = append(kept, a)
		}
		deferred = kept
	}

	// curDepth is the nesting an assignment is read at: an open backquote
	// substitution is one level below the `(` nesting around it.
	curDepth := func() int {
		if bqOpen {
			return subDepth + 1
		}
		return subDepth
	}

	// dropAssigns forgets, unrecorded, every pending assignment read at this
	// depth or deeper: its command ends at a lone `&`, which runs it in the
	// background, in a subshell, so nothing it assigns reaches the rest of
	// the body (`X=1 &`, `export X=1 &`).
	dropAssigns := func(depth int) {
		kept := deferred[:0]
		for _, a := range deferred {
			if a.depth >= depth {
				continue
			}
			kept = append(kept, a)
		}
		deferred = kept
	}

	// dropPrefix forgets the command-position assignments of the current
	// command once it turns out to have a verb (see pendingAssign.prefix).
	dropPrefix := func() {
		kept := deferred[:0]
		for _, a := range deferred {
			if a.prefix && (a.depth == subDepth || a.depth == curDepth()) {
				continue
			}
			kept = append(kept, a)
		}
		deferred = kept
	}

	// flushCmd discharges the pending spans at a command's end. pipe is true
	// when the command ends at a `|` — its stdout leaves to a downstream
	// the check cannot model.
	flushCmd := func(pipe bool) {
		for _, h := range pending {
			if pipe && h.double && (cmdVerb == "echo" || cmdVerb == "printf") {
				continue
			}
			sc.hits = append(sc.hits, h)
		}
		pending = nil
		cmdVerb = ""
		declareNext, declareFlagged, readNext, forNext = false, false, false, false
		forPending = ""
		verbSeen = false
	}

	// flushWord evaluates the word ending at end (outside any quote).
	flushWord := func(end int) {
		if wordStart < 0 {
			return
		}
		start := wordStart
		w := body[wordStart:end]
		wordStart = -1
		hereWord = false
		// The word after a `for` variable decides it: an `in` binds the
		// variable to a word list, anything else (`do`) leaves it unrecorded.
		if forPending != "" && w != forPending {
			if w == "in" {
				deferred = append(deferred, pendingAssign{forPending, forPendingDepth, false})
			}
			forPending = ""
		}
		// A bare case word consumed the pending flag before this word.
		casePending = false
		// Command-verb tracking: the first word of a command that is not a
		// keyword, an assignment prefix (`FOO=bar echo …`) or a command
		// modifier (`command echo …`, `env echo …`).
		if cmdStart && cmdVerb == "" && !shellKeyword(w) && !isShellAssignWord(w) && !isShellCommandModifier(w) {
			cmdVerb = w
			verbSeen = true
			// The verb ends the command's prefix assignments — unless the
			// word is glued to a `)`, where it continues the assignment the
			// substitution sat in (`X=$(pwd)/sub` assigns X, it runs nothing).
			if start == 0 || isShellBoundaryOpen(body[start-1]) {
				dropPrefix()
			}
		}
		// A command modifier is the command's verb for the writer arms: a
		// `NAME=value` word after it is its operand (`env X=1 cmd`,
		// `command X=1`), a prefix before it the environment it runs with
		// (`GIT_DIR=… env | …`).
		if cmdStart && !verbSeen && isShellCommandModifier(w) {
			verbSeen = true
			if start == 0 || isShellBoundaryOpen(body[start-1]) {
				dropPrefix()
			}
		}
		// The C188 writer arms, judged where bash accepts assignments:
		// command position (an assignment chain keeps it, via cmdStart),
		// the `NAME=value` operands of `export` and `readonly`, `read`'s
		// arguments, and a `for` loop variable — never a case pattern, where
		// `FOO=1)` is a pattern and assigns nothing. A word that is none of
		// these closes the window the previous word opened. The arms are
		// these four, not a complete bash grammar: a name assigned through a
		// form they do not model (`let`, `printf -v`, `mapfile`, a `select`
		// variable, an arithmetic `((NAME=…))`) is a miss — the direction a
		// lint may take, and what the dry run reports.
		//
		// No arm records an offset of its own: each DEFERS the name to its
		// command's terminator (dischargeAssigns), which is where the value
		// starts to exist for the rest of the body — see shellAssign. The
		// C188 lint needs that offset to tell a read the assignment feeds
		// from a read the assignment defeats. The body's line numbers are
		// derived once per finding, from the offset, never per word.
		switch {
		case forNext:
			forNext = false
			if isCIdentifier(w) {
				// Recorded only when `in` follows (see forPending).
				forPending, forPendingDepth = w, curDepth()
			}
		case readNext:
			if strings.HasPrefix(w, "-") {
				// A read flag (-r, -a, -p …): keep looking for the name.
			} else if isCIdentifier(w) {
				deferred = append(deferred, pendingAssign{w, curDepth(), false})
				readNext = false
			} else {
				readNext = false
			}
		case declareNext:
			switch {
			case strings.HasPrefix(w, "-"):
				// A flag (`-n`, `-p`, `-f`, `--`): never the name, and it
				// leaves the window open on the `NAME=value` operand that may
				// follow (under bash `export -n COUNT=1` assigns COUNT; dash
				// refuses `-n` and stops). Past a flag a bare word closes the
				// window, because it is as often no variable at all —
				// `export -f helper` names a function — and past a flag
				// carrying `f` every operand names one, so `export -f X=1`
				// assigns nothing (declareFunc).
				declareFlagged = true
				if !strings.HasPrefix(w, "--") && strings.Contains(w, "f") {
					declareFunc = true
				}
			case shellAssignWordName(w) != "":
				if !rhsOpensSubstitution(w) && !declareFunc {
					deferred = append(deferred, pendingAssign{shellAssignWordName(w), curDepth(), false})
				}
			case !declareFlagged && isCIdentifier(w):
				// A bare operand assigns nothing — `export GH_TOKEN` marks
				// the inherited value for export — so nothing is recorded,
				// and the window stays open on a later `NAME=value`.
			default:
				declareNext, declareFlagged = false, false
			}
		case !casePattern && cmdStart && !verbSeen:
			// Before the command's verb only: an assignment word keeps
			// command position open, but past a verb a `NAME=value` word is
			// an operand (`make build VERSION=1 COMMIT=abc`, `declare A=1
			// B=2`), however many follow.
			if name := shellAssignWordName(w); name != "" && !rhsOpensSubstitution(w) {
				deferred = append(deferred, pendingAssign{name, curDepth(), true})
			}
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
				caseStack = append(caseStack, caseState{awaitingIn, casePattern})
				casePending = true
				awaitingIn = true
				casePattern = false
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
			if cmdStart && len(caseStack) > 0 {
				top := caseStack[len(caseStack)-1]
				caseStack = caseStack[:len(caseStack)-1]
				awaitingIn = top.awaitingIn
				casePattern = top.casePattern
			}
		case "export", "readonly":
			// A declaration utility that writes the global variable, inside
			// a function too: its `NAME=value` operands are assignments (the
			// declareNext arm above). Command position does not survive the
			// utility (flushWord's trailing cmdStart recomputation below
			// closes it), so the flag carries the window instead.
			if cmdStart {
				declareNext = true
				declareFunc = false
			}
		case "declare", "local", "typeset":
			// No window: inside a function these make the name local to it,
			// and the scanner has no function scope — an operand recorded
			// here would arm a read OUTSIDE the function, which bash answers
			// from the global. The documented cost is a function's own
			// braced read of its local, and a top-level `declare NAME=value`
			// read braced.
		case "read":
			// A `read` that is a pipeline element runs in a subshell, so its
			// name never reaches the rest of the body (`cmd | read -r X`);
			// one behind a keyword (`cmd | while read -r L; do …`) still
			// opens the window, its loop body sharing that subshell.
			if cmdStart && !pipeElem {
				readNext = true
			}
		case "for":
			if cmdStart {
				forNext = true
			}
		}
		// The pipeline element's own verb has now been judged: any word but
		// an assignment prefix or a command modifier ends that window.
		if pipeElem && !isShellAssignWord(w) && !isShellCommandModifier(w) {
			pipeElem = false
		}
		// Command-position tracking: a keyword, an assignment prefix or a
		// command modifier keeps it, any other word ends it; the separators
		// set it again below.
		cmdStart = shellKeyword(w) || isShellAssignWord(w) || isShellCommandModifier(w)
	}

	// closeSpan records the span ending at the closing quote i and, when it
	// passes every suppression, buffers it as a candidate for its command's
	// end (the pipe rule, see flushCmd).
	closeSpan := func(close int) {
		sc.quoted = append(sc.quoted, [2]int{spanStart, close + 1})
		if hereWord {
			// The operand of `<<<` is the command's stdin, never an argv
			// word, and bash does not word-split it — neither clause of
			// C158's mechanism applies.
			hereWord = false
			return
		}
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
		pending = append(pending, treeNoiseQuote{body[spanStart : close+1], spanLine, quote == '"'})
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
			switch ch {
			case '\'':
				closeSpan(i)
				quote = 0
			case '\n':
				line++
			}
			i++
		case '"':
			switch ch {
			case '\\': // \" \$ \\ \` — the escaped byte is inert
				if i+1 < len(body) && body[i+1] == '\n' {
					line++
				}
				i += 2
			case '"':
				closeSpan(i)
				quote = 0
				i++
			case '\n':
				line++
				i++
			default:
				// A double-quoted position reads what it carries: the
				// offset of the `${` goes on the record, and the C188 lint
				// intersects it with the segments the engine consults.
				if ch == '$' && i+1 < len(body) && body[i+1] == '{' {
					sc.reads = append(sc.reads, i)
				}
				i++
			}
		default:
			// Arithmetic, $(( … )) or (( … )): nothing inside word-splits
			// and `<<` is a shift, not a heredoc (one shift line used to
			// open a FAKE pending heredoc that swallowed the rest of the
			// body). Only parens, quotes and escapes mean anything here.
			if arithDepth > 0 {
				switch ch {
				case '\\':
					if i+1 < len(body) && body[i+1] == '\n' {
						line++
					}
					i += 2
				case '\'', '"':
					quote = ch
					spanStart = i
					spanLine = line
					i++
				case '(':
					arithDepth++
					i++
				case ')':
					arithDepth--
					i++
				case '\n':
					line++
					i++
				default:
					i++
				}
				continue
			}
			switch {
			case ch == '\\': // an escape outside quotes: \' \$ \\ — never a word char
				// An escape opening a word at command position opens the
				// command's verb (`HOME=/tmp/x \true` runs true), like a
				// quote below; `\` before a newline continues the line.
				if wordStart < 0 && cmdStart && !verbSeen && !casePattern && !casePending && i+1 < len(body) && body[i+1] != '\n' {
					verbSeen = true
					dropPrefix()
				}
				if i+1 < len(body) && body[i+1] == '\n' {
					line++
				}
				i += 2
			case ch == '\n':
				flushWord(i)
				flushCmd(false)
				dischargeAssigns(i, subDepth)
				line++
				cmdStart = true
				testCmd = false
				i++
				if heredocDelim != "" {
					heredocFrom = i
				}
			case ch == ' ' || ch == '\t':
				flushWord(i)
				i++
			case ch == ';' || ch == '&' || ch == '|':
				flushWord(i)
				// The command ends here: at a pipe its stdout leaves to a
				// downstream the check cannot model; `||` is not a pipe.
				pipe := ch == '|' && (i+1 >= len(body) || body[i+1] != '|')
				flushCmd(pipe)
				// A lone `&` (never a byte of `&&`, `|&`, `>&`, `&>`, `<&`)
				// runs its command in the background: the assignments it
				// still holds die with it (dropAssigns).
				if ch == '&' && (i == 0 || !strings.ContainsRune("&|<>", rune(body[i-1]))) && (i+1 >= len(body) || (body[i+1] != '&' && body[i+1] != '>')) {
					dropAssigns(subDepth)
				} else {
					dischargeAssigns(i, subDepth)
				}
				// A single `|` (`|&` too, never the second byte of `||`)
				// makes the next command a pipeline element. A newline does
				// not end that window: a pipeline continues past a `|` that
				// ends its line.
				pipeElem = (pipe && (i == 0 || body[i-1] != '|')) ||
					(ch == '&' && i > 0 && body[i-1] == '|' && pipeElem)
				cmdStart = true
				testCmd = false
				i++
				// A case arm terminator (`;;` or `;&`) returns to pattern
				// position. `||` is not one.
				if len(caseStack) > 0 && ch == ';' && i < len(body) && (body[i] == ';' || body[i] == '&') {
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
				// In a case-pattern position the `(` is the POSIX `(pat)`
				// form's opener and nests nothing: counted, it would never
				// be closed (a pattern's `)` closes no nesting) and would
				// leave every later assignment of the body at depth 1.
				flushWord(i)
				if !casePattern {
					subDepth++
					verbStack = append(verbStack, verbSeen)
					verbSeen = false
				}
				cmdStart = true
				cmdVerb = ""
				i++
			case ch == ')':
				flushWord(i)
				// The `)` after a pattern list: the arm body begins, at
				// command position; a subshell's end discharges its command.
				// A pattern's `)` closes no nesting: the one `(` a pattern
				// may carry, the `(pat)` form's, opened none (see above).
				// Closing a nesting returns to the command around it, whose
				// verb (verbSeen) the `(` saved.
				flushCmd(false)
				dischargeAssigns(i, subDepth)
				if !casePattern && subDepth > 0 {
					subDepth--
					if n := len(verbStack); n > 0 {
						verbSeen = verbStack[n-1]
						verbStack = verbStack[:n-1]
					}
				}
				casePattern = false
				cmdStart = true
				pipeElem = false
				i++
			case ch == '<' && i+2 < len(body) && body[i+1] == '<' && body[i+2] == '<':
				// A herestring has no body: consumed whole, so its second
				// `<` cannot be read as a `<<` whose delimiter is the next
				// word — which opened a heredoc that never closed and ate
				// the rest of the body, writer arms and reads alike. Its
				// operand is stdin (hereWord, see closeSpan).
				flushWord(i)
				i += 3
				hereWord = true
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
				// A quote opening a word at command position opens the
				// command's verb (`"$MAKE" PREFIX=/opt install`): the
				// assignments before it were its prefix, and a `NAME=value`
				// word after it is an operand. verbSeen is the C188 arm's
				// alone — C158's cmdVerb tracking is left as it was.
				if wordStart < 0 && cmdStart && !verbSeen && !casePattern && !casePending {
					verbSeen = true
					dropPrefix()
				}
				quote = ch
				spanStart = i
				spanLine = line
				i++
			case ch == '`':
				// An unquoted backquote opens or closes a command
				// substitution; what it assigns inside dies with it.
				bqOpen = !bqOpen
				if wordStart < 0 {
					wordStart = i
				}
				i++
			case ch == '$' && i+1 < len(body) && body[i+1] == '{':
				// An unquoted position reads what it carries, like the
				// double-quoted branch above: the `${`'s offset, for the
				// C188 lint to intersect with the engine's own segments.
				sc.reads = append(sc.reads, i)
				if wordStart < 0 {
					wordStart = i
				}
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
	flushCmd(false)
	dischargeAssigns(len(body), 0)
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

// shellAssignWordName returns the NAME of an assignment word (`NAME=…`),
// empty when w is not one. The byte parse is isShellAssignWord's own: a
// quoted byte interrupts the name, which is what keeps `X'=1'` — the shell
// would run it as the command X=1 — out of the assignments.
func shellAssignWordName(w string) string {
	i := 0
	for i < len(w) && (w[i] == '_' ||
		(w[i] >= 'a' && w[i] <= 'z') || (w[i] >= 'A' && w[i] <= 'Z') ||
		(i > 0 && w[i] >= '0' && w[i] <= '9')) {
		i++
	}
	if i > 0 && i < len(w) && w[i] == '=' {
		return w[:i]
	}
	return ""
}

// rhsOpensSubstitution reports whether an assignment word opens a command
// substitution it does not close: a `$(` opened inside double quotes, whose
// inner quotes the scanner's flat quote model reads as closing and reopening
// the outer span, or a backquoted one, which it does not model at all. Such
// a word was flushed in the middle of its own right-hand side — a blank or a
// separator inside the substitution — so its command's terminator is not
// where the scan thinks, and the assignment is not recorded: a later braced
// read of the name (`X="$(cmd "a b")"`, then `${X}`) is a documented miss.
func rhsOpensSubstitution(w string) bool {
	return strings.Count(w, "$(") > strings.Count(w, ")") || strings.Count(w, "`")%2 == 1
}

// caseState is the per-depth case-compound state the stack carries (see
// scanShellBody): whether the compound awaits its `in`, and whether the scan
// sits in a pattern position.
type caseState struct {
	awaitingIn  bool
	casePattern bool
}

// isShellCommandModifier reports whether w is a command MODIFIER — a word
// that runs another command without being it (`command echo …`, `env echo
// …`): command position and the command-verb question both pass through it.
func isShellCommandModifier(w string) bool {
	switch w {
	case "command", "env", "builtin", "exec", "noglob":
		return true
	}
	return false
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
