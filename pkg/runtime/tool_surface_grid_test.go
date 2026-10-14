package runtime

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The cell space both readings of a node's tool surface are measured over, and
// the format of the frozen table under testdata/.
//
// It reads the two entry points ONLY — ToolSurfaceCanWrite and
// isMutatingNodeCtx — so the same enumeration answers for any bytes of this
// package that still export them, which is how the frozen table is produced
// from another revision's: check those bytes out, point
// ITERION_TOOL_SURFACE_GRID_OUT at the table and run this file's test.
//
// A grid certifies only the cells it enumerates, and the vocabulary matters as
// much as the count: a reading that depends on the NAMES in a `tools:` list is
// blind-tested by a grid carrying two representatives of a list, so the shapes
// below carry the advisory names (`git_diff`, `tree`, …) the engine reads as
// read-only and codex does not.

// toolSurfaceGridCell is one authored shape: what the node names as its
// `backend:`, the workflow's `default_backend:`, one `fallbacks:` route and the
// `tools:` list.
type toolSurfaceGridCell struct {
	primary  string
	dflt     string
	routeSet bool
	route    string
	shape    string
	tools    []string
}

// toolSurfaceGridEnvNames are the environment references the grid spells.
//
// Pinned empty by every reader: shared-worktree admission is asked with no
// lookup, which reads the PROCESS environment (ir.ExpandWithDefault falls back
// to it), so a host that exported one of these names would answer a different
// grid than the frozen table. Their values are never printed — the grid prints
// the spelling the author wrote.
var toolSurfaceGridEnvNames = []string{
	"ITERION_GRID_UNSET_BACKEND",
	"ITERION_GRID_UNSET_DEFAULT",
	"ITERION_GRID_UNSET_ROUTE",
}

var (
	// The 17 spellings a node's own `backend:` may take: the named backends,
	// the two that leave the route to dispatch, a template nothing resolves
	// before the run, the four reference shapes (braced and bare, with and
	// without an authored default), whitespace, and a name no registry has.
	toolSurfaceGridPrimaries = []string{
		"", "auto", "claw", "claude_code", "codex", "pi", "kimi", "grok", "opencode",
		"{{vars.b}}", "${ITERION_GRID_UNSET_BACKEND}", "$ITERION_GRID_UNSET_BACKEND",
		"${ITERION_GRID_UNSET_BACKEND:-claw}", "${ITERION_GRID_UNSET_BACKEND:-claude_code}",
		"${ITERION_GRID_UNSET_BACKEND:-}", "  ", "unknown_backend",
	}
	// The workflow default, which answers for a node that names no route.
	toolSurfaceGridDefaults = []string{
		"", "claw", "claude_code", "codex", "pi", "auto", "{{vars.d}}", "${ITERION_GRID_UNSET_DEFAULT}",
	}
	// The `fallbacks:` routes: none at all, then an entry that names no
	// backend (it inherits the node's), the registry-less `auto`, four named
	// backends and the four spellings nothing resolves here.
	toolSurfaceGridRoutes = []struct {
		set   bool
		route string
	}{
		{false, ""},
		{true, ""},
		{true, "auto"},
		{true, "claw"},
		{true, "claude_code"},
		{true, "codex"},
		{true, "pi"},
		{true, "kimi"},
		{true, "{{vars.fb}}"},
		{true, "${ITERION_GRID_UNSET_ROUTE}"},
		{true, "$ITERION_GRID_UNSET_ROUTE"},
		{true, "${ITERION_GRID_UNSET_ROUTE:-pi}"},
	}
	// The `tools:` shapes, by the VOCABULARY each carries: no list, a list
	// declared empty, names every reader treats as read-only, the advisory
	// names codex reads as workspace-write, both mixed, and a plain writer.
	toolSurfaceGridShapes = []struct {
		shape string
		tools []string
	}{
		{"none", nil},
		{"declared_empty", []string{}},
		{"readonly", []string{"read_file", "glob"}},
		{"advisory", []string{"git_diff"}},
		{"advisory_all", []string{"git_diff", "git_status", "list_files", "search_codebase", "tree", "web_fetch"}},
		{"mixed", []string{"read_file", "tree"}},
		{"writer", []string{"bash"}},
	}
)

// node builds the agent the cell describes. A judge reads the same two fields
// through the same arms (both LLM cases of isMutatingNodeIn and of
// ToolSurfaceWriteReason call one walk), so one kind carries the grid.
func (c toolSurfaceGridCell) node() *ir.AgentNode {
	n := agentNode("a", c.tools, c.primary, nil)
	if c.routeSet {
		n.Fallbacks = []ir.Fallback{{Name: "rescue", Backend: c.route}}
	}
	return n
}

// String names the cell by the authored spellings, never by what they expanded
// to: it is what a failure message hands a reader.
func (c toolSurfaceGridCell) String() string {
	route := "none"
	if c.routeSet {
		route = fmt.Sprintf("%q", c.route)
	}
	return fmt.Sprintf("%s route=%s tools=%s", toolSurfaceGridLineKey(c), route, c.shape)
}

