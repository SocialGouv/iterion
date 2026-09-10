package ir

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// irDiagConstRe matches a diagnostic-code const declared in THIS package.
var irDiagConstRe = regexp.MustCompile(`(Diag[A-Za-z0-9_]+)\s+DiagCode\s*=\s*"(C[0-9]{3})"`)

// TestDiagCatalogCoversEveryCode refuses a diagnostic code the compiler can
// emit that has no catalogue entry: every diagnostic must arrive with a fix
// line, in `iterion validate`, in the studio badge and in the MCP result
// alike. It scans source rather than reflecting, like diag_codes_test.go,
// because Go consts are not enumerable at runtime.
func TestDiagCatalogCoversEveryCode(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	byCode := map[DiagCode]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range irDiagConstRe.FindAllStringSubmatch(string(data), -1) {
			byCode[DiagCode(m[2])] = m[1]
		}
	}
	if len(byCode) == 0 {
		t.Fatal("scanned no diagnostic-code consts — the scanner regex is stale")
	}
	var missing []string
	for code, name := range byCode {
		info, ok := Catalog[code]
		if !ok || strings.TrimSpace(info.Fix) == "" || strings.TrimSpace(info.Title) == "" {
			missing = append(missing, string(code)+" ("+name+")")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d diagnostic code(s) have no catalogue entry (title + fix line) in diag_catalog.go: %s",
			len(missing), strings.Join(missing, ", "))
	}
}

// TestDiagnosticsCarryHintAndPosition compiles a source with one node-level
// and one edge-level defect and checks that each diagnostic reaches the
// caller with the catalogue hint and the position of the declaration it
// names — the two fields a validate loop acts on.
func TestDiagnosticsCarryHintAndPosition(t *testing.T) {
	src := `schema out:
  ok: bool

agent a:
  model: "m"
  output: out

agent b:
  model: "m"
  output: out
  session: inherit

workflow w:
  entry: a
  a -> b when ok
  b -> a
`
	pr := parser.Parse("hint.bot", src)
	for _, d := range pr.Diagnostics {
		t.Fatalf("unexpected parse diagnostic: %s", d.Error())
	}
	res := Compile(pr.File)
	var seen []string
	for _, d := range res.Diagnostics {
		if d.Severity != SeverityError {
			continue
		}
		seen = append(seen, string(d.Code))
		if d.Hint == "" {
			t.Errorf("%s: no hint", d.Code)
		}
		if d.Hint != HintFor(d.Code) {
			t.Errorf("%s: hint %q is not the catalogue's %q", d.Code, d.Hint, HintFor(d.Code))
		}
		if d.NodeID == "" && d.EdgeID == "" {
			continue
		}
		if d.File != "hint.bot" || d.Line == 0 {
			t.Errorf("%s (node %q, edge %q): no source position, got %s:%d:%d", d.Code, d.NodeID, d.EdgeID, d.File, d.Line, d.Column)
		}
	}
	joined := strings.Join(seen, ",")
	if !strings.Contains(joined, string(DiagMissingFallback)) {
		t.Errorf("expected C012 (missing fallback) among %v", seen)
	}
	if !strings.Contains(joined, string(DiagUndeclaredCycle)) {
		t.Errorf("expected C019 (undeclared cycle) among %v", seen)
	}
}

// TestParseDiagnosticsCarryHint checks the parser side of the same contract.
func TestParseDiagnosticsCarryHint(t *testing.T) {
	pr := parser.Parse("hint.bot", "agent a:\n  temperature: 0.2\n")
	if len(pr.Diagnostics) == 0 {
		t.Fatal("expected an unknown-property diagnostic")
	}
	d := pr.Diagnostics[0]
	if d.Code != parser.DiagUnknownProperty {
		t.Fatalf("code = %s, want %s", d.Code, parser.DiagUnknownProperty)
	}
	if d.Hint == "" || d.Hint != parser.HintFor(d.Code) {
		t.Errorf("hint = %q, want the catalogued one", d.Hint)
	}
}
