package ir

import (
	"strings"
)

// A sh or bash `script:` receives each data reference (ShellScriptData)
// through its interpreter's environment: the reference renders to a read of
// a variable the engine names for that one rendering, and the value never
// enters the script's text (pkg/backend/model, renderShellScript). What the
// shell does with the variable read still depends on where it stands, and
// ShellScriptRefPositions is that one reading — for the renderer, which
// writes the read for its quoting, and for the compiler, which warns where
// the value does not arrive (C343) or where bash evaluates it (C344).
//
// The reading is a lexer, not the shell, and it is allowed to be wrong: every
// form the renderer writes is a parameter expansion, whose result the shell
// never parses again, and every form is balanced for any quoting it lands in
// (`"${N}"` holds two quotes, `${N}` none), so a misread costs at most word
// splitting, a glob, a value that does not arrive, or a warning missed or
// too many — never a command, never a read of the environment. Where it
// knows it lost the shell's reading (a `case` inside `$( … )`, a heredoc
// that never ends, …) it stops, and every later reference is a plain word,
// marked Unsure so no warning rests on it.

// ShellRefContext is the quoting a shell reads a reference's position in.
type ShellRefContext int

const (
	// ShellRefWord: an unquoted word — an argument, an assignment's value,
	// the inside of `$( … )` or of backquotes. The default.
	ShellRefWord ShellRefContext = iota
	// ShellRefDouble: inside the author's double quotes, a `$"…"` string,
	// or a `${ … }` word inside them.
	ShellRefDouble
	// ShellRefSingle: where the shell reads nothing — single quotes, a
	// `$'…'` string.
	ShellRefSingle
	// ShellRefHeredoc: the body of a heredoc whose delimiter is unquoted —
	// the shell expands `$` there and reads no quote.
	ShellRefHeredoc
	// ShellRefLiteralHeredoc: the body of a heredoc whose delimiter is
	// quoted — the shell reads nothing.
	ShellRefLiteralHeredoc
	// ShellRefComment: a comment.
	ShellRefComment
	// ShellRefArith: inside `$(( … ))` or a `(( … ))` command, where the
	// text is an arithmetic expression.
	ShellRefArith
)

// ShellRefPosition is the reading of one reference's position.
type ShellRefPosition struct {
	Context ShellRefContext
	// ArithEval: bash evaluates the word the reference expands into as an
	// arithmetic expression — a `[[ … -gt … ]]` operand, a `let` argument,
	// a `declare -i` / `local -i` assignment, an array subscript, a
	// `${ … }` subscript or offset — which runs the command substitutions a
	// crafted value's array subscript holds (`a[$(cmd)]`). Always so in
	// ShellRefArith.
	ArithEval bool
	// Where names the position for a message: "inside single quotes", "a
	// `let` argument", …; empty for a plain word or a double-quoted string.
	Where string
	// Unsure: the reading lost the shell's before this reference, and the
	// context is the default, not a reading.
	Unsure bool
}

// ShellScriptData reports whether ref is data a shell script receives
// through its environment: a value-bearing namespace in its default form.
// A secret renders its own variable (bodySecrets), the raw `{{!…}}` form is
// the author's text, and the literal open delimiter is source.
func ShellScriptData(ref *Ref) bool {
	if ref == nil || ref.Unquoted {
		return false
	}
	switch ref.Kind {
	case RefInput, RefVars, RefOutputs, RefRun, RefArtifacts, RefAttachments, RefLoop:
		return true
	}
	return false
}

