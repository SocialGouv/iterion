package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// openedFile is what /api/files/open answers, named once: the shape was
// spelled twice — the return type and the decode target — and a field added
// to one of them compiles into a test that silently reads the zero value.
type openedFile struct {
	Source            string          `json:"source"`
	Document          json.RawMessage `json:"document"`
	Diagnostics       []string        `json:"diagnostics"`
	Path              string          `json:"path"`
	ConfirmedDiskPath string          `json:"confirmed_disk_path"`
	Bindable          bool            `json:"bindable"`
	Unit              *unitInfo       `json:"unit"`
}

// openFileFor posts one path to /api/files/open and decodes the answer.
func openFileFor(t *testing.T, s *Server, path string) openedFile {
	t.Helper()
	var out openedFile
	body, err := json.Marshal(openFileRequest{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleOpenFile(rec, httptest.NewRequest(http.MethodPost, "/api/files/open", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("open %s: status %d, body %s", path, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestAFileThatDoesNotParseIsNotWritable.
//
// A parse that leaves errors yields the program the parser SALVAGED — the
// file minus the region it could not read. Handed back as writable, the
// studio marks it saved and the first Save writes the salvaged document over
// what the author wrote. The loss is silent and total for the unreadable
// part.
//
// So the answer says the document is a salvage, and the three sites that
// write one refuse it. The PATH still travels: the editor is about that file,
// and the watcher, the tab binding, the validation scope and the assistant's
// perimeter all read it — taking it away moves the loss instead of removing
// it. `serveDiskExample` takes the same posture for `/api/examples/{name}`.
func TestAFileThatDoesNotParseIsNotWritable(t *testing.T) {
	workdir := t.TempDir()
	// A workflow the parser reads, followed by a declaration it cannot.
	source := "workflow keep:\n  entry: done\n\nagent broken\n  this line is not a declaration\n"
	if err := os.WriteFile(filepath.Join(workdir, "broken.bot"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{WorkDir: workdir}}

	got := openFileFor(t, s, "broken.bot")

	if len(got.Diagnostics) == 0 {
		t.Fatal("no diagnostics: this fixture is supposed to be unparseable, so the test would prove nothing")
	}
	if got.Bindable {
		t.Error("declared writable — a save would land the salvaged program on the author's file")
	}
	// The path stays, and so does the disk path: every surface that asks
	// WHICH file this editor is about reads them.
	if got.Path != "broken.bot" || got.ConfirmedDiskPath == "" {
		t.Errorf("a file that does not parse lost its path: path=%q confirmed=%q", got.Path, got.ConfirmedDiskPath)
	}
	// The text still travels: the editor must be able to show what is there.
	if got.Source != source {
		t.Errorf("source = %q, want the file's own bytes", got.Source)
	}
}

// TestAMainThatDoesNotParseIsASalvageOnEveryRoute.
//
// A main whose syntax broke but whose `import` line survived the salvage is
// served as a UNIT: the merged document is that salvage plus the fragments.
// The three routes that answer for it must say the same thing, or the same
// file is a salvage on one and the program on another — and the sites that
// export it (Download, Copy source, the Source view) follow whichever route
// opened it, so a disagreement hands the author a .bot missing the region
// the parser could not read.
func TestAMainThatDoesNotParseIsASalvageOnEveryRoute(t *testing.T) {
	// The import survives; the declaration under it does not parse.
	frag := unit.FragmentDir + "/nodes.bot"
	main := "import \"" + frag + "\"\n\nworkflow k:\n  entry: done\n\nagent broken\n  not a declaration\n"

	workdir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workdir, unit.FragmentDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "k.bot"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, frag), []byte("prompt p:\n  Hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{WorkDir: workdir}}

	opened := openFileFor(t, s, "k.bot")
	if opened.Unit == nil {
		t.Fatalf("the fixture is not the case under test: it did not open as a unit (diags %v)", opened.Diagnostics)
	}
	if opened.Bindable {
		t.Error("/api/files/open called a unit whose main did not parse the program")
	}

	// Same file, same verdict, through /api/parse on the files map — the
	// route a cloud bot source takes.
	parsed := parseFor(t, parseRequest{
		Files: map[string]string{"k.bot": main, frag: "prompt p:\n  Hello\n"},
		Main:  "k.bot",
	})
	if parsed.Unit == nil {
		t.Fatalf("the files-map fixture did not parse as a unit (diags %v)", parsed.Diagnostics)
	}
	if parsed.Bindable {
		t.Error("/api/parse called a unit whose main did not parse the program")
	}
}

// TestAFileThatParsesIsStillWritable is the end state the check drives
// toward. Without it, refusing every write would look exactly as green.
func TestAFileThatParsesIsStillWritable(t *testing.T) {
	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, "fine.bot"), []byte("workflow fine:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{WorkDir: workdir}}

	got := openFileFor(t, s, "fine.bot")

	if !got.Bindable {
		t.Error("a file that parses clean was declared a salvage: nothing could be saved")
	}
	if got.Path != "fine.bot" {
		t.Errorf("path = %q, want a clean file bound to its own path", got.Path)
	}
	if got.ConfirmedDiskPath == "" {
		t.Error("no confirmed disk path for a file read from disk")
	}
}

// TestOnlyAnErrorUnbinds pins the severity rule directly, because no parser
// diagnostic is a warning TODAY — `parser.SeverityWarning` exists in the type
// and nothing emits it. A fixture would therefore prove nothing: it could only
// be a clean file, which the test above already covers.
//
// The rule is kept anyway, and mirrors unit.HasErrors: the day a parse warns,
// "any diagnostic unbinds" would send every warned file through Save-as.
func TestOnlyAnErrorUnbinds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		diags []parser.Diagnostic
		want  bool
	}{
		{"nothing", nil, false},
		{"a warning alone", []parser.Diagnostic{{Severity: parser.SeverityWarning}}, false},
		{"an error", []parser.Diagnostic{{Severity: parser.SeverityError}}, true},
		{"a warning and an error", []parser.Diagnostic{
			{Severity: parser.SeverityWarning}, {Severity: parser.SeverityError},
		}, true},
	} {
		if got := parseHasErrors(tc.diags); got != tc.want {
			t.Errorf("%s: parseHasErrors = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// parseFor posts one body to /api/parse and decodes the answer.
type parsedProgram struct {
	Document    json.RawMessage `json:"document"`
	Diagnostics []string        `json:"diagnostics"`
	Unit        *unitInfo       `json:"unit"`
	Bindable    bool            `json:"bindable"`
}

func parseFor(t *testing.T, req parseRequest) parsedProgram {
	t.Helper()
	var out parsedProgram
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{WorkDir: t.TempDir()}}
	rec := httptest.NewRecorder()
	s.handleParse(rec, httptest.NewRequest(http.MethodPost, "/api/parse", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("parse: status %d, body %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestParseCarriesTheBindingVerdict.
//
// A cloud bot source never travels through /api/files/open: the studio fetches
// its files and parses them HERE, then builds the open answer itself. Without
// the verdict on this answer, that surface binds a salvaged document and its
// save — a versioned PUT of the rendered document, with no refusal behind it —
// writes the salvage onto the bot source. The same loss as on disk, one route
// over.
func TestParseCarriesTheBindingVerdict(t *testing.T) {
	const broken = "workflow y:\n  entry: done\n\nagent a\n  not a declaration\n"
	got := parseFor(t, parseRequest{Source: broken})
	if len(got.Diagnostics) == 0 || len(got.Document) == 0 {
		t.Fatalf("the fixture is not the case under test: %d diagnostics, %d bytes of document", len(got.Diagnostics), len(got.Document))
	}
	if got.Bindable {
		t.Error("a source the parser only salvaged was declared bindable")
	}

	if clean := parseFor(t, parseRequest{Source: "workflow y:\n  entry: done\n"}); !clean.Bindable {
		t.Errorf("a source that parses clean was declared not bindable: %v", clean.Diagnostics)
	}

	// A unit stays writable, LOADING OR NOT: unparseUnitFiles refuses to
	// write one that does not load, naming the fragment at fault, which is
	// what makes keeping it writable safe. Both halves are exercised —
	// asserting only the loading one leaves the load-bearing half of that
	// decision covered by nothing.
	frag := unit.FragmentDir + "/nodes.bot"
	for _, tc := range []struct {
		name     string
		fragment string
	}{
		{"a unit that loads", "prompt p:\n  Hello\n"},
		{"a unit whose fragment does not parse", "prompt p\n  !!! broken @@@\n"},
	} {
		unitGot := parseFor(t, parseRequest{
			Files: map[string]string{
				"main.bot": "import \"" + frag + "\"\n\nworkflow x:\n  entry: done\n",
				frag:       tc.fragment,
			},
			Main: "main.bot",
		})
		if !unitGot.Bindable || unitGot.Unit == nil || unitGot.Unit.Revision == "" {
			t.Errorf("%s was declared not writable: bindable %v unit %v", tc.name, unitGot.Bindable, unitGot.Unit != nil)
		}
	}
}
