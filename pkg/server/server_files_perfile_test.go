package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// A fragment the writer cannot reproduce: profile 2 escapes every string,
// so the `command:` written over two lines would come back as one escaped
// line — the shape #1612 named and `.fmt-refused` lists for the
// `lib/nodes.bot` of BOTH multi-file bots in the catalogue.
const foldMain = "import \"lib/nodes.bot\"\n\nworkflow w:\n  entry: t\n  t -> done\n"

const foldNodes = "dsl: 2\n\ntool t:\n  description: \"probe\"\n  command: `line one\nline two`\n"

// A fragment of the same unit the writer CAN reproduce: the control that
// keeps the refusal from reading as "a unit cannot be saved".
const foldPlain = "prompt mission:\n  Do the thing.\n"

const foldMainTwo = "import \"lib/nodes.bot\"\nimport \"lib/plain.bot\"\n\nworkflow w:\n  entry: t\n  t -> done\n"

func unparseCall(t *testing.T, s *Server, body map[string]any) (*httptest.ResponseRecorder, unparseResponse) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/unparse", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	s.handleUnparse(rec, req)
	var out unparseResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode unparse: %v\n%s", err, rec.Body.String())
		}
	}
	return rec, out
}

func parseCall(t *testing.T, s *Server, body map[string]any) (*httptest.ResponseRecorder, parseResponse) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/parse", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	s.handleParse(rec, req)
	var out parseResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode parse: %v\n%s", err, rec.Body.String())
		}
	}
	return rec, out
}

// TestSavingAUnitLeavesAFileTheWriterWouldFoldExactlyAsItIs: a save that
// would rewrite a fragment whose multi-line value the writer folds onto one
// line is refused, naming the file — and the file keeps its bytes. Before
// this the save answered 200 and wrote the folded text: the same program,
// and not the same file.
func TestSavingAUnitLeavesAFileTheWriterWouldFoldExactlyAsItIs(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": foldMain, "demo/lib/nodes.bot": foldNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")

	edited := editDocument(t, opened.Document, func(m map[string]any) {
		tools, _ := m["tools"].([]any)
		tools[0].(map[string]any)["description"] = "probe, edited"
	})
	rec, _ := savePath(t, s, "demo/main.bot", edited, opened.Unit.Revision)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("save of a fragment the writer folds: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "lib/nodes.bot") {
		t.Fatalf("the refusal does not name the file: %s", rec.Body.String())
	}
	// The line named is the FILE's own (the `command:` line), never a line
	// of the render — the writer reorders declarations, so a render line
	// points at unrelated text.
	if !strings.Contains(rec.Body.String(), "line 5") {
		t.Fatalf("the refusal does not name the line the value is written on in the file: %s", rec.Body.String())
	}
	if got := readFixture(t, workdir, "demo/lib/nodes.bot"); got != foldNodes {
		t.Fatalf("the fragment was rewritten anyway:\n%s", got)
	}
}

// TestSavingAUnitStillWritesTheFilesTheWriterCanReproduce is a CONTROL,
// not a witness: it asserts a success, so no mutation that removes a
// refusal can redden it. Its job is to keep the refusal from being read as
// "a bot holding one such file cannot be saved at all" — the sibling is
// left alone because its program did not change, and the edited one is
// written because the writer can carry it.
func TestSavingAUnitStillWritesTheFilesTheWriterCanReproduce(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{
		"demo/main.bot":      foldMainTwo,
		"demo/lib/nodes.bot": foldNodes,
		"demo/lib/plain.bot": foldPlain,
	})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")

	edited := editDocument(t, opened.Document, func(m map[string]any) {
		prompts, _ := m["prompts"].([]any)
		prompts[0].(map[string]any)["body"] = "Do the other thing."
	})
	rec, saved := savePath(t, s, "demo/main.bot", edited, opened.Unit.Revision)
	if rec.Code != http.StatusOK {
		t.Fatalf("save of a reproducible fragment: %d %s", rec.Code, rec.Body.String())
	}
	if len(saved.Files) != 1 || saved.Files[0] != "lib/plain.bot" {
		t.Fatalf("files written %v, want lib/plain.bot alone", saved.Files)
	}
	if got := readFixture(t, workdir, "demo/lib/nodes.bot"); got != foldNodes {
		t.Fatalf("the refused sibling was rewritten:\n%s", got)
	}
}

// TestSavingOneFileLeavesItAsItIsWhenTheWriterWouldFoldIt: the same
// refusal on the single-file save — 37 of the 39 files `.fmt-refused`
// lists are bots in ONE file, opened straight on the canvas.
func TestSavingOneFileLeavesItAsItIsWhenTheWriterWouldFoldIt(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"solo.bot": foldNodes + "\nworkflow w:\n  entry: t\n  t -> done\n"})
	before := readFixture(t, workdir, "solo.bot")
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "solo.bot")
	if opened.Unit != nil {
		t.Fatal("a bot in one file opened as a unit")
	}
	edited := editDocument(t, opened.Document, func(m map[string]any) {
		tools, _ := m["tools"].([]any)
		tools[0].(map[string]any)["description"] = "probe, edited"
	})
	rec, _ := savePath(t, s, "solo.bot", edited, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	if got := readFixture(t, workdir, "solo.bot"); got != before {
		t.Fatalf("the file was rewritten anyway:\n%s", got)
	}
}

