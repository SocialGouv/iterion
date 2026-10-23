package ir

import (
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func parseFixtureB(b *testing.B, path string) *parser.ParseResult {
	b.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	res := parsertest.Parse("bench.bot", string(data))
	if res.File == nil {
		b.Fatal("parse failed")
	}
	return res
}

var compileSink *CompileResult

func BenchmarkCompile_Minimal(b *testing.B) {
	pr := parseFixtureB(b, "../testdata/skill/minimal_linear.bot")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		compileSink = Compile(pr.File)
	}
}

func BenchmarkCompile_Medium(b *testing.B) {
	pr := parseFixtureB(b, "../testdata/pr_refine_single_model.bot")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		compileSink = Compile(pr.File)
	}
}

func BenchmarkCompile_Large(b *testing.B) {
	pr := parseFixtureB(b, "../testdata/rust_to_go_port.bot")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		compileSink = Compile(pr.File)
	}
}

func BenchmarkCompile_Exhaustive(b *testing.B) {
	pr := parseFixtureB(b, "../testdata/exhaustive_dsl_coverage.bot")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		compileSink = Compile(pr.File)
	}
}
