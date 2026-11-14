package model

import (
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A shell `script:` (sh, bash) is read by a shell before it runs, so a value
// the template layer writes into it is shell text, and the shell expands
// `$(…)`, `$VAR`, `${…}` and backquotes wherever it reads. A JSON literal is
// a double-quoted word to a shell: data written that way runs commands and
// reads the interpreter's environment. Data in a shell script is therefore
// written for the quoting it stands in, so that the shell's reading of the
// script hands the value over as data and expands nothing in it:
//
//   - an unquoted word — a command's argument, an assignment's value, the
//     inside of a `$( … )` — gets the reading a `command:` gives the same
//     reference: one single-quoted word (shellEscapeValue), shaped by the
//     workflow's declarations (a `string[]` one word per element, a `json`
//     one token of JSON text);
//   - inside the author's double quotes, the value's text (the raw form's
//     text: a string as is, anything else its JSON) as a single-quoted word
//     between a closing and a reopening double quote, so the author's string
//     holds the value byte for byte;
//   - inside the author's single quotes, the value's JSON literal — what
//     that position has always received, the source a nested interpreter
//     (`python3 -c '…'`, `jq -n '…'`) reads — between a closing and a
//     reopening single quote, so an apostrophe in it closes nothing;
//   - in the body of a heredoc whose delimiter is unquoted, the JSON literal
//     with the three characters such a body reads (`\`, `$`, the backquote)
//     escaped, so the body carries the literal exactly;
//   - in the body of a heredoc whose delimiter is quoted, and in a comment,
//     the JSON literal as is: the shell reads nothing there, and a JSON
//     literal holds no newline that could end the comment or the line.
//
// Where the shell reads text as code with no quoting to hold a value — an
// arithmetic expansion, a `${ … }` word, a backquote substitution, a `$'…'`
// string, right after an unescaped `$` or `\` — and wherever this reading
// loses track of the shell's (a `case` inside `$( … )`, an unterminated
// heredoc), a data reference is refused, whatever its value: the run fails
// before the script is written, and the dry run says so.
//
// A secret reference, the raw `{{!…}}` form and the literal `{{"{{"}}` are
// not data written by this reading: they render as in any script body. The
// raw form is the author's own text and keeps its meaning; the reading
// takes it as one opaque word.

// renderScriptIn renders a tool node's `script:` written in language: a shell
// script through renderShellScript, any other language through renderScript
// (every reference a JSON literal). The error names the data references a
// shell script holds where no quoting can hold a value; the text returned
// with it keeps those references as written.
func renderScriptIn(script, language string, refs []*ir.Ref, input, vars map[string]any, td *TemplateData, ctxRunID string, guard *secretguard.Guard, shapes *Shapes, unresolved func(ref string), mark func(text string) string) (string, error) {
	if scriptRunsInAShell(language) {
		return renderShellScript(script, language == "bash", refs, input, vars, td, ctxRunID, guard, shapes, unresolved, mark)
	}
	return renderScript(script, refs, input, vars, td, ctxRunID, guard, unresolved, mark), nil
}

// renderShellScript renders a sh (bash false) or bash script: each reference
// resolved by the shared core (resolveTemplateWith), on its own, and written
// for the quoting the shell reads it in (scanShellSlots). The walk takes the
// same `{{…}}` spans the shared core does.
func renderShellScript(script string, bash bool, refs []*ir.Ref, input, vars map[string]any, td *TemplateData, ctxRunID string, guard *secretguard.Guard, shapes *Shapes, unresolved func(ref string), mark func(text string) string) (string, error) {
	if len(refs) == 0 {
		return script, nil
	}
	byRaw := make(map[string]*ir.Ref, len(refs))
	for _, ref := range refs {
		byRaw[ref.Raw] = ref
	}
	slots := scanShellSlots(script, bash)
	var refused []error
	var b strings.Builder
	b.Grow(len(script))
	last := 0
	for _, span := range templateSpans(script) {
		b.WriteString(script[last:span[0]])
		last = span[1]
		raw := script[span[0]:span[1]]
		ref, ok := byRaw[raw]
		if !ok {
			b.WriteString(raw)
			continue
		}
		render := jsonLiteralValue
		if isShellScriptData(ref) {
			slot := slots[span[0]]
			if slot.ctx == slotRefused {
				refused = append(refused, fmt.Errorf("%s stands %s", raw, slot.reason))
				b.WriteString(raw)
				continue
			}
			render = func(val any, shape ValueShape) string { return shellScriptValue(slot.ctx, val, shape) }
		}
		b.WriteString(resolveTemplateWith(raw, []*ir.Ref{ref}, input, vars, td, ctxRunID, guard, shapes, false, render, true, unresolved, mark))
	}
	b.WriteString(script[last:])
	if len(refused) > 0 {
		return b.String(), fmt.Errorf("a %s script holds data where the shell reads text as code and no quoting can hold a value — write each reference where the shell reads a word, a quoted string or a heredoc body (assign it first, NAME={{…}}, and read \"$NAME\" there): %w", shellDialect(bash), errors.Join(refused...))
	}
	return b.String(), nil
}

func shellDialect(bash bool) string {
	if bash {
		return "bash"
	}
	return "sh"
}

// isShellScriptData reports whether ref is data this reading writes: a
// value-bearing namespace in its default (escaped) form. A secret renders
// its placeholder, the raw form is the author's text, and the literal open
// delimiter is source.
func isShellScriptData(ref *ir.Ref) bool {
	if ref.Unquoted {
		return false
	}
	switch ref.Kind {
	case ir.RefInput, ir.RefVars, ir.RefOutputs, ir.RefRun, ir.RefArtifacts, ir.RefAttachments, ir.RefLoop:
		return true
	}
	return false
}

// shellScriptValue writes val for the quoting ctx it stands in (the reading
// above renderScriptIn). A hole — val nil — is the word `null`, as a script
// body renders a missing value everywhere.
func shellScriptValue(ctx shellSlotContext, val any, shape ValueShape) string {
	switch ctx {
	case slotWord:
		if val == nil {
			return shellEscape("null")
		}
		return shellEscapeValue(val, shape)
	case slotDouble:
		return `"` + shellEscape(rawTemplateValue(val, ShapeUndeclared)) + `"`
	case slotSingle:
		return "'" + shellEscape(jsonLiteralValue(val, shape)) + "'"
	case slotHeredoc:
		return heredocBodyEscaper.Replace(jsonLiteralValue(val, shape))
	default:
		return jsonLiteralValue(val, shape)
	}
}

// heredocBodyEscaper escapes the three characters the body of a heredoc
// with an unquoted delimiter reads: `\` before `$`, the backquote, `\` and a
// newline, `$` and the backquote themselves. A JSON literal holds no
// newline.
var heredocBodyEscaper = strings.NewReplacer(`\`, `\\`, `$`, `\$`, "`", "\\`")

// templateSpans is every `{{…}}` span of s as the template walk takes it
// (resolveTemplateWith, ir.ParseRefs): from a `{{` to the first `}}` after
// it, the next search starting past it.
func templateSpans(s string) [][2]int {
	var spans [][2]int
	for i := 0; i+1 < len(s); {
		if s[i] != '{' || s[i+1] != '{' {
			i++
			continue
		}
		end := strings.Index(s[i:], "}}")
		if end == -1 {
			break
		}
		spans = append(spans, [2]int{i, i + end + 2})
		i += end + 2
	}
	return spans
}

// shellSlotContext is the quoting a shell reads a reference's position in.
type shellSlotContext int

const (
	// slotRefused: where no quoting holds a value, or where this reading
	// cannot tell. The zero value: a slot the scan never classified is
	// refused.
	slotRefused shellSlotContext = iota
	slotWord
	slotDouble
	slotSingle
	slotHeredoc // the body of a heredoc whose delimiter is unquoted
	slotLiteral // the body of a heredoc whose delimiter is quoted
	slotComment
)

// shellSlot is the reading of one reference's position: its context and,
// for a refused one, where it stands.
type shellSlot struct {
	ctx    shellSlotContext
	reason string
}

// scanShellSlots reads body as a sh (bash false) or bash script and returns
// the context of each `{{…}}` span, keyed by its offset. Each span is an
// opaque part of a word — every rendering above is balanced for the quoting
// it stands in, so the reading after a span is the reading before it.
//
// The reading follows quoting, comments, escapes, command substitution
// (`$( … )`), heredocs (`<<`, `<<-`, a quoted delimiter in any of its
// spellings, several on one line, one inside a substitution) and the
// constructs where a value is code: arithmetic (`$(( … ))`, a `((` command),
// a `${ … }` word, backquotes, `$'…'`. Where it cannot follow the shell —
// a `case` inside `$( … )`, whose pattern `)` a parenthesis count reads as
// the substitution's end; a heredoc that never ends; a body line ending in
// a backslash, which the shell joins to the next before it looks for the
// delimiter — it stops, and every reference from there on is refused.
func scanShellSlots(body string, bash bool) map[int]shellSlot {
	sc := &shellScanner{s: body, bash: bash, limit: len(body), out: map[int]shellSlot{}, spans: map[int]int{}}
	for _, span := range templateSpans(body) {
		sc.spans[span[0]] = span[1]
		if strings.Contains(body[span[0]:span[1]], "\n") {
			// A reference is never written across lines; the line reading a
			// heredoc takes would not be the shell's once it is rendered.
			sc.stop("a reference written across a line break")
		}
	}
	sc.cmd(0, false)
	for at := range sc.spans {
		if _, seen := sc.out[at]; !seen {
			reason := sc.taint
			if reason == "" {
				reason = "where this reading does not classify it"
			}
			sc.out[at] = shellSlot{ctx: slotRefused, reason: reason}
		}
	}
	return sc.out
}

type shellScanner struct {
	s     string
	bash  bool
	limit int // the scan stops here: the end of the body, or of a heredoc's
	spans map[int]int
	out   map[int]shellSlot
	// taint is set once the reading cannot follow the shell: the scan stops
	// and every span it did not classify is refused with it.
	taint   string
	pending []heredoc
	// inHeredoc counts the heredoc bodies being read around the position: a
	// word or a double-quoted string there (inside a `$( … )` of the body)
	// gets the value's own bytes, line breaks included, and bash finds the
	// body's end by lines before it reads any quoting — a value line equal
	// to the delimiter would end the body inside the substitution, and the
	// lines after it would run.
	inHeredoc int
}

// heredoc is one `<<` operator whose body starts at the next newline of a
// command.
type heredoc struct {
	delim  string
	strip  bool // `<<-`: leading tabs are stripped from the body and the delimiter line
	quoted bool // nothing in the body is read
}

func (sc *shellScanner) stop(reason string) {
	sc.taint = "after " + reason + ", past which the shell's reading is not followed"
}

func (sc *shellScanner) done(i int) bool { return sc.taint != "" || i >= sc.limit }

// openAtEnd is called by a construct that ran into the end of what is being
// read without its closing byte. At the end of the script that is the
// shell's syntax error, which runs nothing of the command; at the end of a
// heredoc body it is not the shell's reading — the shell expands a body only
// once its lines are read, so nothing it opens runs past the delimiter line.
func (sc *shellScanner) openAtEnd(what string) {
	if sc.taint == "" && sc.limit < len(sc.s) {
		sc.stop(what + " left open at the end of a heredoc body")
	}
}

// slot classifies the span at i, if one starts there, and returns its end.
func (sc *shellScanner) slot(i int, ctx shellSlotContext, reason string) (int, bool) {
	end, ok := sc.spans[i]
	if !ok {
		return i, false
	}
	if sc.inHeredoc > 0 && (ctx == slotWord || ctx == slotDouble) {
		ctx, reason = slotRefused, "in a substitution inside a heredoc body, whose end the shell finds by lines before it reads any quoting: a line break in the value could end the body there"
	}
	sc.out[i] = shellSlot{ctx: ctx, reason: reason}
	return end, true
}

// glued refuses a span right after an active `$` or `\`, which would read
// the first byte the rendering writes.
func (sc *shellScanner) glued(i int, what string) (int, bool) {
	return sc.slot(i, slotRefused, "right after "+what+", which would read the rendering's first byte")
}

func isShellMeta(c byte) bool {
	switch c {
	case ' ', '\t', '\n', ';', '&', '|', '(', ')', '<', '>':
		return true
	}
	return false
}

// cmd reads a command context from i: the script itself, or (subst) the
// inside of a `$( … )`, whose closing `)` it returns past.
func (sc *shellScanner) cmd(i int, subst bool) int {
	s := sc.s
	depth := 0
	wordStart := true
	for !sc.done(i) {
		if end, ok := sc.slot(i, slotWord, ""); ok {
			i, wordStart = end, false
			continue
		}
		c := s[i]
		switch {
		case c == '#' && wordStart:
			i = sc.comment(i)
			continue
		case c == '\'':
			i, wordStart = sc.single(i+1), false
			continue
		case c == '"':
			i, wordStart = sc.double(i+1), false
			continue
		case c == '`':
			i, wordStart = sc.backquote(i+1), false
			continue
		case c == '\\':
			if end, ok := sc.glued(i+1, "a backslash"); ok {
				i, wordStart = end, false
				continue
			}
			if i+1 < len(s) && s[i+1] == '\n' {
				i += 2 // a line continuation: the two bytes are not there
				continue
			}
			i, wordStart = i+2, false
			continue
		case c == '$':
			i, wordStart = sc.dollar(i, false), false
			continue
		case c == '(' && wordStart && i+1 < len(s) && s[i+1] == '(':
			i = sc.arith(i + 2)
			wordStart = false
			continue
		case c == '(':
			depth++
		case c == ')':
			if subst {
				if depth == 0 {
					return i + 1
				}
				depth--
			}
		case c == '<' && i+1 < len(s) && s[i+1] == '<':
			if i+2 < len(s) && s[i+2] == '<' {
				i, wordStart = i+3, true // a here-string: a word follows
				continue
			}
			i = sc.heredocOperator(i + 2)
			wordStart = true
			continue
		case c == '\n':
			if len(sc.pending) > 0 {
				i = sc.heredocBodies(i + 1)
				wordStart = true
				continue
			}
		case subst && wordStart && strings.HasPrefix(s[i:], "case") && (i+4 >= len(s) || isShellMeta(s[i+4])):
			sc.stop("a `case` inside $( … )")
			return i
		}
		wordStart = isShellMeta(c)
		i++
	}
	if subst {
		sc.openAtEnd("a $( … ) substitution")
	}
	return i
}

