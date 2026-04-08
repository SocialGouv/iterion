package ir

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/parser"
)

func parseFixtureB(b *testing.B, path string) *parser.ParseResult {
	b.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	res := parser.Parse("bench.iter", string(data))
	if res.File == nil {
		b.Fatal("parse failed")
	}
	return res
}

var compileSink *CompileResult

func BenchmarkCompile_Minimal(b *testing.B) {
	pr := parseFixtureB(b, "../examples/skill/minimal_linear.iter")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		compileSink = Compile(pr.File)
	}
}

func BenchmarkCompile_Medium(b *testing.B) {
	pr := parseFixtureB(b, "../examples/pr_refine_single_model.iter")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		compileSink = Compile(pr.File)
	}
}

func BenchmarkCompile_Large(b *testing.B) {
	pr := parseFixtureB(b, "../examples/rust_to_go_port.iter")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		compileSink = Compile(pr.File)
	}
}

func BenchmarkCompile_Exhaustive(b *testing.B) {
	pr := parseFixtureB(b, "../examples/exhaustive_dsl_coverage.iter")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		compileSink = Compile(pr.File)
	}
}
