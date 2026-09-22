package cli

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

const looseBot = `## A header the writer keeps.


schema verdict:
  ok: bool



agent check:
  model: "m"
  output: verdict

workflow w:
  worktree: none
  sandbox: none
  entry: check
  check -> done
`

func writeBot(t *testing.T, rel, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rel, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return rel
}

func humanPrinter() (*Printer, *bytes.Buffer) {
	var buf bytes.Buffer
	return &Printer{Format: OutputHuman, W: &buf}, &buf
}

// fmt writes the canonical text — the writer's, proven the same program —
// keeps the leading comment, is idempotent, and --check says so before
// and after.
func TestFmtWritesTheCanonicalTextOnce(t *testing.T) {
	inTempWorkspace(t)
	path := writeBot(t, "loose.bot", looseBot)
	p, out := humanPrinter()
	if _, err := RunFmt(FmtOptions{Paths: []string{path}, Check: true, Printer: p}); !errors.Is(err, ErrFmtWouldChange) {
		t.Fatalf("--check on a loose file: %v\n%s", err, out.String())
	}
	if got, _ := os.ReadFile(path); string(got) != looseBot {
		t.Fatal("--check wrote the file")
	}
	res, err := RunFmt(FmtOptions{Paths: []string{path}, Printer: p})
	if err != nil {
		t.Fatalf("fmt: %v\n%s", err, out.String())
	}
	if len(res.Files) != 1 || !res.Files[0].Changed || !res.Files[0].Written {
		t.Fatalf("outcome %+v", res.Files)
	}
	got, _ := os.ReadFile(path)
	want := unparse.Unparse(parser.Parse(path, looseBot).File)
	if string(got) != want {
		t.Fatalf("the file is not the writer's text:\n%s\n--- want ---\n%s", got, want)
	}
	if !strings.HasPrefix(string(got), "## A header the writer keeps.\n") {
		t.Fatalf("the leading comment did not survive:\n%s", got)
	}
	if !strings.Contains(out.String(), "formatted loose.bot") {
		t.Fatalf("the report does not say what was written:\n%s", out.String())
	}
	again, err := RunFmt(FmtOptions{Paths: []string{path}, Check: true})
	if err != nil || again.Files[0].Changed {
		t.Fatalf("fmt is not idempotent: %v %+v", err, again.Files)
	}
}

// A comment after the first declaration would move to the head or be lost:
// the file is refused and left byte for byte; a file that does not parse
// likewise; the files beside them are formatted all the same.
func TestFmtRefusesWhatItCannotRewriteAndFormatsTheRest(t *testing.T) {
	inTempWorkspace(t)
	commentedText := strings.Replace(looseBot, "  model: \"m\"\n", "  ## the model\n  model: \"m\"\n", 1)
	commented := writeBot(t, "c/commented.bot", commentedText)
	broken := writeBot(t, "c/broken.bot", "agent :\n  model\n")
	loose := writeBot(t, "c/loose.bot", looseBot)
	jp, out := jsonPrinter()
	res, err := RunFmt(FmtOptions{Paths: []string{"c"}, Printer: jp})
	if !errors.Is(err, ErrFmtRefused) {
		t.Fatalf("one refusal and the error is %v", err)
	}
	if len(res.Refused) != 1 || !strings.Contains(res.Refused[0], "does not parse") {
		t.Fatalf("refusals: %v", res.Refused)
	}
	if got, _ := os.ReadFile(broken); string(got) != "agent :\n  model\n" {
		t.Fatalf("%s was rewritten:\n%s", broken, got)
	}
	// The commented file is formatted like any other now, and its comment
	// comes back above the line it led (#1282).
	if got, _ := os.ReadFile(commented); !strings.Contains(string(got), "  ## the model\n  model: \"m\"\n") {
		t.Fatalf("the comment did not keep its place:\n%s", got)
	}
	if len(res.Files) != 2 {
		t.Fatalf("the files beside the refused one were not formatted: %+v", res.Files)
	}
	for _, f := range res.Files {
		if !f.Written || (f.Path != loose && f.Path != commented) {
			t.Fatalf("outcome %+v", res.Files)
		}
	}
	var decoded struct {
		Files   []FmtFile `json:"files"`
		Refused []string  `json:"refused"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || len(decoded.Files) != 2 || len(decoded.Refused) != 1 {
		t.Fatalf("the JSON report: %v\n%s", err, out.String())
	}
}

// A bundle directory is walked: the main and its lib/ fragment are each
// formatted on their own text; a comment before the import line leads the
// file and stays first.
func TestFmtWalksABundleAndItsFragments(t *testing.T) {
	inTempWorkspace(t)
	main := writeBot(t, "b/main.bot", "## before the import: a leading comment\nimport \"lib/s.bot\"\n\n\nagent check:\n  model: \"m\"\n  output: verdict\n\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: check\n  check -> done\n")
	frag := writeBot(t, "b/lib/s.bot", "\n\nschema verdict:\n  ok: bool\n")
	res, err := RunFmt(FmtOptions{Paths: []string{"b"}})
	if err != nil {
		t.Fatalf("fmt: %v %+v", err, res)
	}
	if len(res.Files) != 2 || res.Files[0].Path != frag || res.Files[1].Path != main || !res.Files[0].Written || !res.Files[1].Written {
		t.Fatalf("outcome %+v", res.Files)
	}
	got, _ := os.ReadFile(main)
	if !strings.HasPrefix(string(got), "## before the import: a leading comment\n") || !strings.Contains(string(got), "import \"lib/s.bot\"\n") {
		t.Fatalf("the main lost its head:\n%s", got)
	}
}

// An archive is not a workflow file: named, it is an error, never parsed as
// text; met in a walk, it is passed over like any other file.
func TestFmtRefusesAnArchive(t *testing.T) {
	inTempWorkspace(t)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("main.bot")
	_, _ = w.Write([]byte(looseBot))
	_ = zw.Close()
	if err := os.WriteFile("x.botz", buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RunFmt(FmtOptions{Paths: []string{"x.botz"}}); err == nil || !strings.Contains(err.Error(), "not a workflow file") {
		t.Fatalf("naming an archive: %v", err)
	}
	writeBot(t, "w/one.bot", looseBot)
	if err := os.WriteFile("w/pkg.botz", buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := RunFmt(FmtOptions{Paths: []string{"w"}})
	if err != nil || len(res.Files) != 1 || res.Files[0].Path != "w/one.bot" {
		t.Fatalf("a walk met an archive: %v %+v", err, res)
	}
}