// TestTheSourceViewRendersOneFileOfTheUnit: the picker asks for ONE file
// and gets that file's program — not the merged one, which is what the
// view could show before and what no save can take apart.
func TestTheSourceViewRendersOneFileOfTheUnit(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")

	rec, frag := unparseCall(t, s, map[string]any{"document": opened.Document, "path": "demo/main.bot", "file": "lib/nodes.bot"})
	if rec.Code != http.StatusOK {
		t.Fatalf("render lib/nodes.bot: %d %s", rec.Code, rec.Body.String())
	}
	// The fragment's own declarations, and NOT the main's workflow: the
	// forbidden alternative here is the merged program, not an empty text.
	if !strings.Contains(frag.Source, "agent worker:") {
		t.Fatalf("the fragment's own declaration is missing:\n%s", frag.Source)
	}
	if strings.Contains(frag.Source, "workflow w:") {
		t.Fatalf("the merged program came back instead of the one file:\n%s", frag.Source)
	}
	if frag.Refused != "" {
		t.Fatalf("a reproducible file was reported refused: %s", frag.Refused)
	}

	rec, main := unparseCall(t, s, map[string]any{"document": opened.Document, "path": "demo/main.bot", "file": "main.bot"})
	if rec.Code != http.StatusOK {
		t.Fatalf("render main.bot: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(main.Source, "workflow w:") || strings.Contains(main.Source, "agent worker:") {
		t.Fatalf("main.bot rendered as something else:\n%s", main.Source)
	}
	// The main keeps the import line that makes it a unit; the merged
	// program has none.
	if !strings.Contains(main.Source, "import \"lib/nodes.bot\"") {
		t.Fatalf("the main lost its import line:\n%s", main.Source)
	}
}

// TestTheSourceViewShowsAFoldedFileAsItIsOnDisk: for a file the writer
// cannot reproduce, the view shows the FILE, with the reason. Rendering it
// would show the author a text their file does not contain — the lie the
// salvage path already refuses to tell.
func TestTheSourceViewShowsAFoldedFileAsItIsOnDisk(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": foldMain, "demo/lib/nodes.bot": foldNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")

	rec, frag := unparseCall(t, s, map[string]any{"document": opened.Document, "path": "demo/main.bot", "file": "lib/nodes.bot"})
	if rec.Code != http.StatusOK {
		t.Fatalf("render: %d %s", rec.Code, rec.Body.String())
	}
	if frag.Refused == "" {
		t.Fatal("a file the writer folds was rendered with no reason given")
	}
	if frag.Source != foldNodes {
		t.Fatalf("the view shows a text the file does not contain:\n%s", frag.Source)
	}
	// The forbidden alternative, named: the writer's own text, which puts
	// the two lines on one.
	if strings.Contains(frag.Source, "\\n") {
		t.Fatalf("the folded render came back instead of the file:\n%s", frag.Source)
	}
}

// TestApplyingOneFileReparsesTheUnitAndLeavesTheRevisionAlone: the picker's
// Apply re-parses the unit with that file replaced, and answers NO
// revision. A revision is a claim about the files at rest; an overlay
// changed none. Answering the staged digest would make the very next save
// a false conflict.
func TestApplyingOneFileReparsesTheUnitAndLeavesTheRevisionAlone(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")

	editedFragment := strings.Replace(unitFixtureNodes, "anthropic/claude-opus-4-8", "anthropic/claude-opus-5", 1)
	rec, applied := parseCall(t, s, map[string]any{"path": "demo/main.bot", "file": "lib/nodes.bot", "source": editedFragment})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	if applied.Unit == nil {
		t.Fatal("the apply answered no unit")
	}
	if applied.Unit.Revision != "" {
		t.Fatalf("the apply answered a revision (%q): the files at rest did not move", applied.Unit.Revision)
	}
	var doc map[string]any
	if err := json.Unmarshal(applied.Document, &doc); err != nil {
		t.Fatal(err)
	}
	agent, _ := agentsOf(doc)[0].(map[string]any)
	if agent["model"] != "anthropic/claude-opus-5" || agent["file"] != "lib/nodes.bot" {
		t.Fatalf("the edited fragment did not reach the merged document: %v", agent)
	}

	// The revision the document was OPENED at still saves: this is the
	// assertion that reddens if the overlay ever hands back a staged digest.
	rec, _ = savePath(t, s, "demo/main.bot", applied.Document, opened.Unit.Revision)
	if rec.Code != http.StatusOK {
		t.Fatalf("save after apply: %d %s", rec.Code, rec.Body.String())
	}
	if got := readFixture(t, workdir, "demo/lib/nodes.bot"); !strings.Contains(got, "anthropic/claude-opus-5") {
		t.Fatalf("the applied edit did not reach the file:\n%s", got)
	}
}

// TestApplyingOneFileStillDetectsAFileMovedOnDisk: the counterweight to
// the test above — leaving the revision alone must not mean ignoring it.
// A fragment a colleague changed between the open and the save is still a
// conflict, and the applied text does not land on top of it.
func TestApplyingOneFileStillDetectsAFileMovedOnDisk(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")

	editedFragment := strings.Replace(unitFixtureNodes, "anthropic/claude-opus-4-8", "anthropic/claude-opus-5", 1)
	rec, applied := parseCall(t, s, map[string]any{"path": "demo/main.bot", "file": "lib/nodes.bot", "source": editedFragment})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	onDisk := unitFixtureNodes + "\n## a colleague wrote this meanwhile\n"
	writeUnitFixture(t, workdir, map[string]string{"demo/lib/nodes.bot": onDisk})

	rec, _ = savePath(t, s, "demo/main.bot", applied.Document, opened.Unit.Revision)
	if rec.Code != http.StatusConflict {
		t.Fatalf("save over a file that moved on disk: %d %s", rec.Code, rec.Body.String())
	}
	if got := readFixture(t, workdir, "demo/lib/nodes.bot"); got != onDisk {
		t.Fatalf("the colleague's edit was overwritten:\n%s", got)
	}
}

// TestApplyingAFileThatDoesNotParseIsRefused: a staged fragment the parser
// can only salvage merges as the declarations it COULD read. A document
// missing the rest writes that file back SHORT — 200, no diagnostic, the
// author's declarations gone. The refusal is what the view shows instead.
func TestApplyingAFileThatDoesNotParseIsRefused(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}
	openPath(t, s, "demo/main.bot")

	broken := unitFixtureNodes + "\nagent \n  model\n"
	rec, _ := parseCall(t, s, map[string]any{"path": "demo/main.bot", "file": "lib/nodes.bot", "source": broken})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("apply of a fragment that does not parse: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "lib/nodes.bot") {
		t.Fatalf("the refusal does not name the file: %s", rec.Body.String())
	}
	if got := readFixture(t, workdir, "demo/lib/nodes.bot"); got != unitFixtureNodes {
		t.Fatalf("the file changed:\n%s", got)
	}
}

