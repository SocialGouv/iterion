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

// oneProgram is what the studio receives for a single-file example, an
// embedded bot, or a bot outside the working directory: text and document,
// no unit, no disk path.
type oneProgram struct {
	Source            string          `json:"source"`
	Document          json.RawMessage `json:"document"`
	Diagnostics       []string        `json:"diagnostics"`
	Path              string          `json:"path"`
	ConfirmedDiskPath string          `json:"confirmed_disk_path"`
	Unit              *unitInfo       `json:"unit"`
}

// parseFlat parses a served text as the one program it claims to be: no
// import left, no error, a workflow compiled.
func parseFlat(t *testing.T, name, source string) *ir.CompileResult {
	t.Helper()
	pr := parser.Parse(name, source)
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
	return cr
}

// assertOneProgram checks a response is one program and nothing more: no
// unit, no disk path, a document that names no file.
func assertOneProgram(t *testing.T, got oneProgram) *ast.File {
	t.Helper()
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
		t.Errorf("a flat program was presented as disk-confirmed: %q", got.ConfirmedDiskPath)
	}
	return f
}

// assertWholeFlatProgram checks a served text is the program the embedded
// unit is — the same program, not a text with the same counts.
func assertWholeFlatProgram(t *testing.T, name string, got oneProgram) {
	t.Helper()
	cr := parseFlat(t, name, got.Source)
	files, main, ok := bots.Sources(name)
	if !ok {
		t.Fatalf("%s is not embedded", name)
	}
	u := unit.LoadMap(files, main)
	if len(u.Files) < 2 {
		t.Fatalf("%s is embedded as %d file(s): the test lost its witness", name, len(u.Files))
	}
	if why := ir.SameProgram(ir.Compile(u.Merged), cr); why != "" {
		t.Fatalf("the served program differs from the unit: %s", why)
	}
	assertOneProgram(t, got)
}

func getExampleJSON(t *testing.T, url string, into any) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK && into != nil {
		if err := json.Unmarshal(body, into); err != nil {
			t.Fatalf("decode %s: %v\n%s", url, err, body)
		}
	}
	return resp.StatusCode, body
}