// toolSurfaceGridLineKey is the two axes one line of the table holds constant.
func toolSurfaceGridLineKey(c toolSurfaceGridCell) string {
	return fmt.Sprintf("primary=%q default=%q", c.primary, c.dflt)
}

// toolSurfaceGridCellsPerLine is how many cells one line carries: every
// `fallbacks:` route x every `tools:` shape.
func toolSurfaceGridCellsPerLine() int {
	return len(toolSurfaceGridRoutes) * len(toolSurfaceGridShapes)
}

// toolSurfaceGridCells enumerates the cell space in a fixed order, so a table
// read back lines up with it cell by cell.
func toolSurfaceGridCells() []toolSurfaceGridCell {
	cells := make([]toolSurfaceGridCell, 0,
		len(toolSurfaceGridPrimaries)*len(toolSurfaceGridDefaults)*toolSurfaceGridCellsPerLine())
	for _, p := range toolSurfaceGridPrimaries {
		for _, d := range toolSurfaceGridDefaults {
			for _, r := range toolSurfaceGridRoutes {
				for _, s := range toolSurfaceGridShapes {
					cells = append(cells, toolSurfaceGridCell{
						primary: p, dflt: d, routeSet: r.set, route: r.route,
						shape: s.shape, tools: s.tools,
					})
				}
			}
		}
	}
	return cells
}

// toolSurfaceGridRow is one cell and the two verdicts read over it.
type toolSurfaceGridRow struct {
	cell      toolSurfaceGridCell
	key       string
	canWrite  bool
	admission bool
}

// toolSurfaceGridRows reads both questions over every cell: the boundary
// class's (ToolSurfaceCanWrite, which the catalogue guard asks) and
// shared-worktree admission's (isMutatingNodeCtx, which the fan-out guard
// asks). Each reading gets its own node, so neither can answer on a value the
// other left behind.
//
// Admission is read with NO tool-surface and NO backend resolver, which is the
// DECLARED half of it: llmToolSurfaceCanWrite returns on a nil surface seam
// before it asks what the node will HOLD. That is what makes this table
// freezable — the held reading resolves opt-ins outside the IR, and a cell
// space this wide would answer differently on two machines — and it is also
// what this table cannot certify. The held half is measured over the same
// cells, crossed with the appends, in tool_surface_arm_guarantee_test.go.
func toolSurfaceGridRows(t *testing.T) []toolSurfaceGridRow {
	t.Helper()
	for _, name := range toolSurfaceGridEnvNames {
		t.Setenv(name, "")
	}
	cells := toolSurfaceGridCells()
	rows := make([]toolSurfaceGridRow, 0, len(cells))
	for _, c := range cells {
		rows = append(rows, toolSurfaceGridRow{
			cell:      c,
			key:       c.String(),
			canWrite:  ToolSurfaceCanWrite(c.node(), c.dflt, authoredDefaults),
			admission: isMutatingNodeCtx(c.node(), c.dflt, nil, nil, false),
		})
	}
	return rows
}

// toolSurfaceGridVerdicts is one cell of the table: the two verdicts, under the
// key of the LINE that carried them.
type toolSurfaceGridVerdicts struct {
	lineKey   string
	canWrite  bool
	admission bool
}

// toolSurfaceGridHeader says what the table is and where its axes live. The
// axis values themselves are not restated here: one copy, in this file.
var toolSurfaceGridHeader = []string{
	"# The tool-surface grid, frozen: the two verdicts one revision reads over the",
	"# whole cell space. Generated, never hand-edited — the generator is in",
	"# pkg/runtime/tool_surface_grid_test.go (ITERION_TOOL_SURFACE_GRID_OUT).",
	"#",
	"# One line per `backend:` x `default_backend:` pair, carrying every",
	"# `fallbacks:` route x every `tools:` shape in the order the generator",
	"# enumerates them (toolSurfaceGridRoutes x toolSurfaceGridShapes). A cell",
	"# reads `c` where ToolSurfaceCanWrite is true and `m` where isMutatingNodeCtx",
	"# is, `-` for either where it is false.",
}

// toolSurfaceGridCode writes one cell.
func toolSurfaceGridCode(canWrite, admission bool) string {
	code := []byte("--")
	if canWrite {
		code[0] = 'c'
	}
	if admission {
		code[1] = 'm'
	}
	return string(code)
}

// parseToolSurfaceGridCode reads one cell. An unknown character is an error:
// a table this test cannot read is not a table it may pass over.
func parseToolSurfaceGridCode(code string) (canWrite, admission bool, err error) {
	if len(code) != 2 {
		return false, false, fmt.Errorf("cell %q is not two characters", code)
	}
	switch code[0] {
	case 'c':
		canWrite = true
	case '-':
	default:
		return false, false, fmt.Errorf("cell %q: %q is neither 'c' nor '-'", code, code[0])
	}
	switch code[1] {
	case 'm':
		admission = true
	case '-':
	default:
		return false, false, fmt.Errorf("cell %q: %q is neither 'm' nor '-'", code, code[1])
	}
	return canWrite, admission, nil
}

