package author

import (
	"bytes"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// speller turns the YAML tree into `.bot` text, one line at a time, each
// line remembering the YAML node it stands for. The text is always written
// in strict-escape mode — profile 2 reads it so; a profile-1 document
// opens with the directive — so that every string has the one quoted form
// unparse.QuoteStrict gives it, whatever it holds.
type speller struct {
	name    string
	profile int
	// src is the document's text, line by line: what a position computed
	// from a value is checked against before it is claimed (edgeAt).
	src   []string
	lines []line
	diags []parser.Diagnostic
}

// line is one line of the spelled text and where it came from: the node of
// the key or the construct (at), the node of the value when the line
// carries one (val), and the column the value starts at in the spelled
// line (valCol, 0 when the line carries none).
type line struct {
	text   string
	at     pos
	val    pos
	valCol int
}

type pos struct{ line, col int }

func at(n *yaml.Node) pos {
	if n == nil {
		return pos{}
	}
	return pos{n.Line, n.Column}
}

// yamlBreaks cuts the source where yaml.v3's scanner ends a line — CRLF,
// CR, LF, NEL, LS and PS (its is_break) — so the line a node's Line names
// is the line edgeAt reads back; cut on LF alone, every line after a lone
// CR would be read one off, and a line that happens to carry the same text
// would confirm a column the author never wrote.
var yamlBreaks = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u0085", "\n", "\u2028", "\n", "\u2029", "\n")

// spell renders the document rooted at root. It returns the text, its
// line map, the converter's diagnostics and the profile the document
// declares (1 when it declares none, or one the build cannot read).
func spell(name string, src []byte, root *yaml.Node) (text string, lines []line, diags []parser.Diagnostic, profile int) {
	s := &speller{name: name, profile: 1, src: strings.Split(yamlBreaks.Replace(string(src)), "\n")}
	s.document(root)
	var b strings.Builder
	for _, l := range s.lines {
		b.WriteString(l.text)
		b.WriteByte('\n')
	}
	return b.String(), s.lines, s.diags, s.profile
}

// ---- output ----

func (s *speller) emit(indent int, text string, n *yaml.Node) {
	s.lines = append(s.lines, line{text: strings.Repeat(" ", indent) + text, at: at(n)})
}

// kv writes `key: value` with the value's own node for the positions of
// what the parser says about the value.
func (s *speller) kv(indent int, key, value string, k, v *yaml.Node) {
	text := strings.Repeat(" ", indent) + key + ": " + value
	l := line{text: text, at: at(k), val: at(v), valCol: indent + len(key) + 3}
	if v == nil {
		l.valCol = 0
	}
	s.lines = append(s.lines, l)
}

// header writes a line that opens a body (`agent x:`, `budget:`).
func (s *speller) header(indent int, text string, n *yaml.Node) { s.emit(indent, text, n) }

// bare writes a header with nothing under it and the blank line the
// parser reads an empty body by (parser.bodyIsEmpty, blockBodyAfter).
func (s *speller) bare(indent int, text string, n *yaml.Node) {
	s.emit(indent, text, n)
	s.blank()
}

func (s *speller) blank() { s.lines = append(s.lines, line{}) }

func (s *speller) comment(text string, n *yaml.Node) {
	if text == "" {
		s.emit(0, "##", n)
		return
	}
	s.emit(0, "## "+text, n)
}

// ---- diagnostics ----

func (s *speller) diag(n *yaml.Node, code parser.DiagCode, severity parser.Severity, msg, hint string) {
	p := at(n)
	if p.line < 1 {
		p = pos{1, 1}
	}
	if hint == "" {
		hint = parser.HintFor(code)
	}
	s.diags = append(s.diags, parser.Diagnostic{
		Code: code, Severity: severity, Message: msg,
		File: s.name, Line: p.line, Column: p.col, Hint: hint,
	})
}

func (s *speller) refuse(n *yaml.Node, msg string) {
	s.diag(n, parser.DiagAuthorValue, parser.SeverityError, msg, "")
}

func (s *speller) refuseHint(n *yaml.Node, code parser.DiagCode, msg, hint string) {
	s.diag(n, code, parser.SeverityError, msg, hint)
}

func (s *speller) warn(n *yaml.Node, code parser.DiagCode, msg string) {
	s.diag(n, code, parser.SeverityWarning, msg, "")
}

// ---- tree helpers ----

type pair struct{ key, val *yaml.Node }

func pairsOf(m *yaml.Node) []pair {
	out := make([]pair, 0, len(m.Content)/2)
	for i := 0; i+1 < len(m.Content); i += 2 {
		out = append(out, pair{m.Content[i], m.Content[i+1]})
	}
	return out
}

func isNull(n *yaml.Node) bool {
	return n == nil || (n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null")
}

// mapping reads n as the mapping `what` takes, or says what it is instead.
func (s *speller) mapping(n *yaml.Node, what string) ([]pair, bool) {
	if n != nil && n.Kind == yaml.MappingNode {
		return pairsOf(n), true
	}
	s.refuse(n, what+" takes a mapping (an indented block of `key: value` lines), got "+kindWord(n))
	return nil, false
}

// sequence reads n as the list `what` takes.
func (s *speller) sequence(n *yaml.Node, what string) ([]*yaml.Node, bool) {
	if n != nil && n.Kind == yaml.SequenceNode {
		return n.Content, true
	}
	s.refuse(n, what+" takes a list (`[a, b]`, or one `- item` per line), got "+kindWord(n))
	return nil, false
}

func find(pairs []pair, key string) *yaml.Node {
	for _, p := range pairs {
		if p.key.Value == key {
			return p.val
		}
	}
	return nil
}

func without(pairs []pair, keys ...string) []pair {
	out := make([]pair, 0, len(pairs))
	for _, p := range pairs {
		skip := false
		for _, k := range keys {
			if p.key.Value == k {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, p)
		}
	}
	return out
}

// ---- lexical shapes of the .bot ----

// isIdent reports whether s is one bare identifier as the lexer scans it:
// a letter or `_`, then letters, digits and `_`.
func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || unicode.IsLetter(r):
		case i > 0 && unicode.IsDigit(r):
		default:
			return false
		}
	}
	return true
}

// isDotted reports an identifier, dotted or not (`r1.look`, `github.com`).
func isDotted(s string) bool {
	for _, part := range strings.Split(s, ".") {
		if !isIdent(part) {
			return false
		}
	}
	return true
}

var (
	typeRefRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\[\])*$`)
	envFormRe  = regexp.MustCompile(spec.EnvFormPattern)
	numberRe   = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	typeWordRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\[\])?$`)
)

// q is the one quoted form every string has in the spelled text.
func q(v string) string { return unparse.QuoteStrict(v) }

func intOf(n *yaml.Node) (int64, bool) {
	var v int64
	return v, n.Decode(&v) == nil
}

func floatOf(n *yaml.Node) (float64, bool) {
	var v float64
	return v, n.Decode(&v) == nil
}

func boolOf(n *yaml.Node) (bool, bool) {
	var v bool
	return v, n.Decode(&v) == nil
}

// floatText spells a float as the lexer reads one: as written when the
// spelling is digits and a fraction, else the shortest decimal — with a
// fraction when the form must read as a float literal.
func floatText(n *yaml.Node, f float64, fraction bool) string {
	if numberRe.MatchString(n.Value) && (!fraction || strings.Contains(n.Value, ".")) {
		return n.Value
	}
	t := strconv.FormatFloat(f, 'f', -1, 64)
	if fraction && !strings.Contains(t, ".") {
		t += ".0"
	}
	return t
}

// ---- values by form ----

