package unparse

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func TestUnparsePresets_AlphabeticalOrder(t *testing.T) {
	f := &ast.File{
		Presets: &ast.PresetsBlock{
			Entries: []*ast.Preset{
				{
					Name: "prod",
					Values: []*ast.PresetValue{
						{Key: "api_url", Value: &ast.Literal{Kind: ast.LitString, StrVal: "https://prod"}},
					},
				},
				{
					Name: "dev",
					Values: []*ast.PresetValue{
						{Key: "api_url", Value: &ast.Literal{Kind: ast.LitString, StrVal: "http://localhost"}},
						{Key: "debug", Value: &ast.Literal{Kind: ast.LitBool, BoolVal: true}},
					},
				},
			},
		},
	}
	out := Unparse(f)
	// dev should appear before prod (alphabetical order for determinism).
	devIdx := strings.Index(out, "  dev:")
	prodIdx := strings.Index(out, "  prod:")
	if devIdx < 0 || prodIdx < 0 {
		t.Fatalf("missing preset entries in output:\n%s", out)
	}
	if devIdx > prodIdx {
		t.Errorf("dev should appear before prod (alphabetical), got dev=%d prod=%d", devIdx, prodIdx)
	}
	for _, want := range []string{
		"presets:",
		"  dev:",
		`    api_url: "http://localhost"`,
		"    debug: true",
		"  prod:",
		`    api_url: "https://prod"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull output:\n%s", want, out)
		}
	}
}

func TestUnparsePresets_RoundtripsThroughParser(t *testing.T) {
	src := `vars:
  api_url: string
  debug: bool
  retries: int

presets:
  dev:
    api_url: "http://localhost:8080"
    debug: true
    retries: 1
  prod:
    api_url: "https://api.example.com"
    debug: false
    retries: 5
`
	res := parser.Parse("test.iter", src)
	for _, d := range res.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("initial parse error: %s", d.Error())
		}
	}
	out := Unparse(res.File)

	// Re-parse the unparsed output; should be diagnostic-free.
	res2 := parser.Parse("test.iter.roundtrip", out)
	for _, d := range res2.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("re-parse error: %s\nunparsed:\n%s", d.Error(), out)
		}
	}
	if res2.File.Presets == nil || len(res2.File.Presets.Entries) != 2 {
		t.Fatalf("round-trip lost presets:\n%s", out)
	}
}
