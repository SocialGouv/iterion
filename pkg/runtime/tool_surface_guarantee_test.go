package runtime

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
)

// toolSurfaceGridFreeze is the table of the two verdicts read over the whole
// cell space BEFORE this reading was unified — generated from those bytes by
// the generator in tool_surface_grid_test.go, and never edited by hand.
//
// It is what turns an attack on one cell into a guarantee over all of them:
// the arms below say what the reading now widens and by how many cells, and
// anything else that moves — in either direction, on either question — is a
// change nobody measured.
const toolSurfaceGridFreeze = "testdata/tool_surface_grid_base.txt"

// The extent of the move over the cell space, measured against the frozen
// table. Two numbers a supervised merge owes its operator: what joined the
// prompt-injection class, and what stopped being eligible for a shared
// worktree.
//
// Both are read over the DECLARED half of each question (see
// toolSurfaceGridRows): the boundary class has no other half, and admission's
// held half — the list the route RECEIVES — adds its own refusals on top of
// this number, counted in tool_surface_arm_guarantee_test.go. The move an
// operator is shown is the pair.
const (
	toolSurfaceWidenedCells   = 5722
	toolSurfaceAdmissionMoved = 342
)

// widenedArm is one reason a cell that could not write before can now: the
// CLAUSE the engine words it with, the reading that emitted it, and the cells
// it answers for.
//
// Keyed on the engine's own message rather than on a predicate over the cell,
// so this table re-derives nothing the code under test decides: it reads that
// message and counts. A new arm matches nothing and fails; an arm whose reach
// changed fails with its own number.
//
// The clause ALONE is not that key. The wordings are deliberately shared so
// one sentence serves the node's own route and a `fallbacks:` one, and three
// of them are emitted by BOTH readings — an arm that summed the two would hold
// its total while N cells moved from one reading to the other, which is a
// change nobody measured passing as none at all. The reading is the other half
// of the key, read from the prefix the walk itself writes.
type widenedArm struct {
	arm         string
	clause      string
	viaFallback bool
	cells       int
}

// fallbackReasonPrefix opens every reason the `fallbacks:` walk words, and is
// how an arm says WHICH reading emitted its clause.
const fallbackReasonPrefix = "a `fallbacks:` route"

// matches reports whether the engine's reason for a cell is this arm's: the
// clause, from the reading this arm claims.
func (a widenedArm) matches(reason string) bool {
	return strings.Contains(reason, a.clause) && strings.HasPrefix(reason, fallbackReasonPrefix) == a.viaFallback
}

// theArmsThatWiden are the readings that put a node in the boundary class
// where the frozen table left it out, with the cells each answers for.
var theArmsThatWiden = []widenedArm{
	{
		arm:    "the node's route never receives the list (pi, kimi, grok, opencode, an unknown name)",
		clause: "never receives the `tools:` list, so a declared bound is dropped (#1652)",
		cells:  2016,
	},
	{
		arm:         "a `fallbacks:` route that never receives the list",
		clause:      "never receives the `tools:` list, so a declared bound is dropped (#1652)",
		viaFallback: true,
		cells:       30,
	},
	{
		arm:    "the node's route is claude_code, where the list becomes --disallowedTools over a roster iterion does not own",
		clause: "takes the `tools:` list as --disallowedTools over a closed native roster iterion does not own, so a declared list bounds nothing (#1652)",
		cells:  864,
	},
	{
		arm:         "a `fallbacks:` route onto claude_code",
		clause:      "takes the `tools:` list as --disallowedTools over a closed native roster iterion does not own, so a declared list bounds nothing (#1652)",
		viaFallback: true,
		cells:       10,
	},
	{
		arm:    "codex, where the names decide the sandbox mode",
		clause: "maps the `tools:` list onto a sandbox mode, and what this node declares buys the workspace-write one, where codex's built-in shell acts on the worktree (#1727)",
		cells:  360,
	},
	{
		arm:    "a route left to dispatch (`backend:` absent, or `auto`)",
		clause: "resolves at dispatch, where the compiled-in detection order's first choice is claude_code, which does not bound what the node holds (#1652)",
		cells:  864,
	},
	{
		arm:    "the node's route is a `${VAR}` reference read at an authored default that does not exist",
		clause: "is read at its authored default, which names no route, so nothing here bounds what the node holds",
		cells:  1332,
	},
	{
		arm:         "a `fallbacks:` route spelled as a `${VAR}` reference read at an authored default that does not exist",
		clause:      "is read at its authored default, which names no route, so nothing here bounds what the node holds",
		viaFallback: true,
		cells:       236,
	},
	{
		arm:         "a `fallbacks:` route still spelled as a template",
		clause:      "is unresolved before the run, so no backend it may take bounds what the node holds",
		viaFallback: true,
		cells:       10,
	},
}