// scalar spells the value a form takes on one line — a scalar, or an
// inline list — or reports why it cannot, naming the property (what).
func (s *speller) scalar(f spec.Form, values []string, what string, n *yaml.Node) (string, bool) {
	switch f {
	case spec.IdentList:
		return s.identList(what, n)
	case spec.StringList, spec.ToolList, spec.SkillList, spec.MixedList:
		return s.stringList(what, n)
	case spec.IdentOrList:
		if n != nil && n.Kind == yaml.SequenceNode {
			return s.identList(what, n)
		}
		return s.ident(what, n, false)
	case spec.IntOrStringList:
		if n != nil && n.Kind == yaml.SequenceNode {
			return s.stringList(what, n)
		}
		if n != nil && n.Kind == yaml.ScalarNode && n.ShortTag() == "!!int" {
			return s.integer(what, n)
		}
		s.refuse(n, what+" takes a count (an integer) or a pool of member ids (a list of strings), got "+kindWord(n))
		return "", false
	case spec.JSON:
		return s.jsonText(what, n)
	}
	if n == nil || n.Kind != yaml.ScalarNode {
		s.refuse(n, what+" takes one value ("+string(f)+"), got "+kindWord(n))
		return "", false
	}
	tag := n.ShortTag()
	switch f {
	case spec.String, spec.QuotedString, spec.StringOrIdent:
		return s.str(what, n)
	case spec.PromptRef:
		if tag != "!!str" {
			s.refuse(n, what+" takes a prompt: a declared prompt's name, or the text itself as a string; got "+tagWord(tag))
			return "", false
		}
		if n.Style == 0 && isIdent(n.Value) {
			return n.Value, true
		}
		return q(n.Value), true
	case spec.Enum:
		if tag == "!!null" {
			s.refuse(n, what+" takes one of "+strings.Join(values, ", ")+", got nothing")
			return "", false
		}
		return q(n.Value), true
	case spec.EnumOrEnv:
		// The .bot takes one of the words, bare or quoted — or ANY quoted
		// string, kept as written for a run-time substitution ('$EFFORT',
		// '${EFFORT:-high}', a template). The document says quoted with a
		// quoted scalar; a plain scalar is a word of the list, or an
		// environment form, unmistakable without quotes.
		if tag != "!!str" {
			s.refuse(n, what+" takes one of "+strings.Join(values, ", ")+", or any other value in quotes, got "+tagWord(tag))
			return "", false
		}
		if n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0 {
			return q(n.Value), true
		}
		for _, v := range values {
			if n.Value == v {
				return q(n.Value), true
			}
		}
		if envFormRe.MatchString(n.Value) {
			return q(n.Value), true
		}
		s.refuse(n, what+" takes one of "+strings.Join(values, ", ")+" bare, or any other value in quotes, kept as written for a run-time substitution ('$EFFORT', '${EFFORT:-high}', a template), got "+strconv.Quote(n.Value))
		return "", false
	case spec.Ident:
		return s.ident(what, n, false)
	case spec.DottedIdent:
		return s.ident(what, n, true)
	case spec.TypeRef:
		if tag != "!!str" || !typeRefRe.MatchString(n.Value) {
			s.refuse(n, what+" takes a type name — a builtin type or a declared schema's name, `[]` suffixes allowed — got "+describe(n))
			return "", false
		}
		return n.Value, true
	case spec.Int:
		return s.integer(what, n)
	case spec.Number:
		return s.number(what, n)
	case spec.Bool:
		if tag != "!!bool" {
			s.refuse(n, what+" takes true or false, got "+describe(n)+" (YAML 1.2 reads yes, no, on and off as strings)")
			return "", false
		}
		b, _ := boolOf(n)
		return strconv.FormatBool(b), true
	case spec.StringOrNumber, spec.Setting:
		switch tag {
		case "!!str":
			return q(n.Value), true
		case "!!int":
			v, ok := intOf(n)
			if !ok {
				s.refuse(n, what+": the integer "+n.Value+" is out of range")
				return "", false
			}
			return q(strconv.FormatInt(v, 10)), true
		case "!!float":
			v, ok := floatOf(n)
			if !ok || math.IsInf(v, 0) || math.IsNaN(v) {
				s.refuse(n, what+" takes a finite number, got "+n.Value)
				return "", false
			}
			return q(floatText(n, v, false)), true
		}
		s.refuse(n, what+" takes a string, a bare word or a number, got "+tagWord(tag))
		return "", false
	case spec.Literal:
		return s.literal(what, n)
	}
	s.refuse(n, what+": no spelling for the form "+string(f))
	return "", false
}

// str spells a string-valued property: the scalar must READ as a string.
// A bare date, a number or a bool where a string is wanted is the YAML
// reading the author did not mean; the remedy is to quote it.
func (s *speller) str(what string, n *yaml.Node) (string, bool) {
	if n.ShortTag() != "!!str" {
		s.refuse(n, what+" takes a string; `"+n.Value+"` reads as "+tagWord(n.ShortTag())+" — quote it")
		return "", false
	}
	v := n.Value
	switch n.Style {
	case yaml.LiteralStyle, yaml.FoldedStyle:
		// A block scalar is read as the scanner read it: a line separator
		// it left in the value is the line break it meant (blockBreaks) —
		// the rule a prompt body has, for every text of the document.
		var ok bool
		if v, ok = s.blockBreaks(what, n, v); !ok {
			return "", false
		}
	}
	return q(v), true
}

func (s *speller) ident(what string, n *yaml.Node, dotted bool) (string, bool) {
	if n == nil || n.Kind != yaml.ScalarNode || n.ShortTag() == "!!null" {
		s.refuse(n, what+" takes a bare name, got "+kindWord(n))
		return "", false
	}
	if dotted && isDotted(n.Value) || !dotted && isIdent(n.Value) {
		return n.Value, true
	}
	shape := "a bare name (letters, digits, `_`)"
	if dotted {
		shape = "a bare name, dotted for a group instance's node or a node's field (`r1.look`, `node.field`)"
	}
	s.refuse(n, what+" takes "+shape+", got "+describe(n))
	return "", false
}

func (s *speller) integer(what string, n *yaml.Node) (string, bool) {
	switch n.ShortTag() {
	case "!!int":
		v, ok := intOf(n)
		if !ok {
			s.refuse(n, what+": the integer "+n.Value+" is out of range")
			return "", false
		}
		if v < 0 {
			s.refuse(n, what+" takes a non-negative integer: the .bot has no signed number, got "+n.Value)
			return "", false
		}
		return strconv.FormatInt(v, 10), true
	case "!!float":
		s.refuse(n, what+" takes an integer written as digits (`3`, never `3.0`), got "+n.Value)
		return "", false
	}
	s.refuse(n, what+" takes an integer written as digits, got "+describe(n))
	return "", false
}

func (s *speller) number(what string, n *yaml.Node) (string, bool) {
	switch n.ShortTag() {
	case "!!int":
		return s.integer(what, n)
	case "!!float":
		v, ok := floatOf(n)
		if !ok || math.IsInf(v, 0) || math.IsNaN(v) {
			s.refuse(n, what+" takes a finite number: the .bot has no `.inf` and no `.nan`, got "+n.Value)
			return "", false
		}
		if v < 0 {
			s.refuse(n, what+" takes a non-negative number: the .bot has no signed number, got "+n.Value)
			return "", false
		}
		return floatText(n, v, false), true
	}
	s.refuse(n, what+" takes a number, got "+describe(n))
	return "", false
}

// literal spells a var's default or a preset's value: a quoted string, an
// integer, a float or a bool — the literals the .bot writes.
func (s *speller) literal(what string, n *yaml.Node) (string, bool) {
	if n == nil || n.Kind != yaml.ScalarNode {
		s.refuse(n, what+" takes a scalar literal — a string, an integer, a float or a bool (a json or string[] value is a quoted JSON text) — got "+kindWord(n))
		return "", false
	}
	switch n.ShortTag() {
	case "!!str":
		return q(n.Value), true
	case "!!int":
		return s.integer(what, n)
	case "!!float":
		v, ok := floatOf(n)
		if !ok || math.IsInf(v, 0) || math.IsNaN(v) || v < 0 {
			s.refuse(n, what+" takes a finite non-negative float: the .bot has no sign, no `.inf` and no `.nan`, got "+n.Value)
			return "", false
		}
		return floatText(n, v, true), true
	case "!!bool":
		b, _ := boolOf(n)
		return strconv.FormatBool(b), true
	}
	s.refuse(n, what+" takes a scalar literal — a string, an integer, a float or a bool — got "+describe(n))
	return "", false
}

