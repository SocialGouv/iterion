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
// table, `skill` the compact section the authoring skills carry. `iterion
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
		return "", fmt.Errorf("dsl-spec regions are unbalanced or unrecognised: %d matched, %d begin marker(s), %d end marker(s) — each `%s<what> -->` needs its own `%s` on a later line, LF line ends, and what must be reference, skill or table <kind>", n, begins, ends, beginPrefix, endMarker)
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

// Render produces the body of one region kind: "reference", "table <kind>"
// or "skill".
func Render(what string) (string, error) {
	switch {
	case what == "reference":
		return Reference(), nil
	case what == "skill":
		return SkillSection(), nil
	case strings.HasPrefix(what, "table "):
		kind := strings.TrimPrefix(what, "table ")
		k, ok := Lookup(kind)
		if !ok {
			return "", fmt.Errorf("dsl-spec region: unknown kind %q", kind)
		}
		return Table(k), nil
	}
	return "", fmt.Errorf("dsl-spec region: unknown region %q", what)
}

// Regenerate rewrites every generated region of the documents under root and
// returns the files it changed. Every document is read and spliced before
// any is written, so a document that cannot be regenerated leaves the tree
// as it was rather than half rewritten.
func Regenerate(root string) ([]string, error) {
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
	var changed []string
	for _, p := range todo {
		if err := os.WriteFile(p.path, []byte(p.body), 0o644); err != nil {
			return changed, err
		}
		changed = append(changed, p.rel)
	}
	return changed, nil
}

// Stale returns the files under root whose generated regions differ from
// what the registry renders.
func Stale(root string) ([]string, error) {
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
	return stale, nil
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
		if len(k.Properties) > 0 {
			b.WriteString(Table(k))
			b.WriteString("\n")
		}
		if k.Entries != nil {
			fmt.Fprintf(&b, "Entries: `%s` — %s", k.Entries.Shape, k.Entries.Doc)
			if k.Entries.Body != "" {
				fmt.Fprintf(&b, "; an entry's sub-block is a [%s](#%s)", k.Entries.Body, anchor(k.Entries.Body))
			}
			b.WriteString(".\n\n")
		}
	}
	return b.String()
}

func whereLine(k Kind) string {
	switch k.Role {
	case Declaration:
		if k.Header != "" {
			return "A top-level declaration: `" + k.Header + "`."
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

// profileNote says which profiles still accept a property the language
// removed, so a reader of the table is not sent to write a line profile 2
// refuses.
func profileNote(p Property) string {
	if p.Until == 0 {
		return ""
	}
	return fmt.Sprintf(" — profile %d only (removed in profile %d)", p.Until, p.Until+1)
}

func valueCell(p Property) string {
	switch p.Form {
	case WithMap:
		return "`" + string(p.Form) + "`"
	case Enum:
		return "one of " + codes(p.Values)
	case Block:
		return fmt.Sprintf("block → [%s](#%s)", p.Body, anchor(p.Body))
	case BlockOrIdent:
		return fmt.Sprintf("one of %s, or a block → [%s](#%s)", codes(p.Values), p.Body, anchor(p.Body))
	case Ident, StringOrIdent, String:
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
	b.WriteString("Generated from the parser's property registry (`iterion dsl spec --write`). Forms: `str` quoted string · `id` bare name · `str|id` either · `int` `num` `bool` literals · `a|b` one of · `\"a|b\"` one of, quoted · `[id]` `[str]` `[tool]` `[skill]` inline lists · `map` `{K: \"v\"}` or an indented block · `with{}` a `with { k: \"v\" }` map · `{kind}` an indented block described under that kind.\n\n")
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
		if len(k.Properties) > 0 {
			parts := make([]string, 0, len(k.Properties))
			for _, p := range k.Properties {
				part := p.Name + " " + shortForm(p)
				if p.Until > 0 {
					part += fmt.Sprintf(" (profile ≤%d)", p.Until)
				}
				parts = append(parts, part)
			}
			b.WriteString(" — " + strings.Join(parts, " · "))
		}
		if k.Entries != nil {
			fmt.Fprintf(&b, " — entries `%s`", k.Entries.Shape)
		}
		b.WriteString("\n")
	}
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
	case Enum:
		return strings.Join(p.Values, "|")
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
