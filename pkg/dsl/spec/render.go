package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The generated regions. A committed document carries
//
//	<!-- dsl-spec:begin <what> -->
//	…
//	<!-- dsl-spec:end -->
//
// and Splice rewrites what lies between the two markers from the registry:
// `reference` is the whole property reference, `table <kind>` one kind's
// table, `skill` the compact section the authoring skills carry, `author`
// the author document's reference (AuthorReference). `iterion
// dsl spec --write` (task dsl:gen) regenerates every file in Files;
// TestGeneratedDSLDocsAreFresh (task dsl:check) fails when a committed
// region differs from what the registry renders — the same freshness
// contract as the pi extension's embedded asset.
const (
	beginPrefix = "<!-- dsl-spec:begin "
	endMarker   = "<!-- dsl-spec:end -->"
)

var regionRe = regexp.MustCompile(`(?s)<!-- dsl-spec:begin ([a-z]+(?: [a-z_.]+)?) -->\n.*?<!-- dsl-spec:end -->`)

// Files are the repository-relative documents that carry generated regions.
var Files = []string{
	"docs/references/dsl-properties.md",
	"docs/references/dsl-grammar.md",
	"SKILL.md",
	"bots/whats-next/skills/iterion-dsl-quickref.md",
	"docs/references/author-schema.md",
}

// Splice rewrites every generated region of doc from the registry. A
// region naming something the registry cannot render is an error, not a
// silent no-op — and so is a region the pattern cannot take whole: a
// `begin` without its `end` (or with CRLF line ends) would otherwise be
// left as it is and reported fresh, and in a document with several regions
// the pattern would run from that `begin` to the NEXT region's `end` and
// the rewrite would delete the hand-written text between them. Every
// marker must belong to exactly one matched region.
func Splice(doc string) (string, error) {
	begins, ends := strings.Count(doc, beginPrefix), strings.Count(doc, endMarker)
	if n := len(regionRe.FindAllString(doc, -1)); n != begins || n != ends {
		return "", fmt.Errorf("dsl-spec regions are unbalanced or unrecognised: %d matched, %d begin marker(s), %d end marker(s) — each `%s<what> -->` needs its own `%s` on a later line, LF line ends, and what must be reference, skill, author or table <kind>", n, begins, ends, beginPrefix, endMarker)
	}
	var firstErr error
	out := regionRe.ReplaceAllStringFunc(doc, func(m string) string {
		sub := regionRe.FindStringSubmatch(m)
		body, err := Render(sub[1])
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return m
		}
		return beginPrefix + sub[1] + " -->\n" + body + endMarker
	})
	if firstErr != nil {
		return "", firstErr
	}
	if !strings.Contains(doc, beginPrefix) {
		return "", fmt.Errorf("the document carries no dsl-spec region")
	}
	return out, nil
}

// Render produces the body of one region kind: "reference", "table <kind>",
// "skill" or "author".
func Render(what string) (string, error) {
	switch {
	case what == "reference":
		return Reference(), nil
	case what == "skill":
		return SkillSection(), nil
	case what == "author":
		return AuthorReference(), nil
	case strings.HasPrefix(what, "table "):
		kind := strings.TrimPrefix(what, "table ")
		k, ok := Lookup(kind)
		if !ok {
			return "", fmt.Errorf("dsl-spec region: unknown kind %q", kind)
		}
		// A `block → [kind](#kind)` link resolves on the reference page,
		// where every kind has a heading; a table spliced into another page
		// (dsl-grammar.md, beside the reference in docs/references/) has no
		// such heading to offer, so its links point at the reference instead.
		return strings.ReplaceAll(Table(k), "](#", "](dsl-properties.md#"), nil
	}
	return "", fmt.Errorf("dsl-spec region: unknown region %q", what)
}