func (s *speller) identList(what string, n *yaml.Node) (string, bool) {
	items, ok := s.sequence(n, what)
	if !ok {
		return "", false
	}
	var out []string
	clean := true
	for _, it := range items {
		v, ok := s.ident("an element of "+what, it, true)
		if !ok {
			clean = false
			continue
		}
		out = append(out, v)
	}
	return "[" + strings.Join(out, ", ") + "]", clean
}

func (s *speller) stringList(what string, n *yaml.Node) (string, bool) {
	items, ok := s.sequence(n, what)
	if !ok {
		return "", false
	}
	var out []string
	clean := true
	for _, it := range items {
		if it.Kind != yaml.ScalarNode {
			s.refuse(it, "an element of "+what+" is a string, got a "+kindWord(it))
			clean = false
			continue
		}
		v, ok := s.str("an element of "+what, it)
		if !ok {
			clean = false
			continue
		}
		out = append(out, v)
	}
	return "[" + strings.Join(out, ", ") + "]", clean
}

// jsonText spells a JSON value as the .bot writes one, on one line, in the
// document's ordinary quoting: text, a non-negative number without an
// exponent, true, false, null, a list, an object (keys in the author's
// order).
func (s *speller) jsonText(what string, n *yaml.Node) (string, bool) {
	if n == nil {
		s.refuse(n, what+" takes a JSON value, got nothing")
		return "", false
	}
	switch n.Kind {
	case yaml.ScalarNode:
		switch n.ShortTag() {
		case "!!str":
			return q(n.Value), true
		case "!!int":
			return s.integer(what, n)
		case "!!float":
			return s.number(what, n)
		case "!!bool":
			b, _ := boolOf(n)
			return strconv.FormatBool(b), true
		case "!!null":
			return "null", true
		}
		s.refuse(n, what+" takes a JSON value — text, a number, true, false, null, a list or an object — got "+tagWord(n.ShortTag()))
		return "", false
	case yaml.SequenceNode:
		var parts []string
		clean := true
		for _, c := range n.Content {
			t, ok := s.jsonText(what, c)
			if !ok {
				clean = false
				continue
			}
			parts = append(parts, t)
		}
		return "[" + strings.Join(parts, ", ") + "]", clean
	case yaml.MappingNode:
		var parts []string
		clean := true
		for _, p := range pairsOf(n) {
			t, ok := s.jsonText(what, p.val)
			if !ok {
				clean = false
				continue
			}
			parts = append(parts, q(p.key.Value)+": "+t)
		}
		return "{" + strings.Join(parts, ", ") + "}", clean
	}
	s.refuse(n, what+" takes a JSON value, got a "+kindWord(n))
	return "", false
}

// describe names a scalar for a message: its text, and what it reads as
// when that is not a string.
func describe(n *yaml.Node) string {
	if n == nil {
		return "nothing"
	}
	if n.Kind != yaml.ScalarNode {
		return "a " + kindWord(n)
	}
	if tag := n.ShortTag(); tag != "!!str" {
		if tag == "!!null" {
			return "nothing"
		}
		return "`" + n.Value + "` (" + tagWord(tag) + ")"
	}
	return strconv.Quote(n.Value)
}

// typeWord spells a var's, a field's or an attachment's type: a bare
// word the parser then holds to its list (E021).
func (s *speller) typeWord(what string, n *yaml.Node) (string, bool) {
	if n == nil || n.Kind != yaml.ScalarNode || n.ShortTag() == "!!null" {
		s.refuse(n, what+" needs its type, got "+kindWord(n))
		return "", false
	}
	if n.ShortTag() != "!!str" || !typeWordRe.MatchString(n.Value) {
		s.refuse(n, what+" takes a type name, got "+describe(n))
		return "", false
	}
	return n.Value, true
}

// ---- the document ----

// topLevelKeys are the keys of the document, in the order the head is
// written and the declarations may follow; every other key is refused.
var topLevelKeys = []string{"dsl", "catalog", "imports", "vars", "presets", "attachments", "secrets", "schemas", "prompts", "cursors", "supervisors", "mcp_servers", "contracts", "groups", "uses", "nodes", "workflow"}

func (s *speller) document(root *yaml.Node) {
	pairs, ok := s.mapping(root, "the document")
	if !ok {
		return
	}
	s.head(pairs)
	for _, p := range pairs {
		switch p.key.Value {
		case "dsl", "catalog", "imports":
			// the head
		case "vars", "presets", "attachments", "secrets":
			s.topBlock(p.key, p.val)
		case "schemas":
			s.declarations(p, func(k, v *yaml.Node) { s.schema(k, v) })
		case "prompts":
			s.declarations(p, func(k, v *yaml.Node) { s.prompt(k, v) })
		case "cursors":
			s.declarations(p, func(k, v *yaml.Node) { s.kindDecl("cursor", k, v) })
		case "supervisors":
			s.declarations(p, func(k, v *yaml.Node) { s.kindDecl("supervisor", k, v) })
		case "mcp_servers":
			s.declarations(p, func(k, v *yaml.Node) { s.kindDecl("mcp_server", k, v) })
		case "contracts":
			s.declarations(p, func(k, v *yaml.Node) { s.kindDecl("contract", k, v) })
		case "groups":
			s.groups(p.val)
		case "uses":
			s.uses(p.val)
		case "nodes":
			s.nodes(p.val, nil, 0)
		case "workflow":
			s.workflow(p.val)
		default:
			s.refuse(p.key, "the document's top level has no key `"+p.key.Value+"`; it has: "+strings.Join(topLevelKeys, ", "))
		}
	}
}

// head writes what the .bot reads before any declaration: the frontmatter
// (the `catalog:` key), the escape directive of a profile-1 text, the
// `dsl:` header, the imports.
func (s *speller) head(pairs []pair) {
	if cat := find(pairs, "catalog"); cat != nil {
		s.frontmatter(cat)
	}
	dsl := find(pairs, "dsl")
	headerText := ""
	switch {
	case dsl == nil:
		s.refuseHint(nil, parser.DiagAuthorHeader, "no `dsl:` key: the author document names its syntax profile (`dsl: 2`, or `dsl: 1`)", "")
	case dsl.Kind != yaml.ScalarNode:
		s.refuseHint(dsl, parser.DiagUnknownProfile, "dsl: takes the syntax profile as a positive integer (`dsl: 2`), got a "+kindWord(dsl), "")
	case dsl.ShortTag() == "!!float":
		s.refuseHint(dsl, parser.DiagUnknownProfile, "dsl: takes the syntax profile as a bare integer (`2`, never `"+dsl.Value+"`): the .bot header refuses a float", "")
	case dsl.ShortTag() != "!!int":
		s.refuseHint(dsl, parser.DiagUnknownProfile, "dsl: takes the syntax profile as a positive integer (`dsl: 2`), got "+describe(dsl), "")
	default:
		v, ok := intOf(dsl)
		if !ok || v < 1 {
			s.refuseHint(dsl, parser.DiagUnknownProfile, "dsl: takes the syntax profile as a positive integer (`dsl: 2`), got '"+dsl.Value+"'", "")
			break
		}
		headerText = "dsl: " + strconv.FormatInt(v, 10)
		if v <= parser.MaxProfile {
			s.profile = int(v)
		}
	}
	if s.profile < 2 {
		// The text is spelled with the standard escapes whatever the
		// profile; profile 1 reads them by its directive (parser.Preamble).
		s.comment("strict-escape: on", nil)
	}
	if headerText != "" {
		s.emit(0, headerText, dsl)
	}
	if imports := find(pairs, "imports"); imports != nil {
		if items, ok := s.sequence(imports, "`imports`"); ok {
			for _, it := range items {
				if it.Kind != yaml.ScalarNode || it.ShortTag() != "!!str" {
					s.refuse(it, "an import is the path of a fragment under lib/, a string, got "+kindWord(it))
					continue
				}
				s.emit(0, "import "+q(it.Value), it)
			}
		}
	}
	s.blank()
}

