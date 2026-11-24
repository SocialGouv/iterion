package toolindex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Declaration renders a tool as the TypeScript overload of `call` a program
// writes against:
//
//	/** Read a file. */
//	declare function call(name: "read_file", input: {
//	  /** Absolute path. */
//	  path: string;
//	  offset?: number;
//	}): Result;
//
// It is TOLERANT by contract: a tool's schema comes from whoever wrote the
// tool — an MCP server included — so a schema this renderer does not follow
// becomes `unknown` where it stops, never an error and never a dropped tool.
// It follows local `$ref`s (`#`, `#/$defs/…`, `#/definitions/…`, any JSON
// pointer into the document) as `type` aliases, so a recursive schema renders
// as a recursive alias and terminates; a non-object input renders as that
// type; and the 2020-12 keywords that shape a value (`prefixItems`, `$defs`,
// `const`) are read alongside their draft-07 spellings.
//
// A declaration over [maxDeclarationBytes] is rendered again shallower, and
// cut with a marker only if the shallow form is still too large.
func Declaration(t Tool) string {
	for _, depth := range []int{maxDepth, 2} {
		if d := declaration(t, depth); len(d) <= maxDeclarationBytes {
			return d
		}
	}
	d := declaration(t, 1)
	if len(d) <= maxDeclarationBytes {
		return d
	}
	return truncateBytes(d, maxDeclarationBytes-64) + fmt.Sprintf("\n/* declaration cut at %d bytes */", maxDeclarationBytes)
}

const (
	// maxDepth bounds how deep a schema is followed; past it a value is
	// `unknown`. Deep enough for any real tool, shallow enough that a hostile
	// schema cannot make one declaration expensive.
	maxDepth = 24
	// maxDeclarationBytes bounds one rendered declaration.
	maxDeclarationBytes = 16 << 10
	// maxDocRunes bounds a property's doc comment.
	maxDocRunes = 300
)

func declaration(t Tool, depth int) string {
	g := &tsGen{maxDepth: depth, aliasOf: map[string]string{}, aliasBody: map[string]string{}, taken: map[string]bool{"Result": true}}
	input := "input?: Record<string, unknown>"
	switch {
	case t.SchemaOmitted > 0:
		input = fmt.Sprintf("input: unknown /* the input schema is %d bytes, over the catalogue's bound: call the tool directly to see it */", t.SchemaOmitted)
	case len(t.InputSchema) > 0:
		root, err := parseOrdered(t.InputSchema)
		if err != nil {
			input = "input: unknown /* the input schema is not JSON this renderer reads */"
			break
		}
		g.root = root
		typ := g.typ(root, "", 0)
		optional := ""
		if !g.inputRequired(root) {
			optional = "?"
		}
		input = "input" + optional + ": " + typ
	}

	var b strings.Builder
	for _, name := range g.aliasOrder {
		fmt.Fprintf(&b, "type %s = %s;\n", name, g.aliasBody[name])
	}
	if doc := docBlock(t.Description, ""); doc != "" {
		b.WriteString(doc)
	}
	fmt.Fprintf(&b, "declare function call(name: %s, %s): Result;", quote(t.Name), input)
	return b.String()
}

// tsGen renders one declaration.
type tsGen struct {
	root     *jnode
	maxDepth int

	aliasOf    map[string]string // $ref pointer → alias name
	aliasBody  map[string]string // alias name → its type
	aliasOrder []string          // aliases in first-reference order
	taken      map[string]bool
}

// inputRequired: an object input whose schema requires nothing may be
// omitted; any other input is required.
func (g *tsGen) inputRequired(root *jnode) bool {
	if root.kind != kindObject {
		return true
	}
	if t := root.get("type"); t != nil && t.kind == kindString && t.str != "object" {
		return true
	}
	return len(requiredSet(root)) > 0
}

