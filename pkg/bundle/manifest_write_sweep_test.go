package bundle

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
)

// The gate's corpus probe (#1349, revi round): every shipped
// bots/*/manifest.yaml must take a `dsl migrate --floor`-style Requires
// patch AND a studio-shaped 8-field autosave patch, with zero value drift
// on everything the patch does not touch. The studio's BotMetadataForm
// sends all eight fields on every autosave, so "insert a missing key AND
// replace its neighbour in one write" is the common case, not a corner.
func TestWriteManifest_ShippedCorpusPatchSweep(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "bots", "*", "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 30 {
		t.Fatalf("only %d shipped manifests found — the sweep is not reaching the corpus", len(files))
	}

	for _, src := range files {
		t.Run(src, func(t *testing.T) {
			body, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			before, err := decodeManifest(body, src)
			if err != nil {
				t.Fatalf("shipped manifest does not decode: %v", err)
			}
			beforeKeys := manifestKeySet(t, body)

			// (a) the floor raise: one key.
			sweepOne(t, src, body, before, beforeKeys, ManifestPatch{Requires: &Requires{Iterion: ">= 9.9.9"}}, "requires")

			// (b) the studio's autosave: all eight metadata fields, with
			// the manifest's own current values — insert-or-replace per key
			// depending on what the file already holds.
			studio := ManifestPatch{
				DisplayName: ptr(before.DisplayName),
				Icon:        ptr(before.Icon),
				Description: ptr(before.Description),
				Author:      ptr(before.Author),
				Version:     ptr(before.Version),
				WhenToUse:   ptr(before.WhenToUse),
				Triggers:    ptr(before.Triggers),
			}
			enabled := before.IsEnabled()
			studio.Enabled = &enabled
			sweepOne(t, src, body, before, beforeKeys, studio,
				"display_name", "icon", "description", "author", "version", "when_to_use", "enabled", "triggers")
		})
	}
}

// sweepOne applies one patch to a temp copy of a shipped manifest and
// asserts success, zero drift on every unpatched field, and an unchanged
// top-level key set apart from the patched keys and schema_version.
func sweepOne(t *testing.T, src string, body []byte, before *Manifest, beforeKeys map[string]bool, patch ManifestPatch, patchKeys ...string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := WriteManifest(path, patch)
	if err != nil {
		t.Fatalf("patch failed: %v", err)
	}

	// Zero drift on the fields the patch does not name, compared over the
	// WHOLE decoded manifest: the patched fields are masked out on both
	// sides and everything else must be identical.
	mask := func(x *Manifest) Manifest {
		y := *x
		y.Name, y.DisplayName, y.Icon, y.Version, y.Description = "", "", "", "", ""
		y.Author, y.WhenToUse, y.Enabled, y.Triggers, y.Requires = "", "", nil, nil, nil
		return y
	}
	if a, b := mask(m), mask(before); !reflect.DeepEqual(a, b) {
		t.Errorf("unpatched fields drifted:\nbefore: %+v\nafter:  %+v", b, a)
	}

	// The top-level key set changes only by the patched keys (a missing one
	// inserted) and schema_version — never by a deletion.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterKeys := manifestKeySet(t, raw)
	allowed := map[string]bool{"schema_version": true}
	for _, k := range patchKeys {
		allowed[k] = true
	}
	for k := range beforeKeys {
		if !afterKeys[k] {
			t.Errorf("top-level key %q was deleted by the patch", k)
		}
	}
	for k := range afterKeys {
		if !beforeKeys[k] && !allowed[k] {
			t.Errorf("top-level key %q appeared that is not the patch's", k)
		}
	}
}

// manifestKeySet is the set of top-level keys of a manifest's bytes, read
// with the same yaml.v3 the writer uses.
func manifestKeySet(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(body, &doc); err != nil {
		t.Fatalf("key set: %v", err)
	}
	out := map[string]bool{}
	if doc.Kind != yamlv3.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yamlv3.MappingNode {
		return out
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		out[root.Content[i].Value] = true
	}
	return out
}
