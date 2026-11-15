package ir

import (
	"strings"
)

// A sh or bash `script:` receives each data reference (ShellScriptData)
// through its interpreter's environment: the reference renders to a read of
// a variable the engine names for that one rendering, and the value never
// enters the script's text (pkg/backend/model, renderShellScript), in one
// context-free form (`${N+"$N"}`) but in an unquoted heredoc's body. What the
// shell does with the read still depends on where it stands, and
// ShellScriptRefPositions is that one reading — for the renderer, which
// writes a heredoc's read and refuses a value bash would evaluate where it is
// sure of such a position, and for the compiler, which warns (C343 – C346).
//
// The reading is a lexer, not the shell, and it is allowed to be wrong: the
// read a misread position gets is the same one, so a misread costs a
// heredoc's JSON form, a refusal or a warning missed or one too many — what
// the value becomes is the shell's reading of the author's text either way.
// Where it knows it lost the shell's reading (a `case` inside `$( … )`, a
// heredoc that never ends, a `$((` closed by one `)`, a delimiter-led line
// holding `)` in a sh script, …) it stops, and every later reference is a
// plain word, marked Unsure so no warning or refusal rests on it.

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
	// NameTaking: bash takes the expanded word as a variable NAME — after
	// `[[ -v`, `test -v`, `printf -v`, `read`, `unset`, `export`, the name
	// of a `declare NAME=…`, the value of `declare -n r=…` — and evaluates
	// the array subscript a name holds (`a[$(cmd)]`) the same way.
	NameTaking bool
	// CommandWord: the reference is a command's first word — the shell runs
	// the value as a program.
	CommandWord bool
	// Code: the word is code another shell parses — the string of `sh -c`,
	// an `eval` or `trap` argument, the command `ssh` hands the remote
	// shell, the value of PS4 / BASH_ENV / ENV / PROMPT_COMMAND, an `alias`
	// — or the body of a heredoc a shell reads as its script. A
	// single-quoted read there is the safe form: the other shell reads the
	// variable from the environment it inherits. A double-quoted one, or
	// one in an unquoted heredoc, hands it the value, which it parses.
	Code bool
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
	// arithWhere, when set, marks every span recorded as evaluated by bash
	// as arithmetic, with that position's name: a `${…}` subscript or
	// offset holds a double-quoted string as well as a word.
	arithWhere string
	// intVars are the names a `declare -i` (or local, typeset) declared: an
	// assignment to one later is evaluated as arithmetic.
	intVars map[string]bool
}

type shellHeredoc struct {
	delim  string
	strip  bool
	quoted bool
	fed    bool // the command reading it is a shell: the body is its script
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
	if sc.arithWhere != "" && ctx != ShellRefSingle {
		p.ArithEval, p.Where = true, sc.arithWhere
	}
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

// mark sets what set does on the position of each span of spans but the
// single-quoted ones, which nothing expands.
func (sc *shellRefScanner) mark(spans []int, set func(*ShellRefPosition)) {
	for _, at := range spans {
		p := sc.out[at]
		if p.Context == ShellRefSingle {
			continue
		}
		set(&p)
		sc.out[at] = p
	}
}

// markCode marks spans as code another shell parses, single-quoted ones
// included: there the other shell reads the variable itself.
func (sc *shellRefScanner) markCode(spans []int, where string) {
	for _, at := range spans {
		p := sc.out[at]
		p.Code = true
		if p.Where == "" || p.Context == ShellRefSingle {
			p.Where = where
		}
		sc.out[at] = p
	}
}

func (sc *shellRefScanner) nameTaking(spans []int, where string) {
	sc.mark(spans, func(p *ShellRefPosition) { p.NameTaking, p.Where = true, where })
}

// shellRunner reports whether the command verb runs a shell that reads its
// `-c` string or stdin as a script.
func shellRunner(verb string) bool {
	if i := strings.LastIndexByte(verb, '/'); i >= 0 {
		verb = verb[i+1:]
	}
	switch verb {
	case "sh", "bash", "dash", "zsh", "ksh", "ash", "mksh", "ssh":
		return true
	}
	return false
}

// shellCodeVars are the variables whose value a shell runs or evaluates.
var shellCodeVars = map[string]bool{"PS4": true, "PS1": true, "PS2": true, "BASH_ENV": true, "ENV": true, "PROMPT_COMMAND": true}

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
	intFlag   bool   // a declaration utility saw a flag holding `i`
	refFlag   bool   // a declaration utility saw a flag holding `n`
	cond      bool   // inside `[[ … ]]`
	arithNext bool   // the previous word was an integer operator of `[[ … ]]`
	nameNext  bool   // the next word is a name (after `-v`, `printf -v`, `read -a`)
	skipNext  bool   // the next word is an option's value (`read -p prompt`)
	codeNext  bool   // the next word is a shell's `-c` string
	hasC      bool   // the shell runs a `-c` string, not its stdin
	hostSeen  bool   // `ssh` saw its host: the words after it are the remote command
	array     bool   // inside a compound array assignment's `( … )`: its words are values
	prevWord  string // the previous word
	prevSpans []int  // the previous word's spans
}

