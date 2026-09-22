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
			// YAML refused it: the refusal the fixture documents only when
			// its first line says so.
			if first, _, _ := strings.Cut(string(raw), "\n"); !strings.Contains(first, "YAML") {
				t.Errorf("%s: refused by the YAML decoder (%v), which the first line does not name", path, err)
			}
			continue
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

// TestKnownLaxSpellingsAreSaidNotHidden: the spellings the schema ACCEPTS
// and the .bot refuses — JSON has one number type, YAML two — are written
// down, one fixture each under testdata/author/lax with its first line
// naming why, and held: still accepted by the schema today (the day the
// validator or the schema refuses one, the fixture moves to invalid), and
// the converter's to refuse by the YAML tag (PR B's round-trip test reads
// the same directory).
func TestKnownLaxSpellingsAreSaidNotHidden(t *testing.T) {
	_, combined := authorSchemas(t)
	for _, path := range fixtures(t, "lax") {
		raw, _ := os.ReadFile(path)
		if !strings.HasPrefix(string(raw), "# lax:") {
			t.Errorf("%s: the first line must say why the schema is lax here", path)
		}
		doc, err := loadFixture(t, path)
		if err != nil {
			t.Errorf("%s: not YAML: %v", path, err)
			continue
		}
		if err := combined.Validate(doc); err != nil {
			t.Errorf("%s: the schema now refuses it — move the fixture to invalid: %v", path, err)
		}
	}
}

// coverage is what the valid fixtures WRITE, walked with the registry: every
// kind reached, every property of every kind (a block's too, a sub-block's
// too), every part of every entry (`kind.part`, nested `kind.entries.part`),
// every Form those properties and parts carry, every top-level key.
type coverage struct {
	kinds, props, forms, top map[string]bool
}

func newCoverage() *coverage {
	return &coverage{map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}}
}

