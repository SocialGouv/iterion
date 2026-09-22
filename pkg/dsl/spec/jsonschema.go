package spec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The author JSON Schema: the shape of the YAML twin of a `.bot` (lot 5 of
// #1010), rendered from this registry so that the two surfaces cannot
// drift. One document per syntax profile — a property outside its
// Since/Until window is absent from that profile's schema, a deprecated one
// is marked — and one combined document that dispatches on the required
// `dsl:` key. The three are committed under docs/references and held fresh
// by TestGeneratedDSLDocsAreFresh like the Monaco module: complete
// generated files, not spliced regions.
//
// The document's shape (lot5-plan § 2): a root object with `dsl`
// (required), `catalog` (the `## ---` frontmatter), `imports`, the four
// author-named top-level blocks (`vars`, `presets`, `attachments`,
// `secrets`), the named declarations as mappings keyed by name (`prompts`,
// `schemas`, `cursors`, `supervisors`, `mcp_servers`, `contracts`), the
// ordered declarations as sequences (`groups`, `uses`, `nodes` — a node is
// `{<kind>: <name>, …properties}`), and one `workflow`. A block of
// author-named entries is a mapping keyed by name — a sequence of objects
// carrying the name under Entries.SequenceKey when order is meaning — each
// entry the scalar of its one required part when that is all it needs, or
// an object of its parts and its sub-block's properties; an entry whose
// line may stop at its name admits null. Edge lines keep the `.bot`'s own
// grammar, one string each.
//
// What the schema cannot see — JSON has one number type, YAML two — and
// the converter checks on the YAML tag instead (testdata/author/lax): a
// float spelled with a zero fraction where the `.bot` wants an integer
// (`dsl: 2.0`, `max_tokens: 1.0` — the lexer refuses a Float), and a
// non-finite float (`.inf`, `.nan`) where a bounded number is wanted. The
// schema is an author's aid; the `.bot` the converter writes is the truth.

// SchemaDialect is the JSON Schema dialect the artefacts declare.
const SchemaDialect = "https://json-schema.org/draft/2020-12/schema"

// schemaIDBase is where the committed artefacts are served from.
const schemaIDBase = "https://raw.githubusercontent.com/SocialGouv/iterion/main/docs/references/"

// SchemaFile is the repository-relative path of one profile's author schema.
func SchemaFile(profile int) string {
	return fmt.Sprintf("docs/references/iterion-author.v%d.schema.json", profile)
}

// CombinedSchemaFile is the artefact an editor points at when the document's
// profile is not known in advance: `oneOf` the per-profile schemas,
// discriminated by the required `dsl:` value.
const CombinedSchemaFile = "docs/references/iterion-author.schema.json"

// SchemaProfiles are the syntax profiles a schema is rendered for, 1 to
// maxProfile — the caller passes parser.MaxProfile; spec stays a leaf.
func SchemaProfiles(maxProfile int) []int {
	out := make([]int, 0, maxProfile)
	for p := 1; p <= maxProfile; p++ {
		out = append(out, p)
	}
	return out
}

type obj = map[string]any

// RenderSchema renders the author JSON Schema of one syntax profile.
func RenderSchema(profile int) ([]byte, error) {
	return renderSchemaFor(Kinds, profile)
}

func renderSchemaFor(kinds []Kind, profile int) ([]byte, error) {
	if profile < 1 {
		return nil, fmt.Errorf("author schema: %d is not a syntax profile", profile)
	}
	b := newSchemaBuilder(kinds, profile)
	root := b.root()
	root["$schema"] = SchemaDialect
	root["$id"] = schemaIDBase + strings.TrimPrefix(SchemaFile(profile), "docs/references/")
	root["title"] = fmt.Sprintf("iterion author document, syntax profile %d", profile)
	root["$defs"] = b.defs
	return marshalSchema(root)
}

// RenderCombinedSchema renders the schema that accepts a document of any of
// the given profiles, choosing by its `dsl:` value. Each profile's
// definitions live under their own prefix, so two profiles never share a
// fragment whose window differs.
func RenderCombinedSchema(profiles []int) ([]byte, error) {
	if len(profiles) == 0 {
		return nil, fmt.Errorf("author schema: no profile to combine")
	}
	defs := obj{}
	alternatives := make([]any, 0, len(profiles))
	for _, p := range profiles {
		if p < 1 {
			return nil, fmt.Errorf("author schema: %d is not a syntax profile", p)
		}
		b := newSchemaBuilder(Kinds, p)
		root := b.root()
		key := fmt.Sprintf("v%d", p)
		for name, def := range b.defs {
			defs[key+"."+name] = rewriteRefs(def, "#/$defs/", "#/$defs/"+key+".")
		}
		defs[key] = rewriteRefs(root, "#/$defs/", "#/$defs/"+key+".")
		alternatives = append(alternatives, obj{"$ref": "#/$defs/" + key})
	}
	root := obj{
		"$schema":     SchemaDialect,
		"$id":         schemaIDBase + strings.TrimPrefix(CombinedSchemaFile, "docs/references/"),
		"title":       "iterion author document",
		"description": "The YAML twin of a .bot: a draft an author writes and `iterion fmt --to bot` converts; never the committed truth. `dsl:` is required and selects the syntax profile the document is read in.",
		"oneOf":       alternatives,
		"$defs":       defs,
	}
	return marshalSchema(root)
}

// marshalSchema encodes with sorted keys, two-space indentation and a
// trailing newline — the stable text the freshness test compares.
func marshalSchema(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// rewriteRefs returns a deep copy of v with every `$ref` string prefixed
// from → to.
func rewriteRefs(v any, from, to string) any {
	switch t := v.(type) {
	case obj:
		out := make(obj, len(t))
		for k, val := range t {
			if s, ok := val.(string); ok && k == "$ref" && strings.HasPrefix(s, from) {
				out[k] = to + strings.TrimPrefix(s, from)
				continue
			}
			out[k] = rewriteRefs(val, from, to)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = rewriteRefs(val, from, to)
		}
		return out
	}
	return v
}

type schemaBuilder struct {
	profile int
	kinds   []Kind
	defs    obj
}

func newSchemaBuilder(kinds []Kind, profile int) *schemaBuilder {
	return &schemaBuilder{profile: profile, kinds: kinds, defs: obj{}}
}

func (b *schemaBuilder) lookup(name string) (Kind, bool) {
	for _, k := range b.kinds {
		if k.Name == name {
			return k, true
		}
	}
	return Kind{}, false
}

func ref(name string) obj { return obj{"$ref": "#/$defs/" + name} }

// def registers a definition once and returns a reference to it; the
// placeholder set first breaks the recursion of a group holding nodes.
func (b *schemaBuilder) def(name string, build func() obj) obj {
	if _, ok := b.defs[name]; !ok {
		b.defs[name] = obj{}
		b.defs[name] = build()
	}
	return ref(name)
}

const (
	identPattern       = `^[A-Za-z_][A-Za-z0-9_]*$`
	dottedIdentPattern = `^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`
	envPattern         = EnvFormPattern
)

// EnvFormPattern is the environment form a quoted EnumOrEnv value takes,
// `${VAR}` or `${VAR:-default}`, substituted at run time. The default may
// be anything, another environment form included (`${A:-${B:-high}}`, a
// spelling the shipped bots use); the pattern is the one the author schema
// renders and the one the converter (pkg/dsl/author) holds a value to, so
// the two cannot disagree on what an environment form is.
const EnvFormPattern = `^\$\{[A-Za-z_][A-Za-z0-9_]*(:-.*)?\}$`

// accepts reports whether the property is in the profile's window.
func (b *schemaBuilder) accepts(p Property) bool {
	return (p.Since == 0 || p.Since <= b.profile) && (p.Until == 0 || b.profile <= p.Until)
}

// root is the document: `dsl` required, then every top-level key.
func (b *schemaBuilder) root() obj {
	props := obj{
		"dsl": obj{
			"type":        "integer",
			"const":       b.profile,
			"description": fmt.Sprintf("The syntax profile the document is read in: %d, written as a bare integer (`%d`, never `%d.0`: the .bot refuses a float, and JSON Schema cannot tell the two apart — the converter checks the YAML tag). Required — a new surface has nothing to guess.", b.profile, b.profile, b.profile),
		},
		"catalog": b.def("catalog", catalogDef),
		"imports": obj{
			"type":        "array",
			"items":       obj{"type": "string", "minLength": 1},
			"description": "Fragments under the bot's lib/ directory (`lib/<file>.bot`), resolved from the document's directory, as `import` lines do in a .bot.",
		},
	}
	for _, name := range []string{"vars", "presets", "attachments", "secrets"} {
		if k, ok := b.lookup(name); ok {
			props[name] = b.kindSchema(k)
		}
	}
	for _, m := range []struct{ kind, key string }{
		{"prompt", "prompts"}, {"schema", "schemas"}, {"cursor", "cursors"},
		{"supervisor", "supervisors"}, {"mcp_server", "mcp_servers"}, {"contract", "contracts"},
	} {
		k, ok := b.lookup(m.kind)
		if !ok {
			continue
		}
		props[m.key] = obj{
			"type":                 "object",
			"propertyNames":        obj{"pattern": identPattern},
			"additionalProperties": b.kindSchema(k),
			"description":          k.Doc,
		}
	}
	if group, ok := b.lookup("group"); ok {
		props["groups"] = obj{"type": "array", "items": b.headerKind(group), "description": group.Doc}
	}
	if use, ok := b.lookup("use"); ok {
		props["uses"] = obj{"type": "array", "items": b.headerKind(use), "description": use.Doc}
	}
	props["nodes"] = obj{"type": "array", "items": b.nodeUnion(nil), "description": "The graph's nodes, in order: each one `{<kind>: <name>, …properties}`."}
	if wf, ok := b.lookup("workflow"); ok {
		props["workflow"] = b.workflow(wf)
	}
	return obj{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"dsl"},
		"properties":           props,
		"description":          "The YAML twin of a .bot file: the same declarations, in the .bot's order, with YAML's native values. A draft `iterion fmt --to bot` converts; never the committed truth.",
	}
}

// catalogDef is the `## ---` frontmatter of a .bot (bundle.Frontmatter).
func catalogDef() obj {
	return obj{
		"type":                 "object",
		"additionalProperties": false,
		"description":          "The catalog identity the .bot carries in its `## ---` frontmatter; a non-empty triggers or capabilities OVERRIDES the manifest's at discovery.",
		"properties": obj{
			"name":         obj{"type": "string"},
			"description":  obj{"type": "string"},
			"triggers":     obj{"type": "array", "items": obj{"type": "string"}},
			"capabilities": obj{"type": "array", "items": obj{"type": "string"}},
		},
	}
}

// kindSchema is the schema of a kind's body: its property table, its
// entries, its text.
func (b *schemaBuilder) kindSchema(k Kind) obj {
	return b.def(k.Name, func() obj { return b.body(k) })
}

func (b *schemaBuilder) body(k Kind) obj {
	if k.Text {
		return obj{"type": "string", "description": k.Doc}
	}
	if k.Entries != nil && len(k.Properties) == 0 {
		return b.entries(k, k.Entries)
	}
	o := b.propertiesObject(k.Properties, nil)
	o["description"] = k.Doc
	if k.Entries != nil {
		// A block with a fixed table AND free entries (cursors): every
		// other key is an entry.
		o["additionalProperties"] = b.entryValue(k.Entries)
	}
	return o
}

// propertiesObject is an object schema over a property table, closed to
// other keys; extra adds fixed properties (a node's kind key).
func (b *schemaBuilder) propertiesObject(props []Property, extra obj) obj {
	properties := obj{}
	for k, v := range extra {
		properties[k] = v
	}
	for _, p := range props {
		if b.accepts(p) {
			properties[p.Name] = b.property(p)
		}
	}
	return obj{"type": "object", "additionalProperties": false, "properties": properties}
}

// property is the fragment of one property, from its Form.
func (b *schemaBuilder) property(p Property) obj {
	o := b.form(p.Form, p.Values, p.Body)
	if p.Doc != "" {
		o["description"] = p.Doc
	}
	if p.Deprecated {
		o["deprecated"] = true
	}
	return o
}

// form is the fragment of one value form; values and body are the
// property's or the field's.
func (b *schemaBuilder) form(f Form, values []string, body string) obj {
	switch f {
	case String, QuotedString, StringOrIdent, PromptRef, TypeRef:
		return obj{"type": "string"}
	case Ident:
		return obj{"type": "string", "pattern": identPattern}
	case DottedIdent:
		return obj{"type": "string", "pattern": dottedIdentPattern}
	case StringOrNumber:
		return obj{"anyOf": []any{obj{"type": "string"}, obj{"type": "number", "minimum": 0}}}
	case Int:
		return obj{"type": "integer", "minimum": 0, "$comment": "an integer literal (`3`, never `3.0`): the .bot refuses a float, JSON Schema cannot tell the two apart — the converter checks the YAML tag"}
	case Number:
		return obj{"type": "number", "minimum": 0, "$comment": "a finite non-negative number; `.inf` and `.nan` pass JSON Schema's bound and are the converter's to refuse"}
	case Bool:
		return obj{"type": "boolean"}
	case JSON:
		return b.def("json_value", jsonValueDef)
	case Literal:
		return obj{"anyOf": []any{obj{"type": "string"}, obj{"type": "number", "minimum": 0}, obj{"type": "boolean"}}}
	case Enum:
		return obj{"enum": stringsToAny(values)}
	case EnumOrEnv:
		return obj{"anyOf": []any{
			obj{"enum": stringsToAny(values)},
			obj{"type": "string", "pattern": envPattern, "description": "An environment form, substituted at run time; the .bot also takes any quoted string, resolved then"},
		}}
	case IdentList:
		return obj{"type": "array", "items": obj{"type": "string", "pattern": identPattern}}
	case StringList, ToolList, SkillList, MixedList:
		return obj{"type": "array", "items": obj{"type": "string"}}
	case IdentOrList:
		return obj{"anyOf": []any{
			obj{"type": "string", "pattern": identPattern},
			obj{"type": "array", "items": obj{"type": "string", "pattern": identPattern}},
		}}
	case IntOrStringList:
		return obj{"anyOf": []any{obj{"type": "integer", "minimum": 0}, obj{"type": "array", "items": obj{"type": "string"}}}}
	case Setting:
		return obj{"anyOf": []any{obj{"type": "string"}, obj{"type": "number"}}}
	case Map:
		return obj{"type": "object", "additionalProperties": obj{"type": "string"}}
	case WithMap:
		return obj{"type": "object", "additionalProperties": obj{"anyOf": []any{obj{"type": "string"}, obj{"type": "number"}, obj{"type": "boolean"}}}}
	case Block:
		if k, ok := b.lookup(body); ok {
			return b.kindSchema(k)
		}
	case BlockOrIdent:
		if k, ok := b.lookup(body); ok {
			return obj{"anyOf": []any{obj{"type": "string", "pattern": identPattern}, b.kindSchema(k)}}
		}
	}
	return obj{"description": "unrendered form " + string(f)}
}

func jsonValueDef() obj {
	return obj{
		"description": "One JSON value: text, a non-negative number, a bool, null, a list or an object of these — the subset a .bot writes (no signed number, no exponent).",
		"anyOf": []any{
			obj{"type": "string"}, obj{"type": "number", "minimum": 0}, obj{"type": "boolean"}, obj{"type": "null"},
			obj{"type": "array", "items": ref("json_value")},
			obj{"type": "object", "additionalProperties": ref("json_value")},
		},
	}
}

func stringsToAny(values []string) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, v)
	}
	return out
}

