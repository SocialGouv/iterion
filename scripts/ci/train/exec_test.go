package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// gateRepo lays out the two files scriptGate reads — scripts/ci/gate.sh and
// .github/ci-gate-map.yaml — with a gate.sh miniature that writes the matrix
// and log lines the real one writes, and exits with GATE_EXIT.
func gateRepo(t *testing.T, matrix, logLine string) string {
	t.Helper()
	dir := t.TempDir()
	gate := `#!/usr/bin/env bash
set -euo pipefail
mkdir -p "$ITERION_CI_GATE_EVIDENCE"
` + logLine + `
printf '` + matrix + `' > "$ITERION_CI_GATE_EVIDENCE/matrix.tsv"
exit "${GATE_EXIT:-0}"
`
	writeFiles(t, dir, map[string]string{
		"scripts/ci/gate.sh":       gate,
		".github/ci-gate-map.yaml": "refusing_members: [\"ci:gofmt\", \"ci:mongo-conformance\", \"ci:vendor-check\"]\n",
	})
	return dir
}

func TestScriptGateReadsRedRefusedAndIncompleteBehindAnExit1(t *testing.T) {
	dir := gateRepo(t, `test\t1\tx\nci:mongo-conformance\t2\tx\ndocs\t0\tx\nlint\t2\tx\n`,
		`echo "ci:gate: INCOMPLETE — required/staged context(s) with no member: brand" >&2`)
	t.Setenv("GATE_EXIT", "1")
	res, err := scriptGate{Log: io.Discard}.Full(context.Background(), dir, filepath.Join(t.TempDir(), "ev"))
	if err != nil {
		t.Fatal(err)
	}
	// lint's rc 2 is red: only a DECLARED refusing member's 2 is a refusal.
	if res.Code != 1 || !slices.Equal(res.Red, []string{"test", "lint"}) || !slices.Equal(res.Refused, []string{"ci:mongo-conformance"}) || !res.Incomplete {
		t.Fatalf("res %+v, want exit 1, red [test lint], refused [ci:mongo-conformance], incomplete", res)
	}
}

func TestScriptGateCompleteGreen(t *testing.T) {
	dir := gateRepo(t, `test\t0\tx\n`, `echo "ci:gate: green" >&2`)
	t.Setenv("GATE_EXIT", "0")
	res, err := scriptGate{Log: io.Discard}.Full(context.Background(), dir, filepath.Join(t.TempDir(), "ev"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != 0 || len(res.Red)+len(res.Refused) != 0 || res.Incomplete {
		t.Fatalf("res %+v, want a plain green", res)
	}
}

// fakeTask puts a `task` on PATH that exits with the code named for each
// member in its own table.
func fakeTask(t *testing.T, codes map[string]int) {
	t.Helper()
	bin := t.TempDir()
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\nm=\"$2\"\ncase \"$m\" in\n")
	for m, c := range codes {
		b.WriteString("  '" + m + "') exit " + strconv.Itoa(c) + " ;;\n")
	}
	b.WriteString("  *) exit 0 ;;\nesac\n")
	if err := os.WriteFile(filepath.Join(bin, "task"), []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestScriptGateProbeSeparatesRedFromRefusal(t *testing.T) {
	dir := gateRepo(t, ``, ``)
	fakeTask(t, map[string]int{"test": 1, "lint": 2, "ci:mongo-conformance": 2})
	red, err := scriptGate{Log: io.Discard}.Probe(context.Background(), dir, filepath.Join(t.TempDir(), "ev"), []string{"test", "lint", "docs"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(red, []string{"test", "lint"}) {
		t.Fatalf("red %v, want [test lint]: an undeclared member's 2 is red", red)
	}
	_, err = scriptGate{Log: io.Discard}.Probe(context.Background(), dir, filepath.Join(t.TempDir(), "ev2"), []string{"ci:mongo-conformance"})
	if err == nil || !strings.Contains(err.Error(), "refused to run") {
		t.Fatalf("err %v, want a declared member's refusal as an error, not a verdict", err)
	}
}