// toolSurfaceGridPair is one cell, the verdicts the frozen table holds for it
// and the verdicts the current bytes read.
type toolSurfaceGridPair struct {
	cell toolSurfaceGridCell
	key  string
	was  toolSurfaceGridVerdicts
	now  toolSurfaceGridRow
}

// toolSurfaceGridPairs lines the frozen table up with the live reading, cell by
// cell. A table that does not describe the same cell space in the same order is
// a hard failure: lining up 11 424 verdicts by position only means something
// while the keys match.
func toolSurfaceGridPairs(t *testing.T) []toolSurfaceGridPair {
	t.Helper()
	// The generator writes the very table these two gates read, and
	// `go test ./pkg/runtime/` runs it in the same binary: under that variable
	// a gate would compare the current reading to a freeze of the current
	// reading and report that nothing moved — "the reading widened 0 cells"
	// against a measured 5722, with no word about why. Refused here, so the
	// cause is the failure rather than a riddle. Regenerating the table is
	// still one `-run` on the generator alone, with the frozen revision's
	// bytes checked out.
	if out := strings.TrimSpace(os.Getenv(toolSurfaceGridOutEnv)); out != "" {
		t.Fatalf("%s=%s is set: the generator in tool_surface_grid_test.go writes the table this gate reads, in the same run, so a verdict here would judge the reading against a freeze of itself — unset it, or run only TestTheToolSurfaceGridRoundTripsItsOwnFormat",
			toolSurfaceGridOutEnv, out)
	}
	data, err := os.ReadFile(toolSurfaceGridFreeze)
	if err != nil {
		t.Fatalf("read the frozen grid: %v", err)
	}
	frozen, err := parseToolSurfaceGrid(data)
	if err != nil {
		t.Fatalf("parse %s: %v", toolSurfaceGridFreeze, err)
	}
	rows := toolSurfaceGridRows(t)
	if len(frozen) != len(rows) {
		t.Fatalf("%s holds %d cells and the enumeration reads %d: the cell space moved, so regenerate the table from the bytes it froze (%s)",
			toolSurfaceGridFreeze, len(frozen), len(rows), toolSurfaceGridOutEnv)
	}
	pairs := make([]toolSurfaceGridPair, 0, len(rows))
	for i, r := range rows {
		if want := toolSurfaceGridLineKey(r.cell); frozen[i].lineKey != want {
			t.Fatalf("cell %d of %s is on line %s and the enumeration's is on %s: the cell space moved, so regenerate the table from the bytes it froze (%s)",
				i, toolSurfaceGridFreeze, frozen[i].lineKey, want, toolSurfaceGridOutEnv)
		}
		pairs = append(pairs, toolSurfaceGridPair{cell: r.cell, key: r.key, was: frozen[i], now: r})
	}
	return pairs
}

// TestTheBoundaryClassOnlyWidens is the guarantee the catalogue's
// prompt-injection contract rests on, read over the whole cell space rather
// than on the shapes someone thought to attack.
//
// A node that could write before must still be able to: the class may only
// grow, since a cell that leaves it takes a system prompt's UNTRUSTED INPUT
// BOUNDARY paragraph with it, and nothing in the catalogue guard can report a
// prompt that quietly stopped owing one (the complement in
// bots/catalog_untrusted_input_boundary_test.go names the one node that is out
// of the class on purpose).
//
// And every cell that joins it joins through a named arm, with the engine's own
// wording and the cells that arm answers for: the blast radius of the move,
// counted rather than described.
func TestTheBoundaryClassOnlyWidens(t *testing.T) {
	pairs := toolSurfaceGridPairs(t)
	var narrowed, unexplained []string
	counted := map[string]int{}
	widened := 0
	for _, p := range pairs {
		switch {
		case p.was.canWrite && !p.now.canWrite:
			narrowed = append(narrowed, p.key)
			continue
		case p.was.canWrite || !p.now.canWrite:
			continue
		}
		widened++
		reason := ToolSurfaceWriteReason(p.cell.node(), p.cell.dflt, authoredDefaults)
		var hit []string
		for _, arm := range theArmsThatWiden {
			if arm.matches(reason) {
				hit = append(hit, arm.arm)
			}
		}
		if len(hit) != 1 {
			unexplained = append(unexplained, fmt.Sprintf("%s\n      reason %q\n      matches %d arms %v", p.key, reason, len(hit), hit))
			continue
		}
		counted[hit[0]]++
	}
	sort.Strings(narrowed)
	if len(narrowed) > 0 {
		t.Errorf("%d cell(s) left the boundary class — a prompt that stops owing an UNTRUSTED INPUT BOUNDARY paragraph must be a deliberate, named change, never a side effect:\n  %s",
			len(narrowed), strings.Join(narrowed[:min(len(narrowed), 20)], "\n  "))
	}
	if len(unexplained) > 0 {
		t.Errorf("%d widened cell(s) carry a reason no arm of this table claims — add the arm with its clause and the cells it answers for, or fix the reading:\n  %s",
			len(unexplained), strings.Join(unexplained[:min(len(unexplained), 10)], "\n  "))
	}
	if widened != toolSurfaceWidenedCells {
		t.Errorf("the reading widened %d cells, the measured move is %d: every cell that joins the class is a prompt that may owe a paragraph, so the count is reviewed, not inferred", widened, toolSurfaceWidenedCells)
	}
	for _, arm := range theArmsThatWiden {
		if counted[arm.arm] != arm.cells {
			t.Errorf("arm %q widened %d cells, want %d", arm.arm, counted[arm.arm], arm.cells)
		}
	}
}

