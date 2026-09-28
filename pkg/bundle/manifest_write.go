package bundle

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"

	// The loader (manifest.go) parses with go.yaml.in/yaml/v2, which
	// cannot round-trip comments or preserve key order on marshal. The
	// writer therefore parses with gopkg.in/yaml.v3 for the tree — the
	// position and the decoded value of every key — but edits the SOURCE
	// TEXT of the keys it touches, leaving every other byte of the file
	// alone: the comments, the blank lines between keys, the flow style of
	// a list, and the characters a re-encode would escape — a non-BMP icon
	// comes back from yaml.v3's emitter as "\U0001F9ED" (#1349), and a
	// blank line between two keys does not survive its node tree at all.
	// The two libraries coexist in this package; the rewritten bytes are
	// cross-validated through decodeManifest (v2 strict) before the file is
	// replaced, so a manifest can never land on disk in a form the loader
	// would reject.
	yamlv3 "gopkg.in/yaml.v3"
)

// ManifestPatch carries the user-editable subset of a Manifest for
// WriteManifest. A nil pointer field means "leave this key untouched"
// (preserving its existing value, comments, and YAML style); a non-nil
// pointer sets the key, where the empty string is a valid value that
// clears it while keeping the key present.
type ManifestPatch struct {
	Name        *string
	DisplayName *string
	// Icon sets the manifest's emoji identity; the empty string clears it
	// while keeping the key. Validated (trim + byte cap) by the
	// decodeManifest pass WriteManifest runs before committing.
	Icon        *string
	Version     *string
	Description *string
	Author      *string
	WhenToUse   *string
	Enabled     *bool
	// Triggers is nil for "no change"; a non-nil slice (even empty) sets
	// the manifest's triggers list. Note: when the bundle's main.bot
	// declares its own `## triggers:` frontmatter, discovery overlays it
	// over the manifest value (see botregistry.parseBundle).
	Triggers *[]string
	// Forge is nil for "no change"; a non-nil pointer rewrites the whole
	// `forge:` block (forge-access requirements). Reserved for a future
	// studio Integrations editor — the value is encoded with its yaml
	// tags and re-validated through decodeManifest before the file lands.
	Forge *ForgeRequirements
	// Requires is nil for "no change"; a non-nil pointer rewrites the whole
	// `requires:` block — the engine floor a bundle declares. A migration
	// to a newer syntax profile raises it to the build that reads the
	// profile, so an older runner refuses the bundle at admission instead
	// of at its first parse.
	Requires *Requires
}

// WriteManifest applies patch to the manifest.yaml at path, preserving
// everything the patch does not touch byte for byte: comments, blank lines
// between keys, key order, and the style each value was authored in.
//
//   - When path is missing or empty, a minimal manifest is scaffolded
//     (schema_version + the patched keys). This supports first-time
//     authoring; the discovery layer never feeds a non-bundle path here.
//   - Every nil patch field is left exactly as it was. Every non-nil field
//     overwrites the matching key's lines in place (a scalar value is
//     replaced inside its own line, the authored key spelling and a line
//     comment kept; a block value's whole span is replaced); a key that
//     does not yet exist is inserted after `description` (or appended) for
//     readability.
//   - The rewritten bytes are validated through LoadManifest before an
//     atomic temp+rename, so a structurally-broken or
//     schema-incompatible result aborts without clobbering the original.
//
// Returns the canonical, re-parsed Manifest on success.
func WriteManifest(path string, patch ManifestPatch) (*Manifest, error) {
	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("bundle: read manifest %s: %w", path, err)
	}

	var out []byte
	if len(bytes.TrimSpace(body)) > 0 {
		out, err = patchManifestText(body, patch)
	} else {
		out, err = scaffoldManifest(patch)
	}
	if err != nil {
		return nil, fmt.Errorf("bundle: parse manifest %s: %w", path, err)
	}

	// Validate the rewritten bytes through the SAME decoder LoadManifest
	// uses (strict v2 + schema + attachment safety) before committing, so a
	// structurally-broken or schema-incompatible result can never land on
	// disk. Then write durably via the shared atomic writer (temp + fsync +
	// rename + dir fsync).
	m, err := decodeManifest(out, path)
	if err != nil {
		return nil, fmt.Errorf("bundle: rewritten manifest invalid: %w", err)
	}
	if err := store.WriteFileAtomic(path, out, 0o644); err != nil {
		return nil, fmt.Errorf("bundle: write manifest %s: %w", path, err)
	}
	return m, nil
}

