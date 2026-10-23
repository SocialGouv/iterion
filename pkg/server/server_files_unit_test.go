package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/runview"
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
	// False when the file did not parse: the document is the salvage, and
	// the studio marks the buffer so the sites that write it refuse.
	Bindable bool `json:"bindable"`
	// Set when a bound of the reader made the document the salvage.
	ReadRefusal *readRefusalInfo `json:"read_refusal"`
	// The digest the open issued (the digest rule): present only because
	// this open read the file whole.
	Digest string `json:"digest"`
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
	return savePathClaimed(t, s, path, document, revision, nil)
}

// digestOfFile is the digest a well-formed client presents for the file as
// it stands: sha256 of its current bytes.
func digestOfFile(t *testing.T, workdir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(workdir, filepath.FromSlash(rel)))
	if errors.Is(err, os.ErrNotExist) {
		return "" // a creation decides on the absence; no digest to present
	}
	if err != nil {
		t.Fatalf("read %s for its digest: %v", rel, err)
	}
	return contentSHA256(string(b))
}

// savePathClaimed is savePath carrying the unit's file list as the client
// holds it (unit_files) — what a save after a per-file edit of a header
// presents, so the file's profile and import lines are written from the
// claim rather than rebuilt from the stored files.
func savePathClaimed(t *testing.T, s *Server, path string, document json.RawMessage, revision string, unitFiles []unitFileInfo) (*httptest.ResponseRecorder, saveFileResponse) {
	t.Helper()
	req := saveFileRequest{Path: path, Document: document, Revision: revision, UnitFiles: unitFiles}
	if revision == "" && len(unitFiles) == 0 && !req.CreateOnly {
		// A single-file save presents the digest of the file as it stands —
		// the well-formed client, which saved from a whole read of it (a
		// file that is not there is a creation, decided on its absence). A
		// test that means to present a stale digest, or none, marshals its
		// own saveFileRequest.
		req.Digest = digestOfFile(t, s.cfg.WorkDir, path)
	}
	body, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/api/files/save", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleSaveFile(rec, r)
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
	if want := unittest.Whole(unit.LoadDir(filepath.Join(workdir, "demo", "main.bot"))).Digest; opened.Unit.Revision != want {
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
	if want := unittest.Whole(unit.LoadDir(filepath.Join(workdir, "demo", "main.bot"))).Digest; saved.Revision != want || saved.Revision == opened.Unit.Revision {
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

// A fragment whose parse errors the loader reports under its ABSOLUTE path:
// the fixture of the root-cut tests below (#1934).
const brokenFragmentFixture = "prompt p:\n  hi\n\nagent \n  model\n"

// A fragment whose parse leaves SEVERAL errors — a `model` line missing
// its colon, the shape #1532 captured — so a verdict that counts them is
// told from one that reports a bare "broken".
const twoErrorFragmentFixture = "prompt p:\n  hi\n\nagent worker:\n  model \"anthropic/claude-opus-4-8\"\n"

// unitWire is a unit answer as the STUDIO reads it off the wire. The
// per-file parse verdict is asserted on the JSON because that is what
// crosses to the launch gate: a Go field read back through unitInfo would
// pass on a verdict the handler never marshalled.
type unitWire struct {
	Bindable    bool     `json:"bindable"`
	Diagnostics []string `json:"diagnostics"`
	Unit        *struct {
		Root  string `json:"root"`
		Main  string `json:"main"`
		Files []struct {
			Rel         string   `json:"rel"`
			Imports     []string `json:"imports"`
			ParseErrors int      `json:"parse_errors"`
			ParseError  string   `json:"parse_error"`
		} `json:"files"`
		Unread     []string `json:"unread"`
		LoadErrors int      `json:"load_errors"`
		LoadError  string   `json:"load_error"`
	} `json:"unit"`
}

// unitFilePath names a unit file the way the studio's watcher does from an
// answer's root and a rel (studio/src/hooks/unitFiles.ts), so an assertion
// about a path the client rebuilds is made on that rebuilding and not on a
// string the test shapes itself.
func unitFilePath(root, rel string) string {
	if root == "" || root == "." {
		return rel
	}
	return root + "/" + rel
}

func decodeUnitWire(t *testing.T, rec *httptest.ResponseRecorder) unitWire {
	t.Helper()
	var out unitWire
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode unit answer: %v\n%s", err, rec.Body.String())
	}
	if out.Unit == nil {
		t.Fatalf("the answer carries no unit: %s", rec.Body.String())
	}
	return out
}

// fileWire is one file's entry of a unit answer, by its relative path.
func fileWire(t *testing.T, w unitWire, rel string) (parseErrors int, parseError string) {
	t.Helper()
	for _, f := range w.Unit.Files {
		if f.Rel == rel {
			return f.ParseErrors, f.ParseError
		}
	}
	t.Fatalf("the unit's file list holds no %s: %+v", rel, w.Unit.Files)
	return 0, ""
}

// errorDiagnostics keeps the answered diagnostics that are errors — the
// list the launch refuses on, as the client reads it. The verdicts are
// cross-checked against it, so neither side is the other's
// reimplementation.
func errorDiagnostics(diags []string) []string {
	var out []string
	for _, d := range diags {
		if strings.Contains(d, ": error [") {
			out = append(out, d)
		}
	}
	return out
}

// errorDiagnosticsOf is errorDiagnostics narrowed to one file's own.
func errorDiagnosticsOf(diags []string, rel string) []string {
	var out []string
	for _, d := range errorDiagnostics(diags) {
		if strings.HasPrefix(d, rel+":") {
			out = append(out, d)
		}
	}
	return out
}

// TestOpenAUnitReportsEachFilesParseVerdict: a bot in several files whose
// FRAGMENT does not parse opens bindable — the main parsed — so no salvage
// flag refuses the launch, and the diagnostics that say which file is
// broken are rewritten the moment auto-validation answers on the merged
// document. The unit's file list carries the verdict per file, which no
// later answer about the DOCUMENT can take away (#1532).
func TestOpenAUnitReportsEachFilesParseVerdict(t *testing.T) {
	workdir := t.TempDir()
	main := "import \"lib/broken.bot\"\n\nworkflow w:\n  entry: done\n"
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": main, "demo/lib/broken.bot": twoErrorFragmentFixture})
	s := &Server{cfg: Config{WorkDir: workdir}}

	rec, _ := openPath(t, s, "demo/main.bot")
	if rec.Code != http.StatusOK {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	w := decodeUnitWire(t, rec)
	if !w.Bindable {
		t.Fatalf("the fixture is not the case under test: the main itself did not parse (%v)", w.Diagnostics)
	}
	if n, first := fileWire(t, w, "main.bot"); n != 0 || first != "" {
		t.Fatalf("the main parses, and is reported broken: %d %q", n, first)
	}
	want := errorDiagnosticsOf(w.Diagnostics, "lib/broken.bot")
	if len(want) < 2 {
		t.Fatalf("the fixture is not the case under test: %d answered errors in the fragment (%v)", len(want), w.Diagnostics)
	}
	n, first := fileWire(t, w, "lib/broken.bot")
	if n != len(want) {
		t.Fatalf("the fragment's verdict counts %d errors, the answer carries %d: %v", n, len(want), want)
	}
	if first != want[0] {
		t.Fatalf("the fragment's first error is %q, the answer's first is %q", first, want[0])
	}
	if strings.Contains(first, workdir) {
		t.Fatalf("the verdict discloses the server's directory layout: %s", first)
	}

	// The other side of the same answer: `bindable` is the MAIN's parse, so
	// a main that does not parse is reported by both — the verdict agrees
	// with the flag that already existed, rather than contradicting it.
	brokenMain := "import \"lib/broken.bot\"\n\nworkflow w:\n  entry: done\n\nagent \n  model\n"
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": brokenMain})
	rec, _ = openPath(t, s, "demo/main.bot")
	if rec.Code != http.StatusOK {
		t.Fatalf("open a unit whose main does not parse: %d %s", rec.Code, rec.Body.String())
	}
	w = decodeUnitWire(t, rec)
	if w.Bindable {
		t.Fatalf("the fixture is not the case under test: the broken main parsed (%v)", w.Diagnostics)
	}
	if n, _ := fileWire(t, w, "main.bot"); n != len(errorDiagnosticsOf(w.Diagnostics, "main.bot")) || n == 0 {
		t.Fatalf("the main's verdict counts %d errors, the answer carries %v", n, errorDiagnosticsOf(w.Diagnostics, "main.bot"))
	}
}

// TestApplyingOneFileAnswersTheStoredFilesParseVerdict: the per-file apply
// answers the unit's file list, and the studio replaces the list it holds
// with it — so the verdict in that answer is the STORED files', which is
// what the launch reads. An apply writes nothing: the fragment repaired
// through the picker is still broken where the launch looks, and the save
// that would store it is refused until it is fixed there. Were the overlay's
// verdict answered, the first apply would re-open the gate onto the launch
// the server refuses, which is #1532 itself.
func TestApplyingOneFileAnswersTheStoredFilesParseVerdict(t *testing.T) {
	workdir := t.TempDir()
	main := "import \"lib/broken.bot\"\nimport \"lib/plain.bot\"\n\nworkflow w:\n  entry: done\n"
	writeUnitFixture(t, workdir, map[string]string{
		"demo/main.bot":       main,
		"demo/lib/broken.bot": twoErrorFragmentFixture,
		"demo/lib/plain.bot":  "prompt mission:\n  Do the thing.\n",
	})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")

	// An apply of ANOTHER file: the broken fragment is untouched on disk,
	// and the answer still says so.
	rec, _ := parseCall(t, s, map[string]any{"path": "demo/main.bot", "file": "lib/plain.bot", "source": "prompt mission:\n  Do it twice.\n"})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply another file: %d %s", rec.Code, rec.Body.String())
	}
	w := decodeUnitWire(t, rec)
	n, first := fileWire(t, w, "lib/broken.bot")
	if n == 0 || first == "" {
		t.Fatalf("an apply of another file cleared the broken fragment's verdict: %d %q", n, first)
	}
	// The apply's unit was read from DISK too, so its verdict carries the
	// same root cut as the open's (#1934).
	if strings.Contains(first, workdir) {
		t.Fatalf("the verdict discloses the server's directory layout: %s", first)
	}

	// The fragment repaired through the picker: the answer still carries the
	// STORED fragment's verdict, because nothing was stored. The apply takes
	// no claim about the files — parseRequest has no unit_files — so there is
	// no client reading to answer back here; the save is the route a claim
	// reaches, and TestASaveIsRefusedOnTheLoadOfTheStoredFiles is its witness.
	rec, applied := parseCall(t, s, map[string]any{
		"path":   "demo/main.bot",
		"file":   "lib/broken.bot",
		"source": "prompt p:\n  hi\n",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply the repaired fragment: %d %s", rec.Code, rec.Body.String())
	}
	w = decodeUnitWire(t, rec)
	n, first = fileWire(t, w, "lib/broken.bot")
	if n == 0 || first == "" {
		t.Fatalf("the apply of a repaired fragment lifted the verdict of the file on disk: %d %q", n, first)
	}
	if strings.Contains(first, workdir) {
		t.Fatalf("the verdict discloses the server's directory layout: %s", first)
	}
	// Why it may not lift: the launch reads those files, and refuses them.
	if _, err := runview.CompileWorkflow(filepath.Join(workdir, "demo", "main.bot")); err == nil {
		t.Fatal("the fixture is not the case under test: the launch compiles the stored files")
	}
	// And the only way out — the save — is refused while they are broken,
	// so no apply can have repaired what the launch reads.
	srec, _ := savePathClaimed(t, s, "demo/main.bot", applied.Document, opened.Unit.Revision, claimEveryFileParses(t, opened))
	if srec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("the save of the applied document: %d %s", srec.Code, srec.Body.String())
	}
	if !strings.Contains(srec.Body.String(), "do not load as one unit (lib/broken.bot:") {
		t.Fatalf("the save's refusal is not the load of the stored files: %s", srec.Body.String())
	}
}

// TestApplyingOneFileAnswersTheStoredFilesLoadVerdict is the other half of
// the same answer: the LOAD verdict an apply carries is the STORED files',
// which is the load the launch performs. The case no per-file verdict
// covers: every file parses, and the bot does not load as one because the
// main's header imports a fragment that is not there. An overlay of that
// header without the broken line makes the STAGED unit load clean while the
// files on disk still do not load at all — and an apply writes nothing. Were
// the staged verdict answered, that first apply would re-open the gate onto
// the launch the server refuses, which is #1532 itself.
func TestApplyingOneFileAnswersTheStoredFilesLoadVerdict(t *testing.T) {
	workdir := t.TempDir()
	stored := "import \"lib/missing.bot\"\n" + unitFixtureMain
	writeUnitFixture(t, workdir, map[string]string{
		"demo/main.bot":      stored,
		"demo/lib/nodes.bot": unitFixtureNodes,
	})
	s := &Server{cfg: Config{WorkDir: workdir}}

	// The case under test: every file parses, so no per-file verdict
	// refuses — and the files do not load as one bot, which the launch
	// reading them refuses.
	rec, _ := openPath(t, s, "demo/main.bot")
	if rec.Code != http.StatusOK {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	w := decodeUnitWire(t, rec)
	if w.Unit.LoadErrors == 0 {
		t.Fatalf("the fixture is not the case under test: the open reports no load error: %+v", w.Unit)
	}
	for _, f := range w.Unit.Files {
		if f.ParseErrors != 0 {
			t.Fatalf("the fixture is not the case under test: %s does not parse (%s)", f.Rel, f.ParseError)
		}
	}
	if _, err := runview.CompileWorkflow(filepath.Join(workdir, "demo", "main.bot")); err == nil {
		t.Fatal("the fixture is not the case under test: the launch compiles the stored files")
	}

	rec, _ = parseCall(t, s, map[string]any{"path": "demo/main.bot", "file": "main.bot", "source": unitFixtureMain})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply the repaired main: %d %s", rec.Code, rec.Body.String())
	}
	w = decodeUnitWire(t, rec)
	if w.Unit.LoadErrors == 0 || !strings.Contains(w.Unit.LoadError, "lib/missing.bot") {
		t.Fatalf("the apply of a repaired header lifted the load verdict of the files on disk: %d %q", w.Unit.LoadErrors, w.Unit.LoadError)
	}
	// Read from disk like the open's, so it carries the same root cut (#1934).
	if strings.Contains(w.Unit.LoadError, workdir) {
		t.Fatalf("the verdict discloses the server's directory layout: %s", w.Unit.LoadError)
	}
	if got := readFixture(t, workdir, "demo/main.bot"); got != stored {
		t.Fatalf("the apply wrote the main:\n%s", got)
	}
}

// TestApplyingOneFileAnswersTheFragmentsTheStoredLoadAwaits: the verdict of
// an apply travels with the NAME of the file whose arrival would lift it.
// The same shape as above, read for what the author does next: the refusal
// says `lib/missing.bot` and the author writes it, on disk, because the save
// is refused until the stored files load. That write is a file event the tab
// must read the unit again on — and the apply's own file list cannot say so,
// the staged header having dropped the very `import` line the stored verdict
// is about. So `unread` is answered from the unit the verdict describes, and
// the open answers the same name under the root the client rebuilds paths
// with.
func TestApplyingOneFileAnswersTheFragmentsTheStoredLoadAwaits(t *testing.T) {
	workdir := t.TempDir()
	stored := "import \"lib/missing.bot\"\n" + unitFixtureMain
	writeUnitFixture(t, workdir, map[string]string{
		"demo/main.bot":      stored,
		"demo/lib/nodes.bot": unitFixtureNodes,
	})
	s := &Server{cfg: Config{WorkDir: workdir}}

	rec, _ := openPath(t, s, "demo/main.bot")
	if rec.Code != http.StatusOK {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	opened := decodeUnitWire(t, rec)
	// What the watcher waits for, as the client builds it.
	want := "demo/lib/missing.bot"
	if len(opened.Unit.Unread) != 1 || unitFilePath(opened.Unit.Root, opened.Unit.Unread[0]) != want {
		t.Fatalf("the open awaits %q under root %q, want %q", opened.Unit.Unread, opened.Unit.Root, want)
	}

	rec, _ = parseCall(t, s, map[string]any{"path": "demo/main.bot", "file": "main.bot", "source": unitFixtureMain})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply the repaired main: %d %s", rec.Code, rec.Body.String())
	}
	w := decodeUnitWire(t, rec)
	if !slices.Equal(w.Unit.Unread, opened.Unit.Unread) {
		t.Fatalf("the apply answers %q of the stored load, want the open's %q", w.Unit.Unread, opened.Unit.Unread)
	}
	// And the reason it has to be answered rather than re-derived: nothing
	// of the staged list names that fragment any more.
	for _, f := range w.Unit.Files {
		if slices.Contains(f.Imports, "lib/missing.bot") {
			t.Fatalf("the fixture is not the case under test: %s still imports the fragment", f.Rel)
		}
	}
}

// TestTheLoadVerdictHoldsEachFilesParseErrorsToo pins what load_errors
// counts, since both verdicts refuse the launch and the gate shows one of
// them. The loader appends every file's parse diagnostics to the unit's own
// list as it reads it, so the load verdict is a SUPERSET of the per-file
// ones — which is what makes it agree exactly with compileLoadedUnit, and
// why the per-file reason is shown first: it names the file to repair, not a
// fact the load verdict misses.
func TestTheLoadVerdictHoldsEachFilesParseErrorsToo(t *testing.T) {
	workdir := t.TempDir()
	main := "import \"lib/broken.bot\"\n\nworkflow w:\n  entry: done\n"
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": main, "demo/lib/broken.bot": twoErrorFragmentFixture})
	s := &Server{cfg: Config{WorkDir: workdir}}

	rec, _ := openPath(t, s, "demo/main.bot")
	if rec.Code != http.StatusOK {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	w := decodeUnitWire(t, rec)
	n, first := fileWire(t, w, "lib/broken.bot")
	if n == 0 {
		t.Fatalf("the fixture is not the case under test: the fragment parses (%+v)", w.Unit.Files)
	}
	// The main parses and the loader adds nothing of its own here, so the
	// fragment's errors are the whole load's: same count, same first error.
	if w.Unit.LoadErrors != n || w.Unit.LoadError != first {
		t.Fatalf("the load verdict does not hold the fragment's parse errors: %d %q against the file's %d %q", w.Unit.LoadErrors, w.Unit.LoadError, n, first)
	}
}

// claimEveryFileParses is the unit's file list as a client that reports
// every file as parsing would carry it back — the claim a save presents
// (saveFileRequest.unit_files), whose verdict fields the server answers and
// never reads.
func claimEveryFileParses(t *testing.T, opened openedUnit) []unitFileInfo {
	t.Helper()
	if opened.Unit == nil {
		t.Fatal("the open answered no unit")
	}
	claimed := 0
	claim := make([]unitFileInfo, 0, len(opened.Unit.Files))
	for _, f := range opened.Unit.Files {
		claimed += f.ParseErrors
		f.ParseErrors, f.ParseError = 0, ""
		claim = append(claim, f)
	}
	if claimed == 0 {
		t.Fatal("the fixture is not the case under test: the open reported no parse error to strip")
	}
	return claim
}

// TestASaveIsRefusedOnTheLoadOfTheStoredFiles: what refuses a save of a bot
// in several files is the LOAD of those files as they are stored — the
// sentence saveUnit answers, naming the fragment at fault — and a claim
// reporting every file as parsing changes nothing about it.
func TestASaveIsRefusedOnTheLoadOfTheStoredFiles(t *testing.T) {
	workdir := t.TempDir()
	main := "import \"lib/broken.bot\"\n\nworkflow w:\n  entry: done\n"
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": main, "demo/lib/broken.bot": twoErrorFragmentFixture})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")

	rec, _ := savePathClaimed(t, s, "demo/main.bot", opened.Document, opened.Unit.Revision, claimEveryFileParses(t, opened))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a save claiming every file parses: %d %s", rec.Code, rec.Body.String())
	}
	// The refusal it exercises, not a generic 422: the load of the stored
	// files, quoting the fragment's own error.
	if !strings.Contains(rec.Body.String(), "do not load as one unit (lib/broken.bot:") {
		t.Fatalf("the refusal is not the load of the stored files: %s", rec.Body.String())
	}
	if readFixture(t, workdir, "demo/lib/broken.bot") != twoErrorFragmentFixture {
		t.Fatal("a refused save wrote the fragment")
	}
}

// TestOpenAUnitReportsWhatItsLoadRefuses: every file of a bot can parse and
// the bot still not load as ONE — an `import` naming no fragment, a cycle.
// Those errors are the loader's own, which the load verdict holds on top of
// every file's parse errors (TestTheLoadVerdictHoldsEachFilesParseErrorsToo)
// and no per-file verdict carries; the merged document is built from what
// loaded and auto-validation answers it clean; and the launch refuses on
// exactly that list. So the unit's answer carries the load's verdict, and it
// says "refused" exactly when the launch does (#1532).
func TestOpenAUnitReportsWhatItsLoadRefuses(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		loads bool
	}{
		{
			name: "a fragment that is not there",
			files: map[string]string{
				"demo/main.bot":      "import \"lib/missing.bot\"\n\n" + unitFixtureMain,
				"demo/lib/nodes.bot": unitFixtureNodes,
			},
		},
		{
			name: "an import cycle",
			files: map[string]string{
				"demo/main.bot":      "import \"lib/a.bot\"\n\n" + unitFixtureMain,
				"demo/lib/a.bot":     "import \"b.bot\"\n\nprompt pa:\n  a\n",
				"demo/lib/b.bot":     "import \"a.bot\"\n\nprompt pb:\n  b\n",
				"demo/lib/nodes.bot": unitFixtureNodes,
			},
			// unitFixtureMain's own import keeps the cycle the only fault.
		},
		{
			name: "a bot that loads",
			files: map[string]string{
				"demo/main.bot":      unitFixtureMain,
				"demo/lib/nodes.bot": unitFixtureNodes,
			},
			loads: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workdir := t.TempDir()
			writeUnitFixture(t, workdir, tc.files)
			s := &Server{cfg: Config{WorkDir: workdir}}
			rec, opened := openPath(t, s, "demo/main.bot")
			if rec.Code != http.StatusOK {
				t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
			}
			w := decodeUnitWire(t, rec)
			if !w.Bindable {
				t.Fatalf("the fixture is not the case under test: the main did not parse (%v)", w.Diagnostics)
			}
			// The fault is BETWEEN the files: every one of them parses, so
			// the per-file verdict is silent and this is the only fact left.
			for _, f := range w.Unit.Files {
				if f.ParseErrors != 0 {
					t.Fatalf("the fixture is not the case under test: %s does not parse (%s)", f.Rel, f.ParseError)
				}
			}
			// What auto-validation writes over the open's diagnostics.
			valid, vdiags := validateDocument(t, s, opened.Document, "demo/main.bot")
			// The launch's own verdict on the same files, which is what the
			// gate has to agree with.
			_, launchErr := runview.CompileWorkflow(filepath.Join(workdir, "demo", "main.bot"))
			if tc.loads {
				if launchErr != nil {
					t.Fatalf("the fixture is not the case under test: the launch refuses a bot that loads: %v", launchErr)
				}
				if w.Unit.LoadErrors != 0 || w.Unit.LoadError != "" {
					t.Fatalf("a bot that loads is reported refused: %d %q", w.Unit.LoadErrors, w.Unit.LoadError)
				}
				return
			}
			if launchErr == nil {
				t.Fatal("the fixture is not the case under test: the launch compiles these files")
			}
			// The whole reason the verdict has to exist: the document
			// auto-validation answers on is built from what loaded, and it
			// validates CLEAN — so the diagnostics the open answered are
			// rewritten with nothing, and no fact about the document is
			// left for the gate to refuse on.
			if !valid || len(vdiags) != 0 {
				t.Fatalf("the fixture is not the case under test: the merged document does not validate clean (%v %v)", valid, vdiags)
			}
			if w.Unit.LoadErrors == 0 {
				t.Fatalf("the launch refuses these files (%v) and the unit reports no load error: %+v", launchErr, w.Unit)
			}
			want := errorDiagnostics(w.Diagnostics)
			if len(want) == 0 {
				t.Fatalf("the answer carries no error diagnostic to be the verdict's first: %v", w.Diagnostics)
			}
			if w.Unit.LoadErrors != len(want) {
				t.Fatalf("the load verdict counts %d errors, the answer carries %d: %v", w.Unit.LoadErrors, len(want), want)
			}
			if w.Unit.LoadError != want[0] {
				t.Fatalf("the load verdict's first error is %q, the answer's first is %q", w.Unit.LoadError, want[0])
			}
			if strings.Contains(w.Unit.LoadError, workdir) {
				t.Fatalf("the verdict discloses the server's directory layout: %s", w.Unit.LoadError)
			}
		})
	}
}