// TestAPerFileRequestNamesAFileTheBotDoesNotHold: a stale or mistyped name
// — a fragment deleted under the open picker, above all — is an error on
// both new modes. Answering the whole unit, or answering unchanged, would
// read as success and send the author's typed text to the bin.
func TestAPerFileRequestNamesAFileTheBotDoesNotHold(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")

	for _, rel := range []string{"lib/gone.bot", "../escape.bot", "/etc/passwd"} {
		rec, _ := unparseCall(t, s, map[string]any{"document": opened.Document, "path": "demo/main.bot", "file": rel})
		if rec.Code == http.StatusOK {
			t.Fatalf("render %q answered 200: %s", rel, rec.Body.String())
		}
		rec, _ = parseCall(t, s, map[string]any{"path": "demo/main.bot", "file": rel, "source": "prompt p:\n  hi\n"})
		if rec.Code == http.StatusOK {
			t.Fatalf("apply %q answered 200: %s", rel, rec.Body.String())
		}
	}
}

// TestAPerFileRequestOnACloudBundleReadsItsFilesMap: the picker works the
// same on a bundle the client holds as a files map, where there is no path.
func TestAPerFileRequestOnACloudBundleReadsItsFilesMap(t *testing.T) {
	s := &Server{}
	files := map[string]any{"main.bot": unitFixtureMain, "lib/nodes.bot": unitFixtureNodes}
	rec, parsed := parseCall(t, s, map[string]any{"files": files, "main": "main.bot"})
	if rec.Code != http.StatusOK {
		t.Fatalf("parse unit: %d %s", rec.Code, rec.Body.String())
	}
	rec, frag := unparseCall(t, s, map[string]any{"document": parsed.Document, "files": files, "main": "main.bot", "file": "lib/nodes.bot"})
	if rec.Code != http.StatusOK {
		t.Fatalf("render: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(frag.Source, "agent worker:") || strings.Contains(frag.Source, "workflow w:") {
		t.Fatalf("the cloud per-file render is not the one file:\n%s", frag.Source)
	}
	edited := strings.Replace(unitFixtureNodes, "anthropic/claude-opus-4-8", "anthropic/claude-opus-5", 1)
	rec, applied := parseCall(t, s, map[string]any{"files": files, "main": "main.bot", "file": "lib/nodes.bot", "source": edited})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	if applied.Unit == nil || applied.Unit.Revision != "" {
		t.Fatalf("the cloud apply answered a revision: %+v", applied.Unit)
	}
	var doc map[string]any
	if err := json.Unmarshal(applied.Document, &doc); err != nil {
		t.Fatal(err)
	}
	if agentsOf(doc)[0].(map[string]any)["model"] != "anthropic/claude-opus-5" {
		t.Fatal("the cloud apply did not carry the edit")
	}
}

// TestAUnitAlwaysCarriesAFilesArray: executed, never read off the struct
// tag — `Files []unitFileInfo` with no omitempty marshals a nil slice as
// JSON `null`, and the studio's UnitFileInfo[] declares itself non-nullable
// (#1581: the transport erases nil-from-empty, so prove the round trip).
func TestAUnitAlwaysCarriesAFilesArray(t *testing.T) {
	raw, err := json.Marshal(unitInfoOf(&unit.Unit{Main: "main.bot"}, "main.bot"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"files":[]`) {
		t.Fatalf("a unit with no file marshals as %s", raw)
	}
	var back struct {
		Files []unitFileInfo `json:"files"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Files == nil {
		t.Fatalf("the round trip put the nil back: %s", raw)
	}
}

// TestRenderingOneFileRefusesADocumentWithoutProvenance: the per-file render is
// a projection of the open document, so a document whose declarations name
// no file cannot be split — it would fold every file into the main.
func TestRenderingOneFileRefusesADocumentWithoutProvenance(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")
	stripped := editDocument(t, opened.Document, func(m map[string]any) { dropKey(m, "file") })
	rec, _ := unparseCall(t, s, map[string]any{"document": stripped, "path": "demo/main.bot", "file": "lib/nodes.bot"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("render from a document with no provenance: %d %s", rec.Code, rec.Body.String())
	}
}

// helper: the fixture parses, so a broken fixture fails here and not in a
// test whose subject is something else.
func TestPerFileFixturesParse(t *testing.T) {
	for name, src := range map[string]string{"foldNodes": foldNodes, "foldMain": foldMain, "foldPlain": foldPlain, "foldMainTwo": foldMainTwo} {
		pr := parser.Parse(name, src)
		if parseHasErrors(pr.Diagnostics) {
			t.Fatalf("%s: %v", name, pr.Diagnostics)
		}
	}
	if _, err := ast.MarshalFile(parser.Parse("f", foldNodes).File); err != nil {
		t.Fatal(err)
	}
}

// TestPuttingABotSourceFileRefusesAWriteThatFoldsAStoredValue: the one
// write a CLIENT performs — the cloud single-file save and the files
// drawer — goes to a route that takes file CONTENT, not a document, so it
// cannot ask the writer whether it could have produced it. It asks the two
// texts it holds instead: would this write put on ONE line a value the
// stored file writes over several?
func TestPuttingABotSourceFileRefusesAWriteThatFoldsAStoredValue(t *testing.T) {
	pinServerBuild(t, "v"+parser.ImportSince+"+deadbeef")
	s, editor, _ := newBotSourceTestServer(t)
	s.runnerBuilds = &fakeBuildObserver{builds: []string{"v" + parser.ImportSince + "+abc123"}}
	edCtx := auth.WithIdentity(context.Background(), editor)
	stored := "dsl: 2\n\ntool t:\n  description: \"probe\"\n  command: `line one\nline two`\n"
	files := map[string]string{
		"main.bot":      "import \"lib/nodes.bot\"\n\nworkflow main:\n  entry: t\n  t -> done\n",
		"lib/nodes.bot": stored,
		"manifest.yaml": "name: folded\nversion: 1.0.0\nrequires:\n  iterion: \">= " + parser.ImportSince + "\"\n",
	}
	body, _ := json.Marshal(botSourcePutReq{Files: files})
	r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/folded", strings.NewReader(string(body))).WithContext(edCtx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", "folded")
	w := httptest.NewRecorder()
	s.handlePutBotSource(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", w.Code, w.Body.String())
	}
	putFile := func(path, content string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]any{"content": content})
		r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/folded/files/"+path, strings.NewReader(string(b))).WithContext(edCtx)
		r.SetPathValue("id", "t1")
		r.SetPathValue("slug", "folded")
		r.SetPathValue("path", path)
		w := httptest.NewRecorder()
		s.handlePutBotSourceFile(w, r)
		return w
	}
	// The same program, written with the two lines folded onto one: what
	// the studio's own renderer produces for this file, and what the
	// compile check happily accepts.
	folded := "dsl: 2\n\ntool t:\n  description: \"probe\"\n  command: \"line one\\nline two\"\n"
	if w := putFile("lib/nodes.bot", folded); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a write that folds a stored value: %d %s", w.Code, w.Body.String())
	}
	after, err := s.botSources.GetBySlug(context.Background(), "t1", "folded")
	if err != nil {
		t.Fatal(err)
	}
	if after.Files["lib/nodes.bot"] != stored {
		t.Fatalf("the refused write landed anyway:\n%s", after.Files["lib/nodes.bot"])
	}
	// The control: an edit that keeps the value over its lines is written.
	// Without it the test would pass just as well on a route that refuses
	// every write.
	kept := "dsl: 2\n\ntool t:\n  description: \"probe, edited\"\n  command: `line one\nline two`\n"
	if w := putFile("lib/nodes.bot", kept); w.Code != http.StatusOK {
		t.Fatalf("an edit that folds nothing was refused: %d %s", w.Code, w.Body.String())
	}
}