// manifestEdit is one replacement of the source lines [start, end)
// (0-based) by text; an insertion has start == end.
type manifestEdit struct {
	start, end int
	text       []string
}

// patchManifestText applies patch to the authored text of an existing
// manifest: the tree gives every key's position, and only the lines of a
// patched key are replaced.
func patchManifestText(body []byte, patch ManifestPatch) ([]byte, error) {
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yamlv3.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yamlv3.MappingNode {
		return nil, fmt.Errorf("the manifest is not a YAML mapping")
	}
	root := doc.Content[0]
	lines := strings.Split(string(body), "\n")

	var edits []manifestEdit
	// Guarantee a schema_version so the file stays loadable; an existing
	// key is left untouched, a missing one is appended — where the
	// pre-surgical writer put it.
	if _, ok := findMapKey(root, "schema_version"); !ok {
		line, err := manifestValueLines("schema_version", CurrentManifestSchema, false)
		if err != nil {
			return nil, err
		}
		edits = append(edits, manifestEdit{start: appendPos(lines), end: appendPos(lines), text: line})
	}

	apply := func(key string, value any, literal bool, afterKey string) error {
		text, err := manifestValueLines(key, value, literal)
		if err != nil {
			return err
		}
		if idx, ok := findMapKey(root, key); ok {
			k, v := root.Content[idx], root.Content[idx+1]
			edits = append(edits, replaceKeyLines(lines, key, k, v, text))
			return nil
		}
		at := appendPos(lines)
		if afterKey != "" {
			if idx, ok := findMapKey(root, afterKey); ok {
				at = entryEnd(lines, root.Content[idx], root.Content[idx+1])
			}
		}
		edits = append(edits, manifestEdit{start: at, end: at, text: text})
		return nil
	}

	if patch.Name != nil {
		if err := apply("name", *patch.Name, false, ""); err != nil {
			return nil, err
		}
	}
	if patch.DisplayName != nil {
		if err := apply("display_name", *patch.DisplayName, false, ""); err != nil {
			return nil, err
		}
	}
	if patch.Icon != nil {
		if err := apply("icon", *patch.Icon, false, "display_name"); err != nil {
			return nil, err
		}
	}
	if patch.Version != nil {
		if err := apply("version", *patch.Version, false, ""); err != nil {
			return nil, err
		}
	}
	if patch.Description != nil {
		if err := apply("description", *patch.Description, true, ""); err != nil {
			return nil, err
		}
	}
	if patch.Author != nil {
		if err := apply("author", *patch.Author, false, ""); err != nil {
			return nil, err
		}
	}
	if patch.WhenToUse != nil {
		if err := apply("when_to_use", *patch.WhenToUse, true, "description"); err != nil {
			return nil, err
		}
	}
	if patch.Enabled != nil {
		if err := apply("enabled", *patch.Enabled, false, "description"); err != nil {
			return nil, err
		}
	}
	if patch.Triggers != nil {
		if err := apply("triggers", *patch.Triggers, false, ""); err != nil {
			return nil, err
		}
	}
	if patch.Forge != nil {
		if err := apply("forge", *patch.Forge, false, ""); err != nil {
			return nil, err
		}
	}
	if patch.Requires != nil {
		if err := apply("requires", *patch.Requires, false, ""); err != nil {
			return nil, err
		}
	}

	return []byte(applyManifestEdits(lines, edits)), nil
}

