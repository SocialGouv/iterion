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
)

// openFileFor posts one path to /api/files/open and decodes the answer.
func openFileFor(t *testing.T, s *Server, path string) struct {
	Source            string          `json:"source"`
	Document          json.RawMessage `json:"document"`
	Diagnostics       []string        `json:"diagnostics"`
	Path              string          `json:"path"`
	ConfirmedDiskPath string          `json:"confirmed_disk_path"`
} {
	t.Helper()
	var out struct {
		Source            string          `json:"source"`
		Document          json.RawMessage `json:"document"`
		Diagnostics       []string        `json:"diagnostics"`
		Path              string          `json:"path"`
		ConfirmedDiskPath string          `json:"confirmed_disk_path"`
	}
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

// TestAFileThatDoesNotParseIsNotBoundToItsPath.
//
// A parse that leaves errors yields the program the parser SALVAGED — the
// file minus the region it could not read. Handed back with its path, the
// studio binds it and marks it saved, and the first Save writes the salvaged
// document over what the author wrote. The loss is silent and total for the
// unreadable part.
//
// So nothing is bound: the text and the salvaged program travel with the
// diagnostics, and a save has to ask where. `serveDiskExample` already takes
// this posture for `/api/examples/{name}`; this is the same file, opened the
// other way.
func TestAFileThatDoesNotParseIsNotBoundToItsPath(t *testing.T) {
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
	if got.Path != "" || got.ConfirmedDiskPath != "" {
		t.Errorf("bound to path=%q confirmed=%q — a save would land the salvaged program on the author's file",
			got.Path, got.ConfirmedDiskPath)
	}
	// The text still travels: the editor must be able to show what is there.
	if got.Source != source {
		t.Errorf("source = %q, want the file's own bytes", got.Source)
	}
}

// TestAFileThatParsesIsStillBound is the end state the check drives toward.
// Without it, refusing to bind everything would look exactly as green.
func TestAFileThatParsesIsStillBound(t *testing.T) {
	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, "fine.bot"), []byte("workflow fine:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{WorkDir: workdir}}

	got := openFileFor(t, s, "fine.bot")

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
