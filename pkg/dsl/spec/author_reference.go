package spec

import (
	"fmt"
	"strings"
)

// The author document's reference (docs/references/author-schema.md, region
// `author`): the document's top-level keys, where each node kind sits, and
// how each value form is written in YAML — what differs from the .bot. The
// properties of every kind are the .bot's, under the same names, and the
// page links the .bot reference for them rather than repeat it.

// authorBlockKinds are the four author-named top-level blocks, written under
// their own name.
var authorBlockKinds = []string{"vars", "presets", "attachments", "secrets"}

// authorDocOverrides rewrites, for the author surface only, registry Doc
// lines whose .bot reading is false in YAML: in a .bot a bare header
// declares an empty declaration, but the converter takes a value and
// refuses a bare one (E051), so the page and the JSON Schemas spell the
// empty forms instead. A test renders every author surface and refuses
// the phrase, so a registry Doc that grows one re-reddens here.
var authorDocOverrides = map[string]string{
	"schema":   "A structured-output shape; an empty schema is written `verdict: {}`.",
	"prompt":   "A named text block, referenced by `system:` / `user:` / `instructions:`; its body is free text with `{{…}}` references and `{{include \"file\"}}` directives, the first line's indentation stripped from every line. Blank lines in the body are dropped by the lexer in profile 1 and kept as paragraph breaks in profile 2; an empty prompt is written `ask: ''`.",
	"contract": "The public interface of a bot, named by the workflow's `contract:`; no prompts, tools or provider configuration. An empty contract is written `c1: {}`.",
	"workflow": "The graph: entry, edges (`src -> dst [when …|else] [as loop(N)] [with {…}]`), and the run-wide settings; the document always writes it as a mapping (`workflow:` with no value is refused, E051).",
}

// authorDoc is a kind's Doc as the author surface tells it.
func authorDoc(k Kind) string {
	if d, ok := authorDocOverrides[k.Name]; ok {
		return d
	}
	return k.Doc
}

// authorNamedDeclarations are the declarations a document writes as a
// mapping keyed by the declaration's name, with the key it sits under.
var authorNamedDeclarations = []struct{ Kind, Key string }{
	{"schema", "schemas"}, {"prompt", "prompts"}, {"cursor", "cursors"},
	{"supervisor", "supervisors"}, {"mcp_server", "mcp_servers"}, {"contract", "contracts"},
}

// AuthorKey is one top-level key of the author document.
type AuthorKey struct {
	Name  string
	Shape string // the shape its value takes
	Doc   string // what it holds
}

// AuthorKeys are the author document's top-level keys, in the order the
// writer puts them (pkg/dsl/author.Write) — the keys the author JSON
// Schema's root accepts, no more, no less (a test holds the two together).
func AuthorKeys() []AuthorKey {
	keys := []AuthorKey{
		{"dsl", "an integer, required", "The syntax profile the document is read in — `1` or `2`, a bare integer, never `2.0`. A document always names it (E052)."},
		{"catalog", "a mapping of `name`, `description`, `triggers`, `capabilities`", "The catalog identity the .bot carries in its `## ---` frontmatter."},
		{"imports", "a list of `lib/<file>.bot`", "The fragments of the bot's unit, read beside the .bot the document stands for, as `import` lines are."},
	}
	for _, name := range authorBlockKinds {
		if k, ok := Lookup(name); ok {
			keys = append(keys, AuthorKey{name, authorEntriesShape(k.Entries), authorDoc(k)})
		}
	}
	for _, d := range authorNamedDeclarations {
		if k, ok := Lookup(d.Kind); ok {
			keys = append(keys, AuthorKey{d.Key, "a mapping keyed by name, each value " + authorBodyShape(k), authorDoc(k)})
		}
	}
	if k, ok := Lookup("group"); ok {
		keys = append(keys, AuthorKey{"groups", "a list, each item `group: <name>` with its header parts and its nodes", k.Doc})
	}
	if k, ok := Lookup("use"); ok {
		keys = append(keys, AuthorKey{"uses", "a list, each item `use: <group>` with its header parts", k.Doc})
	}
	keys = append(keys,
		AuthorKey{"nodes", "a list, each item `<kind>: <name>` and that kind's properties", "The graph's nodes, in order."},
		AuthorKey{"workflow", "a mapping of `name`, `entry`, `edges` — one `.bot` edge line per item — and the workflow's properties", "The one workflow of the file."},
	)
	return keys
}

// authorBodyShape is how one declaration's value is written.
func authorBodyShape(k Kind) string {
	switch {
	case k.Text:
		return "its text"
	case k.Entries != nil && len(k.Properties) == 0:
		return authorEntriesShape(k.Entries)
	}
	return "a mapping of its properties"
}

// authorEntriesShape is how a block of author-named entries is written.
func authorEntriesShape(e *Entries) string {
	if e == nil {
		return "a mapping of its properties"
	}
	if e.SequenceKey != "" {
		return fmt.Sprintf("a list of mappings, each naming its entry under `%s`", e.SequenceKey)
	}
	name := e.KeyName
	if name == "" {
		name = "name"
	}
	return "a mapping keyed by " + name
}