// validateDocument is what auto-validation asks of the server for an open
// document: the verdict on the merged DOCUMENT, which overwrites the
// diagnostics the open answered.
func validateDocument(t *testing.T, s *Server, doc json.RawMessage, path string) (bool, []string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"document": doc, "path": path})
	req := httptest.NewRequest(http.MethodPost, "/api/validate", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleValidate(rec, req)
	var out struct {
		Valid       bool     `json:"valid"`
		Diagnostics []string `json:"diagnostics"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode validate: %v\n%s", err, rec.Body.String())
	}
	return out.Valid, out.Diagnostics
}

// TestAUnitsRootAndMainComposeIntoThePathThatWasOpened: every path the
// studio rebuilds of a unit file is root + rel (unitFilePath), and the
// main's own rel must therefore compose back into the file that was asked
// for. The request's DIRECTORY is not that root: a fragment opened alone is
// loaded from the root its `lib/` belongs to, so its rel carries that
// `lib/` and the directory is one level too deep — leaving every rebuilt
// path, the awaited fragment's included, wrong by one directory.
func TestAUnitsRootAndMainComposeIntoThePathThatWasOpened(t *testing.T) {
	// A fragment: no workflow of its own, under a lib/, importing a sibling
	// that is not there — so it answers a unit with a load verdict.
	const fragmentAlone = "import \"schemas.bot\"\n\nprompt mission:\n  Do the thing.\n"
	cases := []struct {
		name string
		path string
		main string
		// awaits is every fragment the load could not read, named as the
		// watcher names a file — the main asks for lib/missing.bot, the
		// fragment for its sibling schemas.bot, and opening the main
		// reaches both.
		awaits []string
	}{
		{"a main", "demo/main.bot", "main.bot", []string{"demo/lib/missing.bot", "demo/lib/schemas.bot"}},
		{"a fragment opened alone", "demo/lib/nodes.bot", "lib/nodes.bot", []string{"demo/lib/schemas.bot"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workdir := t.TempDir()
			writeUnitFixture(t, workdir, map[string]string{
				"demo/main.bot":      "import \"lib/missing.bot\"\n\n" + unitFixtureMain,
				"demo/lib/nodes.bot": fragmentAlone,
			})
			s := &Server{cfg: Config{WorkDir: workdir}}
			rec, _ := openPath(t, s, tc.path)
			if rec.Code != http.StatusOK {
				t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
			}
			w := decodeUnitWire(t, rec)
			if w.Unit.Main != tc.main {
				t.Fatalf("main %q, want %q", w.Unit.Main, tc.main)
			}
			if got := unitFilePath(w.Unit.Root, w.Unit.Main); got != tc.path {
				t.Fatalf("root %q and main %q name %q, not the file that was opened (%s)", w.Unit.Root, w.Unit.Main, got, tc.path)
			}
			// And so does every fragment the load awaits, which is the
			// write the refusal asks for.
			var awaits []string
			for _, rel := range w.Unit.Unread {
				awaits = append(awaits, unitFilePath(w.Unit.Root, rel))
			}
			if !slices.Equal(awaits, tc.awaits) {
				t.Fatalf("the open awaits %q (root %q, unread %q), want %q", awaits, w.Unit.Root, w.Unit.Unread, tc.awaits)
			}
		})
	}
}

// TestAUnitRootedAboveTheWorkspaceAnswersNoRoot: the composition above
// holds for every unit rooted INSIDE the workspace, and a workspace whose
// own directory is named `lib/` is not one — a workflow-less file under it
// is read as a fragment of the bot rooted at the workspace's PARENT, so
// every Rel the answer carries starts with a `lib/` no path under the
// workspace has, and no root composes one back. The answer carries NO root
// there, rather than the request's directory: that would read as a root
// and name a workspace file for every Rel under it.
func TestAUnitRootedAboveTheWorkspaceAnswersNoRoot(t *testing.T) {
	workdir := filepath.Join(t.TempDir(), "lib")
	writeUnitFixture(t, workdir, map[string]string{
		"bots/frag.bot": "import \"schemas.bot\"\n\nprompt mission:\n  Do the thing.\n",
	})
	s := &Server{cfg: Config{WorkDir: workdir}}
	rec, _ := openPath(t, s, "bots/frag.bot")
	if rec.Code != http.StatusOK {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	w := decodeUnitWire(t, rec)
	// The case under test: the Main the loader answers is not a suffix of
	// the path the request named, so no root composes the two.
	if w.Unit.Main != "lib/bots/frag.bot" {
		t.Fatalf("main %q: the fixture is not the case under test", w.Unit.Main)
	}
	if w.Unit.Root != "" {
		t.Fatalf("root %q, want none: no root composes main %q back into the opened bots/frag.bot", w.Unit.Root, w.Unit.Main)
	}
	// And the fragment it awaits is named from that same root, so the one
	// file a client can still recognise is the one it opened.
	if !slices.Equal(w.Unit.Unread, []string{"lib/bots/schemas.bot"}) {
		t.Fatalf("the open awaits %q, want the sibling under the bot's own root", w.Unit.Unread)
	}
}

// TestAnImportTheLoaderRefusesByPathIsAwaitedByNothing: the other load
// failure an `import` leaves — a path resolving outside the bot's `lib/`,
// which the loader refuses BEFORE reading it. The file it names is in no
// list either, like a fragment that is not there, but nothing written at
// that path lifts the refusal: it is about the path. And such a directory
// is exactly where another bot's main lives, so answering it as a file this
// bot awaits would make every write to that neighbour a change to this tab.
func TestAnImportTheLoaderRefusesByPathIsAwaitedByNothing(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{
		"demo/main.bot":      "import \"other.bot\"\n\n" + unitFixtureMain,
		"demo/other.bot":     "workflow other:\n  entry: done\n",
		"demo/lib/nodes.bot": unitFixtureNodes,
	})
	s := &Server{cfg: Config{WorkDir: workdir}}
	rec, _ := openPath(t, s, "demo/main.bot")
	if rec.Code != http.StatusOK {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	w := decodeUnitWire(t, rec)
	if w.Unit.LoadErrors == 0 || !strings.Contains(w.Unit.LoadError, "outside the bot's `lib/` directory") {
		t.Fatalf("the fixture is not the case under test: %d %q", w.Unit.LoadErrors, w.Unit.LoadError)
	}
	for _, f := range w.Unit.Files {
		if f.Rel == "other.bot" {
			t.Fatalf("the fixture is not the case under test: the unit holds other.bot")
		}
	}
	if len(w.Unit.Unread) != 0 {
		t.Fatalf("the answer awaits %q, which no write makes readable", w.Unit.Unread)
	}
	// The import line is what the author repairs, and it is in the main the
	// tab already holds — the one file membership always answers for.
	held := false
	for _, f := range w.Unit.Files {
		if f.Rel == w.Unit.Main {
			held = true
		}
	}
	if !held {
		t.Fatalf("the unit does not hold its own main: %+v", w.Unit.Files)
	}
}

// TestOpenAUnitAnswersDiagnosticsWithoutTheWorkspaceRoot: the unit an open
// answers was read from DISK, and its diagnostics ride the 200 — citing
// the fragment under its absolute path before the root cut (#1934). The
// fragment is named by its unit-relative path, the position kept.
func TestOpenAUnitAnswersDiagnosticsWithoutTheWorkspaceRoot(t *testing.T) {
	workdir := t.TempDir()
	main := "import \"lib/broken.bot\"\n\nworkflow w:\n  entry: done\n"
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": main, "demo/lib/broken.bot": brokenFragmentFixture})
	s := &Server{cfg: Config{WorkDir: workdir}}

	rec, opened := openPath(t, s, "demo/main.bot")
	if rec.Code != http.StatusOK || len(opened.Diagnostics) == 0 {
		t.Fatalf("the fixture is not the case under test: %d %v", rec.Code, opened.Diagnostics)
	}
	joined := strings.Join(opened.Diagnostics, "\n")
	if !strings.Contains(joined, "lib/broken.bot:") {
		t.Fatalf("the diagnostics do not cite the fragment by its relative path: %s", joined)
	}
	if strings.Contains(joined, workdir) {
		t.Fatalf("the diagnostics disclose the server's directory layout: %s", joined)
	}
}

// TestAUnitSaveRefusalNeverNamesTheWorkspaceRoot: the save reloads the unit
// from DISK, and a fragment that does not parse there is a 422 whose body
// quotes the loader's diagnostic — positioned under the fragment's
// absolute path before the root cut (#1934).
func TestAUnitSaveRefusalNeverNamesTheWorkspaceRoot(t *testing.T) {
	workdir := t.TempDir()
	main := "import \"lib/broken.bot\"\n\nworkflow w:\n  entry: done\n"
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": main, "demo/lib/broken.bot": brokenFragmentFixture})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")
	if opened.Unit == nil || opened.Unit.Revision == "" {
		t.Fatalf("a unit that does not load opened with no revision: %+v", opened.Unit)
	}

	rec, _ := savePath(t, s, "demo/main.bot", opened.Document, opened.Unit.Revision)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("saving over a broken fragment: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "lib/broken.bot:") {
		t.Fatalf("the refusal does not cite the fragment by its relative path: %s", body)
	}
	if strings.Contains(body, workdir) {
		t.Fatalf("the refusal discloses the server's directory layout: %s", body)
	}
}

// TestASiblingRefusalNeverNamesTheWorkspaceRoot: the sibling check reloads
// the other main from DISK against the staged fragment, so its loader
// diagnostics are positioned under absolute paths — and the refusal is
// answered to the client (#1934). Here the staged fragment declares a
// workflow of its own, so the sibling fails to LOAD (the loader's
// diagnostic, not a compile verdict).
func TestASiblingRefusalNeverNamesTheWorkspaceRoot(t *testing.T) {
	workdir := t.TempDir()
	sibling := "import \"lib/nodes.bot\"\n\nworkflow other:\n  entry: worker\n  worker -> done\n"
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes, "demo/other.bot": sibling})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")
	edited := editDocument(t, opened.Document, func(m map[string]any) {
		m["workflows"] = append(m["workflows"].([]any), map[string]any{"name": "rogue", "entry": "done", "file": "lib/nodes.bot"})
	})

	rec, _ := savePath(t, s, "demo/main.bot", edited, opened.Unit.Revision)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a save that breaks a sibling's load: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "other.bot") {
		t.Fatalf("the refusal does not name the sibling: %s", body)
	}
	if !strings.Contains(body, "lib/nodes.bot:") {
		t.Fatalf("the refusal does not cite the fragment by its relative path: %s", body)
	}
	if strings.Contains(body, workdir) {
		t.Fatalf("the refusal discloses the server's directory layout: %s", body)
	}
	if readFixture(t, workdir, "demo/lib/nodes.bot") != unitFixtureNodes {
		t.Fatal("a refused save wrote the fragment")
	}
}
