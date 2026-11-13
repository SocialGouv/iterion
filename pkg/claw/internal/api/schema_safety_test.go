package api

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSchemaInvalidUTF8Replacement(t *testing.T) {
	raw := []byte("{\"type\":\"object\",\"properties\":{},\"pattern\":\"\xff\xfe\"}")
	for _, value := range []any{&InputSchema{}, &Property{}} {
		if err := json.Unmarshal(raw, value); err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if !utf8.Valid(body) {
			t.Errorf("%T emitted invalid UTF-8: %q", value, body)
		}
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if got["pattern"] != "\ufffd\ufffd" {
			t.Errorf("replacement = %q", got["pattern"])
		}
	}
}
func TestSchemaNullIsNoOp(t *testing.T) {
	for _, value := range []any{&InputSchema{}, &Property{}, &InputSchema{Type: "object"}, &Property{Type: "string"}} {
		before, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(" null "), value); err != nil {
			t.Fatal(err)
		}
		after, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Errorf("%T null changed %s to %s", value, before, after)
		}
		switch v := value.(type) {
		case *InputSchema:
			if v.Raw != nil {
				t.Error("null retained Raw")
			}
		case *Property:
			if v.Raw != nil {
				t.Error("null retained Raw")
			}
		}
	}
	for _, value := range []any{&InputSchema{}, &Property{}} {
		if err := json.Unmarshal([]byte(`{"type":"object","x-custom":true}`), value); err != nil {
			t.Fatal(err)
		}
		before, _ := json.Marshal(value)
		if err := json.Unmarshal([]byte(`null`), value); err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(value)
		if !bytes.Equal(before, after) {
			t.Errorf("null overwrote existing decoded schema: %s", after)
		}
	}
}
func TestSchemaEmptyRawUsesTypedView(t *testing.T) {
	for _, value := range []any{InputSchema{Type: "object", Raw: json.RawMessage{}}, Property{Type: "string", Raw: json.RawMessage{}}} {
		got, err := json.Marshal(value)
		if err != nil || !bytes.Contains(got, []byte(`"type"`)) {
			t.Errorf("%T empty Raw: %s, %v", value, got, err)
		}
	}
}
func TestSchemaRetainsOnlyRootRaw(t *testing.T) {
	raw := []byte(`{"type":"object","$defs":{"n":{"type":"string"}},"properties":{"a":` + strings.Repeat(`{"type":"array","items":`, 100) + `{"type":"string"}` + strings.Repeat(`}`, 100) + `}}`)
	var s InputSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	retained := len(s.Raw)
	var walk func(Property)
	walk = func(p Property) {
		retained += len(p.Raw)
		if p.Items != nil {
			walk(*p.Items)
		}
		for _, v := range p.Properties {
			walk(v)
		}
	}
	for _, p := range s.Properties {
		walk(p)
	}
	for _, p := range s.Defs {
		walk(p)
	}
	if retained != len(raw) {
		t.Fatalf("retained Raw bytes = %d, want one root = %d", retained, len(raw))
	}
}

func TestSchemaPropertyNullKeywords(t *testing.T) {
	var child Property
	if err := json.Unmarshal([]byte(`{"description":null,"items":null,"enum":null,"const":null,"default":{"description":null},"examples":[null],"x-extension":null}`), &child); err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(InputSchema{Type: "object", Properties: map[string]Property{"p": child}})
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Properties map[string]map[string]json.RawMessage
	}
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"description", "enum"} {
		if _, ok := root.Properties["p"][key]; ok {
			t.Errorf("null keyword %s survived: %s", key, got)
		}
	}
	if string(root.Properties["p"]["items"]) != "{}" {
		t.Error("null child schema must become {}")
	}
	for _, key := range []string{"const", "default", "examples", "x-extension"} {
		if _, ok := root.Properties["p"][key]; !ok {
			t.Errorf("instance data %s lost: %s", key, got)
		}
	}
}

// Each public boundary and manually assigned Raw must pass the ONE shared
// UTF-8 choke point. Checking Raw itself prevents a later marshal masking a
// missing decode-time normalization.
func TestSchemaRawNormalizationBoundaries(t *testing.T) {
	bad := []byte("{\"description\":\"\xff\xfe\",\"required\":null}")
	var s InputSchema
	var p Property
	for _, v := range []any{&s, &p} {
		if err := json.Unmarshal(bad, v); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range [][]byte{s.Raw, p.Raw} {
		if !utf8.Valid(raw) || bytes.Contains(raw, []byte(`"required"`)) {
			t.Errorf("Raw not normalized: %q", raw)
		}
	}
	for _, v := range []any{InputSchema{Raw: bad}, Property{Raw: bad}} {
		out, err := json.Marshal(v)
		if err != nil || !utf8.Valid(out) || bytes.Contains(out, []byte(`"required"`)) {
			t.Errorf("manual Raw: %q, %v", out, err)
		}
	}
}

func TestSchemaNumberLexemes(t *testing.T) {
	raw := []byte(`{"description":null,"minimum":-1.20e+3,"maximum":9007199254740993,"multipleOf":1e-400,"default":123456789012345678901234567890}`)
	var s InputSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"-1.20e+3", "9007199254740993", "1e-400", "123456789012345678901234567890"} {
		if !bytes.Contains(s.Raw, []byte(n)) {
			t.Errorf("numeric lexeme lost: %s", s.Raw)
		}
	}
	for _, tc := range []struct {
		n             string
		integer, want bool
	}{{"1e1000000000", true, true}, {"1e-1000000000", true, false}, {"1e-1000000000", false, true}, {"-0", true, true}, {"0", false, false}, {"1.20e1", true, true}, {"1.20", true, false}} {
		if got := numberIs(json.Number(tc.n), tc.integer); got != tc.want {
			t.Errorf("numberIs(%s,%v)=%v", tc.n, tc.integer, got)
		}
	}
}

func TestSchemaRawEditsAreNormalized(t *testing.T) {
	for _, name := range []string{"InputSchema", "Property"} {
		t.Run(name, func(t *testing.T) {
			var s InputSchema
			var p Property
			var target interface {
				UnmarshalJSON([]byte) error
				MarshalJSON() ([]byte, error)
			}
			var raw *json.RawMessage
			if name == "InputSchema" {
				target = &s
				raw = &s.Raw
			} else {
				target = &p
				raw = &p.Raw
			}
			if err := target.UnmarshalJSON([]byte(`{"exclusiveMinimum":1000}`)); err != nil {
				t.Fatal(err)
			}
			// Same-length in-place edit: still valid JSON, now invalid 2020-12.
			// json.Marshal cannot mask a stale-cache defect by rejecting its syntax.
			copy((*raw)[len(*raw)-5:], []byte("true}"))
			if !json.Valid(*raw) {
				t.Fatal("mutation witness must remain valid JSON")
			}
			for _, mode := range []string{"in-place", "replacement"} {
				if mode == "replacement" {
					*raw = json.RawMessage(`{"required":null}`)
				}
				out, err := target.MarshalJSON()
				if err != nil || string(out) != "{}" {
					t.Errorf("%s edit bypassed cleanup: %s, %v", mode, out, err)
				}
			}
		})
	}
}
