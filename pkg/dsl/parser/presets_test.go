package parser_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func TestPresetsBlock_Basic(t *testing.T) {
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
	assertNoDiags(t, res)

	pb := res.File.Presets
	if pb == nil || len(pb.Entries) != 2 {
		t.Fatalf("expected 2 presets, got %v", pb)
	}

	dev := pb.Entries[0]
	assertEq(t, "dev.Name", dev.Name, "dev")
	if len(dev.Values) != 3 {
		t.Fatalf("dev: expected 3 values, got %d", len(dev.Values))
	}
	assertEq(t, "dev.api_url.Key", dev.Values[0].Key, "api_url")
	assertEq(t, "dev.api_url.StrVal", dev.Values[0].Value.StrVal, "http://localhost:8080")
	assertEq(t, "dev.debug.BoolVal", dev.Values[1].Value.BoolVal, true)
	assertEq(t, "dev.retries.IntVal", dev.Values[2].Value.IntVal, int64(1))

	prod := pb.Entries[1]
	assertEq(t, "prod.Name", prod.Name, "prod")
	assertEq(t, "prod.api_url.StrVal", prod.Values[0].Value.StrVal, "https://api.example.com")
	assertEq(t, "prod.debug.BoolVal", prod.Values[1].Value.BoolVal, false)
	assertEq(t, "prod.retries.IntVal", prod.Values[2].Value.IntVal, int64(5))
}

func TestPresetsBlock_MixedTypes(t *testing.T) {
	src := `vars:
  ratio: float
  label: string

presets:
  fast:
    ratio: 0.95
    label: "speed"
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	pb := res.File.Presets
	if pb == nil || len(pb.Entries) != 1 {
		t.Fatalf("expected 1 preset, got %v", pb)
	}
	fast := pb.Entries[0]
	if got := fast.Values[0].Value.FloatVal; got != 0.95 {
		t.Errorf("ratio = %v, want 0.95", got)
	}
}
