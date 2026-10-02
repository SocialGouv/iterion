package bundle

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"

	yamlv2 "go.yaml.in/yaml/v2"

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

	// The icon is stored in its loader-normalized form: decodeManifest
	// trims it on every read (manifest.go), so rendering the trimmed value
	// is a fixed point where the padded one would read back as another
	// string (round 2).
	if patch.Icon != nil {
		trimmed := strings.TrimSpace(*patch.Icon)
		patch.Icon = &trimmed
	}

	var out []byte
	var beforeKeys map[string]bool
	if len(bytes.TrimSpace(body)) > 0 {
		out, beforeKeys, err = patchManifestText(body, patch)
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
	// Decoding cleanly is not enough: the bytes must READ BACK as the patch
	// (the round-1 lesson — a block-valued key's old body survived the
	// splice and decoded INTO the new value, valid YAML and all). The gate
	// turns any span misread into a loud refusal, never a corrupted file.
	if err := manifestPatchReadBack(m, patch); err != nil {
		return nil, fmt.Errorf("bundle: rewritten manifest does not read back the patch: %w", err)
	}
	// And the patched fields are not the whole story: a patch ADDS and
	// REPLACES keys, it never deletes one. Comparing only the patched
	// fields let a flow-collection span running to the file's end wipe
	// every key below it in silence (revi round); the whole document's key
	// set is the gate that says it.
	if beforeKeys != nil {
		afterKeys, err := manifestTopKeys(out)
		if err != nil {
			return nil, fmt.Errorf("bundle: rewritten manifest does not re-parse: %w", err)
		}
		for k := range beforeKeys {
			if !afterKeys[k] {
				return nil, fmt.Errorf("bundle: rewritten manifest lost top-level key %q", k)
			}
		}
	}
	if err := store.WriteFileAtomic(path, out, 0o644); err != nil {
		return nil, fmt.Errorf("bundle: write manifest %s: %w", path, err)
	}
	return m, nil
}

// manifestTopKeys is the set of a manifest's top-level keys.
func manifestTopKeys(body []byte) (map[string]bool, error) {
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	if doc.Kind == yamlv3.DocumentNode && len(doc.Content) > 0 && doc.Content[0].Kind == yamlv3.MappingNode {
		root := doc.Content[0]
		for i := 0; i+1 < len(root.Content); i += 2 {
			out[root.Content[i].Value] = true
		}
	}
	return out, nil
}

// manifestPatchReadBack is the read-back gate: every field the patch sets
// must decode as exactly the patched value.
func manifestPatchReadBack(m *Manifest, patch ManifestPatch) error {
	checks := []struct {
		name string
		want *string
	}{
		{"name", patch.Name},
		{"display_name", patch.DisplayName},
		{"icon", patch.Icon},
		{"version", patch.Version},
		{"description", patch.Description},
		{"author", patch.Author},
		{"when_to_use", patch.WhenToUse},
	}
	got := map[string]*string{
		"name": &m.Name, "display_name": &m.DisplayName, "icon": &m.Icon,
		"version": &m.Version, "description": &m.Description, "author": &m.Author,
		"when_to_use": &m.WhenToUse,
	}
	for _, c := range checks {
		if c.want == nil {
			continue
		}
		if *got[c.name] != *c.want {
			return fmt.Errorf("%s reads %q, patched %q", c.name, *got[c.name], *c.want)
		}
	}
	if patch.Enabled != nil {
		if m.Enabled == nil || *m.Enabled != *patch.Enabled {
			return fmt.Errorf("enabled reads %v, patched %v", m.Enabled, *patch.Enabled)
		}
	}
	if patch.Triggers != nil && !slices.Equal(m.Triggers, *patch.Triggers) {
		return fmt.Errorf("triggers read %v, patched %v", m.Triggers, *patch.Triggers)
	}
	if patch.Requires != nil {
		if m.Requires == nil || m.Requires.Iterion != patch.Requires.Iterion {
			return fmt.Errorf("requires read %+v, patched %+v", m.Requires, patch.Requires)
		}
	}
	if patch.Forge != nil && !reflect.DeepEqual(m.Forge, patch.Forge) {
		return fmt.Errorf("forge read %+v, patched %+v", m.Forge, patch.Forge)
	}
	return nil
}