// comment reads a comment from its `#` to the end of the line, leaving the
// newline to the command.
func (sc *shellScanner) comment(i int) int {
	for !sc.done(i) && sc.s[i] != '\n' {
		if end, ok := sc.slot(i, slotComment, ""); ok {
			i = end
			continue
		}
		i++
	}
	return i
}

// single reads a single-quoted span from past its opening quote.
func (sc *shellScanner) single(i int) int {
	for !sc.done(i) {
		if end, ok := sc.slot(i, slotSingle, ""); ok {
			i = end
			continue
		}
		if sc.s[i] == '\'' {
			return i + 1
		}
		i++
	}
	sc.openAtEnd("a single-quoted string")
	return i
}

// double reads a double-quoted span from past its opening quote.
func (sc *shellScanner) double(i int) int {
	s := sc.s
	for !sc.done(i) {
		if end, ok := sc.slot(i, slotDouble, ""); ok {
			i = end
			continue
		}
		switch s[i] {
		case '"':
			return i + 1
		case '\\':
			if end, ok := sc.glued(i+1, "a backslash"); ok {
				i = end
				continue
			}
			i += 2
		case '`':
			i = sc.backquote(i + 1)
		case '$':
			i = sc.dollar(i, true)
		default:
			i++
		}
	}
	sc.openAtEnd("a double-quoted string")
	return i
}