func (g *tsGen) typ(s *jnode, indent string, depth int) string {
	if s == nil {
		return "unknown"
	}
	if s.kind == kindBool {
		if s.b {
			return "unknown"
		}
		return "never"
	}
	if s.kind != kindObject || depth > g.maxDepth {
		return "unknown"
	}

	if ref := s.get("$ref"); ref != nil && ref.kind == kindString {
		return g.ref(ref.str, depth)
	}
	if c := s.get("const"); c != nil {
		return literal(c)
	}
	if e := s.get("enum"); e != nil && e.kind == kindArray && len(e.arr) > 0 {
		parts := make([]string, 0, len(e.arr))
		for _, v := range e.arr {
			parts = append(parts, literal(v))
		}
		return union(parts)
	}

	var parts []string
	if of := firstArray(s, "anyOf", "oneOf"); of != nil {
		alts := make([]string, 0, len(of.arr))
		for _, alt := range of.arr {
			alts = append(alts, g.typ(alt, indent, depth+1))
		}
		parts = append(parts, paren(union(alts)))
	}
	if all := s.get("allOf"); all != nil && all.kind == kindArray && len(all.arr) > 0 {
		alts := make([]string, 0, len(all.arr))
		for _, alt := range all.arr {
			alts = append(alts, paren(g.typ(alt, indent, depth+1)))
		}
		parts = append(parts, strings.Join(alts, " & "))
	}
	if own := g.ownType(s, indent, depth); own != "" {
		parts = append(parts, own)
	}

	var out string
	switch len(parts) {
	case 0:
		out = "unknown"
	case 1:
		out = parts[0]
	default:
		for i := range parts {
			parts[i] = paren(parts[i])
		}
		out = strings.Join(parts, " & ")
	}
	if n := s.get("nullable"); n != nil && n.kind == kindBool && n.b && out != "unknown" {
		out = union([]string{out, "null"})
	}
	return out
}

// ownType renders what `type` (or the keywords that imply one) says, or ""
// when the schema says nothing of its own beside a combinator.
func (g *tsGen) ownType(s *jnode, indent string, depth int) string {
	var kinds []string
	if t := s.get("type"); t != nil {
		switch t.kind {
		case kindString:
			kinds = []string{t.str}
		case kindArray:
			for _, k := range t.arr {
				if k.kind == kindString {
					kinds = append(kinds, k.str)
				}
			}
		}
	}
	if len(kinds) == 0 {
		switch {
		case s.get("properties") != nil || s.get("additionalProperties") != nil || s.get("patternProperties") != nil:
			kinds = []string{"object"}
		case s.get("items") != nil || s.get("prefixItems") != nil:
			kinds = []string{"array"}
		default:
			return ""
		}
	}
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		switch k {
		case "string":
			parts = append(parts, "string")
		case "number", "integer":
			parts = append(parts, "number")
		case "boolean":
			parts = append(parts, "boolean")
		case "null":
			parts = append(parts, "null")
		case "array":
			parts = append(parts, g.array(s, indent, depth))
		case "object":
			parts = append(parts, g.object(s, indent, depth))
		default:
			parts = append(parts, "unknown")
		}
	}
	return union(parts)
}

func (g *tsGen) array(s *jnode, indent string, depth int) string {
	prefix := s.get("prefixItems")
	items := s.get("items")
	if prefix == nil && items != nil && items.kind == kindArray {
		// draft-07's tuple form: `items` as an array, `additionalItems` for
		// the rest.
		prefix, items = items, s.get("additionalItems")
	}
	if prefix != nil && prefix.kind == kindArray && len(prefix.arr) > 0 {
		elems := make([]string, 0, len(prefix.arr)+1)
		for _, it := range prefix.arr {
			elems = append(elems, g.typ(it, indent, depth+1))
		}
		switch {
		case items == nil || (items.kind == kindBool && items.b):
			elems = append(elems, "...unknown[]")
		case items.kind == kindBool && !items.b:
		default:
			elems = append(elems, "..."+paren(g.typ(items, indent, depth+1))+"[]")
		}
		return "[" + strings.Join(elems, ", ") + "]"
	}
	if items == nil || items.kind == kindArray {
		return "unknown[]"
	}
	return paren(g.typ(items, indent, depth+1)) + "[]"
}