// writeToolSurfaceGrid writes the table, one line per backend/default pair.
func writeToolSurfaceGrid(w io.Writer, rows []toolSurfaceGridRow) error {
	per := toolSurfaceGridCellsPerLine()
	if len(rows) == 0 || len(rows)%per != 0 {
		return fmt.Errorf("%d cells do not fill lines of %d", len(rows), per)
	}
	var b strings.Builder
	for _, line := range toolSurfaceGridHeader {
		b.WriteString(line + "\n")
	}
	for start := 0; start < len(rows); start += per {
		line := rows[start : start+per]
		key := toolSurfaceGridLineKey(line[0].cell)
		b.WriteString(key + " |")
		for _, r := range line {
			if got := toolSurfaceGridLineKey(r.cell); got != key {
				return fmt.Errorf("cell %s sits on the line of %s: one line holds one backend/default pair, so the enumeration must vary the route and the list inside it", got, key)
			}
			b.WriteString(" " + toolSurfaceGridCode(r.canWrite, r.admission))
		}
		b.WriteString("\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// parseToolSurfaceGrid reads a table back, cell by cell. Every malformed line
// is an error naming it.
func parseToolSurfaceGrid(data []byte) ([]toolSurfaceGridVerdicts, error) {
	per := toolSurfaceGridCellsPerLine()
	var out []toolSurfaceGridVerdicts
	for n, text := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(text) == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, codes, ok := strings.Cut(text, " |")
		if !ok {
			return nil, fmt.Errorf("line %d: no %q separator in %q", n+1, " |", text)
		}
		fields := strings.Fields(codes)
		if len(fields) != per {
			return nil, fmt.Errorf("line %d (%s) holds %d cells, want %d", n+1, key, len(fields), per)
		}
		for i, code := range fields {
			canWrite, admission, err := parseToolSurfaceGridCode(code)
			if err != nil {
				return nil, fmt.Errorf("line %d (%s) cell %d: %w", n+1, key, i, err)
			}
			out = append(out, toolSurfaceGridVerdicts{lineKey: key, canWrite: canWrite, admission: admission})
		}
	}
	return out, nil
}

// toolSurfaceGridOutEnv names the file the generator writes, and is the ONE way
// the frozen table is produced: pointed at testdata/tool_surface_grid_base.txt
// with the revision's bytes checked out. Run on the current tree it would
// freeze the current reading, which is the one the guarantee test reads the
// table to judge.
const toolSurfaceGridOutEnv = "ITERION_TOOL_SURFACE_GRID_OUT"

// TestTheToolSurfaceGridRoundTripsItsOwnFormat exercises the pair the frozen
// table is written and read with, over the real cell space: a writer and a
// parser that disagree would make a table say something neither reading ever
// answered.
//
// It is also the generator. Where toolSurfaceGridOutEnv names a path, the table
// of the bytes under test is written there.
func TestTheToolSurfaceGridRoundTripsItsOwnFormat(t *testing.T) {
	rows := toolSurfaceGridRows(t)
	if len(rows) != len(toolSurfaceGridPrimaries)*len(toolSurfaceGridDefaults)*toolSurfaceGridCellsPerLine() {
		t.Fatalf("the grid read %d cells, which is not the product of its four axes", len(rows))
	}
	var buf strings.Builder
	if err := writeToolSurfaceGrid(&buf, rows); err != nil {
		t.Fatalf("write the grid: %v", err)
	}
	back, err := parseToolSurfaceGrid([]byte(buf.String()))
	if err != nil {
		t.Fatalf("read back the grid this test just wrote: %v", err)
	}
	if len(back) != len(rows) {
		t.Fatalf("wrote %d cells and read back %d", len(rows), len(back))
	}
	for i, r := range rows {
		got := back[i]
		if got.lineKey != toolSurfaceGridLineKey(r.cell) || got.canWrite != r.canWrite || got.admission != r.admission {
			t.Fatalf("cell %d (%s) round-tripped as line %s %s, want line %s %s",
				i, r.key, got.lineKey, toolSurfaceGridCode(got.canWrite, got.admission),
				toolSurfaceGridLineKey(r.cell), toolSurfaceGridCode(r.canWrite, r.admission))
		}
	}
	if out := os.Getenv(toolSurfaceGridOutEnv); out != "" {
		if err := os.WriteFile(out, []byte(buf.String()), 0o644); err != nil {
			t.Fatalf("write the grid to %s=%s: %v", toolSurfaceGridOutEnv, out, err)
		}
		t.Logf("wrote %d cells to %s", len(rows), out)
	}
}