// TestRenderingABotInOneFileShowsTheFileWhenTheWriterWouldFoldIt: the
// second member of the same class as the per-file render. The document is
// rendered for DISPLAY, so the fold is reported rather than refused — but
// the text handed back is the file's own. The view says "this is your
// file" above it, and the writer's folded text is precisely the one thing
// the author's file does not contain.
func TestRenderingABotInOneFileShowsTheFileWhenTheWriterWouldFoldIt(t *testing.T) {
	workdir := t.TempDir()
	solo := foldNodes + "\nworkflow w:\n  entry: t\n  t -> done\n"
	writeUnitFixture(t, workdir, map[string]string{"solo.bot": solo})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "solo.bot")

	rec, out := unparseCall(t, s, map[string]any{"document": opened.Document, "path": "solo.bot"})
	if rec.Code != http.StatusOK {
		t.Fatalf("render: %d %s", rec.Code, rec.Body.String())
	}
	if out.Refused == "" {
		t.Fatal("a file the writer folds was rendered with no reason given")
	}
	if out.Source != solo {
		t.Fatalf("the answer is not the file's own text:\n%s", out.Source)
	}
	// The forbidden alternative, named: the writer's text, which puts the
	// two lines on one.
	if strings.Contains(out.Source, "\\n") {
		t.Fatalf("the folded render came back instead of the file:\n%s", out.Source)
	}

	// The BEFORE is the server's own read of the file, never a text the
	// client supplies: an unbound buffer has no file to be about, so
	// nothing is claimed rather than something being claimed wrongly.
	rec, unbound := unparseCall(t, s, map[string]any{"document": opened.Document})
	if rec.Code != http.StatusOK || unbound.Refused != "" {
		t.Fatalf("a document with no file claimed a fold: %d %q", rec.Code, unbound.Refused)
	}
}

// TestPuttingAWholeBundleRefusesAWriteThatFoldsAStoredValue: the cloud
// save of a bot in SEVERAL files lands here, as one versioned PUT of the
// whole bundle — so the refusal the per-file route makes has to be made
// here too, or the class is fixed at one site of two.
func TestPuttingAWholeBundleRefusesAWriteThatFoldsAStoredValue(t *testing.T) {
	pinServerBuild(t, "v"+parser.ImportSince+"+deadbeef")
	s, editor, _ := newBotSourceTestServer(t)
	s.runnerBuilds = &fakeBuildObserver{builds: []string{"v" + parser.ImportSince + "+abc123"}}
	edCtx := auth.WithIdentity(context.Background(), editor)
	stored := "dsl: 2\n\ntool t:\n  description: \"probe\"\n  command: `line one\nline two`\n"
	files := map[string]string{
		"main.bot":      "import \"lib/nodes.bot\"\n\nworkflow main:\n  entry: t\n  t -> done\n",
		"lib/nodes.bot": stored,
		"manifest.yaml": "name: whole\nversion: 1.0.0\nrequires:\n  iterion: \">= " + parser.ImportSince + "\"\n",
	}
	put := func(f map[string]string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(botSourcePutReq{Files: f})
		r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/whole", strings.NewReader(string(body))).WithContext(edCtx)
		r.SetPathValue("id", "t1")
		r.SetPathValue("slug", "whole")
		w := httptest.NewRecorder()
		s.handlePutBotSource(w, r)
		return w
	}
	// A slug with nothing stored is a creation: no before, no claim — and
	// a bundle whose own files hold multi-line values must still be
	// pushable, or no refused bot could ever reach the cloud.
	if w := put(files); w.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", w.Code, w.Body.String())
	}
	folded := map[string]string{}
	for k, v := range files {
		folded[k] = v
	}
	folded["lib/nodes.bot"] = "dsl: 2\n\ntool t:\n  description: \"probe\"\n  command: \"line one\\nline two\"\n"
	if w := put(folded); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a whole-bundle write that folds a stored value: %d %s", w.Code, w.Body.String())
	}
	after, err := s.botSources.GetBySlug(context.Background(), "t1", "whole")
	if err != nil {
		t.Fatal(err)
	}
	if after.Files["lib/nodes.bot"] != stored {
		t.Fatalf("the refused write landed anyway:\n%s", after.Files["lib/nodes.bot"])
	}
	// The control: a push that folds nothing still goes through.
	kept := map[string]string{}
	for k, v := range files {
		kept[k] = v
	}
	kept["lib/nodes.bot"] = "dsl: 2\n\ntool t:\n  description: \"probe, edited\"\n  command: `line one\nline two`\n"
	if w := put(kept); w.Code != http.StatusOK {
		t.Fatalf("a push that folds nothing was refused: %d %s", w.Code, w.Body.String())
	}
}