// TemplateSpans is every `{{…}}` span of s as the template walk takes it
// (ParseRefs, the renderer's single pass): from a `{{` to the first `}}`
// after it, the next search starting past it.
func TemplateSpans(s string) [][2]int {
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

// ShellScriptRefPositions reads body as a sh (bash false) or bash script and
// returns the position of each `{{…}}` span, keyed by its offset. Each span
// is an opaque part of a word.
func ShellScriptRefPositions(body string, bash bool) map[int]ShellRefPosition {
	sc := &shellRefScanner{s: body, bash: bash, limit: len(body), out: map[int]ShellRefPosition{}, spans: map[int]int{}}
	for _, span := range TemplateSpans(body) {
		sc.spans[span[0]] = span[1]
		if strings.Contains(body[span[0]:span[1]], "\n") {
			sc.lost = true // a reference across a line break: no line reading is the shell's
		}
	}
	sc.cmd(0, false)
	for at := range sc.spans {
		if _, seen := sc.out[at]; !seen {
			sc.out[at] = ShellRefPosition{Context: ShellRefWord, Unsure: true}
		}
	}
	return sc.out
}

type shellRefScanner struct {
	s     string
	bash  bool
	limit int // the end of the body, or of the heredoc body being read
	spans map[int]int
	out   map[int]ShellRefPosition
	// lost is set once the reading cannot follow the shell's: the scan
	// stops, and every span it did not reach is an unsure word.
	lost    bool
	pending []shellHeredoc
	// word collects the spans of the word the innermost command is reading,
	// for the arithmetic-evaluation rules that judge a whole word.
	word *[]int
	// subst counts the `$( … )` substitutions around the position.
	subst int
}

type shellHeredoc struct {
	delim  string
	strip  bool
	quoted bool
}

func (sc *shellRefScanner) done(i int) bool { return sc.lost || i >= sc.limit }

// openAtEnd: a construct ran into the end of what is read without its
// closing byte. At the end of the script that is the shell's syntax error;
// at the end of a heredoc body it is not the shell's reading.
func (sc *shellRefScanner) openAtEnd() {
	if sc.limit < len(sc.s) {
		sc.lost = true
	}
}

// slot records the span starting at i, if one does, and returns its end.
func (sc *shellRefScanner) slot(i int, ctx ShellRefContext, where string) (int, bool) {
	end, ok := sc.spans[i]
	if !ok {
		return i, false
	}
	p := ShellRefPosition{Context: ctx, Where: where, ArithEval: ctx == ShellRefArith}
	sc.out[i] = p
	if sc.word != nil && (ctx == ShellRefWord || ctx == ShellRefDouble || ctx == ShellRefSingle) {
		*sc.word = append(*sc.word, i)
	}
	return end, true
}

func (sc *shellRefScanner) arithEval(spans []int, where string) {
	for _, at := range spans {
		p := sc.out[at]
		if p.Context == ShellRefSingle {
			continue // a single-quoted span is not expanded: nothing reaches the evaluation
		}
		p.ArithEval, p.Where = true, where
		sc.out[at] = p
	}
}

func isShellMetaByte(c byte) bool {
	switch c {
	case ' ', '\t', '\n', ';', '&', '|', '(', ')', '<', '>':
		return true
	}
	return false
}

// shellArithOps are the `[[ … ]]` operators bash reads as integer
// comparisons, evaluating each operand as an arithmetic expression.
var shellArithOps = map[string]bool{"-eq": true, "-ne": true, "-lt": true, "-le": true, "-gt": true, "-ge": true}

// shellCommand is the state of the command a cmd() is reading, for the
// arithmetic-evaluation rules.
type shellCommand struct {
	words     int
	verb      string
	intFlag   bool  // a declaration utility saw a flag holding `i`
	cond      bool  // inside `[[ … ]]`
	arithNext bool  // the previous word was an integer operator of `[[ … ]]`
	prevSpans []int // the previous word's spans
}

// endWord applies the rules that judge a whole word, word being the text of
// the word that ends here and spans its references.
func (sc *shellRefScanner) endWord(c *shellCommand, ws int, word string, spans []int) {
	if word == "" {
		return
	}
	// An array element assignment, `name[sub]=…`: bash evaluates the
	// subscript as arithmetic.
	if eq := shellSubscriptEnd(word); eq > 0 {
		var in []int
		for _, at := range spans {
			if at-ws < eq {
				in = append(in, at)
			}
		}
		sc.arithEval(in, "an array subscript")
	}
	defer func() { c.prevSpans = spans }()
	if c.words == 0 {
		if isShellAssignmentWord(word) {
			return // a prefix assignment keeps command position
		}
		if shellReservedWord(word) {
			return // `{`, `then`, `do`, … — the command's verb comes next
		}
		c.words, c.verb = 1, word
		c.cond = word == "[["
		return
	}
	c.words++
	switch {
	case c.cond && word == "]]":
		c.cond = false
	case c.cond && shellArithOps[word]:
		sc.arithEval(c.prevSpans, "an operand of `[[ … "+word+" … ]]`")
		c.arithNext = true
	case c.arithNext:
		sc.arithEval(spans, "an operand of an integer comparison in `[[ … ]]`")
		c.arithNext = false
	case c.verb == "let":
		sc.arithEval(spans, "a `let` argument")
	case c.verb == "declare" || c.verb == "local" || c.verb == "typeset" || c.verb == "readonly" || c.verb == "export":
		if strings.HasPrefix(word, "-") && !strings.Contains(word, "=") {
			c.intFlag = c.intFlag || strings.Contains(word, "i")
		} else if c.intFlag {
			sc.arithEval(spans, "a `"+c.verb+" -i` assignment")
		}
	}
}

// shellReservedWord reports whether w, at command position, leaves it there
// for the next word.
func shellReservedWord(w string) bool {
	switch w {
	case "{", "then", "do", "else", "elif", "if", "while", "until", "!", "time":
		return true
	}
	return false
}

// isShellAssignmentWord reports whether w is `NAME=…` or `NAME[…]=…`.
func isShellAssignmentWord(w string) bool {
	n := shellNameLen(w)
	if n == 0 || n >= len(w) {
		return false
	}
	if w[n] == '=' || (w[n] == '+' && n+1 < len(w) && w[n+1] == '=') {
		return true
	}
	return shellSubscriptEnd(w) > 0
}

func shellNameLen(w string) int {
	n := 0
	for n < len(w) {
		c := w[n]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (n > 0 && c >= '0' && c <= '9') {
			n++
			continue
		}
		break
	}
	return n
}

// shellSubscriptEnd returns the index in w of the `]` closing a `NAME[` that
// an `=` or `+=` follows, or 0.
func shellSubscriptEnd(w string) int {
	n := shellNameLen(w)
	if n == 0 || n >= len(w) || w[n] != '[' {
		return 0
	}
	for _, op := range []string{"]=", "]+="} {
		if j := strings.Index(w, op); j > n {
			return j
		}
	}
	return 0
}

// cmd reads a command context from i: the script itself, or (subst) the
// inside of a `$( … )`, whose closing `)` it returns past.
func (sc *shellRefScanner) cmd(i int, subst bool) int {
	s := sc.s
	depth := 0
	wordStart := true
	ws := -1 // the start of the word being read, -1 between words
	var spans []int
	outer := sc.word
	sc.word = &spans
	if subst {
		sc.subst++
	}
	defer func() {
		sc.word = outer
		if subst {
			sc.subst--
		}
	}()
	var c shellCommand
	end := func(at int) {
		if ws >= 0 {
			sc.endWord(&c, ws, s[ws:at], spans)
		}
		ws, spans = -1, nil
	}
	begin := func(at int) {
		if ws < 0 {
			ws = at
		}
	}
	for !sc.done(i) {
		if e, ok := sc.slot(i, ShellRefWord, ""); ok {
			begin(i)
			i, wordStart = e, false
			continue
		}
		ch := s[i]
		switch {
		case ch == '#' && wordStart:
			end(i)
			i = sc.comment(i)
			continue
		case ch == '\'':
			begin(i)
			i, wordStart = sc.single(i+1, "inside single quotes"), false
			continue
		case ch == '"':
			begin(i)
			i, wordStart = sc.double(i+1), false
			continue
		case ch == '`':
			begin(i)
			i, wordStart = sc.backquote(i+1), false
			continue
		case ch == '\\':
			if i+1 < len(s) && s[i+1] == '\n' {
				i += 2 // a line continuation: the two bytes are not there
				continue
			}
			begin(i)
			if _, ok := sc.spans[i+1]; ok {
				i, wordStart = i+1, false // the span is read next, glued to the backslash
				continue
			}
			i, wordStart = i+2, false
			continue
		case ch == '$':
			begin(i)
			i, wordStart = sc.dollar(i, false), false
			continue
		case ch == '(' && wordStart && i+1 < len(s) && s[i+1] == '(':
			end(i)
			i = sc.arith(i + 2)
			wordStart = false
			continue
		case ch == '(':
			end(i)
			if !c.cond {
				c = shellCommand{}
			}
			depth++
		case ch == ')':
			end(i)
			if !c.cond {
				c = shellCommand{}
			}
			if subst {
				if depth == 0 {
					return i + 1
				}
				depth--
			}
		case ch == '<' && i+1 < len(s) && s[i+1] == '<':
			end(i)
			if i+2 < len(s) && s[i+2] == '<' {
				i, wordStart = i+3, true // a here-string: a word follows
				continue
			}
			i = sc.heredocOperator(i + 2)
			wordStart = true
			continue
		case ch == '\n':
			end(i)
			if !c.cond {
				c = shellCommand{}
			}
			if len(sc.pending) > 0 {
				i = sc.heredocBodies(i + 1)
				wordStart = true
				continue
			}
		case ch == ';' || ch == '&' || ch == '|':
			end(i)
			if !c.cond || ch == ';' {
				c = shellCommand{}
			}
		case isShellMetaByte(ch):
			end(i)
		case subst && wordStart && strings.HasPrefix(s[i:], "case") && (i+4 >= len(s) || isShellMetaByte(s[i+4])):
			// A `case` pattern's `)` reads, to a parenthesis count, as the
			// substitution's end.
			sc.lost = true
			return i
		default:
			begin(i)
		}
		wordStart = isShellMetaByte(ch)
		i++
	}
	end(i)
	if subst {
		sc.openAtEnd()
	}
	return i
}

// comment reads a comment from its `#` to the end of the line, leaving the
// newline to the command.
func (sc *shellRefScanner) comment(i int) int {
	for !sc.done(i) && sc.s[i] != '\n' {
		if e, ok := sc.slot(i, ShellRefComment, "in a comment"); ok {
			i = e
			continue
		}
		i++
	}
	return i
}

// single reads a single-quoted span from past its opening quote.
func (sc *shellRefScanner) single(i int, where string) int {
	for !sc.done(i) {
		if e, ok := sc.slot(i, ShellRefSingle, where); ok {
			i = e
			continue
		}
		if sc.s[i] == '\'' {
			return i + 1
		}
		i++
	}
	sc.openAtEnd()
	return i
}

// double reads a double-quoted span from past its opening quote.
func (sc *shellRefScanner) double(i int) int {
	s := sc.s
	for !sc.done(i) {
		if e, ok := sc.slot(i, ShellRefDouble, ""); ok {
			i = e
			continue
		}
		switch s[i] {
		case '"':
			return i + 1
		case '\\':
			if _, ok := sc.spans[i+1]; ok {
				i++
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
	sc.openAtEnd()
	return i
}

// dollar reads what a `$` at i opens: a substitution, an arithmetic
// expansion, a `${ … }` word, a `$'…'` or `$"…"` string, or nothing.
func (sc *shellRefScanner) dollar(i int, inDouble bool) int {
	s := sc.s
	if _, ok := sc.spans[i+1]; ok || i+1 >= len(s) {
		return i + 1 // a `$` glued to a reference is a literal byte before it
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
		sc.lost = true // bash's legacy `$[ … ]`, dash's two literal bytes
		return i
	case '\'':
		if inDouble {
			return i + 1
		}
		return sc.ansiC(i + 2)
	case '"':
		if inDouble {
			return i + 1
		}
		return sc.double(i + 2)
	}
	return i + 1
}

// ansiC reads a `$'…'` string. bash decodes escapes in it, so `\'` does not
// end it; a sh without `$'…'` (dash) reads a `$` and a single-quoted span,
// which `\'` ends — the two readings part at the first backslash.
func (sc *shellRefScanner) ansiC(i int) int {
	s := sc.s
	for !sc.done(i) {
		if e, ok := sc.slot(i, ShellRefSingle, "inside a $'…' string"); ok {
			i = e
			continue
		}
		switch s[i] {
		case '\'':
			return i + 1
		case '\\':
			if !sc.bash {
				sc.lost = true
				return i
			}
			i += 2
			continue
		}
		i++
	}
	sc.openAtEnd()
	return i
}

// param reads a `${ … }` word from past its `${`. A reference in it reads
// as a word, or as a double-quoted string inside double quotes; one in a
// subscript (`${a[…]}`) or an offset (`${s:…}`) is evaluated by bash as
// arithmetic.
func (sc *shellRefScanner) param(i int, inDouble bool) int {
	s := sc.s
	ctx := ShellRefWord
	if inDouble {
		ctx = ShellRefDouble
	}
	// The parameter's name — `#` or `!` before it, then an identifier, a
	// position or one special character — then a subscript or an offset,
	// which bash evaluates as arithmetic.
	if i < sc.limit && (s[i] == '#' || s[i] == '!') {
		i++
	}
	switch {
	case i < sc.limit && (s[i] == '_' || (s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z')):
		i += shellNameLen(s[i:sc.limit])
	case i < sc.limit && s[i] >= '0' && s[i] <= '9':
		for i < sc.limit && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	case i < sc.limit && strings.IndexByte("@*#?$!-", s[i]) >= 0:
		i++
	}
	arith := ""
	if i < sc.limit && s[i] == '[' {
		arith = "a `${…[…]}` subscript"
	} else if i+1 < sc.limit && s[i] == ':' && strings.IndexByte("-=?+", s[i+1]) < 0 {
		arith = "a `${…:offset}` substring offset"
	}
	for !sc.done(i) {
		if e, ok := sc.slot(i, ctx, ""); ok {
			if arith != "" {
				sc.arithEval([]int{i}, arith)
			}
			i = e
			continue
		}
		switch s[i] {
		case '}':
			return i + 1
		case ']':
			if strings.HasSuffix(arith, "subscript") {
				arith = ""
			}
		case '{':
			sc.lost = true // a brace inside ${ … }, or bash 5.3's `${ cmd; }`
			return i
		case '\'':
			if inDouble {
				sc.lost = true // shells read a single quote inside a double-quoted ${ … } differently
				return i
			}
			i = sc.single(i+1, "inside single quotes")
			continue
		case '"':
			i = sc.double(i + 1)
			continue
		case '`':
			i = sc.backquote(i + 1)
			continue
		case '\\':
			if _, ok := sc.spans[i+1]; ok {
				i++
				continue
			}
			i += 2
			continue
		case '$':
			i = sc.dollar(i, inDouble)
			continue
		}
		i++
	}
	sc.openAtEnd()
	return i
}

// arith reads an arithmetic expansion or command from past its `((`, to
// past its `))`.
func (sc *shellRefScanner) arith(i int) int {
	s := sc.s
	depth := 0
	for !sc.done(i) {
		if e, ok := sc.slot(i, ShellRefArith, "inside arithmetic"); ok {
			i = e
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
			sc.lost = true
			return i
		case '\'', '"':
			sc.lost = true
			return i
		case '`':
			i = sc.backquote(i + 1)
			continue
		case '\\':
			i += 2
			continue
		case '$':
			i = sc.dollar(i, true)
			continue
		}
		i++
	}
	sc.openAtEnd()
	return i
}

// backquote reads a backquote substitution from past its opening backquote:
// it ends at the first unescaped backquote, and its text is read again as a
// command, so a reference in it is a word.
func (sc *shellRefScanner) backquote(i int) int {
	s := sc.s
	for !sc.done(i) {
		if e, ok := sc.slot(i, ShellRefWord, ""); ok {
			i = e
			continue
		}
		switch s[i] {
		case '`':
			return i + 1
		case '\\':
			if _, ok := sc.spans[i+1]; ok {
				i++
				continue
			}
			i += 2
			continue
		}
		i++
	}
	sc.openAtEnd()
	return i
}

// heredocOperator reads the delimiter word of a `<<` / `<<-` from past the
// `<<` (parseHeredocDelim's quote removal), and queues the heredoc for the
// next newline of a command.
func (sc *shellRefScanner) heredocOperator(i int) int {
	rest := sc.s[i:sc.limit]
	for j := 0; j < len(rest) && !isShellMetaByte(rest[j]); j++ {
		if _, ok := sc.spans[i+j]; ok {
			sc.lost = true // a reference in a heredoc delimiter
			return i
		}
	}
	delim, strip, quoted, n := parseHeredocDelim(rest)
	if delim == "" || strings.ContainsAny(delim, "$`") || strings.Contains(delim, "{{") {
		sc.lost = true
		return i
	}
	sc.pending = append(sc.pending, shellHeredoc{delim: delim, strip: strip, quoted: quoted})
	return i + n
}

// heredocBodies reads the bodies of the queued heredocs, in order, from the
// start of the line after the command that queued them, and returns the
// offset past the last delimiter line.
func (sc *shellRefScanner) heredocBodies(i int) int {
	queued := sc.pending
	sc.pending = nil
	for _, h := range queued {
		start, end := i, -1
		for i < sc.limit {
			lineEnd := sc.limit
			if nl := strings.IndexByte(sc.s[i:sc.limit], '\n'); nl >= 0 {
				lineEnd = i + nl
			}
			line := sc.s[i:lineEnd]
			if h.strip {
				line = strings.TrimLeft(line, "\t")
			}
			next := lineEnd
			if next < sc.limit {
				next++
			}
			if line == h.delim {
				end, i = i, next
				break
			}
			// Inside `$( … )`, bash ends the body at a line that starts with
			// the delimiter and holds a `)`, and reads the rest of the line
			// as the command (`EOF)` closing the substitution).
			if sc.subst > 0 && strings.HasPrefix(line, h.delim) && strings.Contains(line[len(h.delim):], ")") {
				sc.heredocBody(start, i, h)
				return lineEnd - len(line) + len(h.delim)
			}
			if !h.quoted && (len(line)-len(strings.TrimRight(line, `\`)))%2 == 1 {
				sc.lost = true // the shell joins the next line to this one before it looks for the delimiter
				return i
			}
			i = next
		}
		if end < 0 {
			sc.lost = true // a heredoc that never ends
			return i
		}
		sc.heredocBody(start, end, h)
		if sc.lost {
			return i
		}
	}
	return i
}

// heredocBody classifies the spans of one heredoc body, [start, end).
func (sc *shellRefScanner) heredocBody(start, end int, h shellHeredoc) {
	outer := sc.word
	sc.word = nil
	defer func() { sc.word = outer }()
	if h.quoted {
		for at := range sc.spans {
			if at >= start && at < end {
				sc.out[at] = ShellRefPosition{Context: ShellRefLiteralHeredoc, Where: "in the body of a heredoc whose delimiter is quoted"}
			}
		}
		return
	}
	limit := sc.limit
	sc.limit = end
	defer func() { sc.limit = limit }()
	s := sc.s
	for i := start; !sc.done(i); {
		if e, ok := sc.slot(i, ShellRefHeredoc, ""); ok {
			i = e
			continue
		}
		switch s[i] {
		case '\\':
			if _, ok := sc.spans[i+1]; ok {
				i++
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