// dollar reads what a `$` at i opens: a substitution, an arithmetic
// expansion, a `${ … }` word, a `$'…'` or `$"…"` string, or nothing (a
// plain `$NAME`, `$1`, `$?` is read by the caller's next bytes).
func (sc *shellScanner) dollar(i int, inDouble bool) int {
	s := sc.s
	if end, ok := sc.glued(i+1, "an unescaped $"); ok {
		return end
	}
	if i+1 >= len(s) {
		return i + 1
	}
	switch s[i+1] {
	case '(':
		if i+2 < len(s) && s[i+2] == '(' {
			return sc.arith(i + 3)
		}
		return sc.cmd(i+2, true)
	case '{':
		return sc.param(i+2, inDouble)
	case '[':
		sc.stop("a `$[`")
		return i
	case '\'':
		if inDouble {
			return i + 1 // a `$'` inside double quotes is two literal bytes
		}
		return sc.ansiC(i + 2)
	case '"':
		if inDouble {
			return i + 1 // the `"` closes the caller's string
		}
		return sc.double(i + 2)
	}
	return i + 1
}

// ansiC reads a `$'…'` string. bash decodes escapes in it, so `\'` does not
// end it; a sh that has no `$'…'` (dash) reads a `$` and a single-quoted
// span, which `\'` ends. A span inside is refused either way: its escapes
// would read the rendering.
func (sc *shellScanner) ansiC(i int) int {
	s := sc.s
	for !sc.done(i) {
		if end, ok := sc.slot(i, slotRefused, "inside a $'…' string, which decodes escapes"); ok {
			i = end
			continue
		}
		switch s[i] {
		case '\'':
			return i + 1
		case '\\':
			if !sc.bash {
				sc.stop("a $'…' string holding a backslash, which a sh reads two ways")
				return i
			}
			if end, ok := sc.slot(i+1, slotRefused, "inside a $'…' string, which decodes escapes"); ok {
				i = end
				continue
			}
			i += 2
			continue
		}
		i++
	}
	sc.openAtEnd("a $'…' string")
	return i
}

