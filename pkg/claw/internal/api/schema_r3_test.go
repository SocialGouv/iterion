package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestSchemaHugeIntegerExponentPreserved(t *testing.T) {
	// The meta-schema compiler materializes numbers; test the wire lexeme
	// directly so this witness neither allocates an enormous integer nor
	// silently loses a mathematically integral count after ParseInt overflows.
	raw := []byte(`{"maxLength":1e9223372036854775808,"required":null}`)
	var schema InputSchema
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	out, err := schema.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, []byte(`{"maxLength":1e9223372036854775808}`)) {
		t.Fatalf("integral count was lost: %s", out)
	}
}

func TestSchemaOrderThroughMigration(t *testing.T) {
	for _, raw := range []string{
		`{"description":null,"default":{"z":1,"a":2},"enum":[{"z":1,"a":2}],"x-extension":{"z":1,"a":2}}`,
		`{"items":[{"properties":{"z":{},"a":{}}}],"prefixItems":[{"properties":{"z":{},"a":{}}}],"additionalItems":{"properties":{"z":{},"a":{}}}}`,
		`{"dependencies":{"x":{"properties":{"z":{},"a":{}}}},"dependentSchemas":{"x":{"properties":{"z":{},"a":{}}}}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var schema InputSchema
			if err := json.Unmarshal([]byte(raw), &schema); err != nil {
				t.Fatal(err)
			}
			out, err := schema.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			if got, want := bytes.Count(out, []byte(`"z":`)), bytes.Count([]byte(raw), []byte(`"z":`)); got != want {
				t.Fatalf("objects lost: %s", out)
			}
			for rest := out; ; {
				z, a := bytes.Index(rest, []byte(`"z":`)), bytes.Index(rest, []byte(`"a":`))
				if z < 0 && a < 0 {
					break
				}
				if z < 0 || a < z {
					t.Fatalf("object order changed: %s", out)
				}
				rest = rest[a+4:]
			}
			var again InputSchema
			if err := json.Unmarshal(out, &again); err != nil {
				t.Fatal(err)
			}
			twice, err := again.MarshalJSON()
			if err != nil || !bytes.Equal(out, twice) {
				t.Fatalf("normalization is not idempotent: %s -> %s (%v)", out, twice, err)
			}
		})
	}
}

type r4wOffline struct{}

func (r4wOffline) Load(url string) (any, error) { return nil, fmt.Errorf("offline: %s", url) }

func r4wEmit(t *testing.T, raw string) []byte {
	t.Helper()
	var s InputSchema
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := s.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}

func r4wCompile(t *testing.T, out []byte) *jsonschema.Schema {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(r4wOffline{})
	if err := c.AddResource("https://w.test/s", v); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("https://w.test/s")
	if err != nil {
		t.Fatalf("wire schema does not compile offline: %v\nwire: %s", err, out)
	}
	return s
}

func r4wDupKeys(b []byte) bool {
	d := json.NewDecoder(bytes.NewReader(b))
	var walk func() bool
	walk = func() bool {
		tok, err := d.Token()
		if err != nil {
			return false
		}
		dup := false
		switch tok {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				k, _ := d.Token()
				if seen[k.(string)] {
					dup = true
				}
				seen[k.(string)] = true
				dup = walk() || dup
			}
			d.Token()
		case json.Delim('['):
			for d.More() {
				dup = walk() || dup
			}
			d.Token()
		}
		return dup
	}
	return walk()
}

func TestR4Witnesses(t *testing.T) {
	t.Run("MO1-duplicate-key-last-value-wins-like-encoding-json", func(t *testing.T) {
		out := r4wEmit(t, `{"properties":{"b":{"type":"string"},"a":{},"b":{"type":"number"}},"nullable":false}`)
		if !bytes.Contains(out, []byte(`"b":{"type":"number"}`)) || bytes.Contains(out, []byte(`"string"`)) {
			t.Fatalf("duplicate key must keep encoding/json's LAST value: %s", out)
		}
	})
	t.Run("MO2-no-duplicate-keys-on-wire", func(t *testing.T) {
		out := r4wEmit(t, `{"properties":{"b":{"type":"string"},"a":{},"b":{"type":"number"}},"nullable":false}`)
		if r4wDupKeys(out) {
			t.Fatalf("wire schema repeats an object key: %s", out)
		}
	})
	t.Run("MO3-synthesized-keys-deterministic", func(t *testing.T) {
		raw := `{"items":[{}],"id":"#a","anyOf":[],"nullable":false}`
		first := string(r4wEmit(t, raw))
		for i := 0; i < 300; i++ {
			if got := string(r4wEmit(t, raw)); got != first {
				t.Fatalf("wire bytes are not deterministic:\n%s\n%s", first, got)
			}
		}
	})
	t.Run("MO5-collisions-deterministic", func(t *testing.T) {
		raw := `{"dependencies":{"a":{"type":"integer"},"b":{"type":"boolean"},"c":{"type":"null"},"d":["x"],"e":["y"]},"dependentSchemas":{"a":{"type":"string"},"b":{"type":"null"},"c":{"type":"string"}},"dependentRequired":{"d":["z"],"e":["w"]}}`
		first := string(r4wEmit(t, raw))
		for i := 0; i < 300; i++ {
			if got := string(r4wEmit(t, raw)); got != first {
				t.Fatalf("wire bytes are not deterministic:\n%s\n%s", first, got)
			}
		}
	})
	for _, dialect := range []string{"http://json-schema.org/draft-04/schema#", "http://json-schema.org/draft-06/schema#", "http://json-schema.org/draft-07/schema#", "https://json-schema.org/draft/2019-09/schema"} {
		t.Run("MO6-MO7-nullable-opaque-under-"+dialect, func(t *testing.T) {
			out := r4wEmit(t, `{"$schema":"`+dialect+`","type":"object","properties":{"q":{"type":"string","nullable":true}}}`)
			s := r4wCompile(t, out)
			v, _ := jsonschema.UnmarshalJSON(strings.NewReader(`{"q":null}`))
			if s.Validate(v) == nil {
				t.Fatalf("declared %s ignores nullable; null must stay rejected: %s", dialect, out)
			}
		})
	}
	t.Run("MO8-id-is-an-annotation-under-2019-09", func(t *testing.T) {
		out := r4wEmit(t, `{"$schema":"https://json-schema.org/draft/2019-09/schema","type":"object","$defs":{"n":{"type":"integer"}},"properties":{"a":{"id":"field-a","type":"object","properties":{"b":{"$ref":"#/$defs/n"}}}}}`)
		r4wCompile(t, out)
	})
}

// A manually assigned Raw bypasses json.Unmarshal's scanner. Keep the same
// 10000-container limit even inside opaque instance data, before recursing.
func TestSchemaRawDepthLimit(t *testing.T) {
	for _, depth := range []int{10000, 10001} {
		for _, changed := range []bool{false, true} {
			t.Run(fmt.Sprintf("depth=%d/changed=%v", depth, changed), func(t *testing.T) {
				suffix := "}"
				if changed {
					suffix = `,"required":null}`
				}
				raw := []byte(`{"enum":` + strings.Repeat("[", depth-1) + "0" + strings.Repeat("]", depth-1) + suffix)
				for _, marshal := range []func() ([]byte, error){
					(InputSchema{Raw: raw}).MarshalJSON,
					(Property{Raw: raw}).MarshalJSON,
				} {
					out, err := marshal()
					if depth == 10000 {
						if err != nil {
							t.Fatalf("depth 10000 must remain accepted: %v", err)
						}
						if !json.Valid(out) {
							t.Fatal("depth 10000 must emit valid JSON")
						}
						continue
					}
					var syntax *json.SyntaxError
					if !errors.As(err, &syntax) || !strings.Contains(err.Error(), "exceeded max depth") {
						t.Fatalf("depth 10001 must return a typed depth error, got %T: %v", err, err)
					}
				}
			})
		}
	}
}

func TestSchemaRemovedUnevaluatedItems(t *testing.T) {
	for _, raw := range []string{
		`{"prefixItems":[{}],"unevaluatedItems":0}`,
		`{"$schema":"http://json-schema.org/draft-07/schema#","definitions":{"tuple":{"items":[{}]}},"$ref":"#/definitions/tuple","unevaluatedItems":false}`,
	} {
		t.Run(raw, func(t *testing.T) {
			out := r4wEmit(t, raw)
			again := r4wEmit(t, string(out))
			if !bytes.Equal(out, again) {
				t.Fatalf("removed unevaluatedItems must not defer a tail to the next normalization: %s -> %s", out, again)
			}
		})
	}
}
