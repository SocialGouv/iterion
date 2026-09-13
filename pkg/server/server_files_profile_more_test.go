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

// A document that carries no profile — an older studio build's — must not
// rewrite a profile-2 file in profile 1: the save guard cannot see it (it
// holds the text to the document), so the handler holds the document to the
// file. Refused with 422, the file untouched; the same document with its
// profile saves.
func TestSaveFileRefusesToLowerTheProfile(t *testing.T) {
	srv, hs := newTestServer(t)
	src := "dsl: 2\n\ntool t:\n  command: \"a\\nb\"\n\nworkflow w:\n  entry: t\n  t -> done\n"
	path := filepath.Join(srv.cfg.WorkDir, "keep.bot")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	res := parser.Parse("keep.bot", src)
	doc, err := ast.MarshalFile(res.File)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(doc, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "profile")
	stripped, _ := json.Marshal(m)

	post := func(document []byte) *http.Response {
		body, _ := json.Marshal(map[string]any{"path": "keep.bot", "document": json.RawMessage(document)})
		resp, err := http.Post(hs.URL+"/api/files/save", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := post(stripped)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("save without a profile: status %d, want 422", resp.StatusCode)
	}
	if written, _ := os.ReadFile(path); string(written) != src {
		t.Fatalf("the file was rewritten:\n%s", written)
	}
	resp2 := post(doc)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("save with the profile: status %d", resp2.StatusCode)
	}
	if written, _ := os.ReadFile(path); !strings.HasPrefix(string(written), "dsl: 2\n") {
		t.Fatalf("the saved file lost its profile:\n%s", written)
	}
}