// TestEditingTheValueTheWriterFoldsDoesNotUnlockTheSave: the refusal must
// not be keyed on which values the file and the render have in COMMON.
// Keyed that way, the one value it missed was the one being EDITED — the
// `command:` script the author opened the file to change — so touching it
// wrote the whole file folded and answered 200, while leaving it alone
// answered 422. The control below is the half that makes this bite.
func TestEditingTheValueTheWriterFoldsDoesNotUnlockTheSave(t *testing.T) {
	workdir := t.TempDir()
	solo := foldNodes + "\nworkflow w:\n  entry: t\n  t -> done\n"
	writeUnitFixture(t, workdir, map[string]string{"solo.bot": solo})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "solo.bot")

	// The author edits the multi-line command itself.
	edited := editDocument(t, opened.Document, func(m map[string]any) {
		tools, _ := m["tools"].([]any)
		tools[0].(map[string]any)["command"] = "line one\nline two\nline three"
	})
	rec, _ := savePath(t, s, "solo.bot", edited, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("save after editing the folded value: %d %s", rec.Code, rec.Body.String())
	}
	if got := readFixture(t, workdir, "solo.bot"); got != solo {
		t.Fatalf("the file was rewritten anyway:\n%s", got)
	}
	// The control: a file the writer CAN reproduce still saves after the
	// same kind of edit, so the refusal is not "no save ever succeeds".
	writeUnitFixture(t, workdir, map[string]string{"plain.bot": "tool t:\n  description: \"probe\"\n  command: `line one\nline two`\n\nworkflow w:\n  entry: t\n  t -> done\n"})
	_, plain := openPath(t, s, "plain.bot")
	plainEdit := editDocument(t, plain.Document, func(m map[string]any) {
		tools, _ := m["tools"].([]any)
		tools[0].(map[string]any)["command"] = "line one\nline two\nline three"
	})
	if rec, _ := savePath(t, s, "plain.bot", plainEdit, ""); rec.Code != http.StatusOK {
		t.Fatalf("a profile-1 file the writer keeps over its lines was refused: %d %s", rec.Code, rec.Body.String())
	}
	if got := readFixture(t, workdir, "plain.bot"); !strings.Contains(got, "line three") || strings.Contains(got, "\\n") {
		t.Fatalf("the profile-1 edit did not land over its lines:\n%s", got)
	}
}

