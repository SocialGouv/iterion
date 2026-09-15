package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

const unitFixtureMain = "import \"lib/nodes.bot\"\n\nworkflow w:\n  entry: worker\n  worker -> done\n"

// Pinned model and backend: the compile refuses C018 on a credential-less
// host, and these tests measure the save.
const unitFixtureNodes = "schema out:\n  ok: bool\n\nprompt mission:\n  Do the thing.\n\nagent worker:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: mission\n  output: out\n"

func writeUnitFixture(t *testing.T, workdir string, files map[string]string) {
	t.Helper()
	for rel, src := range files {
		full := filepath.Join(workdir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

type openedUnit struct {
	Source      string          `json:"source"`
	Document    json.RawMessage `json:"document"`
	Diagnostics []string        `json:"diagnostics"`
	Unit        *unitInfo       `json:"unit"`
}

func openPath(t *testing.T, s *Server, path string) (*httptest.ResponseRecorder, openedUnit) {
	t.Helper()
	body, _ := json.Marshal(openFileRequest{Path: path})
	req := httptest.NewRequest(http.MethodPost, "/api/files/open", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleOpenFile(rec, req)
	var out openedUnit
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode open response: %v\n%s", err, rec.Body.String())
		}
	}
	return rec, out
}

func savePath(t *testing.T, s *Server, path string, document json.RawMessage, revision string) (*httptest.ResponseRecorder, saveFileResponse) {
	t.Helper()
	body, _ := json.Marshal(saveFileRequest{Path: path, Document: document, Revision: revision})
	req := httptest.NewRequest(http.MethodPost, "/api/files/save", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleSaveFile(rec, req)
	var out saveFileResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode save response: %v\n%s", err, rec.Body.String())
		}
	}
	return rec, out
}

// editDocument decodes a document, lets edit change it, and re-encodes it.
func editDocument(t *testing.T, doc json.RawMessage, edit func(map[string]any)) json.RawMessage {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(doc, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func agentsOf(m map[string]any) []any {
	agents, _ := m["agents"].([]any)
	return agents
}

func readFixture(t *testing.T, workdir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(workdir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestOpenAUnitReturnsTheMergedDocumentWithProvenanceAndRevision: a bot in
// several files opens as its unit — one document, each declaration naming
// its file — with the unit's files and its revision; a bot in one file
// opens as it always did, with no unit at all.
func TestOpenAUnitReturnsTheMergedDocumentWithProvenanceAndRevision(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes, "single.bot": "workflow s:\n  entry: done\n"})
	s := &Server{cfg: Config{WorkDir: workdir}}

	rec, opened := openPath(t, s, "demo/main.bot")
	if rec.Code != http.StatusOK {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	if opened.Unit == nil || opened.Unit.Main != "main.bot" || opened.Unit.Root != "demo" || len(opened.Unit.Files) != 2 || opened.Unit.Files[0].Imports[0] != "lib/nodes.bot" {
		t.Fatalf("unit %+v", opened.Unit)
	}
	if want := unit.LoadDir(filepath.Join(workdir, "demo", "main.bot")).Digest; opened.Unit.Revision != want {
		t.Fatalf("revision %q, want the unit's digest %q", opened.Unit.Revision, want)
	}
	if opened.Source != unitFixtureMain {
		t.Fatalf("source %q, want the main's text", opened.Source)
	}
	var doc map[string]any
	if err := json.Unmarshal(opened.Document, &doc); err != nil {
		t.Fatal(err)
	}
	agent, _ := agentsOf(doc)[0].(map[string]any)
	if agent["name"] != "worker" || agent["file"] != "lib/nodes.bot" {
		t.Fatalf("the fragment's agent %v carries no provenance", agent)
	}
	if _, has := doc["imports"]; has {
		t.Fatal("the merged document still carries import lines")
	}

	rec, single := openPath(t, s, "single.bot")
	if rec.Code != http.StatusOK || single.Unit != nil {
		t.Fatalf("a bot in one file opened as a unit: %d %+v", rec.Code, single.Unit)
	}
}

// TestSaveAUnitRewritesOnlyTheFileThatChanged: an edit to a declaration
// the fragment holds rewrites the fragment and leaves the main byte for
// byte as it was; saving the same document again rewrites nothing.
func TestSaveAUnitRewritesOnlyTheFileThatChanged(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")

	edited := editDocument(t, opened.Document, func(m map[string]any) {
		agentsOf(m)[0].(map[string]any)["model"] = "anthropic/claude-opus-5"
	})
	rec, saved := savePath(t, s, "demo/main.bot", edited, opened.Unit.Revision)
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	if len(saved.Files) != 1 || saved.Files[0] != "lib/nodes.bot" {
		t.Fatalf("files written %v, want the fragment alone", saved.Files)
	}
	if got := readFixture(t, workdir, "demo/lib/nodes.bot"); !strings.Contains(got, "anthropic/claude-opus-5") || strings.Contains(got, "import ") {
		t.Fatalf("fragment after save:\n%s", got)
	}
	if got := readFixture(t, workdir, "demo/main.bot"); got != unitFixtureMain {
		t.Fatalf("the main was rewritten:\n%s", got)
	}
	if want := unit.LoadDir(filepath.Join(workdir, "demo", "main.bot")).Digest; saved.Revision != want || saved.Revision == opened.Unit.Revision {
		t.Fatalf("revision after save %q, want the new digest %q", saved.Revision, want)
	}
	// The same program again: nothing to write.
	rec, again := savePath(t, s, "demo/main.bot", edited, saved.Revision)
	if rec.Code != http.StatusOK || len(again.Files) != 0 || again.Revision != saved.Revision {
		t.Fatalf("a no-op save: %d files %v revision %q", rec.Code, again.Files, again.Revision)
	}
}

// TestSaveAUnitRefusesAStaleOrMissingRevision: a file of the unit edited
// on disk since the document was opened is a conflict, never overwritten;
// a document that names no revision comes from a client that would save
// the main alone, and is refused.
func TestSaveAUnitRefusesAStaleOrMissingRevision(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")
	edited := editDocument(t, opened.Document, func(m map[string]any) {
		agentsOf(m)[0].(map[string]any)["model"] = "anthropic/claude-opus-5"
	})

	onDisk := unitFixtureNodes + "\n## edited on disk meanwhile\n"
	writeUnitFixture(t, workdir, map[string]string{"demo/lib/nodes.bot": onDisk})
	if rec, _ := savePath(t, s, "demo/main.bot", edited, opened.Unit.Revision); rec.Code != http.StatusConflict {
		t.Fatalf("stale revision: %d %s", rec.Code, rec.Body.String())
	}
	if got := readFixture(t, workdir, "demo/lib/nodes.bot"); got != onDisk {
		t.Fatalf("the on-disk edit was overwritten:\n%s", got)
	}
	if rec, _ := savePath(t, s, "demo/main.bot", edited, ""); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing revision: %d %s", rec.Code, rec.Body.String())
	}
	if got := readFixture(t, workdir, "demo/lib/nodes.bot"); got != onDisk {
		t.Fatalf("a save without revision wrote:\n%s", got)
	}
}

// TestSaveAUnitRefusesADocumentWithoutProvenance: a document of a bot in
// several files that names no file would fold every file into the main
// and empty the fragments; it is refused, and so is a document with
// provenance headed for a bot in one file.
func TestSaveAUnitRefusesADocumentWithoutProvenance(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes, "single.bot": "workflow s:\n  entry: done\n"})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")

	stripped := editDocument(t, opened.Document, func(m map[string]any) { dropKey(m, "file") })
	if rec, _ := savePath(t, s, "demo/main.bot", stripped, opened.Unit.Revision); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no provenance: %d %s", rec.Code, rec.Body.String())
	}
	if readFixture(t, workdir, "demo/lib/nodes.bot") != unitFixtureNodes || readFixture(t, workdir, "demo/main.bot") != unitFixtureMain {
		t.Fatal("a document without provenance was written")
	}
	if rec, _ := savePath(t, s, "single.bot", opened.Document, ""); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("provenance onto a single file: %d %s", rec.Code, rec.Body.String())
	}
	if readFixture(t, workdir, "single.bot") != "workflow s:\n  entry: done\n" {
		t.Fatal("the single file was overwritten by a unit's document")
	}
}

