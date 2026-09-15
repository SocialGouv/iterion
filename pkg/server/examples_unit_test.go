package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/bots"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// oneProgram is what the studio receives for a single-file example or an
// embedded bot: text and document, no unit.
type oneProgram struct {
	Source            string          `json:"source"`
	Document          json.RawMessage `json:"document"`
	Diagnostics       []string        `json:"diagnostics"`
	ConfirmedDiskPath string          `json:"confirmed_disk_path"`
	Unit              *unitInfo       `json:"unit"`
}

// assertWholeFlatProgram checks a served text is a program on its own —
// no import left, no error, and it compiles to the node set of the
// embedded unit it stands for — and that its document names no file.
func assertWholeFlatProgram(t *testing.T, name string, got oneProgram) {
	t.Helper()
	pr := parser.Parse(name, got.Source)
	if pr.File == nil {
		t.Fatalf("the served text does not parse: %v", pr.Diagnostics)
	}
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Errorf("served text: %s", d.Error())
		}
	}
	if len(pr.File.Imports) > 0 {
		t.Fatalf("the served text still imports %d file(s): the main alone was served", len(pr.File.Imports))
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		t.Fatal("the served text compiles to no workflow")
	}
	for _, d := range cr.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Errorf("served text compile: %s", d.Error())
		}
	}
	files, main, ok := bots.Sources(name)
	if !ok {
		t.Fatalf("%s is not embedded", name)
	}
	u := unit.LoadMap(files, main)
	if len(u.Files) < 2 {
		t.Fatalf("%s is embedded as %d file(s): the test lost its witness", name, len(u.Files))
	}
	want := ir.Compile(u.Merged)
	if want.Workflow == nil || len(cr.Workflow.Nodes) != len(want.Workflow.Nodes) || len(cr.Workflow.Edges) != len(want.Workflow.Edges) {
		t.Fatalf("served program has %d nodes / %d edges, the unit %d / %d", len(cr.Workflow.Nodes), len(cr.Workflow.Edges), len(want.Workflow.Nodes), len(want.Workflow.Edges))
	}
	f, err := ast.UnmarshalFile(got.Document)
	if err != nil {
		t.Fatalf("served document: %v", err)
	}
	if hasProvenance(f) {
		t.Error("the served document names files: the studio would treat a flat program as a unit")
	}
	if got.Unit != nil {
		t.Error("a flat program was served with unit info")
	}
	if got.ConfirmedDiskPath != "" {
		t.Errorf("an embedded bot was presented as disk-confirmed: %q", got.ConfirmedDiskPath)
	}
}

// The examples list names recipes, never the fragments a recipe is made of.
func TestListExamplesNamesNoFragment(t *testing.T) {
	_, hs := newTestServer(t)
	resp, err := http.Get(hs.URL + "/api/examples")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var entries []exampleEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	seenMain := false
	for _, e := range entries {
		if strings.Contains(e.Name, "/"+unit.FragmentDir+"/") {
			t.Errorf("a fragment is listed as a recipe: %s", e.Name)
		}
		if e.Name == "feature-dev/main.bot" {
			seenMain = true
		}
	}
	if !seenMain {
		t.Fatal("feature-dev/main.bot is not listed")
	}
}

// An embedded bot in several files is served by /api/examples as one flat
// program: what the studio launches inline and may save as a new file.
func TestLoadExampleServesAnEmbeddedBotInSeveralFilesAsOneProgram(t *testing.T) {
	_, hs := newTestServer(t)
	resp, err := http.Get(hs.URL + "/api/examples/feature-dev/main.bot")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var got oneProgram
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	assertWholeFlatProgram(t, "feature-dev/main.bot", got)
}

// The file-open fallback to the embed serves the same flat program.
func TestOpenFileFallsBackToTheEmbeddedBotAsOneProgram(t *testing.T) {
	s := &Server{cfg: Config{WorkDir: t.TempDir()}}
	body, _ := json.Marshal(openFileRequest{Path: "bots/feature-dev/main.bot"})
	req := httptest.NewRequest(http.MethodPost, "/api/files/open", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleOpenFile(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got oneProgram
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	assertWholeFlatProgram(t, "feature-dev/main.bot", got)
}

// A bot in several files ON DISK is served by /api/examples as its unit,
// the way /api/files/open serves it: the merged document with each
// declaration's file, the unit's files and revision, the disk path.
func TestLoadExampleOpensADiskBotInSeveralFilesAsItsUnit(t *testing.T) {
	examples := t.TempDir()
	if err := os.MkdirAll(filepath.Join(examples, "x", unit.FragmentDir), 0o755); err != nil {
		t.Fatal(err)
	}
	mainSrc := "import \"" + unit.FragmentDir + "/nodes.bot\"\n\nworkflow x:\n  entry: done\n"
	if err := os.WriteFile(filepath.Join(examples, "x", "main.bot"), []byte(mainSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(examples, "x", unit.FragmentDir, "nodes.bot"), []byte("prompt p:\n  Hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	srv := New(Config{
		WorkDir:                 workDir,
		StoreDir:                filepath.Join(workDir, ".iterion"),
		ExamplesDir:             examples,
		SkipProjectRegistration: true,
	}, iterlog.New(iterlog.LevelError, os.Stderr))
	hs := httptest.NewServer(srv.mux)
	t.Cleanup(hs.Close)

	resp, err := http.Get(hs.URL + "/api/examples/x/main.bot")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var got struct {
		Source            string          `json:"source"`
		Document          json.RawMessage `json:"document"`
		Path              string          `json:"path"`
		ConfirmedDiskPath string          `json:"confirmed_disk_path"`
		Unit              *unitInfo       `json:"unit"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Source != mainSrc {
		t.Errorf("source is not the main's text:\n%s", got.Source)
	}
	if got.Unit == nil || len(got.Unit.Files) != 2 || got.Unit.Revision == "" {
		t.Fatalf("unit info %+v, want 2 files and a revision", got.Unit)
	}
	if got.Path != "bots/x/main.bot" || got.Unit.Root != "bots/x" {
		t.Errorf("path %q root %q, want bots/x/main.bot under bots/x", got.Path, got.Unit.Root)
	}
	if want := filepath.Join(examples, "x", "main.bot"); got.ConfirmedDiskPath != want {
		t.Errorf("confirmed_disk_path %q, want %q", got.ConfirmedDiskPath, want)
	}
	f, err := ast.UnmarshalFile(got.Document)
	if err != nil {
		t.Fatal(err)
	}
	if !hasProvenance(f) {
		t.Error("the merged document names no file: a save could not route each declaration home")
	}
	if len(f.Prompts) != 1 {
		t.Errorf("the merged document holds %d prompt(s), want the fragment's one", len(f.Prompts))
	}
}
