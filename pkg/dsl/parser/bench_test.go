package parser_test

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
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
	src := readFixtureB(b, "../testdata/skill/minimal_linear.bot")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parseSink = parser.ParseLenient("bench.bot", src)
	}
}

func BenchmarkParse_Medium(b *testing.B) {
	src := readFixtureB(b, "../testdata/pr_refine_single_model.bot")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parseSink = parser.ParseLenient("bench.bot", src)
	}
}

func BenchmarkParse_Large(b *testing.B) {
	src := readFixtureB(b, "../testdata/rust_to_go_port.bot")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parseSink = parser.ParseLenient("bench.bot", src)
	}
}

func BenchmarkParse_Exhaustive(b *testing.B) {
	src := readFixtureB(b, "../testdata/exhaustive_dsl_coverage.bot")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parseSink = parser.ParseLenient("bench.bot", src)
	}
}