// TestSharedWorktreeAdmissionMovesOnlyWhereCodexSandboxesTheListWritable is the
// other half of the same guarantee, on the question that REFUSES work: a cell
// that moves here stops being eligible for a parallel read-only fan-out, so a
// false positive costs an author a workflow that used to run.
//
// The move is one class and the test says which: a cell that names codex, with
// a declared list codex's own classifier does not sandbox read-only
// (delegate.CodexDeclaredListIsReadOnly — the producer the engine reads, never
// a table of this test's own). Codex runs its built-in shell outside the SDK's
// per-name callbacks, so such a node acts on the worktree whatever its list
// says, and two of them on one worktree is the git-index race the guard exists
// to refuse (the end-to-end pair in fan_out_codex_declaration_test.go).
//
// Nothing else moves, in either direction, and the extent is counted. The
// cells are read with no tool-surface resolver, so what is counted here is the
// move of the DECLARED reading — by `tools:` shape, the only axis this table
// has. The held reading's own move, over the same cells crossed with the
// runtime's appends, is the subject of
// TestTheEffectiveSurfaceHalfOnlyRefusesAndOnlyThroughANamedArm; a supervised
// merge is shown both numbers.
func TestSharedWorktreeAdmissionMovesOnlyWhereCodexSandboxesTheListWritable(t *testing.T) {
	pairs := toolSurfaceGridPairs(t)
	var freed, offClass []string
	moved := 0
	byShape := map[string]int{}
	for _, p := range pairs {
		if p.was.admission == p.now.admission {
			continue
		}
		if !p.now.admission {
			freed = append(freed, p.key)
			continue
		}
		moved++
		byShape[p.cell.shape]++
		namesCodex := p.cell.primary == delegate.BackendCodex || p.cell.dflt == delegate.BackendCodex ||
			(p.cell.routeSet && p.cell.route == delegate.BackendCodex)
		sandboxed := delegate.CodexDeclaredListIsReadOnly(p.cell.tools, toolcatalog.ToolsDeclared(p.cell.tools))
		if !namesCodex || sandboxed {
			offClass = append(offClass, fmt.Sprintf("%s (names codex: %v, codex sandboxes the list read-only: %v)", p.key, namesCodex, sandboxed))
		}
	}
	sort.Strings(freed)
	sort.Strings(offClass)
	if len(freed) > 0 {
		t.Errorf("%d cell(s) became eligible for a shared worktree — this change only refuses, and admitting a node is the race the guard exists to prevent:\n  %s",
			len(freed), strings.Join(freed[:min(len(freed), 20)], "\n  "))
	}
	if len(offClass) > 0 {
		t.Errorf("%d cell(s) lost their shared-worktree eligibility outside the one class this change refuses — a fan-out that used to run now fails at the router:\n  %s",
			len(offClass), strings.Join(offClass[:min(len(offClass), 20)], "\n  "))
	}
	if moved != toolSurfaceAdmissionMoved {
		t.Errorf("admission moved on %d cells, the measured move is %d (by `tools:` shape: %v): the refusals this change adds are reviewed one by one, not inferred",
			moved, toolSurfaceAdmissionMoved, byShape)
	}
}