// param reads a `${ … }` word from past its `${`. A span inside is refused:
// the word is expanded as the shell's, and its quoting differs between
// shells inside double quotes.
func (sc *shellScanner) param(i int, inDouble bool) int {
	s := sc.s
	const reason = "inside a ${ … } expansion, whose word the shell expands"
	for !sc.done(i) {
		if end, ok := sc.slot(i, slotRefused, reason); ok {
			i = end
			continue
		}
		switch s[i] {
		case '}':
			return i + 1
		case '{':
			sc.stop("a `{` inside ${ … }")
			return i
		case '\'':
			if inDouble {
				sc.stop("a single quote inside a double-quoted ${ … }, which shells read differently")
				return i
			}
			i = sc.single(i + 1)
		case '"':
			i = sc.double(i + 1)
		case '`':
			i = sc.backquote(i + 1)
		case '\\':
			if end, ok := sc.glued(i+1, "a backslash"); ok {
				i = end
				continue
			}
			i += 2
		case '$':
			i = sc.dollar(i, inDouble)
		default:
			i++
		}
	}
	sc.openAtEnd("a ${ … } expansion")
	return i
}

// arith reads an arithmetic expansion or command from past its `((`, to
// past its `))`. A span inside is refused: arithmetic evaluates its text,
// subscripts included, after expanding it, and no quoting holds a value.
func (sc *shellScanner) arith(i int) int {
	s := sc.s
	const reason = "inside arithmetic, which evaluates its text as code"
	depth := 0
	for !sc.done(i) {
		if end, ok := sc.slot(i, slotRefused, reason); ok {
			i = end
			continue
		}
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
				break
			}
			if i+1 < len(s) && s[i+1] == ')' {
				return i + 2
			}
			sc.stop("an arithmetic expression closed by a single `)`")
			return i
		case '\'', '"':
			sc.stop("a quote inside arithmetic")
			return i
		case '`':
			i = sc.backquote(i + 1)
			continue
		case '\\':
			if end, ok := sc.glued(i+1, "a backslash"); ok {
				i = end
				continue
			}
			i += 2
			continue
		case '$':
			i = sc.dollar(i, true)
			continue
		}
		i++
	}
	sc.openAtEnd("an arithmetic expression")
	return i
}

