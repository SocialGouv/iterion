package spec_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// resolved renders one artefact and resolves it as JSON Schema 2020-12 —
// an artefact the validator cannot resolve (a dangling $ref, a malformed
// keyword) is a failing test before any fixture is read.
func resolved(t *testing.T, raw []byte) *jsonschema.Resolved {
	t.Helper()
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("the artefact is not a JSON Schema: %v", err)
	}
	rs, err := s.Resolve(nil)
	if err != nil {
		t.Fatalf("the artefact does not resolve: %v", err)
	}
	return rs
}

func authorSchemas(t *testing.T) (byProfile map[int]*jsonschema.Resolved, combined *jsonschema.Resolved) {
	t.Helper()
	byProfile = map[int]*jsonschema.Resolved{}
	for _, p := range spec.SchemaProfiles(parser.MaxProfile) {
		raw, err := spec.RenderSchema(p)
		if err != nil {
			t.Fatal(err)
		}
		byProfile[p] = resolved(t, raw)
	}
	raw, err := spec.RenderCombinedSchema(spec.SchemaProfiles(parser.MaxProfile))
	if err != nil {
		t.Fatal(err)
	}
	return byProfile, resolved(t, raw)
}

// loadFixture decodes a YAML fixture into JSON-shaped Go values, as the
// converter will hand them to the schema. YAML 1.2 core tags apply: `yes`
// is a string, a bare date a timestamp (an object to the validator).
func loadFixture(t *testing.T, path string) (any, error) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func fixtures(t *testing.T, sub string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "author", sub, "*.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no %s fixtures: %v", sub, err)
	}
	sort.Strings(paths)
	return paths
}

// TestAuthorSchemaAcceptsEveryValidFixture: each valid fixture passes the
// schema of the profile its `dsl:` names, and the combined schema.
func TestAuthorSchemaAcceptsEveryValidFixture(t *testing.T) {
	byProfile, combined := authorSchemas(t)
	for _, path := range fixtures(t, "valid") {
		doc, err := loadFixture(t, path)
		if err != nil {
			t.Errorf("%s: not YAML: %v", path, err)
			continue
		}
		profile, _ := doc.(map[string]any)["dsl"].(int)
		rs, ok := byProfile[profile]
		if !ok {
			t.Errorf("%s: dsl %v names no profile", path, doc.(map[string]any)["dsl"])
			continue
		}
		if err := rs.Validate(doc); err != nil {
			t.Errorf("%s: refused by the profile-%d schema: %v", path, profile, err)
		}
		if err := combined.Validate(doc); err != nil {
			t.Errorf("%s: refused by the combined schema: %v", path, err)
		}
	}
}

// TestAuthorSchemaRefusesEveryInvalidFixture: each invalid fixture — its
// first line says why — is refused by the combined schema, and by its
// profile's when it names one. A fixture YAML itself refuses (a duplicate
// key) counts as refused.
func TestAuthorSchemaRefusesEveryInvalidFixture(t *testing.T) {
	byProfile, combined := authorSchemas(t)
	for _, path := range fixtures(t, "invalid") {
		raw, _ := os.ReadFile(path)
		if !strings.HasPrefix(string(raw), "# refused:") {
			t.Errorf("%s: the first line must say why the fixture is refused", path)
		}
		doc, err := loadFixture(t, path)
		if err != nil {
			continue // YAML refused it: that is the refusal the fixture documents
		}
		if err := combined.Validate(doc); err == nil {
			t.Errorf("%s: accepted by the combined schema", path)
		}
		if profile, ok := doc.(map[string]any)["dsl"].(int); ok {
			if rs, ok := byProfile[profile]; ok {
				if err := rs.Validate(doc); err == nil {
					t.Errorf("%s: accepted by the profile-%d schema", path, profile)
				}
			}
		}
	}
}