func dropKey(v any, key string) {
	switch vv := v.(type) {
	case map[string]any:
		delete(vv, key)
		for _, e := range vv {
			dropKey(e, key)
		}
	case []any:
		for _, e := range vv {
			dropKey(e, key)
		}
	}
}

// TestANewDeclarationLandsInTheMain: a declaration the editor adds names
// no file; it is written to the main, and the fragment is left alone.
func TestANewDeclarationLandsInTheMain(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")
	edited := editDocument(t, opened.Document, func(m map[string]any) {
		m["agents"] = append(agentsOf(m), map[string]any{"name": "extra", "backend": "claude_code", "model": "anthropic/claude-opus-4-8", "description": "added in the editor"})
	})
	rec, saved := savePath(t, s, "demo/main.bot", edited, opened.Unit.Revision)
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	if len(saved.Files) != 1 || saved.Files[0] != "main.bot" {
		t.Fatalf("files written %v, want the main alone", saved.Files)
	}
	main := readFixture(t, workdir, "demo/main.bot")
	if !strings.Contains(main, "agent extra:") || !strings.Contains(main, "import \"lib/nodes.bot\"") {
		t.Fatalf("main after save:\n%s", main)
	}
	if readFixture(t, workdir, "demo/lib/nodes.bot") != unitFixtureNodes {
		t.Fatal("the fragment was rewritten")
	}
	if saved.Source != main {
		t.Fatalf("the response's source is not the main written:\n%s", saved.Source)
	}
}