// Regenerate rewrites the document regions, the Monaco module and the author
// schema artefacts under root, using lexicalKeywords from parser.Keywords()
// and maxProfile from parser.MaxProfile, and returns the changed files.
// Every document is read and spliced before
// any is written, so a document that cannot be regenerated leaves the tree
// as it was rather than half rewritten.
func Regenerate(root string, lexicalKeywords []string, maxProfile int) ([]string, error) {
	type pending struct {
		rel, path, body string
	}
	var todo []pending
	for _, rel := range Files {
		path := filepath.Join(root, rel)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		fresh, err := Splice(string(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		if fresh != string(raw) {
			todo = append(todo, pending{rel, path, fresh})
		}
	}
	monacoPath := filepath.Join(root, MonacoFile)
	monacoRaw, err := os.ReadFile(monacoPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if fresh := Monaco(lexicalKeywords); fresh != string(monacoRaw) {
		todo = append(todo, pending{MonacoFile, monacoPath, fresh})
	}
	artefacts, err := SchemaArtefacts(maxProfile)
	if err != nil {
		return nil, err
	}
	for _, a := range artefacts {
		path := filepath.Join(root, a.Path)
		raw, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if a.Body != string(raw) {
			todo = append(todo, pending{a.Path, path, a.Body})
		}
	}
	var changed []string
	for _, p := range todo {
		if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
			return changed, err
		}
		if err := os.WriteFile(p.path, []byte(p.body), 0o644); err != nil {
			return changed, err
		}
		changed = append(changed, p.rel)
	}
	return changed, nil
}

// Stale returns the documents, the Monaco module or the schema artefacts that
// differ from the registry, lexicalKeywords (parser.Keywords()) and maxProfile
// (parser.MaxProfile). A missing module or artefact is stale too.
func Stale(root string, lexicalKeywords []string, maxProfile int) ([]string, error) {
	var stale []string
	for _, rel := range Files {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			return stale, err
		}
		fresh, err := Splice(string(raw))
		if err != nil {
			return stale, fmt.Errorf("%s: %w", rel, err)
		}
		if fresh != string(raw) {
			stale = append(stale, rel)
		}
	}
	monacoRaw, err := os.ReadFile(filepath.Join(root, MonacoFile))
	if err != nil && !os.IsNotExist(err) {
		return stale, err
	}
	if string(monacoRaw) != Monaco(lexicalKeywords) {
		stale = append(stale, MonacoFile)
	}
	artefacts, err := SchemaArtefacts(maxProfile)
	if err != nil {
		return stale, err
	}
	for _, a := range artefacts {
		raw, err := os.ReadFile(filepath.Join(root, a.Path))
		if err != nil && !os.IsNotExist(err) {
			return stale, err
		}
		if string(raw) != a.Body {
			stale = append(stale, a.Path)
		}
	}
	return stale, nil
}

// Artefact is one complete generated file: its repository-relative path and
// the text the registry renders for it today.
type Artefact struct {
	Path string
	Body string
}

// SchemaArtefacts renders the author schema files: one per syntax profile
// up to maxProfile, and the combined one that dispatches on `dsl:`.
func SchemaArtefacts(maxProfile int) ([]Artefact, error) {
	profiles := SchemaProfiles(maxProfile)
	var out []Artefact
	for _, p := range profiles {
		body, err := RenderSchema(p)
		if err != nil {
			return nil, err
		}
		out = append(out, Artefact{SchemaFile(p), string(body)})
	}
	body, err := RenderCombinedSchema(profiles)
	if err != nil {
		return nil, err
	}
	return append(out, Artefact{CombinedSchemaFile, string(body)}), nil
}

// Reference renders every kind: a heading, its doc, where it lives, its
// table or its entry shape.
func Reference() string {
	var b strings.Builder
	b.WriteString("_Generated from the parser's property registry (`pkg/dsl/spec`) by `iterion dsl spec --write`; do not edit by hand. A conformance test holds the registry to the parser in both directions._\n\n")
	for _, k := range Kinds {
		fmt.Fprintf(&b, "### %s\n\n", k.Name)
		b.WriteString(k.Doc + "\n\n")
		if line := whereLine(k); line != "" {
			b.WriteString(line + "\n\n")
		}
		if k.Header != nil && len(k.Header.Fields) > 0 {
			b.WriteString("| Header part | Value | Meaning |\n|---|---|---|\n")
			for _, f := range k.Header.Fields {
				fmt.Fprintf(&b, "| `%s` | %s | %s%s |\n", f.Name, fieldValueCell(f), escapePipes(f.Doc), optionalNote(f))
			}
			b.WriteString("\n")
		}
		if len(k.Holds) > 0 {
			fmt.Fprintf(&b, "The body holds %s declarations and edge lines.\n\n", codes(k.Holds))
		}
		if k.Text {
			b.WriteString("The body is free text, one indented block.\n\n")
		}
		if len(k.Properties) > 0 {
			b.WriteString(Table(k))
			b.WriteString("\n")
		}
		if k.Entries != nil {
			b.WriteString(entriesParagraph(k.Entries))
		}
	}
	b.WriteString("### Deterministic public criteria\n\n")
	b.WriteString("A contract's `criteria:` name one of these evaluators by `kind:`; a kind this table does not have is declared and rendered but not evaluated (C303). Parameters are JSON data.\n\n")
	b.WriteString("| Criterion | Port types | Parameters | Meaning |\n|---|---|---|---|\n")
	for _, criterion := range PublicCriteria {
		var params []string
		for _, parameter := range criterion.Parameters {
			label := parameter.Name + ": " + string(parameter.Type)
			if parameter.Required {
				label += " (required)"
			}
			params = append(params, label)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", codes([]string{criterion.Name}), codes(criterion.Types), codes(params), criterion.Description)
	}
	b.WriteString("\n")
	return b.String()
}

// entriesParagraph renders what a block's author-named entries look like:
// the entry line's shape, its meaning, the sub-block an entry may open, and
// — when the line has several parts — a table of the parts.
func entriesParagraph(e *Entries) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Entries: `%s` — %s", EntryShape(e), e.Doc)
	if e.Body != "" {
		fmt.Fprintf(&b, "; an entry's sub-block is a [%s](#%s)", e.Body, anchor(e.Body))
	}
	b.WriteString(".\n\n")
	if len(e.Fields) > 1 {
		b.WriteString("| Entry part | Value | Meaning |\n|---|---|---|\n")
		for _, f := range e.Fields {
			fmt.Fprintf(&b, "| `%s` | %s | %s%s |\n", f.Name, fieldValueCell(f), escapePipes(f.Doc), optionalNote(f))
		}
		b.WriteString("\n")
	}
	if e.Entries != nil {
		b.WriteString("Each entry's indented lines: " + entriesParagraph(e.Entries))
	}
	return b.String()
}

// EntryShape renders the line one entry of a block is written on, from the
// entries' structure: the key, then each part in its own spelling, an
// optional part in brackets unless its spelling already opens with one (the
// `[enum: …]` constraint is written with brackets) — `<name>: string | bool
// | int [enum: "a", "b"] [= <default>]`.
func EntryShape(e *Entries) string {
	key := "<" + keyName(e) + ">"
	if e.Key == String {
		key = `"` + key + `"`
	}
	var b strings.Builder
	b.WriteString(key + ":")
	for _, f := range e.Fields {
		s := fieldSpelling(f)
		if !f.Required && !strings.HasPrefix(s, "[") {
			s = "[" + s + "]"
		}
		b.WriteString(" " + s)
	}
	if e.Entries != nil {
		b.WriteString(" (indented) " + EntryShape(e.Entries))
	}
	return b.String()
}

func keyName(e *Entries) string {
	if e.KeyName != "" {
		return e.KeyName
	}
	return "name"
}

// fieldSpelling is how a header part or an entry part is written on its
// line: its own Spelling when it has one, else a placeholder named after
// the part, quoted for a string, or the listed words of an enum.
func fieldSpelling(f Field) string {
	switch {
	case f.Spelling != "":
		return f.Spelling
	case f.Form == Enum && len(f.Values) > 0:
		return strings.Join(f.Values, " | ")
	case f.Form == String:
		return `"<` + f.Name + `>"`
	}
	return "<" + f.Name + ">"
}

func fieldValueCell(f Field) string {
	return valueCell(Property{Name: f.Name, Form: f.Form, Values: f.Values})
}

func optionalNote(f Field) string {
	if f.Required {
		return ""
	}
	return " — optional"
}

func whereLine(k Kind) string {
	switch k.Role {
	case Declaration:
		if k.Header != nil {
			return "A top-level declaration: `" + k.Header.Syntax + "`."
		}
		return "A top-level declaration: `" + k.Name + " <name>:`."
	case Node:
		return "A node: `" + k.Name + " <name>:` at the top level or inside a `group`."
	case BlockRole, Entry:
		hosts := make([]string, 0, len(k.Hosts))
		for _, h := range k.Hosts {
			if h == "file" {
				hosts = append(hosts, "the top level")
			} else {
				hosts = append(hosts, "`"+h+"`")
			}
		}
		what := "A block"
		if k.Role == Entry {
			what = "An entry"
		}
		return fmt.Sprintf("%s opened by `%s:` inside %s.", what, k.Opener, strings.Join(hosts, ", "))
	}
	return ""
}

// Table renders one kind's property table.
func Table(k Kind) string {
	var b strings.Builder
	b.WriteString("| Property | Value | Meaning |\n|---|---|---|\n")
	for _, p := range k.Properties {
		fmt.Fprintf(&b, "| `%s` | %s | %s%s |\n", p.Name, valueCell(p), escapePipes(p.Doc), profileNote(p))
	}
	return b.String()
}

// profileNote says which profiles accept a property the language added or
// removed, and which one is deprecated, so a reader of the table is not
// sent to write a line the file's profile refuses — or a line the language
// is leaving.
func profileNote(p Property) string {
	var notes []string
	switch {
	case p.Since > 0 && p.Until > 0:
		notes = append(notes, fmt.Sprintf("profiles %d to %d only", p.Since, p.Until))
	case p.Since > 0:
		notes = append(notes, fmt.Sprintf("since profile %d", p.Since))
	case p.Until > 0:
		notes = append(notes, fmt.Sprintf("profile %d only (removed in profile %d)", p.Until, p.Until+1))
	}
	if p.Deprecated {
		notes = append(notes, "deprecated")
	}
	if len(notes) == 0 {
		return ""
	}
	return " — " + strings.Join(notes, "; ")
}

func valueCell(p Property) string {
	switch p.Form {
	case WithMap:
		return "`" + string(p.Form) + "`"
	case Enum:
		return "one of " + codes(p.Values)
	case EnumOrEnv:
		return "one of " + codes(p.Values) + ", or a quoted `${VAR:-default}` string"
	case PromptRef:
		return "prompt name, or its text as a string"
	case Block:
		return fmt.Sprintf("block → [%s](#%s)", p.Body, anchor(p.Body))
	case BlockOrIdent:
		return fmt.Sprintf("one of %s, or a block → [%s](#%s)", codes(p.Values), p.Body, anchor(p.Body))
	case Ident, StringOrIdent, String, DottedIdent:
		if len(p.Values) > 0 {
			return escapePipes(string(p.Form)) + " — " + codes(p.Values)
		}
	case IdentList:
		if len(p.Values) > 0 {
			return string(p.Form) + " over " + codes(p.Values)
		}
	}
	return escapePipes(string(p.Form))
}

func codes(values []string) string {
	q := make([]string, 0, len(values))
	for _, v := range values {
		q = append(q, "`"+v+"`")
	}
	return strings.Join(q, ", ")
}

func escapePipes(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

// anchor is the GitHub-style heading anchor of a kind name.
func anchor(name string) string { return strings.ReplaceAll(name, ".", "") }

// SkillSection renders the compact property list the authoring skills carry:
// one line per kind, each property with a short form code. Kept terse on
// purpose — it is read by an agent before every draft, so every byte is paid
// on every draft.
func SkillSection() string {
	var b strings.Builder
	b.WriteString("Generated from the parser's property registry (`iterion dsl spec --write`). Forms: `str` quoted string or one bare word · `id` bare name · `id.id` bare name, dotted for a group instance's node or a node's field · `str|id` either · `str|num` a string or a bare number (`30s`, `3`) · `prompt` a prompt's name, or its text as a string · `int` `num` `bool` literals · `a|b` one of, bare or quoted · `a|b|\"${VAR}\"` one of, or a quoted env string · `\"a|b\"` one of, quoted · `[id]` `[str]` `[tool]` `[skill]` lists, inline `[a, b]` or one `- item` per indented line · `map` `{K: \"v\"}` or an indented block · `with{}` a `with { k: \"v\" }` map · `{kind}` an indented block described under that kind.\n\n")
	seen := map[string]bool{}
	for _, k := range Kinds {
		if seen[k.Name] {
			continue
		}
		label := "`" + k.Name + "`"
		if k.Name == "agent" {
			label = "`agent` / `judge`"
			seen["judge"] = true
		}
		where := ""
		if k.Role == BlockRole || k.Role == Entry {
			where = " (`" + k.Opener + ":` in " + strings.Join(hostNames(k), ", ") + ")"
		}
		fmt.Fprintf(&b, "- %s%s", label, where)
		if k.Header != nil {
			fmt.Fprintf(&b, " — `%s`", k.Header.Syntax)
		}
		if len(k.Holds) > 0 {
			fmt.Fprintf(&b, " — body: %s declarations and edges", strings.Join(k.Holds, "/"))
		}
		if k.Text {
			b.WriteString(" — body: free text")
		}
		if len(k.Properties) > 0 {
			parts := make([]string, 0, len(k.Properties))
			for _, p := range k.Properties {
				part := p.Name + " " + shortForm(p)
				if p.Since > 0 {
					part += fmt.Sprintf(" (profile ≥%d)", p.Since)
				}
				if p.Until > 0 {
					part += fmt.Sprintf(" (profile ≤%d)", p.Until)
				}
				if p.Deprecated {
					part += " (deprecated)"
				}
				parts = append(parts, part)
			}
			b.WriteString(" — " + strings.Join(parts, " · "))
		}
		if k.Entries != nil {
			fmt.Fprintf(&b, " — entries `%s`", EntryShape(k.Entries))
		}
		b.WriteString("\n")
	}
	b.WriteString("- Public criteria (deterministic; parameters are JSON data; an unregistered kind is declared, not evaluated): ")
	for i, criterion := range PublicCriteria {
		if i != 0 {
			b.WriteString(" · ")
		}
		fmt.Fprintf(&b, "`%s`", criterion.Name)
		for _, parameter := range criterion.Parameters {
			fmt.Fprintf(&b, " `%s:%s`", parameter.Name, parameter.Type)
		}
	}
	b.WriteString(".\n")
	return b.String()
}

func hostNames(k Kind) []string {
	out := make([]string, 0, len(k.Hosts))
	for _, h := range k.Hosts {
		if h == "file" {
			out = append(out, "the file")
		} else {
			out = append(out, h)
		}
	}
	return out
}

func shortForm(p Property) string {
	switch p.Form {
	case String:
		if len(p.Values) > 0 {
			return `"` + strings.Join(p.Values, "|") + `"`
		}
		return "str"
	case Ident:
		if len(p.Values) > 0 {
			return strings.Join(p.Values, "|")
		}
		return "id"
	case StringOrIdent:
		if len(p.Values) > 0 {
			return strings.Join(p.Values, "|")
		}
		return "str|id"
	case Int:
		return "int"
	case Number:
		return "num"
	case Bool:
		return "bool"
	case JSON:
		return "json"
	case Enum:
		return strings.Join(p.Values, "|")
	case EnumOrEnv:
		return strings.Join(p.Values, "|") + `|"${VAR}"`
	case PromptRef:
		return "prompt"
	case StringOrNumber:
		return "str|num"
	case DottedIdent:
		return "id.id"
	case IdentList:
		return "[id]"
	case StringList:
		return "[str]"
	case ToolList:
		return "[tool]"
	case SkillList:
		return "[skill]"
	case MixedList:
		return "[str|id]"
	case IdentOrList:
		return "id|[id]"
	case Map:
		return "map"
	case WithMap:
		return "with{}"
	case Block:
		return "{" + p.Body + "}"
	case BlockOrIdent:
		return strings.Join(p.Values, "|") + "|{" + p.Body + "}"
	}
	return string(p.Form)
}