// manifestEdit is one replacement of the source lines [start, end)
// (0-based) by text; an insertion has start == end.
type manifestEdit struct {
	start, end int
	text       []string
}

// patchManifestText applies patch to the authored text of an existing
// manifest: the tree gives every key's position, and only the lines of a
// patched key are replaced. The second return is the document's top-level
// key set before the patch (the key-set gate's "before").
func patchManifestText(body []byte, patch ManifestPatch) ([]byte, map[string]bool, error) {
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(body, &doc); err != nil {
		return nil, nil, err
	}
	if doc.Kind != yamlv3.DocumentNode || len(doc.Content) == 0 {
		// A comments-only document is an empty manifest with notes above
		// it: the scaffold path, the notes kept (the pre-surgical writer
		// scaffolded too, and dropped them). A document that is a list or
		// a scalar is no manifest at all and stays refused below.
		out, err := scaffoldManifest(patch)
		if err != nil {
			return nil, nil, err
		}
		text := strings.TrimRight(string(body), "\n") + "\n" + string(out)
		return []byte(text), map[string]bool{}, nil
	}
	if doc.Content[0].Kind != yamlv3.MappingNode {
		return nil, nil, fmt.Errorf("the manifest is not a YAML mapping")
	}
	root := doc.Content[0]
	lines := strings.Split(string(body), "\n")
	beforeKeys, err := manifestTopKeys(body)
	if err != nil {
		return nil, nil, err
	}

	var edits []manifestEdit
	// Guarantee a schema_version so the file stays loadable; an existing
	// key is left untouched, a missing one is appended — where the
	// pre-surgical writer put it.
	if _, ok := findMapKey(root, "schema_version"); !ok {
		line, err := manifestValueLines("schema_version", CurrentManifestSchema, false)
		if err != nil {
			return nil, nil, err
		}
		edits = append(edits, manifestEdit{start: appendPos(lines), end: appendPos(lines), text: line})
	}

	// A patch on a manifest holding anchors or aliases is refused outright:
	// the tree parse RESOLVES them, so the text span an anchored value came
	// from is not the value's — splicing there would rewrite the anchor's
	// body as if it were the alias's. The pre-surgical writer silently
	// unrolled them, which was no better; a loud refusal it is (round 1).
	// The gate is the PATCH alone: a no-op patch has nothing to splice, and
	// the schema_version guarantee is a pure append at the file's end,
	// which touches no span and is anchor-safe by construction (round 2).
	if !manifestPatchEmpty(patch) && treeHoldsAnchor(root) {
		return nil, nil, fmt.Errorf("the manifest holds YAML anchors or aliases: edit it by hand, the patch writer cannot follow them")
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
			return nil, nil, err
		}
	}
	if patch.DisplayName != nil {
		if err := apply("display_name", *patch.DisplayName, false, ""); err != nil {
			return nil, nil, err
		}
	}
	if patch.Icon != nil {
		if err := apply("icon", *patch.Icon, false, "display_name"); err != nil {
			return nil, nil, err
		}
	}
	if patch.Version != nil {
		if err := apply("version", *patch.Version, false, ""); err != nil {
			return nil, nil, err
		}
	}
	if patch.Description != nil {
		if err := apply("description", *patch.Description, true, ""); err != nil {
			return nil, nil, err
		}
	}
	if patch.Author != nil {
		if err := apply("author", *patch.Author, false, ""); err != nil {
			return nil, nil, err
		}
	}
	if patch.WhenToUse != nil {
		if err := apply("when_to_use", *patch.WhenToUse, true, "description"); err != nil {
			return nil, nil, err
		}
	}
	if patch.Enabled != nil {
		if err := apply("enabled", *patch.Enabled, false, "description"); err != nil {
			return nil, nil, err
		}
	}
	if patch.Triggers != nil {
		if err := apply("triggers", *patch.Triggers, false, ""); err != nil {
			return nil, nil, err
		}
	}
	if patch.Forge != nil {
		if err := apply("forge", *patch.Forge, false, ""); err != nil {
			return nil, nil, err
		}
	}
	if patch.Requires != nil {
		if err := apply("requires", *patch.Requires, false, ""); err != nil {
			return nil, nil, err
		}
	}

	return []byte(applyManifestEdits(lines, edits)), beforeKeys, nil
}