// endWord applies the rules that judge a whole word, word being the text of
// the word that ends here and spans its references.
func (sc *shellRefScanner) endWord(c *shellCommand, ws int, word string, spans []int) {
	if word == "" {
		return
	}
	// An array element assignment, `name[sub]=…`, and a compound array's
	// index, `([sub]=…)`: bash evaluates the subscript as arithmetic.
	if eq := shellSubscriptEnd(word); eq > 0 {
		sc.arithEval(spansBefore(spans, ws, eq), "an array subscript")
	} else if strings.HasPrefix(word, "[") {
		if j := strings.Index(word, "]="); j > 0 {
			sc.arithEval(spansBefore(spans, ws, j), "a compound array assignment's index")
		}
	}
	defer func() { c.prevSpans, c.prevWord = spans, word }()
	if c.array {
		return // a compound array's element, not a command's word
	}
	if c.words == 0 {
		if isShellAssignmentWord(word) {
			sc.assignment(word, ws, spans, "") // a prefix assignment keeps command position
			return
		}
		if shellReservedWord(word) {
			return // `{`, `then`, `do`, … — the command's verb comes next
		}
		c.words, c.verb = 1, word
		c.cond = word == "[["
		sc.mark(spans, func(p *ShellRefPosition) {
			p.CommandWord = true
			if p.Where == "" {
				p.Where = "the command word"
			}
		})
		return
	}
	c.words++
	flag := strings.HasPrefix(word, "-") && word != "-" && word != "--" && !strings.Contains(word, "=")
	switch {
	case c.skipNext:
		c.skipNext = false
	case c.nameNext:
		sc.nameTaking(spans, "a variable name after `"+c.prevWord+"`")
		c.nameNext = false
	case c.codeNext && !flag:
		sc.markCode(spans, "the `-c` string of `"+c.verb+"`")
		c.codeNext = false
	case c.cond && word == "]]":
		c.cond = false
	case c.cond && shellArithOps[word]:
		sc.arithEval(c.prevSpans, "an operand of `[[ … "+word+" … ]]`")
		c.arithNext = true
	case c.arithNext:
		sc.arithEval(spans, "an operand of an integer comparison in `[[ … ]]`")
		c.arithNext = false
	case (c.cond || c.verb == "test" || c.verb == "[") && word == "-v":
		c.nameNext = true
	case c.verb == "let":
		sc.arithEval(spans, "a `let` argument")
	case c.verb == "printf" && word == "-v":
		c.nameNext = true
	case c.verb == "read":
		switch {
		case word == "-a":
			c.nameNext = true
		case flag && strings.ContainsAny(word[len(word)-1:], "pdtnNui"):
			c.skipNext = true
		case !flag:
			sc.nameTaking(spans, "a `read` variable name")
		}
	case c.verb == "unset" && !flag:
		sc.nameTaking(spans, "an `unset` name")
	case c.verb == "eval":
		sc.markCode(spans, "an `eval` argument")
	case c.verb == "trap" && c.words == 2:
		sc.markCode(spans, "the command `trap` runs")
	case c.verb == "alias":
		sc.markCode(spans, "an `alias` definition")
	case strings.HasSuffix(c.verb, "ssh"):
		if !flag && c.hostSeen {
			sc.markCode(spans, "the command `ssh` hands the remote shell")
		} else if !flag {
			c.hostSeen = true
		}
	case shellRunner(c.verb) && flag && !strings.HasPrefix(word, "--") && strings.Contains(word, "c"):
		c.codeNext, c.hasC = true, true
	case c.verb == "declare" || c.verb == "local" || c.verb == "typeset" || c.verb == "readonly" || c.verb == "export":
		if flag {
			c.intFlag = c.intFlag || strings.Contains(word, "i")
			c.refFlag = c.refFlag || strings.Contains(word, "n")
			break
		}
		how := ""
		switch {
		case c.intFlag:
			how = "a `" + c.verb + " -i` assignment"
		case c.refFlag:
			how = "nameref"
		}
		sc.assignment(word, ws, spans, how)
		if eq := strings.IndexByte(word, '='); eq < 0 {
			sc.nameTaking(spans, "a name `"+c.verb+"` declares")
			if c.intFlag && shellNameLen(word) == len(word) {
				sc.declareInt(word)
			}
		} else {
			sc.nameTaking(spansBefore(spans, ws, eq), "a name `"+c.verb+"` declares")
			if c.intFlag && shellNameLen(word) == eq {
				sc.declareInt(word[:eq])
			}
		}
	}
}