// entries is the schema of a block of author-named entries: a mapping keyed
// by the entry's name, or a sequence of objects carrying the name under
// SequenceKey when order is meaning.
func (b *schemaBuilder) entries(k Kind, e *Entries) obj {
	description := strings.TrimSpace(k.Doc + " " + e.Doc)
	if e.SequenceKey != "" {
		item := b.entryObject(e, obj{e.SequenceKey: obj{"type": "string", "pattern": identPattern, "description": "The entry's name"}}, []string{e.SequenceKey})
		return obj{"type": "array", "items": item, "description": description}
	}
	o := obj{"type": "object", "additionalProperties": b.entryValue(e), "description": description}
	if e.Key == Ident || e.Key == "" {
		o["propertyNames"] = obj{"pattern": identPattern}
	}
	return o
}

// entryValue is what one entry's value looks like: the scalar of its one
// required part when the entry has nothing else; that scalar OR an object
// of its parts when it has optional parts or a sub-block; the object alone
// when the line stops at the name; null admitted when no part is required
// (a bare `name:`).
func (b *schemaBuilder) entryValue(e *Entries) obj {
	if e.Entries != nil {
		return obj{"type": "object", "additionalProperties": b.entryValue(e.Entries), "description": e.Doc}
	}
	var required []Field
	for _, f := range e.Fields {
		if f.Required {
			required = append(required, f)
		}
	}
	scalarOf := func(f Field) obj {
		o := b.form(f.Form, f.Values, "")
		o["description"] = f.Doc
		return o
	}
	if len(e.Fields) == 1 && e.Body == "" && e.Fields[0].Required {
		return scalarOf(e.Fields[0])
	}
	var alternatives []any
	switch {
	case len(required) == 1:
		s := scalarOf(required[0])
		s["description"] = "The " + required[0].Name + " alone: the entry's shorthand"
		alternatives = append(alternatives, s)
	case len(required) == 0 && len(e.Fields) == 1:
		s := scalarOf(e.Fields[0])
		s["description"] = "The " + e.Fields[0].Name + " alone: the entry's shorthand"
		alternatives = append(alternatives, s)
	}
	alternatives = append(alternatives, b.entryObject(e, nil, nil))
	if len(required) == 0 {
		alternatives = append(alternatives, obj{"type": "null", "description": "The bare name: the entry declared, nothing set on its line"})
	}
	if len(alternatives) == 1 {
		return alternatives[0].(obj)
	}
	return obj{"anyOf": alternatives}
}