// kind walks a value written for a kind's body: a mapping of its
// properties (a block reached through a Block property is walked with its
// own kind), or its entries.
func (c *coverage) kind(k spec.Kind, v any) {
	c.kinds[k.Name] = true
	if k.Entries != nil && len(k.Properties) == 0 {
		c.entries(k.Name, k.Entries, v)
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	for key, val := range m {
		p, ok := k.Property(key)
		if !ok {
			if k.Entries != nil {
				c.entry(k.Name, k.Entries, val)
			}
			continue
		}
		c.property(k.Name, p, val)
	}
}

func (c *coverage) property(kind string, p spec.Property, val any) {
	c.props[kind+"."+p.Name] = true
	c.forms[string(p.Form)] = true
	if p.Form == spec.Block || p.Form == spec.BlockOrIdent {
		if bk, ok := spec.Lookup(p.Body); ok {
			c.kind(bk, val)
		}
	}
}

// entries walks a block of entries: a mapping keyed by name, or the
// sequence a SequenceKey block is written as.
func (c *coverage) entries(name string, e *spec.Entries, v any) {
	switch t := v.(type) {
	case map[string]any:
		for _, ev := range t {
			c.entry(name, e, ev)
		}
	case []any:
		for _, ev := range t {
			c.entry(name, e, ev)
		}
	}
}

// entry walks one entry's value: the scalar shorthand (the one required or
// only part), or an object of its parts and its sub-block's properties.
func (c *coverage) entry(name string, e *spec.Entries, ev any) {
	m, ok := ev.(map[string]any)
	if !ok {
		if ev == nil {
			return
		}
		for _, f := range e.Fields {
			if f.Required || len(e.Fields) == 1 {
				c.props[name+"."+f.Name] = true
				c.forms[string(f.Form)] = true
				return
			}
		}
		return
	}
	if e.Entries != nil {
		for _, x := range m {
			c.entry(name+".entries", e.Entries, x)
		}
		return
	}
	var body spec.Kind
	hasBody := false
	if e.Body != "" {
		body, hasBody = spec.Lookup(e.Body)
	}
	for key, val := range m {
		for _, f := range e.Fields {
			if f.Name == key {
				c.props[name+"."+f.Name] = true
				c.forms[string(f.Form)] = true
			}
		}
		if hasBody {
			if p, ok := body.Property(key); ok {
				c.kinds[body.Name] = true
				c.property(body.Name, p, val)
			}
		}
	}
}

// TestValidFixturesCoverTheRegistry: what the valid fixtures write, walked
// with the registry, reaches every kind, every property of every kind
// (blocks and sub-blocks included), every part of every entry, every Form,
// and every top-level key — computed from the fixtures' CONTENT, so a
// property or a part dropped from a fixture reddens here, wherever it
// sits. A profile-1-only property is written by the profile-1 fixture.
func TestValidFixturesCoverTheRegistry(t *testing.T) {
	c := newCoverage()
	for _, path := range fixtures(t, "valid") {
		doc, err := loadFixture(t, path)
		if err != nil {
			t.Fatal(err)
		}
		root := doc.(map[string]any)
		for key := range root {
			c.top[key] = true
		}
		walkNodes := func(nodes any) {
			list, _ := nodes.([]any)
			for _, n := range list {
				node, _ := n.(map[string]any)
				for key := range node {
					if k, ok := spec.Lookup(key); ok && k.Role == spec.Node {
						c.kind(k, node)
					}
				}
			}
		}
		walkNodes(root["nodes"])
		if groups, ok := root["groups"].([]any); ok {
			for _, g := range groups {
				if gm, ok := g.(map[string]any); ok {
					walkNodes(gm["nodes"])
				}
			}
		}
		for kind, key := range map[string]string{"workflow": "workflow"} {
			if k, ok := spec.Lookup(kind); ok {
				c.kind(k, root[key])
			}
		}
		for _, name := range []string{"vars", "presets", "attachments", "secrets"} {
			if k, ok := spec.Lookup(name); ok {
				c.kind(k, root[name])
			}
		}
		for kind, key := range map[string]string{"schema": "schemas", "cursor": "cursors", "supervisor": "supervisors", "mcp_server": "mcp_servers", "contract": "contracts"} {
			k, _ := spec.Lookup(kind)
			if decls, ok := root[key].(map[string]any); ok {
				for _, d := range decls {
					c.kind(k, d)
				}
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
			if other.Name != k.Name && other.Role == k.Role && len(k.Names()) > 0 && strings.Join(other.Names(), ",") == names {
				out = append(out, other.Name)
			}
		}
		return out
	}
	registryForms := map[string]bool{}
	for _, k := range spec.Kinds {
		if k.Name == "group" || k.Name == "use" || k.Name == "prompt" {
			continue // headers and text, not property tables: held by the header and schema tests
		}
		if !c.kinds[k.Name] {
			t.Errorf("no valid fixture reaches %s", k.Name)
		}
		for _, p := range k.Properties {
			registryForms[string(p.Form)] = true
			written := c.props[k.Name+"."+p.Name]
			for _, twin := range twins(k) {
				written = written || c.props[twin+"."+p.Name]
			}
			if !written {
				t.Errorf("no valid fixture writes %s.%s", k.Name, p.Name)
			}
		}
		if k.Entries != nil {
			for _, part := range fieldNamesOf(k.Entries, k.Name+".") {
				if !c.props[part.name] {
					t.Errorf("no valid fixture writes the entry part %s", part.name)
				}
				registryForms[string(part.form)] = true
			}
		}
	}
	for form := range registryForms {
		if !c.forms[form] {
			t.Errorf("no valid fixture writes a value of form %q", form)
		}
	}
	for _, key := range []string{"dsl", "catalog", "imports", "vars", "presets", "attachments", "secrets", "prompts", "schemas", "cursors", "supervisors", "mcp_servers", "contracts", "groups", "uses", "nodes", "workflow"} {
		if !c.top[key] {
			t.Errorf("no valid fixture writes the top-level %s", key)
		}
	}
	if len(c.props) < 150 || len(c.forms) < 20 {
		t.Fatalf("the fixtures write %d properties of %d forms — the walk is not reading them", len(c.props), len(c.forms))
	}
}

type namedForm struct {
	name string
	form spec.Form
}

func fieldNamesOf(e *spec.Entries, prefix string) []namedForm {
	var out []namedForm
	for _, f := range e.Fields {
		out = append(out, namedForm{prefix + f.Name, f.Form})
	}
	if e.Entries != nil {
		out = append(out, fieldNamesOf(e.Entries, prefix+"entries.")...)
	}
	return out
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
