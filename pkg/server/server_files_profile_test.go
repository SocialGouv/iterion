package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The studio saves a document through the JSON transport: a file opened in
// profile 2 must land on disk in profile 2 — header first, standard
// escapes — or the next parse would read its strings in profile 1 and the
// save guard would not notice (the same AST compiles the same in either).
func TestSaveFileKeepsTheProfileTwoHeader(t *testing.T) {
	srv, hs := newTestServer(t)
	src := "dsl: 2\n\ntool t:\n  command: \"a\\nb\"\n\nworkflow w:\n  entry: t\n  t -> done\n"
	res := parser.Parse("v2.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("fixture: %v", res.Diagnostics)
	}
	doc, err := ast.MarshalFile(res.File)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"path": "v2.bot", "document": json.RawMessage(doc)})
	resp, err := http.Post(hs.URL+"/api/files/save", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save: status %d", resp.StatusCode)
	}
	written, err := os.ReadFile(filepath.Join(srv.cfg.WorkDir, "v2.bot"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(written), "dsl: 2\n") {
		t.Fatalf("the saved file lost its profile:\n%s", written)
	}
	back := parser.Parse("v2.bot", string(written))
	if len(back.Diagnostics) != 0 || back.File.EffectiveProfile() != 2 {
		t.Fatalf("the saved file does not read as profile 2: %v", back.Diagnostics)
	}
	if got := back.File.Tools[0].Command; got != "a\nb" {
		t.Fatalf("the saved command reads as %q", got)
	}
}
