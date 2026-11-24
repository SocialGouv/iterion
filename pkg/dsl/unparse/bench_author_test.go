package unparse

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// benchSink keeps the benchmark's last product alive past the loop.
var (
	benchSink string
	benchFile *ast.File
)

// The written-form proof every author-document validation pays
// (pkg/cli/validate_author.go): Unparse turns the program back into `.bot`
// text and Verify re-parses that text and compiles BOTH sides — the cost
// the E054 decision in docs/cli-reference.md records. The targets are the
// catalogue's largest programs, read by repo-relative path the way the
// docs-snippets sweep in pkg/dsl/ir already reaches the tree.

func benchRead(b *testing.B, rel string) (string, *ast.File) {
	b.Helper()
	src, err := os.ReadFile(rel)
	if err != nil {
		b.Fatal(err)
	}
	pr, err := parser.Parse(rel, string(src))
	if err != nil {
		b.Fatal(err)
	}
	return string(src), pr.File
}

// BenchmarkAuthorProof measures the proof alone on one catalogue program.
func benchAuthorProof(b *testing.B, rel string) {
	b.Helper()
	_, f := benchRead(b, rel)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		text := Unparse(f)
		if err := Verify(f, text); err != nil {
			b.Fatal(err)
		}
		benchSink = text
	}
}

// BenchmarkAuthorParse is the denominator: the stage's own parse of the
// same program, so the decision quotes a ratio and not a vibe.
func benchAuthorParse(b *testing.B, rel string) {
	b.Helper()
	src, _ := benchRead(b, rel)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pr, err := parser.Parse(rel, src)
		if err != nil {
			b.Fatal(err)
		}
		benchFile = pr.File
	}
}

// BenchmarkAuthorProof_GoldenMasterMain measures the proof on the
// catalogue's largest program.
func BenchmarkAuthorProof_GoldenMasterMain(b *testing.B) {
	benchAuthorProof(b, "../../../bots/golden-master/main.bot")
}

// BenchmarkAuthorProof_GoldenMasterSyncHarness measures the proof on the
// catalogue's second-largest program.
func BenchmarkAuthorProof_GoldenMasterSyncHarness(b *testing.B) {
	benchAuthorProof(b, "../../../bots/golden-master/sync-harness.bot")
}

// BenchmarkAuthorProof_ProdWatch measures the proof on a mid-size
// catalogue program.
func BenchmarkAuthorProof_ProdWatch(b *testing.B) {
	benchAuthorProof(b, "../../../bots/prod-watch/main.bot")
}

// BenchmarkAuthorParse_GoldenMasterMain measures the stage's own parse of
// the largest program, the ratio's denominator.
func BenchmarkAuthorParse_GoldenMasterMain(b *testing.B) {
	benchAuthorParse(b, "../../../bots/golden-master/main.bot")
}