// applyManifestEdits splices every edit into lines. Edits are computed
// against the ORIGINAL line numbering, so they apply bottom-up — and the
// ordering must be exact, not just deterministic: when an INSERTION's
// position coincides with a REPLACEMENT's span start (a key inserted after
// `description` lands on the line of the entry that follows it, and that
// entry is itself patched — the studio's every-autosave shape), the
// replacement must splice first, or it overwrites the inserted lines
// (duplicated key, absorbed body — the revi gate's corpus probe). The sort
// is therefore by start DESCENDING with the longer span first on a tie:
// an insertion has start == end, so any coincident replacement wins.
// Two insertions at the same position keep landing in reverse patch
// order, as the pre-surgical writer produced.
func applyManifestEdits(lines []string, edits []manifestEdit) string {
	sort.SliceStable(edits, func(i, j int) bool {
		if edits[i].start != edits[j].start {
			return edits[i].start > edits[j].start
		}
		return edits[i].end > edits[j].end
	})
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
// value written wholly on the key's own line is replaced INSIDE the line —
// the authored key spelling, its spacing and a line comment are kept;
// anything else (a block scalar — whose value node ALSO points at the
// key's line, at the `|` — a quoted scalar spanning lines, a flow
// collection, a block mapping) is replaced over its whole span, the key
// line's comment carried onto the new head line. The fast path used to
// trust `val.Line == key.Line` alone, and a block-valued key kept its old
// body on disk, absorbed into the new value (#1349, round 1).
func replaceKeyLines(lines []string, name string, key, val *yamlv3.Node, text []string) manifestEdit {
	start := key.Line - 1
	end := entryEnd(lines, key, val)
	comment := val.LineComment
	if comment == "" {
		comment = key.LineComment // a block scalar's `| # note` rides the key
	}
	if end == key.Line && val.Tag != "!!null" && len(text) == 1 && !strings.Contains(text[0], "\n") {
		// (An implicit null — `author:` with nothing after the colon — has
		// no value text on the line to keep the line around: the in-line
		// fast path would write `author:someone` with no space, which no
		// YAML reads. The span path below renders the line whole.)
		line := lines[start]
		head := line[:val.Column-1]
		// The rendered form is `key: value`; take the value, keep the line.
		rendered := text[0][len(name)+2:]
		return manifestEdit{start: start, end: start + 1,
			text: []string{head + rendered + lineCommentSuffix(line[val.Column-1:], comment)}}
	}
	text[0] = lines[start][:key.Column-1] + text[0] + lineCommentSuffix(lines[start][key.Column-1+len(name):], comment)
	if val.Kind != yamlv3.ScalarNode {
		// The entry's rationale: comment (and blank) lines sitting between
		// the key line and the block's first child are carried into the new
		// block verbatim — dropping them would lose the note, leaving them
		// out of the span would keep the old child beside the new one
		// (revi round, the shipped `requires:` convention).
		var lead []string
		for i := start + 1; i < end; i++ {
			t := strings.TrimSpace(lines[i])
			if t != "" && !strings.HasPrefix(t, "#") {
				break
			}
			lead = append(lead, lines[i])
		}
		if len(lead) > 0 {
			tail := append([]string{}, text[1:]...)
			text = append(append(text[:1], lead...), tail...)
		}
	}
	return manifestEdit{start: start, end: end, text: text}
}

// entryEnd is the 0-based line index just past a key's entry: its own line
// for a single-line value, the closing quote's for a quoted scalar spanning
// lines (whose continuation owes no indentation), the line its brackets
// balance on for a flow collection, else the first following line not
// indented under the key — with two refinements: a comment line is a block
// SCALAR's content but never a mapping's or sequence's, so it ends those
// spans (a foot note under a replaced `requires:` block stays on disk), and
// blank lines between entries are separators, not the entry's — except the
// ones a `|+`/`>+` block keeps.
func entryEnd(lines []string, key, val *yamlv3.Node) int {
	keyIndent := key.Column - 1
	if val.Kind == yamlv3.ScalarNode {
		switch {
		case val.Style&(yamlv3.LiteralStyle|yamlv3.FoldedStyle) != 0:
			// A block scalar: the walk below, where `#` is content.
		case val.Style&(yamlv3.DoubleQuotedStyle|yamlv3.SingleQuotedStyle) != 0:
			return quotedSpanEnd(lines, val.Line-1, val.Column, val.Style) + 1
		default:
			// A plain scalar: a deeper-indented non-blank, non-comment line
			// below it is its folded continuation, not the next entry.
			if key.Line < len(lines) {
				next := lines[key.Line]
				if t := strings.TrimSpace(next); t != "" && !strings.HasPrefix(t, "#") && yamlIndent(next) > keyIndent {
					break // to the walk
				}
			}
			return key.Line
		}
	} else if val.Style&yamlv3.FlowStyle != 0 {
		return flowSpanEnd(lines, val.Line-1, val.Column-1)
	}
	blockScalar := val.Kind == yamlv3.ScalarNode && val.Style&(yamlv3.LiteralStyle|yamlv3.FoldedStyle) != 0
	end := key.Line // 0-based index of the line after the key's
	keepBlanks := blockScalar && (strings.Contains(lines[key.Line-1], "|+") || strings.Contains(lines[key.Line-1], ">+"))
	seenContent := false
	for end < len(lines) {
		l := lines[end]
		t := strings.TrimSpace(l)
		if t == "" {
			end++
			continue
		}
		if yamlIndent(l) <= keyIndent {
			break
		}
		if !blockScalar && strings.HasPrefix(t, "#") {
			if seenContent {
				// A comment is content only of a block scalar: past the
				// block's first child it is not the span's to take, and it
				// stays, below the replaced span. (An INTERIOR comment
				// between children leaves the old tail on disk — the
				// read-back gate refuses that loudly rather than delete
				// the comment in silence.)
				break
			}
			// A comment between the key line and its first child is the
			// ENTRY's rationale (the shipped `requires:` convention): it is
			// the span's, carried into the new block by replaceKeyLines —
			// left out, the old child survived beside the new one and the
			// strict decoder refused the duplicate (revi round).
			end++
			continue
		}
		seenContent = true
		end++
	}
	if !keepBlanks {
		for end > key.Line && strings.TrimSpace(lines[end-1]) == "" {
			end--
		}
	}
	return end
}

// quotedSpanEnd is the 0-based line a quoted scalar ends on: the line of
// its closing quote. The scan opens at col (1-based, on the quote itself)
// and honors the style's escapes — `\\` and `\"` in double quotes, `”` in
// single — so a quote inside the value does not close it. An unterminated
// scalar runs to the last line; the read-back gate refuses the result.
func quotedSpanEnd(lines []string, lineIdx, col int, style yamlv3.Style) int {
	quote := byte('"')
	if style&yamlv3.SingleQuotedStyle != 0 {
		quote = '\''
	}
	for i := lineIdx; i < len(lines); i++ {
		s := lines[i]
		j := 0
		if i == lineIdx {
			j = col
		}
		for j < len(s) {
			c := s[j]
			if quote == '"' && c == '\\' {
				j += 2
				continue
			}
			if c == quote {
				if quote == '\'' && j+1 < len(s) && s[j+1] == '\'' {
					j += 2
					continue
				}
				return i
			}
			j++
		}
	}
	return len(lines) - 1
}

// flowSpanEnd is the 0-based line index just past a flow collection: the
// line its brackets balance on. A quote OPENS a scalar only where a scalar
// can start — after a flow separator (`[`, `{`, `,`, `:`), whitespace, or
// at the scan's start: an apostrophe inside a plain flow item (`don't`,
// `what's-up`) is content, not an opening quote — treating it as one ran
// the span to the end of the file and the patch deleted every key below
// the collection in silence (revi round). Quotes inside a quoted scalar
// are skipped by the style's escapes.
func flowSpanEnd(lines []string, lineIdx, col int) int {
	depth := 0
	var quote byte
	prev := byte(0) // the character before the current one, outside quotes
	for i := lineIdx; i < len(lines); i++ {
		s := lines[i]
		j := 0
		if i == lineIdx {
			j = col - 1 // col is 1-based, on the opening bracket
		}
		for ; j < len(s); j++ {
			c := s[j]
			if quote != 0 {
				if quote == '"' && c == '\\' {
					j++
					continue
				}
				if c == quote {
					quote = 0
				}
				prev = c
				continue
			}
			switch c {
			case '"', '\'':
				if prev == 0 || prev == '[' || prev == '{' || prev == ',' || prev == ':' || prev == ' ' || prev == '\t' {
					quote = c
				}
			case '[', '{':
				depth++
			case ']', '}':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			prev = c
		}
	}
	// Accepted miss direction: an unbalanced quote inside a flow COMMENT or
	// a genuinely quoted-but-unterminated plain item runs the span to the
	// file's end — and the whole-document key-set gate then refuses LOUDLY
	// with the original file intact, never a silent write.
	return len(lines) // unbalanced: the read-back gate refuses the result
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
// back as exactly v — probed through BOTH readers the file has: yaml.v3
// (YAML 1.2 core schema, what the writer's own tree parse uses) and
// go.yaml.in/yaml/v2 (YAML 1.1, the loader's). The v2 pass is what refuses
// the 1.1 spellings v3 reads back equal — `yes`/`no`/`on`/`off` are strings
// to v3 and bools to the loader — so a plain form that would fool the gate
// is never written (round 1). The probe is the reader itself, not a
// denylist: it cannot drift from the reader, and it covers the whole class
// (hex, octal, exponents, .inf) rather than the two spellings someone
// listed. (`1:20` reads as a string in BOTH — the v2 fork has no
// sexagesimal — so it may stay plain.)
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
	if val.Tag != "!!str" || val.Value != v {
		return false
	}
	var m2 map[string]any
	if err := yamlv2.Unmarshal([]byte("k: "+v+"\n"), &m2); err != nil {
		return false
	}
	got, ok := m2["k"]
	return ok && got == v
}

// manifestLiteralLines renders a multi-line string as a `|` block under its
// key, with the chomping indicator the value's trailing newlines call for
// (`|` clips to one, `|-` strips, `|+` keeps). ok is false when the form
// would not read back as exactly the value — a whitespace-only line (one
// reads as a blank line, its spaces lost), a first content line indented
// deeper than a later one (the strip level the first line sets would end
// the block early), or no content line at all (an all-newline value reads
// back empty) — and the caller falls back to the double-quoted form, which
// always reads back exactly. The strings.Split artifact (the element after
// the last newline) is dropped in EVERY branch: kept under `|+`, it grew
// the value by one newline on every patch (round 1).
func manifestLiteralLines(key string, v string) ([]string, bool) {
	lines := strings.Split(v, "\n")
	var indicator string
	var body []string
	switch {
	case strings.HasSuffix(v, "\n\n"):
		indicator = "|+"
		body = lines[:len(lines)-1]
	case !strings.HasSuffix(v, "\n"):
		indicator = "|-"
		body = lines
	default:
		indicator = "|"
		body = lines[:len(lines)-1] // the clip newline is the form's own
	}
	content := -1
	for i, l := range body {
		if l == "" {
			continue
		}
		if strings.Trim(l, " \t") == "" {
			return nil, false
		}
		if content < 0 {
			content = i
			continue
		}
		if yamlIndent(l) < yamlIndent(body[content]) {
			return nil, false
		}
	}
	if content < 0 {
		return nil, false // no content line: the block reads back empty
	}
	if yamlIndent(body[content]) != 0 {
		// The block's indentation is auto-detected from the first content
		// line's TOTAL indent — the writer's plus the value's own: a first
		// line with its own leading spaces reads every line back de-dented
		// (the same hole the .bot writer had). The double-quoted form
		// carries the value exactly, where the read-back gate would only
		// refuse the save (#1349, round 2).
		return nil, false
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

// manifestPatchEmpty reports whether the patch sets nothing.
func manifestPatchEmpty(p ManifestPatch) bool {
	return p.Name == nil && p.DisplayName == nil && p.Icon == nil && p.Version == nil &&
		p.Description == nil && p.Author == nil && p.WhenToUse == nil && p.Enabled == nil &&
		p.Triggers == nil && p.Forge == nil && p.Requires == nil
}

// treeHoldsAnchor reports whether any node of the tree carries an anchor or
// is an alias — the two shapes a text-splicing patch cannot follow.
func treeHoldsAnchor(n *yamlv3.Node) bool {
	if n.Anchor != "" || n.Kind == yamlv3.AliasNode {
		return true
	}
	for _, c := range n.Content {
		if treeHoldsAnchor(c) {
			return true
		}
	}
	return false
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