// applyManifestEdits splices every edit into lines. Edits are computed
// against the ORIGINAL line numbering, so they apply bottom-up; two
// insertions at the same position land in reverse patch order — what the
// pre-surgical writer produced (each new key inserted directly after the
// anchor pushes the previously inserted one down).
func applyManifestEdits(lines []string, edits []manifestEdit) string {
	for i := 0; i < len(edits); i++ {
		for j := i + 1; j < len(edits); j++ {
			if edits[j].start > edits[i].start {
				edits[i], edits[j] = edits[j], edits[i]
			}
		}
	}
	for _, e := range edits {
		tail := append([]string{}, lines[e.end:]...)
		lines = append(append(lines[:e.start], e.text...), tail...)
	}
	return strings.Join(lines, "\n")
}

// appendPos is the line index an appended key goes at: before the empty
// element a trailing newline splits into existence, so the file keeps
// ending with exactly one.
func appendPos(lines []string) int {
	if n := len(lines); n > 0 && lines[n-1] == "" {
		return n - 1
	}
	return len(lines)
}

// replaceKeyLines is the edit for a key the file already holds. A scalar
// value on the key's own line is replaced INSIDE the line — the authored
// key spelling, its spacing and a line comment are kept; a value over
// several lines (a block scalar, a block mapping) is replaced over its
// whole span, the key line's comment carried onto the new head line.
func replaceKeyLines(lines []string, name string, key, val *yamlv3.Node, text []string) manifestEdit {
	start := key.Line - 1
	if val.Line == key.Line && val.Kind == yamlv3.ScalarNode {
		line := lines[start]
		head := line[:val.Column-1]
		if len(text) == 1 && !strings.Contains(text[0], "\n") {
			// The rendered form is `key: value`; take the value, keep the line.
			rendered := text[0][len(name)+2:]
			return manifestEdit{start: start, end: start + 1,
				text: []string{head + rendered + lineCommentSuffix(line[val.Column-1:], val.LineComment)}}
		}
		// A scalar that now wants a block form (a description that gained
		// lines): the whole line goes.
		text[0] = line[:key.Column-1] + text[0] + lineCommentSuffix(line[val.Column-1:], val.LineComment)
		return manifestEdit{start: start, end: start + 1, text: text}
	}
	end := entryEnd(lines, key, val)
	text[0] = lines[start][:key.Column-1] + text[0] + lineCommentSuffix(lines[start][key.Column-1:], val.LineComment)
	return manifestEdit{start: start, end: end, text: text}
}

// entryEnd is the 0-based line index just past a key's entry: its own line
// for a value written on it, else the first following line not indented
// under the key (blank lines between entries are separators, not the
// entry's — except the ones a `|+`/`>+` block keeps).
func entryEnd(lines []string, key, val *yamlv3.Node) int {
	if val.Line == key.Line && (val.Kind == yamlv3.ScalarNode || val.Style&yamlv3.FlowStyle != 0) {
		return key.Line
	}
	keyIndent := key.Column - 1
	end := key.Line // 0-based index of the line after the key's
	keepBlanks := false
	if val.Kind == yamlv3.ScalarNode {
		keepBlanks = strings.Contains(lines[key.Line-1], "|+") || strings.Contains(lines[key.Line-1], ">+")
	}
	for end < len(lines) {
		l := lines[end]
		if strings.TrimSpace(l) == "" {
			end++
			continue
		}
		if yamlIndent(l) <= keyIndent {
			break
		}
		end++
	}
	if !keepBlanks {
		for end > key.Line && strings.TrimSpace(lines[end-1]) == "" {
			end--
		}
	}
	return end
}

// lineCommentSuffix is the ` # …` suffix of a replaced line, kept verbatim
// off the source, separating spaces included; LineComment carries the `#`.
// A comment that cannot be found back on the line is re-appended rather
// than lost.
func lineCommentSuffix(rest, comment string) string {
	if comment == "" {
		return ""
	}
	if i := strings.LastIndex(rest, comment); i >= 0 {
		for i > 0 && rest[i-1] == ' ' {
			i--
		}
		return rest[i:]
	}
	return " " + comment
}