// backquote reads a backquote substitution from past its opening backquote:
// it ends at the first unescaped backquote. A span inside is refused: the
// shell strips a level of backslashes from it before reading it again.
func (sc *shellScanner) backquote(i int) int {
	s := sc.s
	for !sc.done(i) {
		if end, ok := sc.slot(i, slotRefused, "inside a backquote substitution, which the shell reads twice"); ok {
			i = end
			continue
		}
		switch s[i] {
		case '`':
			return i + 1
		case '\\':
			if end, ok := sc.slot(i+1, slotRefused, "inside a backquote substitution, which the shell reads twice"); ok {
				i = end
				continue
			}
			i += 2
			continue
		}
		i++
	}
	sc.openAtEnd("a backquote substitution")
	return i
}

// heredocOperator reads the delimiter word of a `<<` / `<<-` from past the
// `<<`, and queues the heredoc for the next newline of a command. The
// delimiter is the word with its quotes removed; any quoting makes the body
// literal.
func (sc *shellScanner) heredocOperator(i int) int {
	s := sc.s
	h := heredoc{}
	if i < len(s) && s[i] == '-' {
		h.strip = true
		i++
	}
	for i < sc.limit && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	var delim strings.Builder
	for i < sc.limit && !isShellMeta(s[i]) {
		if _, ok := sc.spans[i]; ok {
			sc.stop("a reference in a heredoc delimiter")
			return i
		}
		switch c := s[i]; c {
		case '\'', '"':
			// Quote removal, as the shell takes the word: nothing is read in
			// single quotes; in double quotes a backslash goes only before
			// the four bytes it escapes there.
			h.quoted = true
			j := i + 1
			for j < sc.limit && s[j] != c {
				if _, ok := sc.spans[j]; ok {
					sc.stop("a reference in a heredoc delimiter")
					return j
				}
				if c == '"' && s[j] == '\\' && j+1 < sc.limit {
					switch s[j+1] {
					case '$', '`', '"', '\\':
						j++
					case '\n':
						sc.stop("a heredoc delimiter continued on the next line")
						return j
					}
				}
				delim.WriteByte(s[j])
				j++
			}
			if j >= sc.limit {
				sc.stop("an unterminated heredoc delimiter")
				return i
			}
			i = j + 1
		case '\\':
			h.quoted = true
			if _, ok := sc.spans[i+1]; ok {
				sc.stop("a reference in a heredoc delimiter")
				return i
			}
			if i+1 >= sc.limit || s[i+1] == '\n' {
				sc.stop("a heredoc delimiter ending in a backslash")
				return i
			}
			delim.WriteByte(s[i+1])
			i += 2
		case '$', '`':
			sc.stop("an expansion in a heredoc delimiter")
			return i
		default:
			delim.WriteByte(c)
			i++
		}
	}
	h.delim = delim.String()
	if h.delim == "" {
		sc.stop("a heredoc operator without a delimiter")
		return i
	}
	sc.pending = append(sc.pending, h)
	return i
}

