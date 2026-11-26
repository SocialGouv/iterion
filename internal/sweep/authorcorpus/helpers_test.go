package authorcorpus

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// The helpers below are copies of pkg/cli test helpers, which the CLI's own
// tests keep using: a test package exports nothing to another.

// newTestPrinter creates a Printer with a buffer for capturing output.
func newTestPrinter(format cli.OutputFormat) (*cli.Printer, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return &cli.Printer{W: buf, Format: format}, buf
}

// runFmtTwin runs one conversion as the operator runs it and returns its
// error and the report it printed.
func runFmtTwin(t *testing.T, opts cli.FmtOptions) (string, error) {
	t.Helper()
	p, out := newTestPrinter(cli.OutputJSON)
	opts.Printer = p
	_, err := cli.RunFmt(t.Context(), opts)
	return out.String(), err
}

// mustReadFile is path's bytes, or a failure.
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// validateDocumentJSON runs validate in JSON mode on the document at path
// and decodes its result.
func validateDocumentJSON(t *testing.T, path string, opts cli.ValidateOptions) (cli.ValidateResult, string, error) {
	t.Helper()
	p, buf := newTestPrinter(cli.OutputJSON)
	err := cli.RunValidateWith(path, p, opts)
	var res cli.ValidateResult
	if jerr := json.Unmarshal(buf.Bytes(), &res); jerr != nil {
		t.Fatalf("validate printed no result JSON (%v): err=%v\n%s", jerr, err, buf.String())
	}
	return res, buf.String(), err
}

// hasDiagCode reports whether the result carries a diagnostic of code.
func hasDiagCode(res cli.ValidateResult, code string) bool {
	for _, d := range res.Diagnostics {
		if d.Code == code {
			return true
		}
	}
	return false
}

// writeBot writes body at rel, creating its directory.
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

// helloDocWith is a document of one agent whose prompt is declared, with
// extra lines under its workflow.
func helloDocWith(workflowLines string) string {
	return "dsl: 2\nprompts:\n  ask: Say hello.\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n" + workflowLines + "  edges:\n    - hello -> done\n"
}
