package bots

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The e2e-coverage bot's verify_run gate owns the matrix contract (marker,
// exactly one contract table, parseable rows, allowed statuses, a written WHY
// on every terminal status, every citation resolving to a real test file).
// TestE2ECoverageMatrixGate drives that gate over FIXTURE workspaces, so the
// contract was pinned on matrices the tests write — never on
// docs/e2e-coverage-matrix.md, the inventory every epic, ticket and the
// stabilisation plan cite. The tests below run the gate's OWN contract code,
// sliced out of the compiled tool command rather than re-implemented, against
// the repository's committed matrix and against the plan derived from it.
//
// What the first run of this guard found: two rows carried an unescaped `|`
// inside their Feature cell, which shifts every cell after it (the contract
// parser splits on every pipe, as GitHub's renderer does for an unescaped
// one). The gate therefore read `list` and `cloud` as the Status of
// cli.remote-assistant-mission and cloud.adaptive-routing-policy — two
// invalid statuses that would have made matrix_ok=false on the real file, and
// two rows no reader could see the Status of. #2800 asks the contract parser
// to honour the GFM `\|` escape and to name an over-wide row for what it is.

const (
	committedMatrixRel = "docs/e2e-coverage-matrix.md"
	stabilisationPlan  = "docs/e2e-stabilisation-plan.md"

	// The committed matrix has 401 rows; a parse that reads a handful has
	// stopped early (a stray prose line, a fence, a renamed header) and must
	// not certify the file by reading almost none of it.
	committedMatrixRowFloor = 350
)

// matrixVerdict is what the contract slice answers about one matrix file.
type matrixVerdict struct {
	MatrixOK      bool     `json:"matrix_ok"`
	MatrixRows    int      `json:"matrix_rows"`
	UncoveredRows int      `json:"uncovered_rows"`
	Problems      []string `json:"problems"`
}

// matrixContractProgram slices the matrix-contract half out of the verify_run
// tool command and makes it runnable on its own: the git helper it calls, the
// citation regexps, and the contract block itself — the gate's code verbatim,
// so there is one implementation of the contract and this guard cannot drift
// into a friendlier copy of it. Every cut is anchored on a marker that must
// appear exactly once; a refactor that moves one fails here, loudly, instead
// of silently shrinking what is enforced.
func matrixContractProgram(t *testing.T) string {
	t.Helper()
	command := toolCommand(t, "e2e-coverage/main.bot", "verify_run")

	cut := func(from, to string) string {
		if n := strings.Count(command, from); n != 1 {
			t.Fatalf("marker %q appears %d times in the verify_run command, want exactly 1 — the contract slice is anchored on it", from, n)
		}
		if n := strings.Count(command, to); n != 1 {
			t.Fatalf("marker %q appears %d times in the verify_run command, want exactly 1 — the contract slice is anchored on it", to, n)
		}
		start := strings.Index(command, from)
		end := strings.Index(command, to)
		if end <= start {
			t.Fatalf("marker %q precedes %q in the verify_run command; the contract slice would be empty", to, from)
		}
		return command[start:end]
	}

	// The gate reads the workspace through this helper only (read-only git
	// subcommands); it is sliced in rather than re-written so a change to its
	// timeout, its -C or its error handling travels here too.
	gitHelper := cut("def git(args):", "\ntry:\n    os.makedirs(scratch, exist_ok=True)")
	// The contract block proper: the citation regexps through to the matrix
	// problem report, stopping before the new-test-code observability half
	// (which measures the bot's own pass, not the matrix).
	contract := cut("rx = re.compile('(_test", "\n# 3) new_test_code")

	// `skipped` is True on purpose: the UNEXECUTED CLAIM check reads the
	// suite's own output as evidence against a citation, and no suite ran
	// here. Resolution (the file exists and names the test) is this guard's
	// half; execution is the bot pass's half.
	preamble := strings.Join([]string{
		"import os, re, json, subprocess",
		"ws = os.environ['WS']",
		"matrix_rel = os.environ['MATRIX']",
		"out = ''",
		"skipped = True",
		"",
	}, "\n")
	epilogue := "\nprint(json.dumps({'matrix_ok': matrix_ok, 'matrix_rows': matrix_rows, 'uncovered_rows': uncovered, 'problems': problems}))\n"

	return preamble + gitHelper + "\n" + contract + epilogue
}

// runMatrixContract runs the contract slice over one workspace.
func runMatrixContract(t *testing.T, program, ws, matrixRel string) matrixVerdict {
	t.Helper()
	prog := filepath.Join(t.TempDir(), "matrix_contract.py")
	if err := os.WriteFile(prog, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-I", prog)
	cmd.Env = append(os.Environ(), "WS="+ws, "MATRIX="+matrixRel)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the matrix contract slice failed to run: %v (out %q)", err, out)
	}
	var v matrixVerdict
	if uerr := json.Unmarshal(out, &v); uerr != nil {
		t.Fatalf("the matrix contract slice did not answer JSON: %v (out %q)", uerr, out)
	}
	return v
}

