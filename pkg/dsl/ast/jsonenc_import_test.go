package ast

import (
	"encoding/json"
	"strings"
	"testing"
)

// The imports travel through the JSON document as their paths, in order,
// and come back as declarations; a file without any carries no key.
func TestImportsRoundTripThroughJSON(t *testing.T) {
	f := &File{Profile: 2, Imports: []*ImportDecl{{Path: "lib/a.bot"}, {Path: "lib/b.bot"}}}
	data, err := MarshalFile(f)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if got, ok := doc["imports"].([]any); !ok || len(got) != 2 || got[0] != "lib/a.bot" || got[1] != "lib/b.bot" {
		t.Fatalf("imports not in the document as their paths: %s", data)
	}
	back, err := UnmarshalFile(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Imports) != 2 || back.Imports[0].Path != "lib/a.bot" || back.Imports[1].Path != "lib/b.bot" {
		t.Fatalf("imports back: %+v", back.Imports)
	}
	plain, _ := MarshalFile(&File{Profile: 2})
	if strings.Contains(string(plain), "imports") {
		t.Fatalf("a file without imports carries the key: %s", plain)
	}
}