// catalogKeys are the frontmatter's keys (bundle.Frontmatter).
var catalogKeys = []string{"name", "description", "triggers", "capabilities"}

// frontmatter writes the `## ---` block the .bot carries its catalog
// identity in: the accepted keys re-encoded as YAML, one comment line each
// (workflowfile.CommentText reads them back).
func (s *speller) frontmatter(cat *yaml.Node) {
	pairs, ok := s.mapping(cat, "`catalog`")
	if !ok {
		return
	}
	doc := &yaml.Node{Kind: yaml.MappingNode}
	for _, p := range pairs {
		var v *yaml.Node
		switch p.key.Value {
		case "name", "description":
			if p.val.Kind != yaml.ScalarNode || p.val.ShortTag() != "!!str" {
				s.refuse(p.val, "`catalog."+p.key.Value+"` takes a string, got "+kindWord(p.val))
				continue
			}
			v = scalarNode(p.val.Value)
		case "triggers", "capabilities":
			items, ok := s.sequence(p.val, "`catalog."+p.key.Value+"`")
			if !ok {
				continue
			}
			list := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
			for _, it := range items {
				if it.Kind != yaml.ScalarNode || it.ShortTag() != "!!str" {
					s.refuse(it, "an element of `catalog."+p.key.Value+"` is a string, got "+kindWord(it))
					continue
				}
				list.Content = append(list.Content, scalarNode(it.Value))
			}
			v = list
		default:
			s.refuse(p.key, "`catalog` carries "+strings.Join(catalogKeys, ", ")+" — the keys of the .bot's frontmatter — not `"+p.key.Value+"`")
			continue
		}
		doc.Content = append(doc.Content, scalarNode(p.key.Value), v)
	}
	if len(doc.Content) == 0 {
		return
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		s.refuse(cat, "`catalog` cannot be written: "+err.Error())
		return
	}
	_ = enc.Close()
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	for _, l := range lines {
		// Every reader of the frontmatter closes the block at the first
		// comment line that reads as the fence, indentation aside — a
		// description with a `---` paragraph rule would be cut there and
		// the keys below it lost, without a word from any of them.
		if strings.TrimSpace(l) == workflowfile.FrontmatterFence {
			s.refuse(cat, "`catalog` has a line of text that reads as `"+workflowfile.FrontmatterFence+"`, the fence that closes the .bot's frontmatter: the keys after it would be lost — reword the text so no line is a bare `"+workflowfile.FrontmatterFence+"`")
			return
		}
	}
	s.comment(workflowfile.FrontmatterFence, cat)
	for _, l := range lines {
		s.comment(l, cat)
	}
	s.comment(workflowfile.FrontmatterFence, cat)
}

func scalarNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

// declarations reads a mapping of named declarations and writes each.
func (s *speller) declarations(p pair, write func(k, v *yaml.Node)) {
	pairs, ok := s.mapping(p.val, "`"+p.key.Value+"`")
	if !ok {
		return
	}
	for _, d := range pairs {
		if !isIdent(d.key.Value) {
			s.refuse(d.key, "an entry of `"+p.key.Value+"` is keyed by the declaration's name, an identifier, got "+strconv.Quote(d.key.Value))
			continue
		}
		write(d.key, d.val)
	}
}

// kindDecl writes `<kind> <name>:` and the body of a kind whose body is a
// property table (cursor, supervisor, mcp_server, contract).
func (s *speller) kindDecl(kind string, k, v *yaml.Node) {
	kd, _ := spec.Lookup(kind)
	pairs, ok := s.mapping(v, "`"+kind+" "+k.Value+"`")
	if !ok {
		return
	}
	if len(pairs) == 0 {
		s.bare(0, kind+" "+k.Value+":", k)
		return
	}
	s.header(0, kind+" "+k.Value+":", k)
	s.body(kd, pairs, 2, "")
	s.blank()
}

func (s *speller) topBlock(k, v *yaml.Node) {
	kd, _ := spec.Lookup(k.Value)
	pairs, ok := s.mapping(v, "`"+k.Value+"`")
	if !ok {
		return
	}
	if len(pairs) == 0 {
		s.bare(0, k.Value+":", k)
		return
	}
	s.header(0, k.Value+":", k)
	s.body(kd, pairs, 2, "")
	s.blank()
}

// schema writes `schema <name>:` and its fields, `name: type [enum: …]`.
func (s *speller) schema(k, v *yaml.Node) {
	kd, _ := spec.Lookup("schema")
	pairs, ok := s.mapping(v, "`schema "+k.Value+"`")
	if !ok {
		return
	}
	if len(pairs) == 0 {
		s.bare(0, "schema "+k.Value+":", k)
		return
	}
	s.header(0, "schema "+k.Value+":", k)
	s.body(kd, pairs, 2, "")
	s.blank()
}

// prompt writes `prompt <name>:` and its body, line by line, as the
// author wrote it: the lexer settles the body (parser.CanonicalPromptBodyIn)
// exactly as it settles a .bot's, and what it drops is said (E053).
func (s *speller) prompt(k, v *yaml.Node) {
	what := "`prompt " + k.Value + "`"
	if v == nil || v.Kind != yaml.ScalarNode || v.ShortTag() == "!!null" {
		s.refuse(v, what+"'s body is text, got "+kindWord(v))
		return
	}
	if v.ShortTag() != "!!str" {
		s.refuse(v, what+"'s body is text; `"+v.Value+"` reads as "+tagWord(v.ShortTag())+" — quote it")
		return
	}
	body := v.Value
	switch v.Style {
	case yaml.LiteralStyle, yaml.FoldedStyle:
		// A block scalar ends with the one newline YAML keeps (clip); a
		// declared body never ends with one, so that newline is the
		// scalar's, not the author's text. A quoted scalar's trailing
		// newline is the author's: the lexer settles it, and says so.
		body = strings.TrimSuffix(body, "\n")
		var ok bool
		if body, ok = s.blockBreaks(what, v, body); !ok {
			return
		}
	}
	if err := parser.CheckPromptBody(body); err != nil {
		s.refuseHint(v, parser.DiagAuthorPromptBody, what+"'s body has no written form: "+err.Error(), "")
		return
	}
	canon := parser.CanonicalPromptBodyIn(s.profile, body)
	if canon != body {
		s.warn(v, parser.DiagAuthorPromptBody, what+"'s body is read as the .bot reads a body — "+promptReading(s.profile, body))
	}
	if canon == "" {
		s.bare(0, "prompt "+k.Value+":", k)
		return
	}
	s.header(0, "prompt "+k.Value+":", k)
	bodyLines := strings.Split(body, "\n")
	for i, l := range bodyLines {
		// A literal block keeps its lines: line i of the body is line i
		// after the indicator's. A folded block or a quoted string does
		// not: every body line is positioned at the scalar's start.
		n := &yaml.Node{Line: v.Line, Column: v.Column}
		if v.Style == yaml.LiteralStyle {
			n.Line = v.Line + 1 + i
		}
		if strings.TrimRight(l, " \r") == "" {
			s.emit(0, "", n)
			continue
		}
		s.emit(2, l, n)
	}
	s.blank()
}

