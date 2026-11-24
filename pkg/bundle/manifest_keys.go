package bundle

import (
	"fmt"

	yamlv3 "gopkg.in/yaml.v3"
)

// ManifestKeys is the set of keys a manifest's own bytes DECLARE, dotted for
// a nested one ("category", "exports.skills"): what the one floors table
// reads when a release floor is asked for by a KEY rather than by a syntax
// the bundle's `.bot` sources are written in.
//
// It is read from the bytes rather than from the decoded Manifest because a
// key declared with an EMPTY value is still declared — `category:` alone, or
// a root merge that brings one in — and no decoded zero value tells that
// apart from an absent key. The floor is owed by the DECLARATION: a build
// that does not know the key decodes the manifest strictly (manifest.go's
// yaml.UnmarshalStrict) and refuses the whole document, whatever the value
// is.
type ManifestKeys map[string]bool

// Declares reports whether the manifest declares any of the keys.
func (k ManifestKeys) Declares(keys ...string) bool {
	for _, key := range keys {
		if k[key] {
			return true
		}
	}
	return false
}

// maxManifestKeyDepth bounds how deep the key reader descends. A floor is
// asked for by a key an author SPELLS, and two levels cover every key the
// floors table asks for today (`category`, `tags`, `exports.skills`,
// `dependencies.skills`); a floor on a deeper key raises this bound, which
// is the one place that decides it.
const maxManifestKeyDepth = 2

// DeclaredManifestKeys reads the keys a manifest's bytes declare. Mappings
// are DECODED (yaml.v3, the writer's reader), so a root merge key resolves
// to the keys it brings in instead of reading as a literal `<<`; a sequence
// is a value, never a key path, so its elements are not walked.
func DeclaredManifestKeys(body []byte) (ManifestKeys, error) {
	var root map[string]any
	if err := yamlv3.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("reading the declared keys: %w", err)
	}
	keys := ManifestKeys{}
	collectManifestKeys(keys, "", root, 1)
	return keys, nil
}

// collectManifestKeys adds node's keys to into, prefixed by the path that
// reaches it, down to maxManifestKeyDepth.
func collectManifestKeys(into ManifestKeys, prefix string, node map[string]any, depth int) {
	for key, value := range node {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		into[path] = true
		if depth >= maxManifestKeyDepth {
			continue
		}
		if nested, ok := value.(map[string]any); ok {
			collectManifestKeys(into, path, nested, depth+1)
		}
	}
}
