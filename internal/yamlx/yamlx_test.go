package yamlx

import "testing"

// The standard testing package; the assertions read the effect (decoded
// values, error presence), not a rendered screen.

type sample struct {
	Name  string `yaml:"name"`
	Count int    `yaml:"count"`
}

func TestMarshalUsesTwoSpaceIndent(t *testing.T) {
	t.Parallel()
	got, err := Marshal(sample{Name: "bot", Count: 2})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := "name: bot\ncount: 2\n"
	if string(got) != want {
		t.Fatalf("marshal output:\n%q\nwant:\n%q", got, want)
	}
}

func TestMarshalNestedUsesTwoSpaceIndent(t *testing.T) {
	t.Parallel()
	type inner struct {
		Key string `yaml:"key"`
	}
	got, err := Marshal(map[string]inner{"step": {Key: "v"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := "step:\n  key: v\n"
	if string(got) != want {
		t.Fatalf("nested map must indent two spaces per level, got:\n%q", got)
	}
}

func TestUnmarshalStrictRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	var out sample
	err := UnmarshalStrict([]byte("name: bot\ncount: 2\nbogus: yes\n"), &out)
	if err == nil {
		t.Fatal("strict decode must reject an unknown field, got nil error")
	}
}

func TestUnmarshalStrictAcceptsKnownFields(t *testing.T) {
	t.Parallel()
	var out sample
	if err := UnmarshalStrict([]byte("name: bot\ncount: 2\n"), &out); err != nil {
		t.Fatalf("strict decode of known fields: %v", err)
	}
	if out.Name != "bot" || out.Count != 2 {
		t.Fatalf("decoded %+v, want name=bot count=2", out)
	}
}

func TestUnmarshalIgnoresUnknownFields(t *testing.T) {
	t.Parallel()
	var out sample
	if err := Unmarshal([]byte("name: bot\nbogus: yes\n"), &out); err != nil {
		t.Fatalf("lenient decode with extension field: %v", err)
	}
	if out.Name != "bot" {
		t.Fatalf("decoded %+v, want name=bot", out)
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	in := sample{Name: "bot", Count: 7}
	blob, err := Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out sample
	if err := UnmarshalStrict(blob, &out); err != nil {
		t.Fatalf("strict decode of marshaled value: %v", err)
	}
	if out != in {
		t.Fatalf("round trip: got %+v, want %+v", out, in)
	}
}
