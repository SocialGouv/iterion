package chart_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"
)

func TestValuesSchemaMatchesTemplates(t *testing.T) {
	raw, err := os.ReadFile("values.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{"global": true} // Helm's reserved dependency overlay.
	re := regexp.MustCompile(`\.Values\.([A-Za-z0-9_]+)`)
	if err := filepath.WalkDir("templates", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range re.FindAllSubmatch(raw, -1) {
			used[string(m[1])] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var chart struct {
		Dependencies []struct {
			Name string `yaml:"name"`
		} `yaml:"dependencies"`
	}
	raw, err = os.ReadFile("Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &chart); err != nil {
		t.Fatal(err)
	}
	for _, dep := range chart.Dependencies {
		used[dep.Name] = true
	}
	for key := range used {
		if _, ok := schema.Properties[key]; !ok {
			t.Errorf("template/dependency key %q missing from schema", key)
		}
	}
	for key := range schema.Properties {
		if !used[key] {
			t.Errorf("schema key %q is consumed by neither templates nor dependencies", key)
		}
	}
	for _, file := range []string{"values.yaml", "values-dev.yaml", "values-prod.yaml"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]any
		if err := yaml.Unmarshal(raw, &values); err != nil {
			t.Fatal(err)
		}
		if err := resolved.Validate(values); err != nil {
			t.Errorf("%s: %v", file, err)
		}
	}
	for _, invalid := range []map[string]any{
		{"renamedRunner": map[string]any{}}, {"runnerPools": map[string]any{}}, {"server": "oops"}, {"mode": "cluod"},
	} {
		if err := resolved.Validate(invalid); err == nil {
			t.Errorf("silent invalid values accepted: %v", invalid)
		}
	}
}
