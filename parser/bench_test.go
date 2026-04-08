package parser_test

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/parser"
)

func readFixtureB(b *testing.B, path string) string {
	b.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	return string(data)
}

var parseSink *parser.ParseResult

func BenchmarkParse_Minimal(b *testing.B) {
	src := readFixtureB(b, "../examples/skill/minimal_linear.iter")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parseSink = parser.Parse("bench.iter", src)
	}
}

func BenchmarkParse_Medium(b *testing.B) {
	src := readFixtureB(b, "../examples/pr_refine_single_model.iter")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parseSink = parser.Parse("bench.iter", src)
	}
}

func BenchmarkParse_Large(b *testing.B) {
	src := readFixtureB(b, "../examples/rust_to_go_port.iter")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parseSink = parser.Parse("bench.iter", src)
	}
}

func BenchmarkParse_Exhaustive(b *testing.B) {
	src := readFixtureB(b, "../examples/exhaustive_dsl_coverage.iter")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parseSink = parser.Parse("bench.iter", src)
	}
}