// assignment applies to a `NAME=value` word the rules of its name: a code
// variable's value is code, an integer variable's (declared `-i`, or how
// says so) is arithmetic, a nameref's is a name.
func (sc *shellRefScanner) assignment(word string, ws int, spans []int, how string) {
	eq := strings.IndexByte(word, '=')
	if eq <= 0 {
		return
	}
	name := strings.TrimSuffix(word[:eq], "+")
	var value []int
	for _, at := range spans {
		if at-ws > eq {
			value = append(value, at)
		}
	}
	switch {
	case shellCodeVars[name]:
		sc.markCode(value, "the value of "+name+", which the shell runs")
	case how == "nameref":
		sc.nameTaking(value, "the target of a `declare -n` nameref")
	case how != "":
		sc.arithEval(value, how)
	case sc.intVars[name]:
		sc.arithEval(value, "an assignment to "+name+", declared `-i`")
	}
}

func (sc *shellRefScanner) declareInt(name string) {
	if sc.intVars == nil {
		sc.intVars = map[string]bool{}
	}
	sc.intVars[name] = true
}

// spansBefore is the spans of a word starting at ws that start before the
// word's byte end.
func spansBefore(spans []int, ws, end int) []int {
	var in []int
	for _, at := range spans {
		if at-ws < end {
			in = append(in, at)
		}
	}
	return in
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
			where := "inside `(( … ))`"
			if !sc.bash {
				where = "inside `(( … ))` — arithmetic to bash, a nested subshell's command to dash"
			}
			i = sc.arith(i+2, where)
			wordStart = false
			continue
		case ch == '(':
			end(i)
			if i > 0 && s[i-1] == '=' {
				c.array = true // `name=( … )`: the words are values
			} else if !c.cond {
				c = shellCommand{}
			}
			depth++
		case ch == ')':
			end(i)
			if c.array {
				c.array = false
			} else if !c.cond {
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
			i = sc.heredocOperator(i+2, shellRunner(c.verb) && !c.hasC)
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
			return sc.arith(i+3, "inside `$(( … ))`")
		}
		return sc.cmd(i+2, true)
	case '{':
		return sc.param(i+2, inDouble)
	case '[':
		if !sc.bash {
			sc.lost = true // bash's legacy `$[ … ]` is two literal bytes to dash
			return i
		}
		return sc.arithBracket(i + 2)
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
	outer := sc.arithWhere
	defer func() { sc.arithWhere = outer }()
	offset := func(at int) {
		if at+1 < sc.limit && s[at] == ':' && strings.IndexByte("-=?+", s[at+1]) < 0 {
			sc.arithWhere = "a `${…:offset}` substring offset"
		}
	}
	if i < sc.limit && s[i] == '[' {
		sc.arithWhere = "a `${…[…]}` subscript"
	} else {
		offset(i)
	}
	for !sc.done(i) {
		if e, ok := sc.slot(i, ctx, ""); ok {
			i = e
			continue
		}
		switch s[i] {
		case '}':
			return i + 1
		case ']':
			if strings.HasSuffix(sc.arithWhere, "subscript") {
				sc.arithWhere = outer
				offset(i + 1)
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
// past its `))`. Its references are recorded only once the `))` is found:
// bash reads a `$((` whose parentheses do not close as `))` as a command
// substitution of a subshell, and so does dash a `((` command.
func (sc *shellRefScanner) arith(i int, where string) int {
	s := sc.s
	depth := 0
	var pending []int
	for !sc.done(i) {
		if e, ok := sc.spans[i]; ok {
			pending = append(pending, i)
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
				for _, at := range pending {
					sc.out[at] = ShellRefPosition{Context: ShellRefArith, ArithEval: true, Where: where}
				}
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

// arithBracket reads bash's legacy `$[ … ]` arithmetic from past its `$[`.
func (sc *shellRefScanner) arithBracket(i int) int {
	s := sc.s
	depth := 0
	for !sc.done(i) {
		if e, ok := sc.slot(i, ShellRefArith, "inside `$[ … ]`"); ok {
			i = e
			continue
		}
		switch s[i] {
		case '[':
			depth++
		case ']':
			if depth == 0 {
				return i + 1
			}
			depth--
		case '\'', '"':
			sc.lost = true
			return i
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
func (sc *shellRefScanner) heredocOperator(i int, fed bool) int {
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
	sc.pending = append(sc.pending, shellHeredoc{delim: delim, strip: strip, quoted: quoted, fed: fed})
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
				if !sc.bash {
					sc.lost = true // dash reads the line as the body's, bash (devbox's sh) as its end
					return i
				}
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
	if h.fed {
		// The body is a shell's script: mark what the scan below records.
		defer func() {
			for at := range sc.spans {
				if at >= start && at < end {
					if p, ok := sc.out[at]; ok && (p.Context == ShellRefHeredoc || p.Context == ShellRefLiteralHeredoc) {
						p.Code, p.Where = true, "the body of a heredoc a shell reads as its script"
						sc.out[at] = p
					}
				}
			}
		}()
	}
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