// TestSaveAUnitRefusesToBreakASiblingImporter: a fragment two mains import
// is validated against the other main before it is written; a rename that
// leaves the sibling referencing a node that no longer exists is refused,
// naming the sibling, and nothing is written.
func TestSaveAUnitRefusesToBreakASiblingImporter(t *testing.T) {
	workdir := t.TempDir()
	sibling := "import \"lib/nodes.bot\"\n\nworkflow other:\n  entry: worker\n  worker -> done\n"
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes, "demo/other.bot": sibling})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")
	renamed := editDocument(t, opened.Document, func(m map[string]any) {
		agentsOf(m)[0].(map[string]any)["name"] = "worker2"
		wf := m["workflows"].([]any)[0].(map[string]any)
		wf["entry"] = "worker2"
		for _, e := range wf["edges"].([]any) {
			edge := e.(map[string]any)
			if edge["from"] == "worker" {
				edge["from"] = "worker2"
			}
		}
	})
	rec, _ := savePath(t, s, "demo/main.bot", renamed, opened.Unit.Revision)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "other.bot") {
		t.Fatalf("breaking a sibling: %d %s", rec.Code, rec.Body.String())
	}
	if readFixture(t, workdir, "demo/lib/nodes.bot") != unitFixtureNodes || readFixture(t, workdir, "demo/main.bot") != unitFixtureMain {
		t.Fatal("a save that breaks a sibling wrote files")
	}
}

// TestSaveAUnitRollsBackWhenAWriteFails: a save that rewrites two files
// and fails on the second leaves both as they were — the first, already
// published, is rolled back — and says so.
func TestSaveAUnitRollsBackWhenAWriteFails(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")
	edited := editDocument(t, opened.Document, func(m map[string]any) {
		agentsOf(m)[0].(map[string]any)["model"] = "anthropic/claude-opus-5"
		m["agents"] = append(agentsOf(m), map[string]any{"name": "extra", "backend": "claude_code", "model": "anthropic/claude-opus-4-8", "description": "added in the editor"})
	})
	publishes := 0
	authoringStepHook = func(stage string) error {
		if stage == "publish" {
			publishes++
			if publishes == 2 {
				return errors.New("injected: disk full")
			}
		}
		return nil
	}
	t.Cleanup(func() { authoringStepHook = nil })
	rec, _ := savePath(t, s, "demo/main.bot", edited, opened.Unit.Revision)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "rolled back") {
		t.Fatalf("failed write: %d %s", rec.Code, rec.Body.String())
	}
	if publishes != 2 {
		t.Fatalf("the fault did not fire on the second publish (%d publishes): the test proves nothing", publishes)
	}
	if readFixture(t, workdir, "demo/lib/nodes.bot") != unitFixtureNodes || readFixture(t, workdir, "demo/main.bot") != unitFixtureMain {
		t.Fatalf("a failed save left a file changed:\n%s\n%s", readFixture(t, workdir, "demo/main.bot"), readFixture(t, workdir, "demo/lib/nodes.bot"))
	}
}
