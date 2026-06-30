package ir

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Diagnostic codes (Cnnn) are scattered string-literal consts across the ir
// package and pkg/bundlelint, with no central registry. That looseness is what
// let DiagInvalidPolicy and DiagEnumLiteralMismatch BOTH ship as "C103" (a code
// in compiler output that mapped to two unrelated checks) and let
// docs/references/diagnostics.md drift 25 codes behind the implementation.
//
// These two tests are the durable guards for both failure classes:
//   - TestDiagCodesAreUnique — no code string is bound to two different consts.
//   - TestDiagCodesAreDocumented — every defined code appears in the reference.
//
// They scan source rather than reflect because Go consts are not enumerable at
// runtime. The scan deliberately reads the package's own *.go files (relative
// to the test's working dir, which `go test` sets to the package source dir),
// mirroring the file-walking idiom in bots/catalog_universality_test.go.

// diagConstRe matches a diagnostic-code const declaration in either the ir
// package (`DiagFoo DiagCode = "C001"`) or pkg/bundlelint (`DiagFoo Code =
// "C200"`). Group 1 is the const name, group 2 the code string.
var diagConstRe = regexp.MustCompile(`(Diag[A-Za-z0-9_]+)\s+(?:DiagCode|Code)\s*=\s*"(C[0-9]{3})"`)

// diagCodeSourceFiles returns every non-test Go file that may declare a
// diagnostic-code const: the ir package itself plus pkg/bundlelint.
func diagCodeSourceFiles(t *testing.T) []string {
	t.Helper()
	irFiles, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob ir sources: %v", err)
	}
	bundleFiles, err := filepath.Glob("../../bundlelint/*.go")
	if err != nil {
		t.Fatalf("glob bundlelint sources: %v", err)
	}
	var out []string
	for _, f := range append(irFiles, bundleFiles...) {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		t.Fatal("no source files found to scan for diagnostic codes")
	}
	return out
}

// scanDiagCodes returns code -> sorted list of const names declaring it.
func scanDiagCodes(t *testing.T) map[string][]string {
	t.Helper()
	codes := make(map[string][]string)
	for _, f := range diagCodeSourceFiles(t) {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range diagConstRe.FindAllStringSubmatch(string(data), -1) {
			name, code := m[1], m[2]
			codes[code] = append(codes[code], name)
		}
	}
	if len(codes) == 0 {
		t.Fatal("scanned sources but found no diagnostic-code consts — scanner regex is stale")
	}
	return codes
}

// TestDiagCodesAreUnique fails if any Cnnn code is bound to more than one const
// name. This is the regression guard for the C103 collision class: a code in
// compiler output must map to exactly one diagnostic.
func TestDiagCodesAreUnique(t *testing.T) {
	for code, names := range scanDiagCodes(t) {
		// Dedup names (a const referenced in the same file twice is fine).
		seen := map[string]bool{}
		var distinct []string
		for _, n := range names {
			if !seen[n] {
				seen[n] = true
				distinct = append(distinct, n)
			}
		}
		if len(distinct) > 1 {
			sort.Strings(distinct)
			t.Errorf("diagnostic code %s is bound to %d consts: %s — codes must be unique so a Cnnn in compiler output is unambiguous",
				code, len(distinct), strings.Join(distinct, ", "))
		}
	}
}

// TestDiagCodesAreDocumented fails if any defined code is absent from the
// committed reference catalog. This is the regression guard for the drift class
// (docs/references/diagnostics.md silently lagging the implementation). It is
// intentionally one-directional: extra/reserved entries in the doc are allowed,
// but every code the compiler can emit must be documented.
func TestDiagCodesAreDocumented(t *testing.T) {
	const docPath = "../../../docs/references/diagnostics.md"
	data, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	doc := string(data)

	var missing []string
	for code := range scanDiagCodes(t) {
		if !strings.Contains(doc, code) {
			missing = append(missing, code)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d diagnostic code(s) defined in source but undocumented in docs/references/diagnostics.md: %s\n"+
			"add a row for each (severity + one-line check) so the predictability surface stays accurate",
			len(missing), strings.Join(missing, ", "))
	}
}