// blockSeparators are the line breaks yaml.v3's scanner leaves in a block
// scalar's value as the character itself: LS (U+2028) and PS (U+2029).
var blockSeparators = strings.NewReplacer(string(rune(0x2028)), string(rune(10)), string(rune(0x2029)), string(rune(10)))

// blockBreaks reads a block scalar's body as yaml.v3's scanner read it.
// The scanner ends a line at LS and PS as at a newline — the next line's
// indentation is stripped — but leaves the character in the value where a
// CR or a NEL becomes a newline; split on newlines alone, two authored
// lines would reach the model as one, joined by an invisible separator.
// In a literal block a break is a newline: the body gets the newline the
// scanner meant, and the author is told where the separator sits. In a
// folded block a break folds — into a space, or into newlines by the lines
// around it — and the scanner did not fold this one: the body is refused
// with the two ways out, rather than read by a rule of the converter's.
func (s *speller) blockBreaks(what string, v *yaml.Node, body string) (string, bool) {
	if !strings.ContainsAny(body, string(rune(0x2028))+string(rune(0x2029))) {
		return body, true
	}
	count, first, line := 0, -1, 0
	for _, r := range body {
		switch r {
		case 10:
			line++
		case 0x2028, 0x2029:
			if first < 0 {
				first = line
			}
			count++
			line++
		}
	}
	sep := "a line separator"
	if count > 1 {
		sep = strconv.Itoa(count) + " line separators"
	}
	if v.Style == yaml.FoldedStyle {
		s.refuseHint(v, parser.DiagAuthorPromptBody, what+"'s text holds "+sep+" (U+2028/U+2029) in a folded block: the document ends a line there but keeps the character, and a fold has no reading for it — write the body as a literal block (`|`), or remove the invisible character", "")
		return body, false
	}
	n := &yaml.Node{Line: v.Line + 1 + first, Column: v.Column}
	s.warn(n, parser.DiagAuthorPromptBody, what+"'s text holds "+sep+" (U+2028/U+2029), the first on line "+strconv.Itoa(n.Line)+": the document reads it as a line break, and so does the .bot written from it — write a newline, or remove the invisible character")
	return blockSeparators.Replace(body), true
}

// promptReading says what the lexer drops from a body, for the warning.
func promptReading(profile int, body string) string {
	lines := strings.Split(body, "\n")
	blank := func(l string) bool { return strings.Trim(strings.TrimRight(l, "\r"), " ") == "" }
	lead, trail := 0, 0
	for lead < len(lines) && blank(lines[lead]) {
		lead++
	}
	for trail < len(lines)-lead && blank(lines[len(lines)-1-trail]) {
		trail++
	}
	interior := 0
	for _, l := range lines[lead : len(lines)-trail] {
		if blank(l) {
			interior++
		}
	}
	var facts []string
	if lead > 0 {
		facts = append(facts, fmt.Sprintf("%d leading blank line(s) dropped", lead))
	}
	if trail > 0 {
		if trail == 1 && strings.HasSuffix(body, "\n") {
			facts = append(facts, "the trailing newline dropped (a body never ends with one)")
		} else {
			facts = append(facts, fmt.Sprintf("%d trailing blank line(s) dropped", trail))
		}
	}
	if interior > 0 && profile < 2 {
		facts = append(facts, fmt.Sprintf("%d interior blank line(s) dropped (profile 1 keeps no paragraph break; profile 2 does)", interior))
	}
	kept := lines[lead : len(lines)-trail]
	if len(kept) > 0 {
		first := kept[0]
		if base := len(first) - len(strings.TrimLeft(first, " ")); base > 0 {
			facts = append(facts, fmt.Sprintf("the first line's %d-space indentation taken off every line", base))
		}
	}
	if strings.Contains(body, "\r") {
		facts = append(facts, "carriage returns at line ends dropped")
	}
	if len(facts) == 0 {
		return "its spacing differs from the settled form"
	}
	return strings.Join(facts, "; ")
}

// nonEmptyBlocks are the blocks the parser opens with an indented body or
// not at all: a bare header is not an empty block there.
var nonEmptyBlocks = map[string]bool{"expr": true, "cursor.values": true, "cursor.bands": true}

// body writes the lines of a kind's body: its properties by their forms,
// its author-named entries, and the names it does not have — refused as
// the parser refuses them, with the registry's remedy (E012).
func (s *speller) body(kind spec.Kind, pairs []pair, indent int, host string) {
	for _, p := range pairs {
		name := p.key.Value
		if prop, ok := kind.Property(name); ok {
			s.property(kind, prop, p.key, p.val, indent)
			continue
		}
		if kind.Entries != nil {
			s.entry(kind, p.key, p.val, indent)
			continue
		}
		s.refuseHint(p.key, parser.DiagUnknownProperty, "unknown "+kind.Name+" property '"+name+"'", spec.UnknownPropertyHintIn(kind.Name, host, name))
	}
}

// property writes one `key: value` line, or a block, by the property's
// form.
func (s *speller) property(kind spec.Kind, p spec.Property, k, v *yaml.Node, indent int) {
	what := "`" + p.Name + "`"
	switch p.Form {
	case spec.Block:
		s.block(kind, p, k, v, indent)
	case spec.BlockOrIdent:
		if v != nil && v.Kind == yaml.MappingNode {
			s.block(kind, p, k, v, indent)
			return
		}
		if isNull(v) {
			s.refuse(v, what+" takes a bare mode ("+strings.Join(p.Values, ", ")+") or an indented block, got nothing")
			return
		}
		if word, ok := s.ident(what, v, false); ok {
			s.kv(indent, p.Name, word, k, v)
		}
	case spec.WithMap:
		if text, ok := s.withText(v); ok {
			s.emit(indent, text, k)
		}
	case spec.Map:
		s.stringMap(p.Name, k, v, indent)
	default:
		if text, ok := s.scalar(p.Form, p.Values, what, v); ok {
			s.kv(indent, p.Name, text, k, v)
		}
	}
}

// block writes a property whose value is a kind's body: `key:` and the
// indented lines, or the bare header the parser reads as an empty block.
func (s *speller) block(host spec.Kind, p spec.Property, k, v *yaml.Node, indent int) {
	body, ok := spec.Lookup(p.Body)
	if !ok {
		s.refuse(k, "`"+p.Name+"`: the registry names no kind "+p.Body)
		return
	}
	what := "`" + p.Name + "`"
	if body.Entries != nil && body.Entries.SequenceKey != "" {
		s.sequenceEntries(body, what, k, v, indent)
		return
	}
	pairs, ok := s.mapping(v, what)
	if !ok {
		return
	}
	if len(pairs) == 0 {
		if nonEmptyBlocks[body.Name] {
			s.refuse(v, what+" names at least one entry")
			return
		}
		s.bare(indent, p.Name+":", k)
		return
	}
	s.header(indent, p.Name+":", k)
	s.body(body, pairs, indent+2, host.Name)
}

// sequenceEntries writes a block whose entries are ordered (fallbacks): a
// list of objects carrying the entry's name under the sequence key.
func (s *speller) sequenceEntries(body spec.Kind, what string, k, v *yaml.Node, indent int) {
	e := body.Entries
	items, ok := s.sequence(v, what)
	if !ok {
		if v != nil && v.Kind == yaml.MappingNode {
			s.diags[len(s.diags)-1].Message = what + " is a list of routes (order is the try order), each `{" + e.SequenceKey + ": <name>, …}`, not a mapping"
		}
		return
	}
	if len(items) == 0 {
		s.bare(indent, k.Value+":", k)
		return
	}
	s.header(indent, k.Value+":", k)
	entryKind, _ := spec.Lookup(e.Body)
	for _, it := range items {
		pairs, ok := s.mapping(it, "an entry of "+what)
		if !ok {
			continue
		}
		nameNode := find(pairs, e.SequenceKey)
		if nameNode == nil {
			s.refuse(it, "an entry of "+what+" carries its name under `"+e.SequenceKey+"`")
			continue
		}
		name, ok := s.ident("`"+e.SequenceKey+"`", nameNode, false)
		if !ok {
			continue
		}
		rest := without(pairs, e.SequenceKey)
		s.header(indent+2, name+":", nameNode)
		s.body(entryKind, rest, indent+4, body.Name)
	}
}