// TestTheMatrixContractSliceStillRefusesWhatTheGateRefuses is the floor under
// the guard below: a slice that lost the checks would certify anything. It
// drives the extracted program over fixtures whose verdict is known, so a
// refactor that cuts a check out of the block turns the committed-matrix guard
// green for the wrong reason HERE instead of there.
func TestTheMatrixContractSliceStillRefusesWhatTheGateRefuses(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	program := matrixContractProgram(t)

	const header = "<!-- e2e-coverage-matrix: v1 -->\n\n# matrix\n\n| ID | Feature | Family | Status | Tests | Notes |\n|---|---|---|---|---|---|\n"
	fixture := func(t *testing.T, rows string) string {
		t.Helper()
		ws := t.TempDir()
		dir := filepath.Join(ws, "docs")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, "e2e_test.go"), []byte("package e2e\n\nfunc TestRealThing(t *testing.T) {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "e2e-coverage-matrix.md"), []byte(header+rows), 0o644); err != nil {
			t.Fatal(err)
		}
		return ws
	}

	const goodRow = "| a.thing | a promise | a | covered-deterministic | TestRealThing (e2e_test.go) | |\n"

	t.Run("a_contract_abiding_matrix_passes", func(t *testing.T) {
		v := runMatrixContract(t, program, fixture(t, goodRow), committedMatrixRel)
		if !v.MatrixOK || v.MatrixRows != 1 {
			t.Fatalf("the slice must accept a contract-abiding matrix: %+v", v)
		}
	})

	for _, tc := range []struct {
		name string
		rows string
		says string
	}{
		{
			name: "a_citation_that_does_not_exist",
			rows: "| a.thing | a promise | a | covered-deterministic | TestGhost (e2e/ghost_test.go) | |\n",
			says: "ORPHAN CLAIM",
		},
		{
			name: "a_terminal_status_without_a_written_why",
			rows: "| a.thing | a promise | a | unit-only | | |\n",
			says: "requires a justification",
		},
		{
			name: "a_status_outside_the_vocabulary",
			rows: "| a.thing | a promise | a | covered-ish | TestRealThing (e2e_test.go) | |\n",
			says: "invalid status",
		},
		{
			name: "an_unescaped_pipe_shifts_the_cells",
			rows: "| a.thing | a promise `start|stop` | a | covered-deterministic | TestRealThing (e2e_test.go) | |\n",
			says: "invalid status",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := runMatrixContract(t, program, fixture(t, tc.rows), committedMatrixRel)
			if v.MatrixOK {
				t.Fatalf("the slice must refuse %s: %+v", tc.name, v)
			}
			if !strings.Contains(strings.Join(v.Problems, "\n"), tc.says) {
				t.Fatalf("the refusal must say %q, got %v", tc.says, v.Problems)
			}
		})
	}
}

// TestTheCommittedCoverageMatrixSatisfiesItsContract runs the gate's contract
// over the repository's own matrix. Nothing did before: the inventory of
// record was trusted because the gate exists, not because it had ever read it.
func TestTheCommittedCoverageMatrixSatisfiesItsContract(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := repoRootForMatrix(t)
	v := runMatrixContract(t, matrixContractProgram(t), root, committedMatrixRel)
	if !v.MatrixOK {
		t.Fatalf("%s does not satisfy the contract its own gate enforces:\n  - %s", committedMatrixRel, strings.Join(v.Problems, "\n  - "))
	}
	if v.MatrixRows < committedMatrixRowFloor {
		t.Fatalf("the contract read only %d rows of %s (floor %d): a parse that stops early certifies a file it did not read", v.MatrixRows, committedMatrixRel, committedMatrixRowFloor)
	}
}