// TestRenderingAgainstAPathStaysInsideTheWorkspaceAndOnWorkflowFiles: a
// refusal echoes the text the server read, so the path it reads has the
// same two bounds every other route that reads a bot applies. A path that
// cannot be resolved is an error, never a quiet "nothing to compare" —
// that is how a guard stops firing without anyone seeing it.
func TestRenderingAgainstAPathStaysInsideTheWorkspaceAndOnWorkflowFiles(t *testing.T) {
	workdir := t.TempDir()
	solo := foldNodes + "\nworkflow w:\n  entry: t\n  t -> done\n"
	writeUnitFixture(t, workdir, map[string]string{
		"solo.bot":  solo,
		"notes.txt": "secret: a\nb: `one\ntwo`\n",
	})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "solo.bot")

	// Not a workflow file: refused before anything is read, so its content
	// is never echoed back.
	rec, out := unparseCall(t, s, map[string]any{"document": opened.Document, "path": "notes.txt"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a non-.bot path: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret") || strings.Contains(out.Source, "secret") {
		t.Fatalf("the answer carried the file's content: %s", rec.Body.String())
	}
	// A traversal is CONFINED, not rejected: safePathWithin cleans it back
	// inside the workdir (the same audited boundary Save uses), so the file
	// outside is never read and its text never comes back.
	outside := filepath.Join(filepath.Dir(workdir), "outside.bot")
	if err := os.WriteFile(outside, []byte("tool leaked:\n  command: `one\ntwo`\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })
	rec, escaped := unparseCall(t, s, map[string]any{"document": opened.Document, "path": "../outside.bot"})
	if strings.Contains(rec.Body.String(), "leaked") || strings.Contains(escaped.Source, "leaked") {
		t.Fatalf("a file outside the workspace was read: %s", rec.Body.String())
	}
	// A path the buffer is headed for but that is not written yet is a
	// legitimate absence: no before, no claim, and no error.
	rec, fresh := unparseCall(t, s, map[string]any{"document": opened.Document, "path": "new.bot"})
	if rec.Code != http.StatusOK || fresh.Refused != "" {
		t.Fatalf("a path not written yet: %d refused=%q", rec.Code, fresh.Refused)
	}
}

// TestThePerFileEditorRefusesAChangeToTheImportLines: the imports are not
// the per-file editor's to change. A save rebuilds every file's import
// lines AND the unit's membership from the files on disk, so an applied
// change to them is never written — it is silently undone. Removing one
// left the import in place and rewrote the fragment it named as the empty
// string, 200 OK; adding one made every later save refuse forever.
func TestThePerFileEditorRefusesAChangeToTheImportLines(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}
	openPath(t, s, "demo/main.bot")

	without := strings.Replace(unitFixtureMain, "import \"lib/nodes.bot\"\n\n", "", 1)
	rec, _ := parseCall(t, s, map[string]any{"path": "demo/main.bot", "file": "main.bot", "source": without})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("removing an import: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "import") {
		t.Fatalf("the refusal does not name what it is about: %s", rec.Body.String())
	}
	// The fragment the removed import named is untouched: the forbidden
	// alternative is an empty file on disk, not merely a non-200.
	if got := readFixture(t, workdir, "demo/lib/nodes.bot"); got != unitFixtureNodes {
		t.Fatalf("the fragment changed:\n%q", got)
	}

	added := strings.Replace(unitFixtureMain, "import \"lib/nodes.bot\"\n", "import \"lib/nodes.bot\"\nimport \"lib/more.bot\"\n", 1)
	if rec, _ := parseCall(t, s, map[string]any{"path": "demo/main.bot", "file": "main.bot", "source": added}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("adding an import: %d %s", rec.Code, rec.Body.String())
	}

	// The control: an edit to the main that leaves the imports alone still
	// applies, so the refusal is not "the main cannot be edited".
	edited := strings.Replace(unitFixtureMain, "entry: worker", "entry: worker\n  ## a note", 1)
	if rec, _ := parseCall(t, s, map[string]any{"path": "demo/main.bot", "file": "main.bot", "source": edited}); rec.Code != http.StatusOK {
		t.Fatalf("an edit that keeps the imports was refused: %d %s", rec.Code, rec.Body.String())
	}
}

// TestApplyingOneFileOfABotWhoseMainIsItselfAFragment: a companion
// workflow under lib/ that imports its siblings opens as a unit of its
// own. The staged map is keyed the way the loader keys it — from the
// directory of the main — and a mismatched key reads as "no staged text":
// the loader falls back to disk and the route answers 200 with the
// author's edit gone, which is the one failure a save cannot catch.
func TestApplyingOneFileOfABotWhoseMainIsItselfAFragment(t *testing.T) {
	workdir := t.TempDir()
	// A fragment — a file declaring no workflow — under lib/, importing a
	// sibling: loaded alone it is read from the bot's root under its lib/
	// name, so its rels are `lib/…` while its own directory is `demo/lib`.
	writeUnitFixture(t, workdir, map[string]string{
		"demo/lib/a.bot": "import \"b.bot\"\n\nagent worker:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: mission\n  output: out\n",
		"demo/lib/b.bot": "prompt mission:\n  Do the thing.\n\nschema out:\n  ok: bool\n",
	})
	s := &Server{cfg: Config{WorkDir: workdir}}
	rec, opened := openPath(t, s, "demo/lib/a.bot")
	if rec.Code != http.StatusOK || opened.Unit == nil {
		t.Fatalf("open: %d unit=%+v", rec.Code, opened.Unit)
	}

	edited := "prompt mission:\n  EDITED BY THE AUTHOR\n\nschema out:\n  ok: bool\n"
	rec, applied := parseCall(t, s, map[string]any{"path": "demo/lib/a.bot", "file": "lib/b.bot", "source": edited})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(applied.Document, &doc); err != nil {
		t.Fatal(err)
	}
	prompts, _ := doc["prompts"].([]any)
	if len(prompts) == 0 {
		t.Fatal("the answered document holds no prompt")
	}
	// The forbidden alternative, named: the text ON DISK, answered with
	// 200 as though the edit had been applied.
	if body := prompts[0].(map[string]any)["body"]; body != "EDITED BY THE AUTHOR" {
		t.Fatalf("the author's edit was dropped and the on-disk text answered instead: %v", body)
	}
}

// TestTheMergedViewSpeaksAboutTheTextItHandsOver: a fold in the MERGED
// program is a different question from a fold in any one file. The merged
// text is rendered at the merged profile, so a unit may hold a file
// `iterion fmt` refuses and still flatten faithfully — and a unit of files
// it accepts may flatten folded. Asked per file it answered both the wrong
// way round: it blocked a download that was byte-faithful, and stayed
// silent on one that was not.
func TestTheMergedViewSpeaksAboutTheTextItHandsOver(t *testing.T) {
	workdir := t.TempDir()
	// The main carries profile 2, so the merged program is written with
	// the strict escape and the fragment's spread value comes back on one
	// line.
	strictMain := "dsl: 2\n\nimport \"lib/nodes.bot\"\n\nworkflow w:\n  entry: t\n  t -> done\n"
	writeUnitFixture(t, workdir, map[string]string{"folds/main.bot": strictMain, "folds/lib/nodes.bot": foldNodes})
	// And a unit whose main keeps profile 1: its fragment is the very file
	// `fmt` refuses, yet the merged program carries the value over its
	// lines — the download is faithful and must not be blocked.
	writeUnitFixture(t, workdir, map[string]string{"faithful/main.bot": foldMain, "faithful/lib/nodes.bot": foldNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}

	_, folding := openPath(t, s, "folds/main.bot")
	rec, out := unparseCall(t, s, map[string]any{"document": folding.Document, "path": "folds/main.bot", "flatten": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("flatten: %d %s", rec.Code, rec.Body.String())
	}
	if out.Refused == "" {
		t.Fatalf("the merged program folds a value and said nothing:\n%s", out.Source)
	}
	if !strings.Contains(out.Refused, "lib/nodes.bot") {
		t.Fatalf("the reason does not name the file it is about: %q", out.Refused)
	}
	// It ANNOTATES: the merged program still comes back.
	if !strings.Contains(out.Source, "tool t:") {
		t.Fatalf("the merged program is missing:\n%s", out.Source)
	}

	_, faithful := openPath(t, s, "faithful/main.bot")
	rec, clean := unparseCall(t, s, map[string]any{"document": faithful.Document, "path": "faithful/main.bot", "flatten": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("flatten: %d %s", rec.Code, rec.Body.String())
	}
	// The forbidden alternative, named: a refusal on a download whose
	// bytes carry the value exactly as its author wrote it.
	if clean.Refused != "" {
		t.Fatalf("a faithful merged program was flagged: %q\n%s", clean.Refused, clean.Source)
	}
	if !strings.Contains(clean.Source, "line one\nline two") {
		t.Fatalf("the fixture does not separate the two cases — its merged text folds too:\n%s", clean.Source)
	}
}

// TestRepairingAMainWhoseBreakSwallowedAnImport: the one shape the
// per-file editor cannot serve, pinned so the message that names it stays
// true. A main whose unreadable region sat between two `import` lines
// opens as a SALVAGE, and the unit it opens as is missing the fragment the
// lost line named. The repair is then refused — the guard compares the
// repaired text against an AST the parser gave up on — and a save could
// not have placed the recovered fragment's declarations either, since it
// reads the unit's membership from the files on disk.
func TestRepairingAMainWhoseBreakSwallowedAnImport(t *testing.T) {
	workdir := t.TempDir()
	broken := "import \"lib/nodes.bot\"\n@@@\nimport \"lib/extra.bot\"\n\nworkflow w:\n  entry: worker\n  worker -> done\n"
	repaired := "import \"lib/nodes.bot\"\nimport \"lib/extra.bot\"\n\nworkflow w:\n  entry: worker\n  worker -> done\n"
	writeUnitFixture(t, workdir, map[string]string{
		"demo/main.bot":      broken,
		"demo/lib/nodes.bot": unitFixtureNodes,
		"demo/lib/extra.bot": "prompt extra:\n  More.\n",
	})
	s := &Server{cfg: Config{WorkDir: workdir}}
	rec, opened := openPath(t, s, "demo/main.bot")
	if rec.Code != http.StatusOK {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	if opened.Bindable {
		t.Fatal("a main that did not parse opened as bindable")
	}
	// The unit the studio holds is missing the fragment the lost line named.
	if len(opened.Unit.Files) != 2 {
		t.Fatalf("unit files %v, want the main and the fragment the salvage could still reach", opened.Unit.Files)
	}

	rec, _ = parseCall(t, s, map[string]any{"path": "demo/main.bot", "file": "main.bot", "source": repaired})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("repairing a swallowed import: %d %s", rec.Code, rec.Body.String())
	}
	// The refusal names the way out that exists — the file on disk — which
	// is what salvageRefusal tells the author before they try.
	if !strings.Contains(rec.Body.String(), "on disk") {
		t.Fatalf("the refusal names no reachable remedy: %s", rec.Body.String())
	}

	// The control: the SAME repair on a main whose break left both imports
	// readable is ACCEPTED by the apply — so the refusal above is about the
	// import, not about salvages in general.
	writeUnitFixture(t, workdir, map[string]string{
		"ok/main.bot":      "import \"lib/nodes.bot\"\n\n@@@\n\nworkflow w:\n  entry: worker\n  worker -> done\n",
		"ok/lib/nodes.bot": unitFixtureNodes,
	})
	_, salvaged := openPath(t, s, "ok/main.bot")
	if salvaged.Bindable {
		t.Fatal("the control's main parsed: it cannot witness a salvage")
	}
	fixed := "import \"lib/nodes.bot\"\n\nworkflow w:\n  entry: worker\n  worker -> done\n"
	rec, applied := parseCall(t, s, map[string]any{"path": "ok/main.bot", "file": "main.bot", "source": fixed})
	if rec.Code != http.StatusOK || !applied.Bindable {
		t.Fatalf("repairing a main whose imports survived: %d bindable=%v %s", rec.Code, applied.Bindable, rec.Body.String())
	}
}

// TestASalvagedUnitIsRepairedWhereTheSaveReadsIt: the apply answering 200
// is the SITE, not the guarantee. A save of a bot in several files
// re-derives the unit from the files as they are ON DISK and refuses one
// that does not load — so a main repaired only in the buffer never reaches
// the file it repairs, whatever the apply said. This is what
// `salvageRefusal` tells the author before they try, and it is why the
// message names the disk and not the Source view.
func TestASalvagedUnitIsRepairedWhereTheSaveReadsIt(t *testing.T) {
	workdir := t.TempDir()
	// The unreadable region swallows no import: this is exactly the case a
	// carve-out for the import would have called repairable.
	broken := "import \"lib/nodes.bot\"\n@@@\n\nworkflow w:\n  entry: worker\n  worker -> done\n"
	repaired := "import \"lib/nodes.bot\"\n\nworkflow w:\n  entry: worker\n  worker -> done\n"
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": broken, "demo/lib/nodes.bot": unitFixtureNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")
	if opened.Bindable {
		t.Fatal("the fixture's main parsed: it cannot witness a salvage")
	}

	rec, applied := parseCall(t, s, map[string]any{"path": "demo/main.bot", "file": "main.bot", "source": repaired})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	rec, _ = savePath(t, s, "demo/main.bot", applied.Document, opened.Unit.Revision)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("save after repairing a salvaged main in the buffer: %d %s", rec.Code, rec.Body.String())
	}
	// And it says where the repair belongs, which is what the studio's own
	// refusal says before the author types.
	if !strings.Contains(rec.Body.String(), "on disk") {
		t.Fatalf("the refusal names no reachable remedy: %s", rec.Body.String())
	}
	if got := readFixture(t, workdir, "demo/main.bot"); got != broken {
		t.Fatalf("the main changed:\n%s", got)
	}

	// The control changes ONE thing — the main is repaired where the bot's
	// files live — and everything else stays: same directory, same files,
	// same edit. That is what isolates the salvage as the cause, and it is
	// the executable proof of what the studio's refusal tells the author
	// to do ("repair the main where this bot's files live, then reopen").
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": repaired})
	_, reopened := openPath(t, s, "demo/main.bot")
	if !reopened.Bindable {
		t.Fatalf("the main repaired where the files live still does not parse")
	}
	edited := strings.Replace(unitFixtureNodes, "Do the thing.", "Do the other thing.", 1)
	rec, ap := parseCall(t, s, map[string]any{"path": "demo/main.bot", "file": "lib/nodes.bot", "source": edited})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply after the repair: %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ := savePath(t, s, "demo/main.bot", ap.Document, reopened.Unit.Revision); rec.Code != http.StatusOK {
		t.Fatalf("save after the repair: %d %s", rec.Code, rec.Body.String())
	}
	if got := readFixture(t, workdir, "demo/lib/nodes.bot"); !strings.Contains(got, "Do the other thing.") {
		t.Fatalf("the edit did not reach the file after the repair:\n%s", got)
	}
}

// TestTheCloudUnitWriteBackRefusesAFileTheWriterWouldFold: the cloud twin
// of saveUnit. It is one of the four write sites the fold refusal covers,
// and nothing in this package asserted it — restoring the pre-#1612
// behaviour there was invisible to every test.
func TestTheCloudUnitWriteBackRefusesAFileTheWriterWouldFold(t *testing.T) {
	s := &Server{}
	files := map[string]any{"main.bot": foldMain, "lib/nodes.bot": foldNodes}
	rec, parsed := parseCall(t, s, map[string]any{"files": files, "main": "main.bot"})
	if rec.Code != http.StatusOK {
		t.Fatalf("parse unit: %d %s", rec.Code, rec.Body.String())
	}
	edited := editDocument(t, parsed.Document, func(m map[string]any) {
		tools, _ := m["tools"].([]any)
		tools[0].(map[string]any)["description"] = "probe, edited"
	})
	rec, out := unparseCall(t, s, map[string]any{
		"document": edited, "files": files, "main": "main.bot", "revision": parsed.Unit.Revision,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("cloud write-back of a fragment the writer folds: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "lib/nodes.bot") {
		t.Fatalf("the refusal does not name the file: %s", rec.Body.String())
	}
	// The forbidden alternative, named — read off the REFUSAL's own body,
	// which is the only place it could appear: unparseCall decodes on 200
	// alone, so asserting on `out` here would be asserting on a zero value.
	if strings.Contains(rec.Body.String(), "\\nline two") {
		t.Fatalf("the folded fragment came back to be written:\n%s", rec.Body.String())
	}
	_ = out

	// The control: a fragment the writer CAN reproduce still comes back, so
	// the refusal is not "no cloud unit can be saved".
	ok := map[string]any{"main.bot": unitFixtureMain, "lib/nodes.bot": unitFixtureNodes}
	rec, fine := parseCall(t, s, map[string]any{"files": ok, "main": "main.bot"})
	if rec.Code != http.StatusOK {
		t.Fatalf("parse control unit: %d %s", rec.Code, rec.Body.String())
	}
	fineEdit := editDocument(t, fine.Document, func(m map[string]any) {
		agentsOf(m)[0].(map[string]any)["model"] = "anthropic/claude-opus-5"
	})
	rec, wrote := unparseCall(t, s, map[string]any{
		"document": fineEdit, "files": ok, "main": "main.bot", "revision": fine.Unit.Revision,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("control write-back: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(wrote.Files["lib/nodes.bot"], "anthropic/claude-opus-5") {
		t.Fatalf("the control's edit did not come back:\n%v", wrote.Files)
	}
}

// TestRenderingAFragmentThatDoesNotParse: a unit's `bindable` is the
// MAIN's parse alone, so a FRAGMENT with a syntax error leaves the buffer
// unsalvaged and the per-file view on. The loader keeps that file's
// partial AST, so the part handed to the render is missing everything
// after the error — rendered back it is a text the author's file does not
// contain, with the lines they have to fix gone.
func TestRenderingAFragmentThatDoesNotParse(t *testing.T) {
	workdir := t.TempDir()
	broken := unitFixtureNodes + "\nagent \n  model\n"
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": broken})
	s := &Server{cfg: Config{WorkDir: workdir}}
	rec, opened := openPath(t, s, "demo/main.bot")
	if rec.Code != http.StatusOK {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	if !opened.Bindable {
		t.Fatal("the MAIN parses: this fixture must leave the buffer unsalvaged")
	}

	rec, out := unparseCall(t, s, map[string]any{"document": opened.Document, "path": "demo/main.bot", "file": "lib/nodes.bot"})
	if rec.Code != http.StatusOK {
		t.Fatalf("render: %d %s", rec.Code, rec.Body.String())
	}
	if out.Refused == "" {
		t.Fatal("a fragment the parser could only salvage was rendered with no reason given")
	}
	// The forbidden alternative, named: the writer's text for the partial
	// AST — the file MINUS the region the parser could not read.
	if out.Source != broken {
		t.Fatalf("the view shows a text the file does not contain:\n%s", out.Source)
	}
}

// TestThePerFileEditorRefusesAChangeToTheProfile: splitByProvenance
// rebuilds every part with the STORED file's profile, so a `dsl:` line
// changed here is never written — while the declarations WOULD have been
// read under the new one. Same shape as the import refusal, same reason.
func TestThePerFileEditorRefusesAChangeToTheProfile(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}
	openPath(t, s, "demo/main.bot")

	raised := "dsl: 2\n\n" + unitFixtureNodes
	rec, _ := parseCall(t, s, map[string]any{"path": "demo/main.bot", "file": "lib/nodes.bot", "source": raised})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("raising the profile of one file: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "dsl:") {
		t.Fatalf("the refusal does not name what it is about: %s", rec.Body.String())
	}
	if got := readFixture(t, workdir, "demo/lib/nodes.bot"); got != unitFixtureNodes {
		t.Fatalf("the fragment changed:\n%s", got)
	}
	// The control: an edit that leaves the profile alone still applies.
	edited := strings.Replace(unitFixtureNodes, "Do the thing.", "Do the other thing.", 1)
	if rec, _ := parseCall(t, s, map[string]any{"path": "demo/main.bot", "file": "lib/nodes.bot", "source": edited}); rec.Code != http.StatusOK {
		t.Fatalf("an edit that keeps the profile was refused: %d %s", rec.Code, rec.Body.String())
	}
}
