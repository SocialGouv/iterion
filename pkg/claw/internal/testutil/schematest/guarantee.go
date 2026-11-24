// Package schematest supplies the offline schema guarantee to transport tests.
// It is imported only by tests; the validator is not a runtime dependency.
package schematest

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed testdata/*.json
var corpus embed.FS

type example struct {
	Name      string                     `json:"name"`
	Input     json.RawMessage            `json:"input"`
	Want      json.RawMessage            `json:"want"`
	Valid     []json.RawMessage          `json:"valid"`
	Invalid   []json.RawMessage          `json:"invalid"`
	Resources map[string]json.RawMessage `json:"resources"`
}

type offline struct{}

func (offline) Load(url string) (any, error) {
	return nil, fmt.Errorf("unexpected schema load: %s", url)
}
func compiler() *jsonschema.Compiler {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(offline{})
	return c
}
func value(t *testing.T, raw []byte) any {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Run checks the actual wire schema against the embedded 2020-12 meta-schema,
// independently of its own $schema declaration, and compiles EVERY output with
// an offline loader to resolve references. Expected trees and instance witnesses
// additionally prevent satisfying the guarantee by dropping constraints.
func Run(t *testing.T, emit func([]byte) ([]byte, error)) {
	t.Helper()
	meta, err := compiler().Compile("https://json-schema.org/draft/2020-12/schema")
	if err != nil {
		t.Fatal(err)
	}
	data, err := corpus.ReadFile("testdata/legacy.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []example
	if err = json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	files, err := corpus.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.Name() == "legacy.json" || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		raw, err := corpus.ReadFile("testdata/" + file.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(file.Name(), "-cases.json") {
			var extra []example
			if err := json.Unmarshal(raw, &extra); err != nil {
				t.Fatal(err)
			}
			cases = append(cases, extra...)
			continue
		}
		tc := example{Name: file.Name(), Input: raw}
		switch file.Name() {
		case "fastmcp-openapi30.json":
			tc.Valid = []json.RawMessage{[]byte(`{"timeout":1,"q":null}`)}
			tc.Invalid = []json.RawMessage{[]byte(`{"timeout":0}`), []byte(`{"timeout":301}`)}
		case "zod-openapi3.json":
			tc.Valid = []json.RawMessage{[]byte(`{"timeout":1,"query":null}`)}
			tc.Invalid = []json.RawMessage{[]byte(`{"timeout":0}`), []byte(`{"timeout":301}`)}
		case "typebox-tuple.json":
			tc.Valid = []json.RawMessage{[]byte(`{"point":[1,2]}`)}
			tc.Invalid = []json.RawMessage{[]byte(`{"point":[1]}`), []byte(`{"point":[1,2,3]}`)}
		case "google-empty.json":
			tc.Valid = []json.RawMessage{[]byte(`{}`)}
			tc.Invalid = []json.RawMessage{[]byte(`{"x":1}`)}
		case "playwright-navigate.json":
			tc.Valid = []json.RawMessage{[]byte(`{"url":"https://example.test"}`)}
			tc.Invalid = []json.RawMessage{[]byte(`{}`), []byte(`{"url":3}`)}
		}
		cases = append(cases, tc)
	}
	// Independent meta-schema shape sweep: every standard keyword crossed
	// with malformed scalars, containers and nested members. No production
	// classification function is used to construct this oracle corpus.
	keywords := strings.Fields(`$schema $id $ref $anchor $dynamicRef $dynamicAnchor $recursiveRef $recursiveAnchor $vocabulary $comment $defs definitions dependencies dependentSchemas dependentRequired properties patternProperties items prefixItems additionalProperties unevaluatedProperties unevaluatedItems propertyNames contains not if then else contentSchema allOf anyOf oneOf type enum required title description format contentEncoding contentMediaType pattern multipleOf minimum maximum exclusiveMinimum exclusiveMaximum maxLength minLength maxItems minItems minContains maxContains minProperties maxProperties uniqueItems readOnly writeOnly deprecated examples`)
	for _, key := range keywords {
		for i, raw := range []string{`null`, `true`, `false`, `"x"`, `0`, `-1`, `0.5`, `[]`, `{}`, `[null]`, `{"x":null}`, `["a","a"]`} {
			// Valid URI strings need resolvable targets for the compilation
			// oracle. Keep the malformed shapes unchanged.
			extra := ""
			if raw == `"x"` {
				switch key {
				case "$schema":
					raw = `"https://json-schema.org/draft/2020-12/schema"`
				case "$ref", "$dynamicRef", "$recursiveRef":
					raw, extra = `"#/$defs/target"`, `,"$defs":{"target":true}`
				}
			}
			cases = append(cases, example{Name: fmt.Sprintf("meta-shapes/%s/%d", key, i), Input: json.RawMessage(fmt.Sprintf("{%q:%s%s}", key, raw, extra))})
		}
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			raw, err := emit(tc.Input)
			if err != nil {
				t.Fatalf("decoded tool must reach wire: %v", err)
			}
			got := value(t, raw)
			if err := meta.Validate(got); err != nil {
				t.Errorf("wire schema violates draft 2020-12: %v\nwire: %s", err, raw)
			}
			if len(tc.Want) > 0 && !reflect.DeepEqual(got, value(t, tc.Want)) {
				t.Errorf("conversion lost or changed semantics\ngot: %s\nwant: %s", raw, tc.Want)
			}
			c := compiler()
			for uri, resource := range tc.Resources {
				if err := c.AddResource(uri, value(t, resource)); err != nil {
					t.Fatal(err)
				}
			}
			if err = c.AddResource("https://fixture.test/schema", got); err != nil {
				t.Fatal(err)
			}
			s, err := c.Compile("https://fixture.test/schema")
			if err != nil {
				t.Fatalf("wire schema does not compile offline: %v\nwire: %s", err, raw)
			}
			for _, v := range tc.Valid {
				if err := s.Validate(value(t, v)); err != nil {
					t.Errorf("must accept %s: %v", v, err)
				}
			}
			for _, v := range tc.Invalid {
				if err := s.Validate(value(t, v)); err == nil {
					t.Errorf("must reject %s", v)
				}
			}
		})
	}
}

// ToolSchema extracts what the provider actually serialized, without decoding
// it through api.InputSchema (which would hide missing wire normalization).
func ToolSchema(body []byte, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	var env struct {
		Tools []struct {
			Schema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err = json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	if len(env.Tools) != 1 {
		return nil, fmt.Errorf("want one tool: %s", body)
	}
	return env.Tools[0].Schema, nil
}

// MetaSchema returns the offline oracle for fuzz tests as well as the corpus.
func MetaSchema(t testing.TB) *jsonschema.Schema {
	t.Helper()
	s, err := compiler().Compile("https://json-schema.org/draft/2020-12/schema")
	if err != nil {
		t.Fatal(err)
	}
	return s
}
