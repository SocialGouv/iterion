package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A region the pattern cannot take whole is an ERROR, never a fresh no-op:
// a begin marker without its end left the document untouched and reported
// fresh, and with two regions the pattern ran from the first begin to the
// second end and the rewrite deleted the prose between them.
func TestSpliceRefusesUnbalancedRegions(t *testing.T) {
	good := "intro\n<!-- dsl-spec:begin skill -->\nstale\n<!-- dsl-spec:end -->\nmiddle prose\n<!-- dsl-spec:begin table budget -->\nstale\n<!-- dsl-spec:end -->\nend\n"
	out, err := Splice(good)
	if err != nil {
		t.Fatalf("balanced document refused: %v", err)
	}
	if !strings.Contains(out, "middle prose") || !strings.Contains(out, "max_cost_usd") || strings.Contains(out, "stale") {
		t.Fatalf("balanced document not spliced as expected:\n%s", out)
	}
	cases := map[string]string{
		"single region, no end":    "<!-- dsl-spec:begin skill -->\nTOTAL GARBAGE\n",
		"first end missing of two": "<!-- dsl-spec:begin skill -->\nx\nmiddle prose\n<!-- dsl-spec:begin table budget -->\ny\n<!-- dsl-spec:end -->\n",
		"end without begin":        "prose\n<!-- dsl-spec:end -->\n",
		"crlf line ends":           "<!-- dsl-spec:begin skill -->\r\nx\r\n<!-- dsl-spec:end -->\r\n",
		"double space in header":   "<!-- dsl-spec:begin  skill -->\nx\n<!-- dsl-spec:end -->\n",
		"unknown region":           "<!-- dsl-spec:begin tables -->\nx\n<!-- dsl-spec:end -->\n",
	}
	for name, doc := range cases {
		if out, err := Splice(doc); err == nil {
			t.Errorf("%s: accepted, produced %q", name, out)
		}
	}
}

// Regenerate leaves every file as it was when one of them cannot be spliced.
func TestRegenerateIsAllOrNothing(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i, rel := range Files {
		body := "<!-- dsl-spec:begin skill -->\nstale\n<!-- dsl-spec:end -->\n"
		if i == len(Files)-1 {
			body = "<!-- dsl-spec:begin skill -->\nno end marker\n" // the last file is broken
		}
		write(rel, body)
	}
	changed, err := Regenerate(root)
	if err == nil {
		t.Fatal("a broken document did not fail Regenerate")
	}
	if len(changed) != 0 {
		t.Fatalf("files were rewritten before the failure: %v", changed)
	}
	for _, rel := range Files[:len(Files)-1] {
		raw, _ := os.ReadFile(filepath.Join(root, rel))
		if !strings.Contains(string(raw), "stale") {
			t.Errorf("%s was rewritten although another document failed", rel)
		}
	}
}