// TestValidFixturesCoverTheRegistry: the valid fixtures exercise every node
// kind, every property of every node kind and of the workflow, every
// top-level key of the document, and every Form — a form nothing writes is
// a fragment nothing proves.
func TestValidFixturesCoverTheRegistry(t *testing.T) {
	seenKind := map[string]bool{}
	seenProp := map[string]bool{} // kind.prop
	seenTop := map[string]bool{}
	for _, path := range fixtures(t, "valid") {
		doc, err := loadFixture(t, path)
		if err != nil {
			t.Fatal(err)
		}
		root := doc.(map[string]any)
		for key := range root {
			seenTop[key] = true
		}
		nodes, _ := root["nodes"].([]any)
		if groups, ok := root["groups"].([]any); ok {
			for _, g := range groups {
				if gn, ok := g.(map[string]any)["nodes"].([]any); ok {
					nodes = append(nodes, gn...)
				}
			}
		}
		for _, n := range nodes {
			node := n.(map[string]any)
			for key := range node {
				if k, ok := spec.Lookup(key); ok && k.Role == spec.Node {
					seenKind[key] = true
					for prop := range node {
						if prop != key {
							seenProp[key+"."+prop] = true
						}
					}
				}
			}
		}
		if wf, ok := root["workflow"].(map[string]any); ok {
			for prop := range wf {
				seenProp["workflow."+prop] = true
			}
		}
	}
	// Two kinds with one property table (agent and judge share
	// llmProperties) are one surface: a property written on either is
	// written for both.
	twins := func(k spec.Kind) []string {
		names := strings.Join(k.Names(), ",")
		var out []string
		for _, other := range spec.Kinds {
			if other.Role == spec.Node && other.Name != k.Name && strings.Join(other.Names(), ",") == names {
				out = append(out, other.Name)
			}
		}
		return out
	}
	forms := map[spec.Form]bool{}
	for _, k := range spec.Kinds {
		if k.Role == spec.Node && !seenKind[k.Name] {
			t.Errorf("no valid fixture writes a %s node", k.Name)
		}
		if k.Role == spec.Node || k.Name == "workflow" {
			for _, p := range k.Properties {
				if p.Until > 0 {
					continue // a profile-1-only property lives in the v1 fixture, checked below
				}
				written := seenProp[k.Name+"."+p.Name]
				for _, twin := range twins(k) {
					written = written || seenProp[twin+"."+p.Name]
				}
				if !written {
					t.Errorf("no valid fixture writes %s.%s", k.Name, p.Name)
				}
			}
		}
		for _, p := range k.Properties {
			forms[p.Form] = true
		}
	}
	for _, key := range []string{"dsl", "catalog", "imports", "vars", "presets", "attachments", "secrets", "prompts", "schemas", "cursors", "supervisors", "mcp_servers", "contracts", "groups", "uses", "nodes", "workflow"} {
		if !seenTop[key] {
			t.Errorf("no valid fixture writes the top-level %s", key)
		}
	}
	if !seenProp["agent.memory"] {
		t.Errorf("the v1 fixture does not write the memory block")
	}
	if len(forms) < 20 {
		t.Fatalf("only %d forms in the registry — the count is not reading it", len(forms))
	}
}

// TestSchemaArtefactsAreRegeneratedAndChecked: the three schema files are
// written by Regenerate and reported stale by Stale when they differ, like
// the Monaco module.
func TestSchemaArtefactsAreRegeneratedAndChecked(t *testing.T) {
	root := t.TempDir()
	for _, rel := range spec.Files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("<!-- dsl-spec:begin skill -->\nstale\n<!-- dsl-spec:end -->\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	changed, err := spec.Regenerate(root, []string{"agent"}, parser.MaxProfile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range append([]string{spec.CombinedSchemaFile}, spec.SchemaFile(1), spec.SchemaFile(parser.MaxProfile)) {
		if !containsString(changed, want) {
			t.Errorf("%s was not generated: %v", want, changed)
		}
	}
	if stale, err := spec.Stale(root, []string{"agent"}, parser.MaxProfile); err != nil || len(stale) != 0 {
		t.Fatalf("fresh generation reports stale files: %v %v", stale, err)
	}
	path := filepath.Join(root, spec.SchemaFile(1))
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if stale, err := spec.Stale(root, []string{"agent"}, parser.MaxProfile); err != nil || !containsString(stale, spec.SchemaFile(1)) {
		t.Fatalf("a rewritten artefact is not reported stale: %v %v", stale, err)
	}
	if err := os.Remove(filepath.Join(root, spec.CombinedSchemaFile)); err != nil {
		t.Fatal(err)
	}
	if stale, err := spec.Stale(root, []string{"agent"}, parser.MaxProfile); err != nil || !containsString(stale, spec.CombinedSchemaFile) {
		t.Fatalf("a missing artefact is not reported stale: %v %v", stale, err)
	}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