// entryObject is the object form of an entry: its parts, then its
// sub-block's properties, closed to other keys.
func (b *schemaBuilder) entryObject(e *Entries, extra obj, requiredKeys []string) obj {
	properties := obj{}
	for k, v := range extra {
		properties[k] = v
	}
	required := append([]string{}, requiredKeys...)
	for _, f := range e.Fields {
		o := b.form(f.Form, f.Values, "")
		o["description"] = f.Doc
		properties[f.Name] = o
		if f.Required {
			required = append(required, f.Name)
		}
	}
	if e.Body != "" {
		if bk, ok := b.lookup(e.Body); ok {
			for _, p := range bk.Properties {
				if !b.accepts(p) {
					continue
				}
				if _, taken := properties[p.Name]; taken {
					continue // the header's value and the sub-block's property are one field (a secret's value)
				}
				properties[p.Name] = b.property(p)
			}
		}
	}
	o := obj{"type": "object", "additionalProperties": false, "properties": properties, "description": e.Doc}
	if len(required) > 0 {
		sort.Strings(required)
		o["required"] = stringsToAny(required)
	}
	return o
}

// headerKind is a declaration written on a header line (group, use): the
// kind's key carries the name, the header parts follow, then the body.
func (b *schemaBuilder) headerKind(k Kind) obj {
	return b.def(k.Name, func() obj {
		nameDoc := "The declaration's name"
		if k.Name == "use" {
			nameDoc = "The group instantiated"
		}
		properties := obj{k.Name: obj{"type": "string", "pattern": identPattern, "description": nameDoc}}
		required := []string{k.Name}
		if k.Header != nil {
			for _, f := range k.Header.Fields {
				o := b.form(f.Form, f.Values, "")
				o["description"] = f.Doc
				properties[f.Name] = o
				if f.Required {
					required = append(required, f.Name)
				}
			}
		}
		if len(k.Holds) > 0 {
			properties["nodes"] = obj{"type": "array", "items": b.nodeUnion(k.Holds), "description": "The declarations the body holds, in order"}
		}
		if k.Edges {
			properties["edges"] = edgesDef()
		}
		sort.Strings(required)
		return obj{"type": "object", "additionalProperties": false, "required": stringsToAny(required), "properties": properties, "description": k.Doc}
	})
}

