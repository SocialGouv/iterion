package unparse_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/internal/dsltest"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// A studio save is parse → JSON → document → unparse. For every shipped
// bot, example and fixture, the text that comes out of that path is the
// text the writer produces from the parse alone: the transport adds no
// header (an empty block a document never declared) and drops none.
func TestCorpusTextSurvivesTheJSONTransport(t *testing.T) {
	checked := 0
	for _, path := range dsltest.CorpusFiles(t, filepath.Join("..", "..", "..")) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		pr := parser.Parse(path, string(src))
		broken := pr.File == nil
		for _, d := range pr.Diagnostics {
			if d.Severity == parser.SeverityError {
				broken = true
			}
		}
		if broken {
			continue
		}
		want := unparse.Unparse(pr.File)
		data, err := ast.MarshalFile(pr.File)
		if err != nil {
			t.Fatalf("%s: marshal: %v", path, err)
		}
		back, err := ast.UnmarshalFile(data)
		if err != nil {
			t.Fatalf("%s: unmarshal: %v", path, err)
		}
		if got := unparse.Unparse(back); got != want {
			t.Errorf("%s: the JSON transport changed the written text", path)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no corpus file was checked")
	}
}
