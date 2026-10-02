package spec

import (
	"encoding/json"
	"strings"
	"testing"
)

// The author schema of a profile carries exactly the properties in their
// Since/Until window, and marks a deprecated one: built here on a synthetic
// registry, since no shipped property carries a Since above 1 yet — the
// schema logic is proven on the shape, not on the registry's contents.
func TestSchemaWindowFollowsSinceUntilAndDeprecated(t *testing.T) {
	kinds := []Kind{
		{Name: "thing", Role: Node, Doc: "A synthetic node.", Properties: []Property{
			prop("always", String, "every profile"),
			Property{Name: "early", Form: String, Until: 1, Doc: "profile 1 only"},
			Property{Name: "late", Form: String, Since: 2, Doc: "from profile 2"},
			Property{Name: "old", Form: String, Deprecated: true, Doc: "still accepted, deprecated"},
			Property{Name: "window", Form: String, Since: 2, Until: 2, Doc: "profile 2 only"},
		}},
		{Name: "workflow", Role: Declaration, Edges: true, Properties: []Property{prop("entry", Ident, "x")}},
	}
	for _, c := range []struct {
		profile int
		present []string
		absent  []string
	}{
		{1, []string{"always", "early", "old"}, []string{"late", "window"}},
		{2, []string{"always", "late", "old", "window"}, []string{"early"}},
		{3, []string{"always", "late", "old"}, []string{"early", "window"}},
	} {
		raw, err := renderSchemaFor(kinds, c.profile)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		node := doc["$defs"].(map[string]any)["node.thing"].(map[string]any)
		props := node["properties"].(map[string]any)
		for _, name := range c.present {
			if _, ok := props[name]; !ok {
				t.Errorf("profile %d: %q absent from the node's properties %v", c.profile, name, keys(props))
			}
		}
		for _, name := range c.absent {
			if _, ok := props[name]; ok {
				t.Errorf("profile %d: %q present in the node's properties", c.profile, name)
			}
		}
		if old, ok := props["old"].(map[string]any); !ok || old["deprecated"] != true {
			t.Errorf("profile %d: the deprecated property is not marked: %v", c.profile, props["old"])
		}
		if always := props["always"].(map[string]any); always["deprecated"] != nil {
			t.Errorf("profile %d: a live property is marked deprecated", c.profile)
		}
		if got := doc["properties"].(map[string]any)["dsl"].(map[string]any)["const"]; got != float64(c.profile) {
			t.Errorf("profile %d: dsl const = %v", c.profile, got)
		}
	}
	if _, err := renderSchemaFor(kinds, 0); err == nil || !strings.Contains(err.Error(), "profile") {
		t.Errorf("profile 0 accepted: %v", err)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