// yamlIndent is a line's leading-space count.
func yamlIndent(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

// manifestValueLines renders one `key: value` entry as source lines: a
// scalar inline (plain when YAML reads it back as itself, double-quoted
// otherwise — a non-BMP character written as the character, never a `\U`
// escape), a multi-line string the literal flag names as a `|` block when
// the form holds it exactly, a struct (requires, forge) through yaml.v3's
// own encoder, indented under the key.
func manifestValueLines(key string, value any, literal bool) ([]string, error) {
	switch v := value.(type) {
	case string:
		if literal && strings.Contains(v, "\n") {
			if lines, ok := manifestLiteralLines(key, v); ok {
				return lines, nil
			}
		}
		return []string{key + ": " + manifestScalar(v)}, nil
	case bool:
		return []string{fmt.Sprintf("%s: %t", key, v)}, nil
	case int:
		return []string{fmt.Sprintf("%s: %d", key, v)}, nil
	case []string:
		parts := make([]string, len(v))
		for i, s := range v {
			parts[i] = manifestScalar(s)
		}
		return []string{key + ": [" + strings.Join(parts, ", ") + "]"}, nil
	}
	// A struct with yaml tags (Requires, ForgeRequirements): yaml.v3's
	// encoder for the sub-tree, one level under the key.
	var n yamlv3.Node
	if err := n.Encode(value); err != nil {
		return nil, fmt.Errorf("bundle: encode field %q: %w", key, err)
	}
	doubleQuoteStrings(&n)
	sub := &yamlv3.Node{Kind: yamlv3.DocumentNode, Content: []*yamlv3.Node{&n}}
	var buf bytes.Buffer
	enc := yamlv3.NewEncoder(&buf)
	enc.SetIndent(2) // match the canonical 2-space style of shipped manifests
	if err := enc.Encode(sub); err != nil {
		return nil, fmt.Errorf("bundle: encode field %q: %w", key, err)
	}
	_ = enc.Close()
	out := []string{key + ":"}
	for _, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		out = append(out, "  "+l)
	}
	return out, nil
}

// doubleQuoteStrings restyles a single-quoted string scalar of an encoded
// sub-tree as double-quoted: the emitter's preference is single, the
// shipped manifests' convention is double (`iterion: ">= 3.141.0"`).
func doubleQuoteStrings(n *yamlv3.Node) {
	if n.Kind == yamlv3.ScalarNode && n.Tag == "!!str" && n.Style == yamlv3.SingleQuotedStyle {
		n.Style = yamlv3.DoubleQuotedStyle
	}
	for _, c := range n.Content {
		doubleQuoteStrings(c)
	}
}