// The examples list names recipes, never the fragments a recipe is made of.
func TestListExamplesNamesNoFragment(t *testing.T) {
	_, hs := newTestServer(t)
	var entries []exampleEntry
	if code, body := getExampleJSON(t, hs.URL+"/api/examples", &entries); code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
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
	var got oneProgram
	if code, body := getExampleJSON(t, hs.URL+"/api/examples/feature-dev/main.bot", &got); code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
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

// writeExampleUnitFixture writes a bot in two files, x/main.bot importing
// lib/nodes.bot, under dir; it returns the main's text.
func writeExampleUnitFixture(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "x", unit.FragmentDir), 0o755); err != nil {
		t.Fatal(err)
	}
	mainSrc := "import \"" + unit.FragmentDir + "/nodes.bot\"\n\nworkflow x:\n  entry: done\n"
	if err := os.WriteFile(filepath.Join(dir, "x", "main.bot"), []byte(mainSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x", unit.FragmentDir, "nodes.bot"), []byte("prompt p:\n  Hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return mainSrc
}

// exampleServer is a local server on workDir whose Home reads its bots
// from examplesDir.
func exampleServer(t *testing.T, workDir, examplesDir string) *httptest.Server {
	t.Helper()
	srv := New(Config{
		WorkDir:                 workDir,
		StoreDir:                filepath.Join(workDir, ".iterion"),
		ExamplesDir:             examplesDir,
		SkipProjectRegistration: true,
	}, iterlog.New(iterlog.LevelError, os.Stderr))
	hs := httptest.NewServer(srv.mux)
	t.Cleanup(hs.Close)
	return hs
}

// A bot in several files INSIDE the working directory — under bots/ or the
// legacy examples/ — is served as its unit under the path the studio opens
// and saves it by, and /api/files/open answers for that very path.
func TestLoadExampleOpensADiskBotInsideTheWorkDirAsItsUnit(t *testing.T) {
	for _, sub := range []string{"bots", "examples"} {
		t.Run(sub, func(t *testing.T) {
			workDir := t.TempDir()
			examples := filepath.Join(workDir, sub)
			mainSrc := writeExampleUnitFixture(t, examples)
			hs := exampleServer(t, workDir, examples)

			var got oneProgram
			if code, body := getExampleJSON(t, hs.URL+"/api/examples/x/main.bot", &got); code != http.StatusOK {
				t.Fatalf("status %d: %s", code, body)
			}
			if got.Source != mainSrc {
				t.Errorf("source is not the main's text:\n%s", got.Source)
			}
			if got.Unit == nil || len(got.Unit.Files) != 2 || got.Unit.Revision == "" {
				t.Fatalf("unit info %+v, want 2 files and a revision", got.Unit)
			}
			if want := sub + "/x/main.bot"; got.Path != want || got.Unit.Root != sub+"/x" {
				t.Errorf("path %q root %q, want %s under %s/x", got.Path, got.Unit.Root, want, sub)
			}
			want, err := filepath.EvalSymlinks(filepath.Join(examples, "x", "main.bot"))
			if err != nil {
				t.Fatal(err)
			}
			if got.ConfirmedDiskPath != want {
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

			// The path the studio was handed is one it can open: the launch's
			// document fetch, the watcher and the save all go through it.
			body, _ := json.Marshal(openFileRequest{Path: got.Path})
			resp, err := http.Post(hs.URL+"/api/files/open", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var opened oneProgram
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("/api/files/open %q: status %d: %s", got.Path, resp.StatusCode, raw)
			}
			if err := json.Unmarshal(raw, &opened); err != nil {
				t.Fatal(err)
			}
			if opened.Unit == nil || opened.Unit.Revision != got.Unit.Revision || opened.ConfirmedDiskPath != got.ConfirmedDiskPath {
				t.Errorf("/api/files/open answers differently for the same path: unit %+v confirmed %q", opened.Unit, opened.ConfirmedDiskPath)
			}
		})
	}
}

// A bot in several files OUTSIDE the working directory — a catalog root the
// studio cannot open by a relative path — is served as one flat program,
// like an embedded bot, the fragment's declarations in it.
func TestLoadExampleServesADiskBotOutsideTheWorkDirAsOneProgram(t *testing.T) {
	workDir := t.TempDir()
	examples := t.TempDir()
	writeExampleUnitFixture(t, examples)
	hs := exampleServer(t, workDir, examples)

	var got oneProgram
	if code, body := getExampleJSON(t, hs.URL+"/api/examples/x/main.bot", &got); code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	parseFlat(t, "x/main.bot", got.Source)
	f := assertOneProgram(t, got)
	if len(f.Prompts) != 1 {
		t.Errorf("the flat program holds %d prompt(s), want the fragment's one", len(f.Prompts))
	}
	if got.Path != "" {
		t.Errorf("a path %q was named for a bot the studio cannot open", got.Path)
	}
}

// A main whose fragment is missing is never served as the main alone: the
// diagnostics name the fragment.
func TestLoadExampleTellsAMissingFragment(t *testing.T) {
	workDir := t.TempDir()
	examples := filepath.Join(workDir, "bots")
	if err := os.MkdirAll(filepath.Join(examples, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(examples, "x", "main.bot"), []byte("import \""+unit.FragmentDir+"/missing.bot\"\n\nworkflow x:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hs := exampleServer(t, workDir, examples)
	var got oneProgram
	code, body := getExampleJSON(t, hs.URL+"/api/examples/x/main.bot", &got)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	named := false
	for _, d := range got.Diagnostics {
		if strings.Contains(d, "missing.bot") {
			named = true
		}
	}
	if !named {
		t.Fatalf("the missing fragment is not named: diagnostics %v", got.Diagnostics)
	}
}

// recipeFromSources hands a bot in one file over as its bytes, writes a bot
// in several files out as one program, and refuses a main whose imports do
// not resolve rather than serve it alone.
func TestRecipeFromSources(t *testing.T) {
	one := "workflow x:\n  entry: done\n"
	if got, err := recipeFromSources("one", map[string]string{"main.bot": one}, "main.bot"); err != nil || got != one {
		t.Fatalf("one file: %q, %v", got, err)
	}
	several := map[string]string{
		"main.bot":      "import \"" + unit.FragmentDir + "/nodes.bot\"\n\nworkflow x:\n  entry: done\n",
		"lib/nodes.bot": "prompt p:\n  Hello\n",
	}
	flat, err := recipeFromSources("several", several, "main.bot")
	if err != nil {
		t.Fatal(err)
	}
	cr := parseFlat(t, "several", flat)
	if len(cr.Workflow.Prompts) != 1 {
		t.Errorf("the flat program compiles %d prompt(s), want the fragment's one", len(cr.Workflow.Prompts))
	}
	unresolved := map[string]string{"main.bot": "import \"" + unit.FragmentDir + "/missing.bot\"\n\nworkflow x:\n  entry: done\n"}
	if got, err := recipeFromSources("unresolved", unresolved, "main.bot"); err == nil || !strings.Contains(err.Error(), "missing.bot") {
		t.Fatalf("a main whose import does not resolve was served (%q, err %v)", got, err)
	}
}