func (g *tsGen) object(s *jnode, indent string, depth int) string {
	props := s.get("properties")
	required := requiredSet(s)
	inner := indent + "  "
	var lines []string
	if props != nil && props.kind == kindObject {
		for _, p := range props.obj {
			name := p.key
			if !isIdentifier(name) {
				name = quote(name)
			}
			opt := "?"
			if required[p.key] {
				opt = ""
			}
			if doc := propertyDoc(p.val); doc != "" {
				lines = append(lines, inner+"/** "+doc+" */")
			}
			lines = append(lines, inner+name+opt+": "+g.typ(p.val, inner, depth+1)+";")
		}
	}
	var extra []string
	if ap := s.get("additionalProperties"); ap != nil && ap.kind != kindBool {
		extra = append(extra, g.typ(ap, inner, depth+1))
	}
	if pp := s.get("patternProperties"); pp != nil && pp.kind == kindObject {
		for _, p := range pp.obj {
			extra = append(extra, g.typ(p.val, inner, depth+1))
		}
	}
	if len(extra) > 0 {
		lines = append(lines, inner+"[key: string]: "+union(extra)+";")
	}
	if len(lines) == 0 {
		if ap := s.get("additionalProperties"); ap != nil && ap.kind == kindBool && !ap.b {
			return "{}"
		}
		return "Record<string, unknown>"
	}
	return "{\n" + strings.Join(lines, "\n") + "\n" + indent + "}"
}

// ref renders a `$ref` as an alias, registering the alias on first use. The
// alias is registered BEFORE its body is rendered, so a schema that refers
// back to itself finds the name and stops: that is what makes a cyclic
// schema terminate.
func (g *tsGen) ref(ptr string, depth int) string {
	if name, ok := g.aliasOf[ptr]; ok {
		return name
	}
	if !strings.HasPrefix(ptr, "#") {
		return "unknown /* external $ref " + commentSafe(ptr) + " */"
	}
	target, err := resolvePointer(g.root, strings.TrimPrefix(ptr, "#"))
	if err != nil {
		return "unknown /* unresolved $ref " + commentSafe(ptr) + " */"
	}
	name := g.aliasName(ptr)
	g.aliasOf[ptr] = name
	g.aliasOrder = append(g.aliasOrder, name)
	g.aliasBody[name] = "unknown"
	g.aliasBody[name] = g.typ(target, "", depth+1)
	return name
}

var nonIdent = regexp.MustCompile(`[^A-Za-z0-9_$]+`)

// aliasName turns a pointer's last segment into a free identifier.
func (g *tsGen) aliasName(ptr string) string {
	base := "Input"
	if segs := strings.Split(strings.TrimPrefix(ptr, "#"), "/"); len(segs) > 0 {
		if last := unescapePointer(segs[len(segs)-1]); last != "" {
			base = nonIdent.ReplaceAllString(last, "_")
		}
	}
	if base == "" || (base[0] >= '0' && base[0] <= '9') {
		base = "T" + base
	}
	base = strings.ToUpper(base[:1]) + base[1:]
	name := base
	for i := 2; g.taken[name]; i++ {
		name = base + strconv.Itoa(i)
	}
	g.taken[name] = true
	return name
}

func resolvePointer(root *jnode, ptr string) (*jnode, error) {
	cur := root
	if ptr == "" {
		return cur, nil
	}
	if !strings.HasPrefix(ptr, "/") {
		return nil, errors.New("not a JSON pointer")
	}
	for _, raw := range strings.Split(ptr[1:], "/") {
		seg := unescapePointer(raw)
		switch cur.kind {
		case kindObject:
			next := cur.get(seg)
			if next == nil {
				return nil, fmt.Errorf("no member %q", seg)
			}
			cur = next
		case kindArray:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(cur.arr) {
				return nil, fmt.Errorf("no element %q", seg)
			}
			cur = cur.arr[i]
		default:
			return nil, fmt.Errorf("cannot descend into a scalar at %q", seg)
		}
	}
	return cur, nil
}

func unescapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
}

