package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

func postDSL(t *testing.T, handler http.HandlerFunc, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// TestParseWithFilesReturnsTheUnitWithProvenance: the cloud editor parses a
// bundle's main in several files by sending the files map; the document
// comes back merged, each declaration naming its file, with the unit's
// revision.
func TestParseWithFilesReturnsTheUnitWithProvenance(t *testing.T) {
	s := &Server{}
	files := map[string]string{"main.bot": unitFixtureMain, "lib/nodes.bot": unitFixtureNodes}
	rec := postDSL(t, s.handleParse, "/api/parse", map[string]any{"files": files, "main": "main.bot"})
	if rec.Code != http.StatusOK {
		t.Fatalf("parse: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Document json.RawMessage `json:"document"`
		Unit     *unitInfo       `json:"unit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Unit == nil || out.Unit.Main != "main.bot" || out.Unit.Root != "" || len(out.Unit.Files) != 2 || out.Unit.Revision != unit.LoadMap(files, "main.bot").Digest {
		t.Fatalf("unit %+v", out.Unit)
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Document, &doc); err != nil {
		t.Fatal(err)
	}
	if agent, _ := agentsOf(doc)[0].(map[string]any); agent["file"] != "lib/nodes.bot" {
		t.Fatalf("the fragment's agent carries %v", agent["file"])
	}
	// A plain source parses as it always did: no unit.
	rec = postDSL(t, s.handleParse, "/api/parse", map[string]any{"source": "workflow w:\n  entry: done\n"})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "\"unit\"") {
		t.Fatalf("plain parse: %d %s", rec.Code, rec.Body.String())
	}
}

// TestUnparseWithFilesReturnsOnlyTheRewrittenFiles: the document of a unit
// is written back by provenance; the response holds the files whose
// program changed and only those, and a document without provenance is
// refused rather than folded into the main.
func TestUnparseWithFilesReturnsOnlyTheRewrittenFiles(t *testing.T) {
	s := &Server{}
	files := map[string]string{"main.bot": unitFixtureMain, "lib/nodes.bot": unitFixtureNodes, "manifest.yaml": "name: demo\n"}
	rec := postDSL(t, s.handleParse, "/api/parse", map[string]any{"files": files, "main": "main.bot"})
	var opened struct {
		Document json.RawMessage `json:"document"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &opened); err != nil {
		t.Fatal(err)
	}
	edited := editDocument(t, opened.Document, func(m map[string]any) {
		agentsOf(m)[0].(map[string]any)["model"] = "anthropic/claude-opus-5"
	})
	rec = postDSL(t, s.handleUnparse, "/api/unparse", map[string]any{"document": edited, "files": files, "main": "main.bot"})
	if rec.Code != http.StatusOK {
		t.Fatalf("unparse: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Source string            `json:"source"`
		Files  map[string]string `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Files) != 1 || !strings.Contains(out.Files["lib/nodes.bot"], "anthropic/claude-opus-5") || strings.Contains(out.Files["lib/nodes.bot"], "import ") {
		t.Fatalf("rewritten files %v", out.Files)
	}
	if out.Source != unitFixtureMain {
		t.Fatalf("source %q, want the main untouched", out.Source)
	}
	stripped := editDocument(t, opened.Document, func(m map[string]any) { dropKey(m, "file") })
	rec = postDSL(t, s.handleUnparse, "/api/unparse", map[string]any{"document": stripped, "files": files, "main": "main.bot"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no provenance: %d %s", rec.Code, rec.Body.String())
	}
}

// TestBundleValidationReadsUnits: a cloud bundle in several files compiles
// as its unit at write time, a fragment is validated through the main that
// imports it, and a broken fragment is reported against the main.
func TestBundleValidationReadsUnits(t *testing.T) {
	files := map[string]string{"main.bot": unitFixtureMain, "lib/nodes.bot": unitFixtureNodes, "manifest.yaml": "name: demo\n"}
	if diags := validateBundleCompileSelected(files, []string{"lib/nodes.bot"}); len(diags) != 0 {
		t.Fatalf("a bot in several files does not validate: %v", diags)
	}
	files["lib/nodes.bot"] = "agent worker:\n  model: \"anthropic/claude-opus-4-8\"\n  description: \"x\"\n  bogus_prop: 1\n"
	diags := validateBundleCompileSelected(files, nil)
	if len(diags) == 0 || !strings.Contains(diags[0], "nodes.bot") {
		t.Fatalf("a broken fragment was not reported through the main: %v", diags)
	}
}
