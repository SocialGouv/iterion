// Package toolindex is codemode's discovery surface (CM-3, ADR-165): the
// catalogue of the tools a program may call, their TypeScript declarations, a
// BM25 index over them, and the index lines the codemode tool's description
// carries.
//
// Two processes read it. The PARENT composes the index lines that go into the
// codemode tool's description, and bounds the catalogue it sends with each
// script. The CHILD builds the declarations and the search index from that
// catalogue and serves `call.describe` and `call.search` to the program. Both
// sides go through this package, so a tool the description lists is a tool the
// program can describe, under one spelling of its name.
//
// Everything here is a pure function of the catalogue. The index lines sit in
// the cached prompt prefix: a byte that moved between two turns of one node
// would re-bill the whole prefix. So the output never depends on map order, on
// the order the tools were registered in, or on anything but the tools.
package toolindex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Tool is one catalogue entry: what a program needs to know to call a tool.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`

	// SchemaOmitted is the size in bytes of a schema the catalogue bound left
	// out, and 0 when the schema travels. The declaration says so rather than
	// rendering an empty input: a program told `input: {}` would call the
	// tool with nothing.
	SchemaOmitted int `json:"schema_omitted,omitempty"`
}

// The catalogue bounds. The catalogue crosses into the child BEFORE it is
// confined, inside the one request document, so its size adds to what the
// child reads with no rlimit in force; [MaxCatalogBytes] is what the child's
// request bound reserves for it.
const (
	// MaxTools is how many tools the catalogue carries. A node past it is
	// told how many it holds beyond the bound, in the index itself.
	MaxTools = 256
	// MaxDescriptionBytes bounds one tool's description in the catalogue,
	// measured JSON-ENCODED: escaping can grow a control character six-fold,
	// so a bound on the raw bytes would not bound what crosses.
	MaxDescriptionBytes = 2 << 10
	// MaxNameBytes bounds one tool's name, encoded. A tool whose name is
	// longer is left out of the catalogue and counted with the tools past
	// MaxTools: its name is the one thing that cannot be shortened, since a
	// program calls the tool by it.
	MaxNameBytes = 256
	// MaxSchemaBytes bounds one tool's input schema. A larger schema is
	// omitted, and its declaration says how large it was.
	MaxSchemaBytes = 32 << 10
	// MaxSchemasBytes bounds all the schemas together. Past it, the largest
	// schemas are omitted first, until the rest fit.
	MaxSchemasBytes = 1 << 20
	// MaxCatalogBytes bounds the ENCODED catalogue: the names, descriptions
	// and schemas above, JSON-escaped, plus their envelopes. Escaping can
	// grow a description up to six-fold, so the bound is measured on the
	// encoding, never estimated from the inputs.
	MaxCatalogBytes = 2 << 20

	// summaryRunes bounds one index line's summary.
	summaryRunes = 120
	// maxLineParams bounds the parameters an index line names.
	maxLineParams = 8
)

// Bound returns the catalogue that travels to the child, and how many tools
// it leaves out.
//
// The result is sorted by name, holds one entry per name, and fits every bound
// above. A name registered twice keeps its LAST registration, the one the
// node's dispatcher serves (buildToolMap overwrites by name).
//
// Over [MaxCatalogBytes] after every reduction is an error and not a
// truncation: the reductions above leave at most a few hundred KiB, so
// reaching the bound means a bound here is wrong, and a catalogue cut at an
// arbitrary byte would describe tools the program cannot find.
func Bound(tools []Tool) ([]Tool, int, error) {
	byName := make(map[string]Tool, len(tools))
	longNames := 0
	for _, t := range tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		if encodedLen(name) > MaxNameBytes {
			longNames++
			continue
		}
		t.Name = name
		byName[name] = t
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)

	omitted := longNames
	if len(names) > MaxTools {
		omitted += len(names) - MaxTools
		names = names[:MaxTools]
	}

	cat := make([]Tool, 0, len(names))
	for _, n := range names {
		t := byName[n]
		t.Description = truncateEncoded(t.Description, MaxDescriptionBytes)
		if len(t.InputSchema) > 0 && !json.Valid(t.InputSchema) {
			// The node's dispatcher would refuse this schema too; the
			// declaration says the input is unknown rather than guessing.
			t.SchemaOmitted = len(t.InputSchema)
			t.InputSchema = nil
		}
		if n := schemaLen(t.InputSchema); t.SchemaOmitted == 0 && n > MaxSchemaBytes {
			t.SchemaOmitted = n
			t.InputSchema = nil
		}
		cat = append(cat, t)
	}

	// The total schema bound: omit the LARGEST first, ties broken by name, so
	// the reduction keeps as many tools described as the budget allows and
	// the same catalogue always loses the same schemas.
	sizes := make([]int, len(cat))
	total := 0
	for i, t := range cat {
		sizes[i] = schemaLen(t.InputSchema)
		total += sizes[i]
	}
	if total > MaxSchemasBytes {
		order := make([]int, len(cat))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool {
			la, lb := sizes[order[a]], sizes[order[b]]
			if la != lb {
				return la > lb
			}
			return cat[order[a]].Name < cat[order[b]].Name
		})
		for _, i := range order {
			if total <= MaxSchemasBytes {
				break
			}
			total -= sizes[i]
			cat[i].SchemaOmitted = sizes[i]
			cat[i].InputSchema = nil
		}
	}

	encoded, err := Encode(cat)
	if err != nil {
		return nil, 0, fmt.Errorf("toolindex: encoding the catalogue: %w", err)
	}
	if len(encoded) > MaxCatalogBytes {
		return nil, 0, fmt.Errorf("toolindex: the catalogue encodes to %d bytes, over the %d-byte bound after every reduction — a bound in this package is wrong",
			len(encoded), MaxCatalogBytes)
	}
	return cat, omitted, nil
}

// Index is a catalogue ready to answer: its lines, its declarations and its
// search.
type Index struct {
	tools   []Tool
	byName  map[string]int
	omitted int
	search  *bm25
}

// New indexes a catalogue [Bound] produced. omitted is the count [Bound]
// returned, so the index lines can say how many tools are callable but not
// indexed.
func New(cat []Tool, omitted int) *Index {
	tools := append([]Tool(nil), cat...)
	sort.SliceStable(tools, func(a, b int) bool { return tools[a].Name < tools[b].Name })
	x := &Index{tools: tools, byName: make(map[string]int, len(tools)), omitted: omitted}
	docs := make([][]string, len(tools))
	for i, t := range tools {
		x.byName[t.Name] = i
		docs[i] = documentTokens(t)
	}
	x.search = newBM25(docs)
	return x
}

// Len is how many tools the index holds.
func (x *Index) Len() int { return len(x.tools) }

// Lines renders the index the codemode tool's description carries: one line
// per tool, sorted by name, each naming the tool's top-level parameters and
// the first sentence of its description.
func (x *Index) Lines() string {
	if len(x.tools) == 0 && x.omitted == 0 {
		return "(none: this node holds no tool a program may call)"
	}
	var b strings.Builder
	for i, t := range x.tools {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("- ")
		b.WriteString(t.Name)
		if params := lineParams(t); params != "" {
			b.WriteString(" {")
			b.WriteString(params)
			b.WriteByte('}')
		}
		if s := Summary(t.Description); s != "" {
			b.WriteString(": ")
			b.WriteString(s)
		}
	}
	if x.omitted > 0 {
		if len(x.tools) > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "- … and %d more a program can call, outside the index's bounds (%d tools, %d-byte names): call.search does not find them, call(name, input) still reaches them",
			x.omitted, MaxTools, MaxNameBytes)
	}
	return b.String()
}

// Describe returns a tool's TypeScript declaration.
func (x *Index) Describe(name string) (string, bool) {
	i, ok := x.byName[strings.TrimSpace(name)]
	if !ok {
		return "", false
	}
	return Declaration(x.tools[i]), true
}

// Hit is one search result.
type Hit struct {
	Name        string
	Summary     string
	Declaration string
}

// DefaultSearchLimit and MaxSearchLimit bound how many hits one search
// answers: a search is for narrowing, and a program that wants every tool
// reads the index instead.
const (
	DefaultSearchLimit = 5
	MaxSearchLimit     = 20
)

// Search ranks the tools against a query by BM25 and returns the best limit
// of them, best first, ties broken by name. A limit outside 1..MaxSearchLimit
// is the default or the maximum. A tool whose name the query spells exactly
// ranks first, whatever its score: a program that knows the name is looking
// for that tool.
func (x *Index) Search(query string, limit int) []Hit {
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	if limit > MaxSearchLimit {
		limit = MaxSearchLimit
	}
	ranked := x.search.rank(tokens(query))
	if exact, ok := x.byName[strings.TrimSpace(query)]; ok {
		kept := []int{exact}
		for _, i := range ranked {
			if i != exact {
				kept = append(kept, i)
			}
		}
		ranked = kept
	}
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	hits := make([]Hit, 0, len(ranked))
	for _, i := range ranked {
		t := x.tools[i]
		hits = append(hits, Hit{Name: t.Name, Summary: Summary(t.Description), Declaration: Declaration(t)})
	}
	return hits
}

// Summary is the first sentence of a description, whitespace collapsed, at
// most summaryRunes runes. A sentence cut short ends with "…", so a reader
// knows there is more for call.describe to tell.
func Summary(desc string) string {
	s := strings.Join(strings.Fields(desc), " ")
	if s == "" {
		return ""
	}
	if end := sentenceEnd(s); end > 0 {
		s = s[:end]
	}
	if utf8.RuneCountInString(s) <= summaryRunes {
		return s
	}
	runes := []rune(s)[:summaryRunes-1]
	cut := string(runes)
	if sp := strings.LastIndexByte(cut, ' '); sp > summaryRunes/2 {
		cut = cut[:sp]
	}
	return strings.TrimRightFunc(cut, unicode.IsSpace) + "…"
}

// sentenceEnd is the byte offset just past the first sentence's full stop,
// or 0 when the text holds one sentence. A stop counts when a space follows
// it, so "v1.2" and "e.g" in the middle of a word do not end a sentence.
func sentenceEnd(s string) int {
	for i := 0; i+1 < len(s); i++ {
		switch s[i] {
		case '.', '!', '?':
			if s[i+1] == ' ' {
				return i + 1
			}
		}
	}
	return 0
}

// lineParams names a tool's top-level parameters, optional ones marked "?",
// in the schema's own order, at most maxLineParams of them.
func lineParams(t Tool) string {
	if len(t.InputSchema) == 0 {
		return ""
	}
	root, err := parseOrdered(t.InputSchema)
	if err != nil || root.kind != kindObject {
		return ""
	}
	props := root.get("properties")
	if props == nil || props.kind != kindObject || len(props.obj) == 0 {
		return ""
	}
	required := requiredSet(root)
	parts := make([]string, 0, len(props.obj))
	for i, p := range props.obj {
		if i == maxLineParams {
			parts = append(parts, "…")
			break
		}
		name := p.key
		if !isIdentifier(name) {
			name = quote(name)
		}
		if !required[p.key] {
			name += "?"
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, ", ")
}

// truncateBytes cuts s to at most n bytes on a rune boundary, marking the cut.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const mark = "…"
	cut := n - len(mark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}

// Encode is the ONE JSON encoding the catalogue is measured in and the
// request carrying it is written in: no HTML escaping. encoding/json's default
// rewrites `<`, `>` and `&` as six-byte escapes — inside a json.RawMessage
// schema too — so a bound measured on one encoding and a request written in
// the other would disagree by up to six-fold. With HTML escaping off, a
// schema's encoding is never longer than its raw bytes; control characters in
// a string are still escaped, which the bounds measure.
func Encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// encodedLen is the length of s in [Encode]'s encoding, quotes excluded.
func encodedLen(s string) int {
	b, _ := Encode(s)
	return len(b) - 2
}

// schemaLen is a schema's length in [Encode]'s encoding, which is what the
// schema bounds measure.
func schemaLen(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	b, err := Encode(raw)
	if err != nil {
		return len(raw)
	}
	return len(b)
}

// truncateEncoded cuts s, on a rune boundary and with a mark, so that its
// JSON encoding fits n bytes.
func truncateEncoded(s string, n int) string {
	if encodedLen(s) <= n {
		return s
	}
	const mark = "…"
	runes := []rune(s)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if encodedLen(string(runes[:mid])+mark) <= n {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(runes[:lo]) + mark
}