// stringMap writes a `Map` property: `{}` inline when empty, else the
// block form, one `KEY: "value"` per line.
func (s *speller) stringMap(name string, k, v *yaml.Node, indent int) {
	what := "`" + name + "`"
	pairs, ok := s.mapping(v, what)
	if !ok {
		return
	}
	if len(pairs) == 0 {
		s.kv(indent, name, "{}", k, v)
		return
	}
	s.header(indent, name+":", k)
	for _, p := range pairs {
		if !isIdent(p.key.Value) {
			s.refuse(p.key, "a key of "+what+" is an identifier, got "+strconv.Quote(p.key.Value))
			continue
		}
		if p.val == nil || p.val.Kind != yaml.ScalarNode {
			s.refuse(p.val, "a value of "+what+" is a string, got a "+kindWord(p.val))
			continue
		}
		text, ok := s.str("a value of "+what, p.val)
		if !ok {
			continue
		}
		s.kv(indent+2, p.key.Value, text, p.key, p.val)
	}
}

// withText spells a with map on one line: `with { key: "value", … }`; a
// number or a bool is the string it spells.
func (s *speller) withText(v *yaml.Node) (string, bool) {
	pairs, ok := s.mapping(v, "`with`")
	if !ok {
		return "", false
	}
	var parts []string
	clean := true
	for _, p := range pairs {
		if !isIdent(p.key.Value) {
			s.refuse(p.key, "a key of `with` is an identifier, got "+strconv.Quote(p.key.Value))
			clean = false
			continue
		}
		if p.val == nil || p.val.Kind != yaml.ScalarNode || p.val.ShortTag() == "!!null" {
			s.refuse(p.val, "a value of `with` is a scalar — a string, a number or a bool — got "+kindWord(p.val))
			clean = false
			continue
		}
		text := p.val.Value
		switch p.val.ShortTag() {
		case "!!int":
			if n, ok := intOf(p.val); ok {
				text = strconv.FormatInt(n, 10)
			}
		case "!!float":
			if f, ok := floatOf(p.val); ok && !math.IsInf(f, 0) && !math.IsNaN(f) {
				text = floatText(p.val, f, false)
			}
		case "!!bool":
			if b, ok := boolOf(p.val); ok {
				text = strconv.FormatBool(b)
			}
		}
		parts = append(parts, p.key.Value+": "+q(text))
	}
	if len(parts) == 0 {
		return "with {}", clean
	}
	return "with { " + strings.Join(parts, ", ") + " }", clean
}

// ---- author-named entries ----

// entry writes one entry of a kind whose body is made of author-named
// entries; the kind's name selects the line's grammar.
func (s *speller) entry(kind spec.Kind, k, v *yaml.Node, indent int) {
	switch kind.Name {
	case "vars":
		s.varEntry(k, v, indent)
	case "schema":
		s.schemaField(k, v, indent)
	case "presets":
		s.presetEntry(k, v, indent)
	case "attachments":
		s.attachmentEntry(k, v, indent)
	case "secrets":
		s.secretEntry(k, v, indent)
	case "resources":
		s.namedValue(kind, k, v, indent, spec.IntOrStringList)
	case "cursor.values":
		s.namedValue(kind, k, v, indent, spec.String)
	case "cursor.bands":
		if v == nil || v.Kind != yaml.ScalarNode {
			s.refuse(v, "a band carries a prompt fragment, a string, got "+kindWord(v))
			return
		}
		if text, ok := s.str("a band of `bands`", v); ok {
			s.kv(indent, q(k.Value), text, k, v)
		}
	case "cursors":
		s.namedValue(kind, k, v, indent, spec.Setting)
	case "params":
		s.paramEntry(k, v, indent)
	case "expr":
		s.namedValue(kind, k, v, indent, spec.QuotedString)
	case "contract.ports":
		s.portEntry(k, v, indent)
	case "contract.criteria", "contract.effects":
		s.subEntry(kind, k, v, indent)
	default:
		s.refuse(k, "no spelling for an entry of "+kind.Name)
	}
}

// namedValue writes `name: value` for an entry whose line is its name and
// one value of the given form.
func (s *speller) namedValue(kind spec.Kind, k, v *yaml.Node, indent int, f spec.Form) {
	if !isIdent(k.Value) {
		s.refuse(k, "an entry of `"+kind.Opener+"` is keyed by an identifier, got "+strconv.Quote(k.Value))
		return
	}
	if f == spec.QuotedString && (v == nil || v.Kind != yaml.ScalarNode || v.ShortTag() != "!!str") {
		s.refuse(v, "`"+k.Value+"` takes a quoted expression (a string), got "+describe(v))
		return
	}
	if text, ok := s.scalar(f, nil, "`"+k.Value+"`", v); ok {
		s.kv(indent, k.Value, text, k, v)
	}
}

// varEntry writes `name: type [enum: "a", "b"] [matching: "re"] = default`.
func (s *speller) varEntry(k, v *yaml.Node, indent int) {
	if !isIdent(k.Value) {
		s.refuse(k, "an entry of `vars` is keyed by an identifier, got "+strconv.Quote(k.Value))
		return
	}
	what := "the var `" + k.Value + "`"
	if isNull(v) {
		s.refuse(k, what+" needs its type: `"+k.Value+": string`, or a mapping of its parts (type, enum, matching, default)")
		return
	}
	var typ, tail string
	switch v.Kind {
	case yaml.ScalarNode:
		t, ok := s.typeWord(what, v)
		if !ok {
			return
		}
		typ = t
	case yaml.MappingNode:
		pairs := pairsOf(v)
		typeNode := find(pairs, "type")
		if typeNode == nil {
			s.refuse(v, what+" needs its type: `type: string`")
			return
		}
		t, ok := s.typeWord(what+"'s type", typeNode)
		if !ok {
			return
		}
		typ = t
		for _, p := range pairs {
			switch p.key.Value {
			case "type":
			case "enum":
				if text, ok := s.stringList("`enum`", p.val); ok {
					tail += " [enum: " + strings.TrimSuffix(strings.TrimPrefix(text, "["), "]") + "]"
				}
			case "matching":
				if p.val == nil || p.val.Kind != yaml.ScalarNode || p.val.ShortTag() != "!!str" {
					s.refuse(p.val, "`matching` takes an RE2 pattern, a string, got "+describe(p.val))
					continue
				}
				tail += " [matching: " + q(p.val.Value) + "]"
			case "default":
				if text, ok := s.literal("`default`", p.val); ok {
					tail += " = " + text
				}
			default:
				s.refuse(p.key, what+" has the parts type, enum, matching and default, not `"+p.key.Value+"`")
			}
		}
	default:
		s.refuse(v, what+" is `"+k.Value+": <type>`, or a mapping of its parts, got a "+kindWord(v))
		return
	}
	s.kv(indent, k.Value, typ+tail, k, v)
}