// nodeUnion is `oneOf` the node kinds — all of them, or the ones listed.
func (b *schemaBuilder) nodeUnion(only []string) obj {
	var alternatives []any
	for _, k := range b.kinds {
		if k.Role != Node || (only != nil && !hasString(only, k.Name)) {
			continue
		}
		alternatives = append(alternatives, b.node(k))
	}
	return obj{"oneOf": alternatives}
}

// node is `{<kind>: <name>, …properties}`.
func (b *schemaBuilder) node(k Kind) obj {
	return b.def("node."+k.Name, func() obj {
		o := b.propertiesObject(k.Properties, obj{k.Name: obj{"type": "string", "pattern": identPattern, "description": "The node's name"}})
		o["required"] = []any{k.Name}
		o["description"] = k.Doc
		return o
	})
}

// workflow is the one workflow: its name, its properties, its edge lines.
func (b *schemaBuilder) workflow(k Kind) obj {
	return b.def("workflow", func() obj {
		o := b.propertiesObject(k.Properties, obj{"name": obj{"type": "string", "pattern": identPattern, "description": "The workflow's name"}})
		if k.Edges {
			o["properties"].(obj)["edges"] = edgesDef()
		}
		o["required"] = []any{"name"}
		o["description"] = k.Doc
		return o
	})
}

func edgesDef() obj {
	return obj{
		"type":        "array",
		"items":       obj{"type": "string", "minLength": 1},
		"description": "Edge lines in the .bot's own grammar, one per string: `src -> dst [when <expr> | else] [as loop(N)] [with { key: \"value\" }]`; quoted only when YAML needs it (a `: ` inside a with map).",
	}
}

func hasString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