// heredocBodies reads the bodies of the queued heredocs, in order, from the
// start of the line after the command that queued them, and returns the
// offset past the last delimiter line.
func (sc *shellScanner) heredocBodies(i int) int {
	queued := sc.pending
	sc.pending = nil
	for _, h := range queued {
		start := i
		end := -1
		for i < sc.limit {
			nl := strings.IndexByte(sc.s[i:sc.limit], '\n')
			lineEnd := sc.limit
			if nl >= 0 {
				lineEnd = i + nl
			}
			line := sc.s[i:lineEnd]
			if h.strip {
				line = strings.TrimLeft(line, "\t")
			}
			if line == h.delim {
				end = i
				i = lineEnd
				if i < sc.limit {
					i++
				}
				break
			}
			if !h.quoted && strings.HasSuffix(line, `\`) && (len(line)-len(strings.TrimRight(line, `\`)))%2 == 1 {
				sc.stop("a heredoc line ending in a backslash, which the shell joins to the next")
				return i
			}
			i = lineEnd
			if i < sc.limit {
				i++
			}
		}
		if end < 0 {
			sc.stop("a heredoc that never ends")
			return i
		}
		sc.heredocBody(start, end, h)
		if sc.taint != "" {
			return i
		}
	}
	return i
}

// jsonSpellableDelims holds what a rendered JSON value could spell on a
// line of its own: a delimiter carrying one of them could be ended early by
// a value, and the author's lines after it would run as commands.
var jsonSpellableDelims = []string{`"`, "{", "}", "[", "]", "true", "false", "null", "0", "1", "2", "3", "4", "5", "6", "7", "8", "9"}

// heredocBody classifies the spans of one heredoc body, [start, end).
func (sc *shellScanner) heredocBody(start, end int, h heredoc) {
	for _, spell := range jsonSpellableDelims {
		if strings.Contains(h.delim, spell) {
			reason := fmt.Sprintf("in the body of a heredoc whose delimiter %q a value could spell on a line of its own", h.delim)
			for at := range sc.spans {
				if at >= start && at < end {
					sc.out[at] = shellSlot{ctx: slotRefused, reason: reason}
				}
			}
			return
		}
	}
	if h.quoted {
		for at := range sc.spans {
			if at >= start && at < end {
				sc.out[at] = shellSlot{ctx: slotLiteral}
			}
		}
		return
	}
	limit := sc.limit
	sc.limit = end
	sc.inHeredoc++
	defer func() { sc.limit, sc.inHeredoc = limit, sc.inHeredoc-1 }()
	s := sc.s
	i := start
	for !sc.done(i) {
		if e, ok := sc.slot(i, slotHeredoc, ""); ok {
			i = e
			continue
		}
		switch s[i] {
		case '\\':
			if e, ok := sc.glued(i+1, "a backslash"); ok {
				i = e
				continue
			}
			i += 2
		case '`':
			i = sc.backquote(i + 1)
		case '$':
			i = sc.dollar(i, true)
		default:
			i++
		}
	}
}