// schemaField writes `name: type [enum: "a", "b"]`.
func (s *speller) schemaField(k, v *yaml.Node, indent int) {
	if !isIdent(k.Value) {
		s.refuse(k, "a field of a schema is keyed by an identifier, got "+strconv.Quote(k.Value))
		return
	}
	what := "the field `" + k.Value + "`"
	if isNull(v) {
		s.refuse(k, what+" needs its type: `"+k.Value+": string`, or a mapping of its parts (type, enum)")
		return
	}
	var typ, tail string
	switch v.Kind {
	case yaml.ScalarNode:
		t, ok := s.typeWord(what, v)
		if !ok {
			return
		}
		typ = t
	case yaml.MappingNode:
		pairs := pairsOf(v)
		typeNode := find(pairs, "type")
		if typeNode == nil {
			s.refuse(v, what+" needs its type: `type: string`")
			return
		}
		t, ok := s.typeWord(what+"'s type", typeNode)
		if !ok {
			return
		}
		typ = t
		for _, p := range pairs {
			switch p.key.Value {
			case "type":
			case "enum":
				if text, ok := s.stringList("`enum`", p.val); ok {
					tail += " [enum: " + strings.TrimSuffix(strings.TrimPrefix(text, "["), "]") + "]"
				}
			default:
				s.refuse(p.key, what+" has the parts type and enum, not `"+p.key.Value+"`")
			}
		}
	default:
		s.refuse(v, what+" is `"+k.Value+": <type>`, or a mapping of its parts, got a "+kindWord(v))
		return
	}
	s.kv(indent, k.Value, typ+tail, k, v)
}

// presetEntry writes `name:` and one `var: literal` line per value.
func (s *speller) presetEntry(k, v *yaml.Node, indent int) {
	if !isIdent(k.Value) {
		s.refuse(k, "an entry of `presets` is keyed by an identifier, got "+strconv.Quote(k.Value))
		return
	}
	pairs, ok := s.mapping(v, "the preset `"+k.Value+"`")
	if !ok {
		return
	}
	if len(pairs) == 0 {
		s.refuse(v, "the preset `"+k.Value+"` sets at least one var")
		return
	}
	s.header(indent, k.Value+":", k)
	for _, p := range pairs {
		if !isIdent(p.key.Value) {
			s.refuse(p.key, "a value of the preset `"+k.Value+"` is keyed by a var's name, an identifier, got "+strconv.Quote(p.key.Value))
			continue
		}
		if text, ok := s.literal("`"+p.key.Value+"`", p.val); ok {
			s.kv(indent+2, p.key.Value, text, p.key, p.val)
		}
	}
}

// attachmentEntry writes `name: type` and the optional sub-block.
func (s *speller) attachmentEntry(k, v *yaml.Node, indent int) {
	if !isIdent(k.Value) {
		s.refuse(k, "an entry of `attachments` is keyed by an identifier, got "+strconv.Quote(k.Value))
		return
	}
	what := "the attachment `" + k.Value + "`"
	if isNull(v) {
		s.refuse(k, what+" needs its type: `"+k.Value+": file` or `image`, or a mapping of its parts")
		return
	}
	kind, _ := spec.Lookup("attachment")
	switch v.Kind {
	case yaml.ScalarNode:
		if t, ok := s.typeWord(what, v); ok {
			s.kv(indent, k.Value, t, k, v)
		}
	case yaml.MappingNode:
		pairs := pairsOf(v)
		typeNode := find(pairs, "type")
		if typeNode == nil {
			s.refuse(v, what+" needs its type: `type: file` or `image`")
			return
		}
		t, ok := s.typeWord(what+"'s type", typeNode)
		if !ok {
			return
		}
		s.kv(indent, k.Value, t, k, typeNode)
		s.body(kind, without(pairs, "type"), indent+2, "attachments")
	default:
		s.refuse(v, what+" is `"+k.Value+": <type>`, or a mapping of its parts, got a "+kindWord(v))
	}
}

// secretEntry writes `name:`, `name: "value"`, or the header and the
// sub-block.
func (s *speller) secretEntry(k, v *yaml.Node, indent int) {
	if !isIdent(k.Value) {
		s.refuse(k, "an entry of `secrets` is keyed by an identifier, got "+strconv.Quote(k.Value))
		return
	}
	what := "the secret `" + k.Value + "`"
	kind, _ := spec.Lookup("secret")
	switch {
	case isNull(v):
		s.emit(indent, k.Value+":", k)
	case v.Kind == yaml.ScalarNode:
		if text, ok := s.str(what+"'s value", v); ok {
			s.kv(indent, k.Value, text, k, v)
		}
	case v.Kind == yaml.MappingNode:
		pairs := pairsOf(v)
		header := k.Value + ":"
		if value := find(pairs, "value"); value != nil {
			text, ok := s.str(what+"'s value", value)
			if !ok {
				return
			}
			header += " " + text
		}
		s.header(indent, header, k)
		s.body(kind, without(pairs, "value"), indent+2, "secrets")
	default:
		s.refuse(v, what+" is `"+k.Value+": \"value\"`, a bare name, or a mapping of its parts, got a "+kindWord(v))
	}
}

// paramEntry writes one argument of a connector action: the key bare when
// it is an identifier, quoted when it is the vendor's own name.
func (s *speller) paramEntry(k, v *yaml.Node, indent int) {
	key := k.Value
	if !isIdent(key) {
		key = q(key)
	}
	if text, ok := s.scalar(spec.StringOrNumber, nil, "the parameter `"+k.Value+"`", v); ok {
		s.kv(indent, key, text, k, v)
	}
}

// portEntry writes `name: type` and the optional sub-block of a contract
// port.
func (s *speller) portEntry(k, v *yaml.Node, indent int) {
	if !isIdent(k.Value) {
		s.refuse(k, "a port is keyed by an identifier, got "+strconv.Quote(k.Value))
		return
	}
	what := "the port `" + k.Value + "`"
	if isNull(v) {
		s.refuse(k, what+" needs its type: `"+k.Value+": string`, or a mapping of its parts")
		return
	}
	kind, _ := spec.Lookup("contract.port")
	switch v.Kind {
	case yaml.ScalarNode:
		if t, ok := s.scalar(spec.TypeRef, nil, what, v); ok {
			s.kv(indent, k.Value, t, k, v)
		}
	case yaml.MappingNode:
		pairs := pairsOf(v)
		typeNode := find(pairs, "type")
		if typeNode == nil {
			s.refuse(v, what+" needs its type: `type: string`")
			return
		}
		t, ok := s.scalar(spec.TypeRef, nil, what+"'s type", typeNode)
		if !ok {
			return
		}
		s.kv(indent, k.Value, t, k, typeNode)
		s.body(kind, without(pairs, "type"), indent+2, "contract.ports")
	default:
		s.refuse(v, what+" is `"+k.Value+": <type>`, or a mapping of its parts, got a "+kindWord(v))
	}
}

// subEntry writes an entry whose line stops at its name (a criterion, an
// effect): `name:` and the sub-block, or the bare header.
func (s *speller) subEntry(kind spec.Kind, k, v *yaml.Node, indent int) {
	if !isIdent(k.Value) {
		s.refuse(k, "an entry of `"+kind.Opener+"` is keyed by an identifier, got "+strconv.Quote(k.Value))
		return
	}
	body, _ := spec.Lookup(kind.Entries.Body)
	if isNull(v) {
		s.bare(indent, k.Value+":", k)
		return
	}
	pairs, ok := s.mapping(v, "the entry `"+k.Value+"` of `"+kind.Opener+"`")
	if !ok {
		return
	}
	if len(pairs) == 0 {
		s.bare(indent, k.Value+":", k)
		return
	}
	s.header(indent, k.Value+":", k)
	s.body(body, pairs, indent+2, kind.Name)
}

// ---- nodes, groups, uses, workflow ----

// nodeKindNames lists the node kinds, for the message a node without one
// gets.
func nodeKindNames() []string {
	var out []string
	for _, k := range spec.Kinds {
		if k.Role == spec.Node {
			out = append(out, k.Name)
		}
	}
	return out
}