// authorFormNotes is how each value form is written in an author document,
// read off the converter (pkg/dsl/author, spell.go): what it takes and what
// it refuses. A form the registry uses and this table omits fails a test, as
// does a row for a form the registry no longer uses.
var authorFormNotes = map[Form]string{
	String:          "a YAML string — plain when it reads back as the same text; quoted when it holds `: ` or ends in `:` (the document is refused otherwise, E050) or holds ` #` (otherwise a comment starts there and the value ends), when it starts with a character YAML reserves — `{`, `[`, `!`, `&`, `*`, `|`, `>`, `%`, `@`, a backtick; a `{{…}}` template written unquoted is a YAML mapping (refused) — or when it would read as a number, a bool, a date or null (refused, E051: quote it); inside single quotes a `'` is written twice (`''`); a `|` block for several lines",
	QuotedString:    "a YAML string, as `string`",
	StringOrIdent:   "a YAML string, as `string`",
	Ident:           "a name — letters, digits, `_` — plain (quoted, it reads the same)",
	DottedIdent:     "a name, dotted for a group instance's node or a node's field (`r1.look`, `node.field`)",
	Int:             "an integer as digits, without a leading 0: `3` — never `3.0`, and none of YAML's other spellings (`010`, the octal 8; `0x10`, `+3`, `1_000`), which are refused, YAML's reading named",
	Number:          "a finite non-negative number as digits, without a leading 0: `3`, `0.8` — YAML's `1e2`, `.5`, `01.5` or `+3` are refused, YAML's reading named",
	Bool:            "`true` or `false` — YAML 1.2 reads `yes`, `no`, `on` and `off` as strings, refused",
	JSON:            "a YAML value of the JSON subset: text, a non-negative number, a bool, null, a list, a mapping",
	Enum:            "one of the listed words",
	EnumOrEnv:       "one of the listed words, plain — or any other value quoted, kept as written for a run-time substitution (`'${EFFORT:-high}'`)",
	PromptRef:       "a declared prompt's name, plain — or the prompt's own text, quoted or as a `|` block",
	StringOrNumber:  "a string, a number or a bool; a number written as digits, a `-` included (`3`, `0.5`, `-10`), or `true`/`false`, is the text it spells (`30s` is a string) — YAML's other spellings (`010`, `0x10`, `+3`, `1e2`, `True`) are refused: quote them if they are text",
	IdentList:       "a list of names: `[a, b]`, or one `- a` per line",
	StringList:      "a list of strings: `[a, b]`, or one `- a` per line",
	ToolList:        "a list of strings, as `string list`",
	SkillList:       "a list of strings, as `string list`",
	MixedList:       "a list of strings, as `string list`",
	IdentOrList:     "a name, or a list of names",
	Map:             "a mapping of names to strings: `{KEY: v}`, or one `KEY: v` per line",
	WithMap:         "a mapping of names to strings; a number or a bool written as the .bot writes one, a `-` included (`3`, `0.5`, `-10`, `true`), is the string it spells — YAML's other spellings (`007`, `True`) are refused: quote them",
	Block:           "a nested mapping of that kind's properties",
	BlockOrIdent:    "a word, or a nested mapping of that kind's properties",
	Literal:         "a string, an integer or a float as digits, or a bool",
	TypeRef:         "a builtin type or a declared schema's name, `[]` suffixes allowed",
	Setting:         "a string or a number, as `string|number`",
	IntOrStringList: "an integer (a count), or a list of strings (a pool of member ids)",
}

// authorFormOrder is the order the forms are listed in.
var authorFormOrder = []Form{
	String, QuotedString, StringOrIdent, Ident, DottedIdent, Int, Number, Bool, JSON,
	Enum, EnumOrEnv, PromptRef, StringOrNumber, IdentList, StringList, ToolList, SkillList,
	MixedList, IdentOrList, Map, WithMap, Block, BlockOrIdent, Literal, TypeRef, Setting,
	IntOrStringList,
}

// AuthorReference is the body of the `author` region.
func AuthorReference() string {
	var b strings.Builder
	b.WriteString("_Generated from the parser's property registry (`pkg/dsl/spec`) by `iterion dsl spec --write`; do not edit by hand. The author JSON Schemas beside this page are rendered from the same registry._\n\n")
	b.WriteString("### Top-level keys\n\n")
	b.WriteString("In the order the writer puts them (`iterion fmt`, `iterion fmt --to yaml`).\n\n")
	b.WriteString("| Key | Written as | Holds |\n|---|---|---|\n")
	for _, k := range AuthorKeys() {
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", k.Name, escapePipes(k.Shape), escapePipes(k.Doc))
	}
	b.WriteString("\n### Nodes\n\n")
	b.WriteString("Each item of `nodes:` names its kind and its node on its first key, then carries that kind's properties — the .bot's, under the same names:\n\n")
	for _, k := range Kinds {
		if k.Role != Node {
			continue
		}
		fmt.Fprintf(&b, "- `%s: <name>` — [properties](dsl-properties.md#%s)\n", k.Name, anchor(k.Name))
	}
	b.WriteString("\n### Values\n\n")
	b.WriteString("A property's value is written by its form — the form the .bot reference gives it:\n\n")
	b.WriteString("| Form | Written in the author document as |\n|---|---|\n")
	for _, f := range authorFormOrder {
		if note, ok := authorFormNotes[f]; ok {
			fmt.Fprintf(&b, "| %s | %s |\n", formCell(f), escapePipes(note))
		}
	}
	return b.String()
}

// formCell is a value form as a table cell: bare, as the .bot reference's
// own table writes it, and in code when it holds a brace — the docs site
// reads a `{ … }` ending a cell as an attribute block (markdown-it-attrs),
// and the page no longer builds.
func formCell(f Form) string {
	s := escapePipes(string(f))
	if strings.ContainsAny(s, "{}") {
		return "`" + s + "`"
	}
	return s
}
