package bundle

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"

	// The loader (manifest.go) parses with go.yaml.in/yaml/v2, which
	// cannot round-trip comments or preserve key order on marshal. The
	// writer therefore uses gopkg.in/yaml.v3's Node API to edit only the
	// touched keys in place, leaving every hand-authored comment and the
	// original block/flow style of unrelated keys intact. The two
	// libraries coexist in this package; the rewritten bytes are
	// cross-validated through decodeManifest (v2 strict) before the file is
	// replaced, so a v3-emitted manifest can never land on disk in a form
	// the loader would reject.
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
}

// WriteManifest applies patch to the manifest.yaml at path, preserving
// comments, key order, and the original block/flow style of keys it does
// not touch.
//
//   - When path is missing or empty, a minimal manifest is scaffolded
//     (schema_version + the patched keys). This supports first-time
//     authoring; the discovery layer never feeds a non-bundle path here.
//   - Every nil patch field is left exactly as it was. Every non-nil
//     field overwrites the matching key in place (carrying over any
//     comments attached to the old value); a key that does not yet
//     exist is inserted after `description` (or appended) for
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

	var doc yamlv3.Node
	if len(bytes.TrimSpace(body)) > 0 {
		if err := yamlv3.Unmarshal(body, &doc); err != nil {
			return nil, fmt.Errorf("bundle: parse manifest %s: %w", path, err)
		}
	}
	root := documentMapping(&doc)

	// Guarantee a schema_version so the scaffold path produces a loadable
	// file; an existing key is left untouched.
	if _, ok := findMapKey(root, "schema_version"); !ok {
		if err := setMapField(root, "schema_version", CurrentManifestSchema, false, ""); err != nil {
			return nil, err
		}
	}

	if patch.Name != nil {
		if err := setMapField(root, "name", *patch.Name, false, ""); err != nil {
			return nil, err
		}
	}
	if patch.DisplayName != nil {
		if err := setMapField(root, "display_name", *patch.DisplayName, false, ""); err != nil {
			return nil, err
		}
	}
	if patch.Icon != nil {
		if err := setMapField(root, "icon", *patch.Icon, false, "display_name"); err != nil {
			return nil, err
		}
	}
	if patch.Version != nil {
		if err := setMapField(root, "version", *patch.Version, false, ""); err != nil {
			return nil, err
		}
	}
	if patch.Description != nil {
		if err := setMapField(root, "description", *patch.Description, true, ""); err != nil {
			return nil, err
		}
	}
	if patch.Author != nil {
		if err := setMapField(root, "author", *patch.Author, false, ""); err != nil {
			return nil, err
		}
	}
	if patch.WhenToUse != nil {
		if err := setMapField(root, "when_to_use", *patch.WhenToUse, true, "description"); err != nil {
			return nil, err
		}
	}
	if patch.Enabled != nil {
		if err := setMapField(root, "enabled", *patch.Enabled, false, "description"); err != nil {
			return nil, err
		}
	}
	if patch.Triggers != nil {
		if err := setMapField(root, "triggers", *patch.Triggers, false, ""); err != nil {
			return nil, err
		}
	}
	if patch.Forge != nil {
		if err := setMapField(root, "forge", *patch.Forge, false, ""); err != nil {
			return nil, err
		}
	}

	var buf bytes.Buffer
	enc := yamlv3.NewEncoder(&buf)
	enc.SetIndent(2) // match the canonical 2-space style of shipped manifests
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("bundle: encode manifest %s: %w", path, err)
	}
	_ = enc.Close()

	// Validate the rewritten bytes through the SAME decoder LoadManifest
	// uses (strict v2 + schema + attachment safety) before committing, so a
	// structurally-broken or schema-incompatible v3-emitted manifest can
	// never land on disk. Then write durably via the shared atomic writer
	// (temp + fsync + rename + dir fsync).
	out := buf.Bytes()
	m, err := decodeManifest(out, path)
	if err != nil {
		return nil, fmt.Errorf("bundle: rewritten manifest invalid: %w", err)
	}
	if err := store.WriteFileAtomic(path, out, 0o644); err != nil {
		return nil, fmt.Errorf("bundle: write manifest %s: %w", path, err)
	}
	return m, nil
}

// documentMapping returns the top-level mapping node of a parsed YAML
// document, scaffolding the document+mapping when the node is empty
// (missing or blank file) or when the root is not a mapping.
func documentMapping(doc *yamlv3.Node) *yamlv3.Node {
	if doc.Kind == 0 || len(doc.Content) == 0 {
		mapping := &yamlv3.Node{Kind: yamlv3.MappingNode, Tag: "!!map"}
		doc.Kind = yamlv3.DocumentNode
		doc.Content = []*yamlv3.Node{mapping}
		return mapping
	}
	root := doc.Content[0]
	if root.Kind != yamlv3.MappingNode {
		mapping := &yamlv3.Node{Kind: yamlv3.MappingNode, Tag: "!!map"}
		doc.Content[0] = mapping
		return mapping
	}
	return root
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

// setMapField upserts key=value in the mapping. Node.Encode picks the
// correct tag/value for the Go value (string, int, bool), so booleans
// emit unquoted and string values that look like booleans are quoted to
// stay strings. literal=true switches multi-line string values to block
// literal (`|`) style — how `description:` is hand-authored today. A new
// key is inserted right after afterKey when present, else appended;
// comments attached to a replaced value node are carried over.
func setMapField(m *yamlv3.Node, key string, value any, literal bool, afterKey string) error {
	val := &yamlv3.Node{}
	if err := val.Encode(value); err != nil {
		return fmt.Errorf("bundle: encode field %q: %w", key, err)
	}
	if literal {
		if s, ok := value.(string); ok && strings.Contains(s, "\n") {
			val.Style = yamlv3.LiteralStyle
		}
	}
	if idx, ok := findMapKey(m, key); ok {
		old := m.Content[idx+1]
		val.HeadComment = old.HeadComment
		val.LineComment = old.LineComment
		val.FootComment = old.FootComment
		m.Content[idx+1] = val
		return nil
	}
	keyNode := &yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: key}
	if afterKey != "" {
		if idx, ok := findMapKey(m, afterKey); ok {
			pos := idx + 2 // just past afterKey's value node
			tail := append([]*yamlv3.Node{}, m.Content[pos:]...)
			m.Content = append(m.Content[:pos], keyNode, val)
			m.Content = append(m.Content, tail...)
			return nil
		}
	}
	m.Content = append(m.Content, keyNode, val)
	return nil
}