// nodes writes a list of `{<kind>: <name>, …properties}` items as node
// declarations, at the given indentation (a group's body is indented).
func (s *speller) nodes(v *yaml.Node, holds []string, indent int) {
	items, ok := s.sequence(v, "`nodes`")
	if !ok {
		return
	}
	for _, it := range items {
		pairs, ok := s.mapping(it, "a node")
		if !ok {
			continue
		}
		var kinds []pair
		for _, p := range pairs {
			if k, ok := spec.Lookup(p.key.Value); ok && k.Role == spec.Node {
				kinds = append(kinds, p)
			}
		}
		if len(kinds) != 1 {
			s.refuse(it, fmt.Sprintf("a node is `{<kind>: <name>, …properties}` with exactly one kind key among %s; got %d", strings.Join(nodeKindNames(), ", "), len(kinds)))
			continue
		}
		kk := kinds[0]
		kind, _ := spec.Lookup(kk.key.Value)
		if holds != nil {
			held := false
			for _, h := range holds {
				if h == kind.Name {
					held = true
					break
				}
			}
			if !held {
				s.refuse(kk.key, "a group holds "+strings.Join(holds, ", ")+" declarations, not a "+kind.Name)
				continue
			}
		}
		name, ok := s.ident("the "+kind.Name+"'s name", kk.val, false)
		if !ok {
			continue
		}
		s.header(indent, kind.Name+" "+name+":", kk.key)
		mark := len(s.lines)
		s.body(kind, without(pairs, kk.key.Value), indent+2, "")
		if len(s.lines) == mark {
			// A header with no body does not parse; an empty description
			// is what an absent one reads as (unparse.ensureBody).
			s.kv(indent+2, "description", `""`, kk.key, nil)
		}
		s.blank()
	}
}

// groups writes `group <name>(<params>):` declarations and their bodies.
func (s *speller) groups(v *yaml.Node) {
	items, ok := s.sequence(v, "`groups`")
	if !ok {
		return
	}
	group, _ := spec.Lookup("group")
	for _, it := range items {
		pairs, ok := s.mapping(it, "a group")
		if !ok {
			continue
		}
		nameNode := find(pairs, "group")
		if nameNode == nil {
			s.refuse(it, "a group carries its name under the `group` key: `{group: <name>, params: […], nodes: […], edges: […]}`")
			continue
		}
		name, ok := s.ident("the group's name", nameNode, false)
		if !ok {
			continue
		}
		header := "group " + name
		if params := find(pairs, "params"); params != nil {
			if text, ok := s.identList("`params`", params); ok && text != "[]" {
				header += "(" + strings.TrimSuffix(strings.TrimPrefix(text, "["), "]") + ")"
			}
		}
		header += ":"
		mark := len(s.lines)
		s.header(0, header, nameNode)
		for _, p := range pairs {
			switch p.key.Value {
			case "group", "params":
			case "nodes":
				s.nodes(p.val, group.Holds, 2)
			case "edges":
				s.edges(p.val, 2)
			default:
				s.refuse(p.key, "a group is `{group: <name>, params: […], nodes: […], edges: […]}`; it has no key `"+p.key.Value+"`")
			}
		}
		if len(s.lines) == mark+1 {
			s.blank()
		}
	}
}

// uses writes `use <group> as <prefix> [with {…}]` lines.
func (s *speller) uses(v *yaml.Node) {
	items, ok := s.sequence(v, "`uses`")
	if !ok {
		return
	}
	for _, it := range items {
		pairs, ok := s.mapping(it, "a use")
		if !ok {
			continue
		}
		groupNode := find(pairs, "use")
		if groupNode == nil {
			s.refuse(it, "a use names the group it instantiates under the `use` key: `{use: <group>, as: <prefix>, with: {…}}`")
			continue
		}
		group, ok := s.ident("`use`", groupNode, false)
		if !ok {
			continue
		}
		asNode := find(pairs, "as")
		if asNode == nil {
			s.refuse(it, "a use names its prefix with `as`: `{use: "+group+", as: <prefix>}`")
			continue
		}
		prefix, ok := s.ident("`as`", asNode, false)
		if !ok {
			continue
		}
		text := "use " + group + " as " + prefix
		clean := true
		for _, p := range pairs {
			switch p.key.Value {
			case "use", "as":
			case "with":
				w, ok := s.withText(p.val)
				if !ok {
					clean = false
					continue
				}
				if w != "with {}" {
					text += " " + w
				}
			default:
				s.refuse(p.key, "a use is `{use: <group>, as: <prefix>, with: {…}}`; it has no key `"+p.key.Value+"`")
				clean = false
			}
		}
		if clean {
			s.emit(0, text, groupNode)
			s.blank()
		}
	}
}

// edges writes edge lines, each held to the grammar before it is written:
// a string that is not exactly one edge line is refused where it stands,
// never read as a property of the body it would land in.
func (s *speller) edges(v *yaml.Node, indent int) {
	items, ok := s.sequence(v, "`edges`")
	if !ok {
		return
	}
	for _, it := range items {
		if it.Kind != yaml.ScalarNode || it.ShortTag() != "!!str" {
			s.refuse(it, "an edge is a string in the .bot's own grammar — `src -> dst [when …|else] [as loop(N)] [with {…}]` — got "+kindWord(it))
			continue
		}
		text := strings.TrimSpace(it.Value)
		if _, diags := parser.ParseEdgeLine(s.profile, text); len(diags) > 0 {
			for _, d := range diags {
				line, col := s.edgeAt(it, text, d.Column)
				s.diags = append(s.diags, parser.Diagnostic{
					Code: d.Code, Severity: d.Severity, Message: d.Message,
					File: s.name, Line: line, Column: col, Hint: d.Hint,
				})
			}
			continue
		}
		s.emit(indent, text, it)
	}
}

// edgeAt is the YAML position of column dcol of an edge line text read off
// the scalar it. It is exact when the document's own text carries the value
// verbatim on the scalar's line — a plain scalar written on one line, a
// quoted one holding no escape — and the scalar's start otherwise: a
// position computed from a length the source does not have (a YAML escape
// that shrank on decoding, a plain scalar folded over two lines, spaces the
// trim took) would name a column the author never wrote, or one past the
// end of a line.
func (s *speller) edgeAt(it *yaml.Node, text string, dcol int) (line, col int) {
	line, col = it.Line, it.Column
	if dcol <= 1 || line < 1 || line > len(s.src) || col < 1 {
		return line, col
	}
	raw := []rune(s.src[line-1])
	if col-1 > len(raw) {
		return line, col
	}
	rest := string(raw[col-1:])
	lead := utf8.RuneCountInString(it.Value[:strings.Index(it.Value, text)])
	switch {
	case it.Style == 0 && strings.HasPrefix(rest, it.Value):
		return line, col + lead + dcol - 1
	case it.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0 && len(rest) > 1 && strings.HasPrefix(rest[1:], it.Value):
		// The text after the opening quote is the value itself: no escape
		// shrank it (a `\"` or a doubled `''` would have).
		return line, col + 1 + lead + dcol - 1
	}
	return line, col
}

// workflow writes `workflow <name>:` and its body: the properties by their
// forms, the edge lines last.
func (s *speller) workflow(v *yaml.Node) {
	pairs, ok := s.mapping(v, "`workflow`")
	if !ok {
		return
	}
	kind, _ := spec.Lookup("workflow")
	nameNode := find(pairs, "name")
	if nameNode == nil {
		s.refuse(v, "the workflow carries its name: `name: <identifier>`")
		return
	}
	name, ok := s.ident("the workflow's name", nameNode, false)
	if !ok {
		return
	}
	s.header(0, "workflow "+name+":", nameNode)
	mark := len(s.lines)
	for _, p := range pairs {
		switch p.key.Value {
		case "name":
		case "edges":
			s.edges(p.val, 2)
		default:
			if prop, ok := kind.Property(p.key.Value); ok {
				s.property(kind, prop, p.key, p.val, 2)
				continue
			}
			s.refuseHint(p.key, parser.DiagUnknownProperty, "unknown workflow property '"+p.key.Value+"'", spec.UnknownPropertyHintIn("workflow", "", p.key.Value))
		}
	}
	if len(s.lines) == mark {
		s.blank()
		return
	}
	s.blank()
}