// TestTheStabilisationPlanTableIsDerivedFromTheCommittedMatrix pins the
// numbers of docs/e2e-stabilisation-plan.md to the matrix they claim to
// measure. The plan's first table was typed from a 2026-09-19 reading and was
// still quoted as current five weeks and seventy rows later; a surface added
// to the matrix after that reading escaped the plan entirely. The guard reads
// the matrix with the contract parser's own rules and refuses any disagreement
// in either direction, printing the table the plan should carry.
func TestTheStabilisationPlanTableIsDerivedFromTheCommittedMatrix(t *testing.T) {
	root := repoRootForMatrix(t)
	measured := measureCommittedMatrix(t, root)
	declared := declaredPlanTable(t, root)

	if len(measured.bySurface) == 0 {
		t.Fatalf("no surface measured from %s: the matrix parse read nothing and the comparison below would pass vacuously", committedMatrixRel)
	}

	// Cross-parser agreement: this test counts rows per surface in Go, the
	// gate's contract counts them in python. A guard whose parser disagrees
	// with the contract's is measuring a file nobody else sees.
	if _, err := exec.LookPath("python3"); err == nil {
		v := runMatrixContract(t, matrixContractProgram(t), root, committedMatrixRel)
		if v.MatrixRows != measured.total.rows {
			t.Fatalf("row count disagreement: this test parses %d rows, the gate's contract parses %d — the two parsers must read the same matrix", measured.total.rows, v.MatrixRows)
		}
		if v.UncoveredRows != measured.total.counts["uncovered"] {
			t.Fatalf("uncovered count disagreement: this test parses %d, the gate's contract parses %d", measured.total.counts["uncovered"], v.UncoveredRows)
		}
	}

	if diff := comparePlanTable(measured, declared); diff != "" {
		t.Fatalf("%s no longer matches %s:\n%s\nthe table it should carry:\n\n%s",
			stabilisationPlan, committedMatrixRel, diff, renderPlanTable(measured))
	}
}

// --- the matrix, read the way the contract reads it -------------------------

var planStatuses = []string{"covered-deterministic", "covered-live", "uncovered", "unit-only", "excluded"}

type surfaceCount struct {
	rows   int
	counts map[string]int
}

type matrixMeasurement struct {
	bySurface map[string]*surfaceCount
	total     *surfaceCount
}

func newSurfaceCount() *surfaceCount {
	return &surfaceCount{counts: map[string]int{}}
}

func (s *surfaceCount) add(status string) {
	s.rows++
	s.counts[status]++
}

func repoRootForMatrix(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, committedMatrixRel)); err != nil {
		t.Fatalf("no %s under %s: this test must run from the bots package of the repository", committedMatrixRel, root)
	}
	return root
}

// contractTableRows returns the data rows of the single contract table of a
// markdown file, applying the gate's own reading rules: fenced and indented
// blocks are blanked (an example table in a fence is not the matrix), the
// header must be the exact contract columns, a blank line does not end the
// table, and the first non-row line does.
func contractTableRows(t *testing.T, path string, wantHeader []string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	backOpen, tildeOpen := false, false
	for _, l := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(trimmed, "```") && !tildeOpen:
			backOpen = !backOpen
			lines = append(lines, "")
			continue
		case strings.HasPrefix(trimmed, "~~~") && !backOpen:
			tildeOpen = !tildeOpen
			lines = append(lines, "")
			continue
		}
		if backOpen || tildeOpen || strings.HasPrefix(l, "    ") || strings.HasPrefix(l, "\t") {
			lines = append(lines, "")
			continue
		}
		lines = append(lines, l)
	}

	cells := func(line string) []string {
		out := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		for i := range out {
			out[i] = strings.TrimSpace(out[i])
		}
		return out
	}
	headerAt, headers := -1, 0
	for i, l := range lines {
		if !strings.HasPrefix(strings.TrimSpace(l), "|") {
			continue
		}
		got := cells(l)
		if len(got) != len(wantHeader) {
			continue
		}
		same := true
		for j := range got {
			if !strings.EqualFold(got[j], wantHeader[j]) {
				same = false
				break
			}
		}
		if same {
			headers++
			if headerAt < 0 {
				headerAt = i
			}
		}
	}
	if headerAt < 0 {
		t.Fatalf("%s carries no table whose header is exactly | %s |", path, strings.Join(wantHeader, " | "))
	}
	if headers > 1 {
		t.Fatalf("%s carries %d tables with the header | %s |; the comparison needs exactly one", path, headers, strings.Join(wantHeader, " | "))
	}

	var rows [][]string
	for i := headerAt + 1; i < len(lines); i++ {
		s := strings.TrimSpace(lines[i])
		if s == "" {
			continue
		}
		if !strings.HasPrefix(s, "|") {
			break
		}
		if strings.Trim(s, "|-: ") == "" {
			continue
		}
		rows = append(rows, cells(s))
	}
	return rows
}

func measureCommittedMatrix(t *testing.T, root string) matrixMeasurement {
	t.Helper()
	m := matrixMeasurement{bySurface: map[string]*surfaceCount{}, total: newSurfaceCount()}
	allowed := map[string]bool{}
	for _, s := range planStatuses {
		allowed[s] = true
	}
	rows := contractTableRows(t, filepath.Join(root, committedMatrixRel), []string{"ID", "Feature", "Family", "Status", "Tests", "Notes"})
	for _, cells := range rows {
		if len(cells) < 6 {
			t.Fatalf("malformed matrix row (%d cells, contract needs 6): %q", len(cells), strings.Join(cells, " | "))
		}
		surface := cells[2]
		status := strings.ToLower(strings.Trim(cells[3], "`"))
		if !allowed[status] {
			t.Fatalf("row %q carries status %q, outside the contract vocabulary — the surface counts below would be meaningless", cells[0], status)
		}
		if _, ok := m.bySurface[surface]; !ok {
			m.bySurface[surface] = newSurfaceCount()
		}
		m.bySurface[surface].add(status)
		m.total.add(status)
	}
	return m
}