// propertyDoc is a property's description and default, on one line.
func propertyDoc(s *jnode) string {
	if s == nil || s.kind != kindObject {
		return ""
	}
	var doc string
	if d := s.get("description"); d != nil && d.kind == kindString {
		doc = strings.Join(strings.Fields(d.str), " ")
	}
	if def := s.get("default"); def != nil {
		if doc != "" {
			doc += " "
		}
		doc += "Default: " + literal(def) + "."
	}
	if doc == "" {
		return ""
	}
	if r := []rune(doc); len(r) > maxDocRunes {
		doc = string(r[:maxDocRunes-1]) + "…"
	}
	return commentSafe(doc)
}

// docBlock renders a tool description as a JSDoc block.
func docBlock(desc, indent string) string {
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(indent + "/**\n")
	for _, line := range strings.Split(desc, "\n") {
		line = strings.TrimRight(line, " \t\r")
		b.WriteString(indent + " *")
		if line != "" {
			b.WriteString(" " + commentSafe(line))
		}
		b.WriteByte('\n')
	}
	b.WriteString(indent + " */\n")
	return b.String()
}

// commentSafe keeps a text from closing the comment it is written into.
func commentSafe(s string) string { return strings.ReplaceAll(s, "*/", "*\\/") }

func union(parts []string) string {
	seen := map[string]bool{}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return strings.Join(out, " | ")
}

// paren wraps a union or intersection so it binds as one type where it is
// composed further.
func paren(t string) string {
	if strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") || strings.HasPrefix(t, "(") {
		return t
	}
	if strings.Contains(t, " | ") || strings.Contains(t, " & ") {
		return "(" + t + ")"
	}
	return t
}

func literal(v *jnode) string {
	switch v.kind {
	case kindString:
		return quote(v.str)
	case kindNumber:
		return v.num
	case kindBool:
		return strconv.FormatBool(v.b)
	case kindNull:
		return "null"
	default:
		return "unknown"
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

var identifier = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

func isIdentifier(s string) bool { return identifier.MatchString(s) }

func requiredSet(s *jnode) map[string]bool {
	out := map[string]bool{}
	if r := s.get("required"); r != nil && r.kind == kindArray {
		for _, v := range r.arr {
			if v.kind == kindString {
				out[v.str] = true
			}
		}
	}
	return out
}

func firstArray(s *jnode, keys ...string) *jnode {
	for _, k := range keys {
		if v := s.get(k); v != nil && v.kind == kindArray && len(v.arr) > 0 {
			return v
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// An ORDERED JSON value: a schema's properties render in the order its author
// wrote them, which a Go map would lose.
// ---------------------------------------------------------------------------

type jkind uint8

const (
	kindNull jkind = iota
	kindBool
	kindNumber
	kindString
	kindArray
	kindObject
)

type jmember struct {
	key string
	val *jnode
}

type jnode struct {
	kind jkind
	b    bool
	num  string
	str  string
	arr  []*jnode
	obj  []jmember
}

// get returns an object's member by key; the LAST one wins on a duplicate
// key, as encoding/json decodes it.
func (n *jnode) get(key string) *jnode {
	if n == nil || n.kind != kindObject {
		return nil
	}
	var found *jnode
	for _, m := range n.obj {
		if m.key == key {
			found = m.val
		}
	}
	return found
}

// maxParseDepth bounds nesting while parsing, so a hostile schema cannot
// exhaust the stack before the renderer's own depth bound applies.
const maxParseDepth = 128

func parseOrdered(raw []byte) (*jnode, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	n, err := parseValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the schema")
	}
	return n, nil
}

func parseValue(dec *json.Decoder, depth int) (*jnode, error) {
	if depth > maxParseDepth {
		return nil, errors.New("the schema nests too deep")
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			n := &jnode{kind: kindObject}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, errors.New("a non-string object key")
				}
				val, err := parseValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				n.obj = append(n.obj, jmember{key: key, val: val})
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		case '[':
			n := &jnode{kind: kindArray}
			for dec.More() {
				val, err := parseValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				n.arr = append(n.arr, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %q", v)
	case string:
		return &jnode{kind: kindString, str: v}, nil
	case json.Number:
		return &jnode{kind: kindNumber, num: v.String()}, nil
	case bool:
		return &jnode{kind: kindBool, b: v}, nil
	case nil:
		return &jnode{kind: kindNull}, nil
	}
	return nil, fmt.Errorf("unexpected token %v", tok)
}