// manifestScalar renders a string as an inline YAML scalar: plain when a
// probe through yaml.v3 reads the bare spelling back as exactly the value
// (which refuses the reserved words, the indicator characters, the `: ` and
// ` #` traps, and the leading/trailing spaces, all at once), double-quoted
// with the standard escapes otherwise. A printable non-ASCII character —
// the catalogue's emoji icons — is written as itself (#1349).
func manifestScalar(v string) string {
	if manifestPlainSafe(v) {
		return v
	}
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range v {
		switch r {
		case '\\':
			sb.WriteString(`\\`)
		case '"':
			sb.WriteString(`\"`)
		case '\n':
			sb.WriteString(`\n`)
		case '\t':
			sb.WriteString(`\t`)
		case '\r':
			sb.WriteString(`\r`)
		case 0:
			sb.WriteString(`\0`)
		default:
			switch {
			case r < 0x20 || r == 0x7F:
				fmt.Fprintf(&sb, `\x%02X`, r)
			case r == 0x85 || r == 0x2028 || r == 0x2029:
				// A scanner break is never written raw: the document would
				// end a line at it (E055's class).
				fmt.Fprintf(&sb, `\u%04X`, r)
			default:
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// manifestPlainSafe reports whether v, written bare after `key: `, reads
// back as exactly v — probed through yaml.v3 itself, so the rule cannot
// drift from the reader's.
func manifestPlainSafe(v string) bool {
	if v == "" {
		return false
	}
	var doc yamlv3.Node
	if yamlv3.Unmarshal([]byte("k: "+v+"\n"), &doc) != nil {
		return false
	}
	if doc.Kind != yamlv3.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yamlv3.MappingNode {
		return false
	}
	root := doc.Content[0]
	idx, ok := findMapKey(root, "k")
	if !ok {
		return false
	}
	val := root.Content[idx+1]
	return val.Tag == "!!str" && val.Value == v
}

// manifestLiteralLines renders a multi-line string as a `|` block under its
// key, with the chomping indicator the value's trailing newlines call for.
// ok is false when the form would not read back as exactly the value — a
// whitespace-only line (one reads as a blank line, its spaces lost), a
// first content line indented deeper than a later one (the strip level the
// first line sets would end the block early) — and the caller falls back
// to the double-quoted form, which always reads back exactly.
func manifestLiteralLines(key string, v string) ([]string, bool) {
	lines := strings.Split(v, "\n")
	indicator := "|"
	switch {
	case strings.HasSuffix(v, "\n\n"):
		indicator = "|+"
	case !strings.HasSuffix(v, "\n"):
		indicator = "|-"
	default:
		lines = lines[:len(lines)-1] // the clip newline is the form's own
	}
	body := lines
	first := -1
	for i, l := range body {
		if l == "" {
			continue
		}
		if strings.Trim(l, " \t") == "" {
			return nil, false
		}
		if first < 0 {
			first = i
			continue
		}
		if yamlIndent(l) < yamlIndent(body[first]) {
			return nil, false
		}
	}
	out := []string{key + ": " + indicator}
	for _, l := range body {
		if l == "" {
			out = append(out, "")
			continue
		}
		out = append(out, "  "+l)
	}
	return out, true
}

// scaffoldManifest is the manifest of a missing or empty file: no authored
// text to keep, so the keys are simply written in the patch's order —
// schema_version first, as the pre-surgical writer produced.
func scaffoldManifest(patch ManifestPatch) ([]byte, error) {
	lines := []string{}
	line, err := manifestValueLines("schema_version", CurrentManifestSchema, false)
	if err != nil {
		return nil, err
	}
	lines = append(lines, line...)
	add := func(key string, value any, literal bool) error {
		text, err := manifestValueLines(key, value, literal)
		if err != nil {
			return err
		}
		lines = append(lines, text...)
		return nil
	}
	if patch.Name != nil {
		if err := add("name", *patch.Name, false); err != nil {
			return nil, err
		}
	}
	if patch.DisplayName != nil {
		if err := add("display_name", *patch.DisplayName, false); err != nil {
			return nil, err
		}
	}
	if patch.Icon != nil {
		if err := add("icon", *patch.Icon, false); err != nil {
			return nil, err
		}
	}
	if patch.Version != nil {
		if err := add("version", *patch.Version, false); err != nil {
			return nil, err
		}
	}
	if patch.Description != nil {
		if err := add("description", *patch.Description, true); err != nil {
			return nil, err
		}
	}
	if patch.Author != nil {
		if err := add("author", *patch.Author, false); err != nil {
			return nil, err
		}
	}
	if patch.WhenToUse != nil {
		if err := add("when_to_use", *patch.WhenToUse, true); err != nil {
			return nil, err
		}
	}
	if patch.Enabled != nil {
		if err := add("enabled", *patch.Enabled, false); err != nil {
			return nil, err
		}
	}
	if patch.Triggers != nil {
		if err := add("triggers", *patch.Triggers, false); err != nil {
			return nil, err
		}
	}
	if patch.Forge != nil {
		if err := add("forge", *patch.Forge, false); err != nil {
			return nil, err
		}
	}
	if patch.Requires != nil {
		if err := add("requires", *patch.Requires, false); err != nil {
			return nil, err
		}
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

// findMapKey returns the index of key's scalar node within a mapping's
// flat [key, value, key, value, …] content slice.
func findMapKey(m *yamlv3.Node, key string) (int, bool) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i, true
		}
	}
	return -1, false
}