// --- the plan's table -------------------------------------------------------

var planTableHeader = []string{"Surface", "rows", "det", "live", "uncov", "unit-only", "excluded"}

func declaredPlanTable(t *testing.T, root string) map[string]*surfaceCount {
	t.Helper()
	declared := map[string]*surfaceCount{}
	for _, cells := range contractTableRows(t, filepath.Join(root, stabilisationPlan), planTableHeader) {
		if len(cells) < len(planTableHeader) {
			t.Fatalf("malformed plan row (%d cells, want %d): %q", len(cells), len(planTableHeader), strings.Join(cells, " | "))
		}
		surface := strings.Trim(cells[0], "*` ")
		sc := newSurfaceCount()
		var err error
		if sc.rows, err = planNumber(cells[1]); err != nil {
			t.Fatalf("plan row %q: unreadable rows cell %q", surface, cells[1])
		}
		for i, status := range planStatuses {
			n, nerr := planNumber(cells[2+i])
			if nerr != nil {
				t.Fatalf("plan row %q: unreadable %s cell %q", surface, status, cells[2+i])
			}
			sc.counts[status] = n
		}
		if _, dup := declared[surface]; dup {
			t.Fatalf("plan table lists surface %q twice", surface)
		}
		declared[surface] = sc
	}
	return declared
}

func planNumber(cell string) (int, error) {
	var n int
	_, err := fmt.Sscanf(strings.TrimSpace(strings.Trim(cell, "*` ")), "%d", &n)
	return n, err
}

// comparePlanTable answers what differs between the measured matrix and the
// plan's table, in both directions: a surface the plan forgot, a surface the
// plan invented, a count that drifted, and the total row that must be the
// column sums.
func comparePlanTable(measured matrixMeasurement, declared map[string]*surfaceCount) string {
	var problems []string
	const totalKey = "Total"

	for _, surface := range sortedSurfaces(measured.bySurface) {
		got, ok := declared[surface]
		if !ok {
			problems = append(problems, fmt.Sprintf("  - surface %q is in the matrix (%d rows) and missing from the plan: a surface the plan does not own is a surface nobody plans to stabilise", surface, measured.bySurface[surface].rows))
			continue
		}
		problems = append(problems, countDiff(surface, measured.bySurface[surface], got)...)
	}
	for _, surface := range sortedSurfaces(declared) {
		if surface == totalKey {
			continue
		}
		if _, ok := measured.bySurface[surface]; !ok {
			problems = append(problems, fmt.Sprintf("  - surface %q is in the plan and not in the matrix (renamed or removed): the plan would describe work on rows that do not exist", surface))
		}
	}
	total, ok := declared[totalKey]
	if !ok {
		problems = append(problems, fmt.Sprintf("  - the plan table has no %q row: the totals are what a reader quotes, so they are measured too", totalKey))
	} else {
		problems = append(problems, countDiff(totalKey, measured.total, total)...)
	}
	return strings.Join(problems, "\n")
}

func countDiff(surface string, want, got *surfaceCount) []string {
	var problems []string
	if want.rows != got.rows {
		problems = append(problems, fmt.Sprintf("  - %s: the matrix has %d rows, the plan says %d", surface, want.rows, got.rows))
	}
	for _, status := range planStatuses {
		if want.counts[status] != got.counts[status] {
			problems = append(problems, fmt.Sprintf("  - %s: the matrix has %d %s row(s), the plan says %d", surface, want.counts[status], status, got.counts[status]))
		}
	}
	return problems
}

func renderPlanTable(m matrixMeasurement) string {
	var b strings.Builder
	b.WriteString("| " + strings.Join(planTableHeader, " | ") + " |\n|---|--:|--:|--:|--:|--:|--:|\n")
	row := func(name string, c *surfaceCount) {
		fmt.Fprintf(&b, "| %s | %d", name, c.rows)
		for _, status := range planStatuses {
			fmt.Fprintf(&b, " | %d", c.counts[status])
		}
		b.WriteString(" |\n")
	}
	surfaces := sortedSurfaces(m.bySurface)
	sort.SliceStable(surfaces, func(i, j int) bool {
		return m.bySurface[surfaces[i]].rows > m.bySurface[surfaces[j]].rows
	})
	for _, s := range surfaces {
		row(s, m.bySurface[s])
	}
	row("Total", m.total)
	return b.String()
}

func sortedSurfaces(m map[string]*surfaceCount) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
