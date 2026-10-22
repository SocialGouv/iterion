package runtime

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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
	toolSurfaceWidenedCells   = 5693
	toolSurfaceAdmissionMoved = 519
)

// The two named classes the admission move splits into, as three DISJOINT
// buckets so no cell is explained twice and none hides in an overlap. They sum
// to toolSurfaceAdmissionMoved.
const (
	// toolSurfaceAdmissionMovedForCodex: a cell naming codex with a declared
	// list codex's own classifier does not sandbox read-only, and whose
	// `backend:` names a route.
	toolSurfaceAdmissionMovedForCodex = 318
	// toolSurfaceAdmissionMovedForBoth: a `backend: auto` node the workflow
	// default routes onto codex, or one carrying a codex `fallbacks:` route —
	// the two classes answer for the same cell.
	toolSurfaceAdmissionMovedForBoth = 58
	// toolSurfaceAutoRoutedByTheDefault: the node spells `backend: auto` and
	// the workflow's `default_backend:` routes it somewhere codex's sandbox
	// does not explain — the route dispatch takes (#2428;
	// theAutoRouteExceptionReason words it).
	toolSurfaceAutoRoutedByTheDefault = 143
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
		cells:  2076,
	},
	{
		arm:         "a `fallbacks:` route that never receives the list",
		clause:      "never receives the `tools:` list, so a declared bound is dropped (#1652)",
		viaFallback: true,
		cells:       36,
	},
	{
		arm:    "the node's route is claude_code, where the list becomes --disallowedTools over a roster iterion does not own",
		clause: "takes the `tools:` list as --disallowedTools over a closed native roster iterion does not own, so a declared list bounds nothing (#1652)",
		cells:  924,
	},
	{
		arm:         "a `fallbacks:` route onto claude_code",
		clause:      "takes the `tools:` list as --disallowedTools over a closed native roster iterion does not own, so a declared list bounds nothing (#1652)",
		viaFallback: true,
		cells:       12,
	},
	{
		arm:    "codex, where the names decide the sandbox mode",
		clause: "maps the `tools:` list onto a sandbox mode, and what this node declares buys the workspace-write one, where codex's built-in shell acts on the worktree (#1727)",
		cells:  396,
	},
	{
		// The route no field named: `backend:` absent or `auto`, under a
		// `default_backend:` that names none either — absent, `auto`, or a
		// reference nothing answers. The default's spelling is what the
		// message quotes where it has one, and carrying a `$` does not make
		// that cell a reference the author wrote on the node.
		arm:    "a route left to dispatch (`backend:` absent or `auto`, under a `default_backend:` that names no route either)",
		clause: "resolves at dispatch, where the compiled-in detection order's first choice is claude_code, which does not bound what the node holds (#1652)",
		cells:  648,
	},
	{
		// `backend: auto` under `default_backend: claw` resolves to claw (the
		// route dispatch takes, #2428), and a claw node is read over the
		// `fallbacks:` route that takes it off claw — the arm every claw node
		// reaches, which the cells spelling `auto` could not reach while their
		// route was read as left to the host.
		arm:         "a claw route (named, or reached through `default_backend:`) with a `fallbacks:` route that leaves claw",
		clause:      "runs the full native toolset, which nothing the node declares bounds",
		viaFallback: true,
		cells:       28,
	},
	{
		arm:         "the same claw route, with a `fallbacks:` route onto codex, whose own sandbox mode a pre-run reading does not credit",
		clause:      "leaves claw, where the `tools:` list is the node's bound, and a pre-run reading does not credit the route's own sandbox mode as one",
		viaFallback: true,
		cells:       2,
	},
	{
		// The same move read on a node that declares NO list: it is routed by
		// `default_backend:` too, and omission is the full native toolset
		// wherever the route reads it that way.
		arm:    "no `tools:` list on a route reached through `default_backend:`, where omission is the full native toolset",
		clause: "no `tools:` list declared on ",
		cells:  36,
	},
	{
		arm:    "a `tools: []` the route reached through `default_backend:` does not credit",
		clause: "names no tool, which is no proof the node holds none",
		cells:  5,
	},
	{
		arm:    "the node's route is a `default_backend:` still spelled as a template",
		clause: "is unresolved before the run, so no route it may take bounds what the node holds",
		cells:  72,
	},
	{
		// Only the node's OWN field spelled as a reference reaches this arm:
		// the cell where the route is the host's to decide because dispatch
		// reads that spelling against the environment before `default_backend:`
		// answers anything.
		arm:    "the node's route is a `${VAR}` reference read at an authored default that does not exist",
		clause: "is read at its authored default, which names no route, so nothing here bounds what the node holds",
		cells:  1188,
	},
	{
		arm:         "a `fallbacks:` route spelled as a `${VAR}` reference read at an authored default that does not exist",
		clause:      "is read at its authored default, which names no route, so nothing here bounds what the node holds",
		viaFallback: true,
		cells:       252,
	},
	{
		arm:         "a `fallbacks:` route still spelled as a template",
		clause:      "is unresolved before the run, so no backend it may take bounds what the node holds",
		viaFallback: true,
		cells:       18,
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
// Its reference is the FROZEN table, and that is the extent of what it holds: a
// cell the freeze had in the class may not leave it, and every cell that joins
// joins through a named arm, with the engine's own wording and the cells that
// arm answers for — the blast radius of the move, counted rather than
// described. A cell that leaves the class takes a system prompt's UNTRUSTED
// INPUT BOUNDARY paragraph with it, and nothing in the catalogue guard can
// report a prompt that quietly stopped owing one (the complement in
// bots/catalog_untrusted_input_boundary_test.go names the one node that is out
// of the class on purpose).
//
// What it CANNOT say is which cell left the class since the freeze was taken:
// a cell the freeze had out, a later reading put in and this one takes out
// again moves no bucket here — only the widened TOTAL, by one. The count is
// therefore the alarm and never the report, and
// TestEveryCellTheWorkflowDefaultTookOutOfTheBoundaryClassIsNamed holds the
// other half: every cell out of the class on a route its own `backend:` does
// not name, enumerated with the reason that took it out.
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

// theAutoRouteExceptionReason is why a cell whose `backend:` is the explicit
// `auto` may lose its shared-worktree eligibility: dispatch resolves `auto`
// through the workflow's `default_backend:` — step 1 of
// model.ClawExecutor.resolveBackendName falls through on an absent field and on
// `auto` alike (ir.BackendFallsThrough) — so the reading now answers for the
// route the run takes. Where that default is claude_code, pi or a template, the
// node's `tools:` list bounds nothing and two such branches on one worktree are
// the race the guard refuses.
//
// What MOVES is the reading taken with NO backend resolver — the declared half
// this table freezes, and the extent of the move. An engine call never reaches
// the walk without one (Engine.backendResolver answers a stand-in rather than
// nil, and any name a resolver answers overrides the fold), so the cells
// counted here are moves of the executor-less reading, whose one consumer is
// the catalogue guard (bots/catalog_untrusted_input_boundary_test.go, through
// ToolSurfaceWriteReason). A run's own admission already read the workflow
// default's route through its executor: the fold brings the executor-less
// reading onto the answer that reading already had, and changes no verdict a
// run takes.
const theAutoRouteExceptionReason = "dispatch resolves `auto` through `default_backend:` — the reading now matches what runs (#2428)"

// routeFallsThroughWhileNamed reports whether a cell's own `backend:`, read the
// way the question reads it, resolves to SOMETHING and still leaves the route
// to the next step of the chain — the fold the workflow's `default_backend:`
// answers for.
//
// Keyed on the engine's own predicate over the RESOLVED field, so what it
// covers is the rule and not a spelling: a `backend: "${VAR:-auto}"` falls
// through exactly as the word does, and would otherwise land outside every
// named class of the two guarantees below. The frozen cell space carries no
// primary that expands to `auto`, which is why that shape is pinned directly
// (TestAReferenceAnsweredToAutoFallsThroughLikeTheWordItNames).
func routeFallsThroughWhileNamed(field string, lookup func(string) string) bool {
	resolved := strings.TrimSpace(ir.ExpandWithDefault(field, lookup))
	return resolved != "" && routeIsUnnamed(resolved)
}

// TestTheLookupDecidesWhereAFieldFallsThroughFrom pins the argument the two
// guarantees pass differently, and that nothing in the frozen cell space can
// witness: the boundary class reads a `backend:` at its AUTHORED default so a
// catalogue verdict does not depend on the host that compiled it, admission
// reads the process environment because that is what the run does.
//
// Every primary of the grid answers the two lookups the same — the grid pins
// its reference names empty, which is what makes the table freezable — so
// swapping a call site would be invisible to it as long as the host stays
// silent. Measured here on the cells of the enumerated guarantee itself: with
// the host answering, the two lookups fold different cell sets, and the
// catalogue question's own still holds the reviewed counts. The spelling that
// tells them apart is one of those references with a value: the reading that
// reads the host sees the fall-through, the one that reads the authored
// defaults sees a field that names no route at all, which is the OTHER bucket
// of the guarantee above.
func TestTheLookupDecidesWhereAFieldFallsThroughFrom(t *testing.T) {
	const name = gridHostAnsweredBackend
	ref := "${" + name + "}"
	if !slices.Contains(toolSurfaceGridEnvNames, name) {
		t.Fatalf("%s is not one of the grid's reference names %v: this pin is about a spelling the frozen cell space carries and pins empty", name, toolSurfaceGridEnvNames)
	}
	if !slices.Contains(toolSurfaceGridPrimaries, ref) {
		t.Fatalf("%q is not one of the grid's primaries: this pin is about a cell the table holds", ref)
	}
	rows := toolSurfaceGridRows(t)
	t.Setenv(name, "auto")
	if !routeFallsThroughWhileNamed(ref, nil) {
		t.Errorf("routeFallsThroughWhileNamed(%q, nil) = false: the lookup a run takes reads the host, which answers this reference with a word that falls through", ref)
	}
	if routeFallsThroughWhileNamed(ref, authoredDefaults) {
		t.Errorf("routeFallsThroughWhileNamed(%q, authoredDefaults) = true: the catalogue's lookup answers no name, so this field names no route and is not the fold the workflow default answers for", ref)
	}
	// And what the swap would DO to the enumerated guarantee, measured on the
	// same cells it counts: the host's reading folds cells the catalogue
	// question does not, and the question's own still answers the reviewed
	// counts — which is what the guarantee asserts under exactly this
	// environment, and what makes its own lookup refusable.
	host, question := requireTheLookupsPart(t, rows)
	if question.fold != theAutoFoldIsOutOfTheBoundaryClass || question.absent != theAbsentFieldIsOutOfTheBoundaryClass {
		t.Errorf("the catalogue question's own lookup folds %d cells and finds %d on an absent field, the reviewed counts are %d and %d: a verdict a guard takes on a compiled `.bot` may not depend on the host it runs on",
			question.fold, question.absent, theAutoFoldIsOutOfTheBoundaryClass, theAbsentFieldIsOutOfTheBoundaryClass)
	}
	if host.fold <= question.fold {
		t.Errorf("the host's reading folds %d cells and the authored defaults' %d: a host answering %s with a word that falls through folds MORE, so a bucketing that read the host would count cells no catalogue verdict takes",
			host.fold, question.fold, name)
	}
}

// requireTheLookupsPart stops the run unless the two lookups answer the grid
// differently — the premise both guarantees below rest on, checked rather than
// assumed. Each counts cells bucketed through ONE lookup, and in a run where
// the host answers nothing the two readings coincide, so the counts could not
// say which one they read and a swapped argument would pass.
//
// It returns both readings so the caller can state what parts them.
func requireTheLookupsPart(t *testing.T, rows []toolSurfaceGridRow) (host, question boundaryDepartures) {
	t.Helper()
	host, question = boundaryClassDepartures(rows, lookupOf(t, unnamedRouteUnread)), boundaryClassDepartures(rows, lookupOf(t, unnamedRouteIsNoBound))
	if host.fold == question.fold && maps.Equal(host.foldByDefault, question.foldByDefault) {
		t.Fatalf("the process environment and the authored defaults fold the same %d cells (%v): %s must answer a word that falls through before a count can say WHICH reading it holds",
			question.fold, question.foldByDefault, gridHostAnsweredBackend)
	}
	return host, question
}

// gridHostAnsweredBackend is the grid reference name the two pins above and
// below answer with a word that FALLS THROUGH: the one host value that tells
// the catalogue question's lookup (the authored defaults alone) from the one a
// run takes (the process environment).
const gridHostAnsweredBackend = "ITERION_GRID_UNSET_BACKEND"

// boundaryDepartures are the cells out of the boundary class on a route their
// own `backend:` does not name, in the three disjoint buckets the guarantee
// below enumerates, plus the cell SETS the two folds are compared by.
type boundaryDepartures struct {
	fold, absent    int
	foldByDefault   map[string]int
	absentByPrimary map[string]int
	// foldCells and absentCells key a cell by everything EXCEPT the node's own
	// `backend:`, so "the absent field takes out the same cells as `auto`" is a
	// comparison of sets and not of two totals on different axes.
	foldCells   map[string]bool
	absentCells map[string]map[string]bool
	misread     []string
	// onAReferenceNothingAnswers must stay empty: see the guarantee's doc.
	onAReferenceNothingAnswers []string
}

// boundaryClassDepartures buckets the departures, reading each cell's
// `backend:` with the LOOKUP its question passes — the argument no frozen cell
// can witness, which is why it is a parameter here and asserted by
// TestTheLookupDecidesWhereAFieldFallsThroughFrom rather than closed over.
func boundaryClassDepartures(rows []toolSurfaceGridRow, lookup func(string) string) boundaryDepartures {
	d := boundaryDepartures{
		foldByDefault: map[string]int{}, absentByPrimary: map[string]int{},
		foldCells: map[string]bool{}, absentCells: map[string]map[string]bool{},
	}
	for _, r := range rows {
		if r.canWrite {
			continue
		}
		field := strings.TrimSpace(ir.ExpandWithDefault(r.cell.primary, lookup))
		if !routeIsUnnamed(field) {
			// The node's own field names the route its bound was read on, so
			// nothing the workflow default says took this cell out.
			continue
		}
		key := fmt.Sprintf("default=%q route=%v/%q tools=%s", r.cell.dflt, r.cell.routeSet, r.cell.route, r.cell.shape)
		switch {
		case routeFallsThroughWhileNamed(r.cell.primary, lookup):
			d.fold++
			d.foldByDefault[r.cell.dflt]++
			d.foldCells[key] = true
			if irOnly, viaDispatch := routeReadings(r.cell, unnamedRouteIsNoBound, lookup); irOnly != viaDispatch || viaDispatch != "" {
				d.misread = append(d.misread, fmt.Sprintf("%s (dispatch resolves %q)\n      IR-only        %q\n      via dispatch   %q",
					r.key, dispatchRoute(r.cell), irOnly, viaDispatch))
			}
		case strings.TrimSpace(r.cell.primary) == "":
			d.absent++
			d.absentByPrimary[r.cell.primary]++
			if d.absentCells[r.cell.primary] == nil {
				d.absentCells[r.cell.primary] = map[string]bool{}
			}
			d.absentCells[r.cell.primary][key] = true
		default:
			d.onAReferenceNothingAnswers = append(d.onAReferenceNothingAnswers, r.key)
		}
	}
	return d
}

// The cells the boundary class does not hold because the workflow's
// `default_backend:` answers for a `backend:` of their own that names no route,
// as two counted classes. Every cell that can leave the class under a change to
// the ROUTE reading is one of them: a route no name resolves always carries a
// reason (unnamedRouteNoBoundReason), so a cell out of the class on a field
// that names nothing is one the default's route bounds.
const (
	// theAutoFoldIsOutOfTheBoundaryClass: the node's `backend:` resolves to
	// something and still falls through — the explicit `auto` — and the route
	// the workflow's `default_backend:` names is one that bounds it. Each of
	// these cells owes no UNTRUSTED INPUT BOUNDARY paragraph where a reading of
	// `auto` as a route left to the host gave it one: the move #2428 makes,
	// counted (theAutoRouteExceptionReason). Only the two `default_backend:`
	// values that BOUND a declaration reach it, in the decomposition
	// theAutoFoldByWorkflowDefault asserts: 24 under claw — 4 of the 12
	// `fallbacks:` routes crossed with 6 of the 7 `tools:` shapes — and 5
	// under codex, whose sandbox bounds the read-only shape alone.
	theAutoFoldIsOutOfTheBoundaryClass = 29
	// theAbsentFieldIsOutOfTheBoundaryClass: the same fold on a node that names
	// NO route at all. Counted beside the other so neither can drift into it —
	// and it is the SAME 29 cells for each of the two primaries that name
	// nothing, held as a SET rather than inferred from equal totals on two
	// different axes, which is what "`auto` is read like the empty field"
	// means measured (theAbsentFieldByPrimarySpelling, and the cell-key
	// comparison the test runs over everything but the node's own `backend:`).
	theAbsentFieldIsOutOfTheBoundaryClass = 58
)

// The two counts above, decomposed on the axis each is reviewed by, so the
// derivation is asserted rather than described: a comment that states a split
// no assertion holds is the next reader's wrong lead.
var (
	// theAutoFoldByWorkflowDefault: the `default_backend:` that answered for
	// each folded cell. 24 under claw, which resolves its tool definitions
	// from the list; 5 under codex, which bounds the list by what its names
	// buy. The grid's six OTHER defaults are absent, and each for its own
	// reason: claude_code and pi name a route no `tools:` list bounds,
	// `{{vars.d}}` is unresolved before the run, and an absent one, `auto` and
	// a reference nothing answers name no route at all — every one of them
	// keeps its cells IN the class.
	theAutoFoldByWorkflowDefault = map[string]int{"claw": 24, "codex": 5}
	// theAbsentFieldByPrimarySpelling: the two `backend:` spellings that name
	// nothing, each answering for the same 29 cells.
	theAbsentFieldByPrimarySpelling = map[string]int{"": 29, "  ": 29}
)

// TestEveryCellTheWorkflowDefaultTookOutOfTheBoundaryClassIsNamed is the
// enumerated half of the boundary guarantee: the freeze-relative test above
// reports a departure since the freeze as a total that moved by one, so the
// cells a change takes out of the class are named HERE, with the reading that
// took them out, and counted.
//
// Three disjoint readings reach a cell out of the class on a route its own
// `backend:` does not name:
//
//   - the fold the explicit `auto` falls into, which the workflow default
//     answers — the route dispatch resolves, checked per cell against
//     dispatch's own resolution rather than against the spelling;
//   - the absent field, the same fold on a node that names nothing;
//   - a `backend:` REFERENCE this reading cannot answer, which must reach no
//     cell at all: at dispatch the host answers that spelling first and
//     `default_backend:` only on a host that names none, so crediting it with
//     the default's bound drops a paragraph on a route no run need take — the
//     defect class #1727 closed, and the one a fold over an empty EXPANSION
//     rather than over the engine's rule walks straight back into.
func TestEveryCellTheWorkflowDefaultTookOutOfTheBoundaryClassIsNamed(t *testing.T) {
	rows := toolSurfaceGridRows(t)
	// The LOOKUP the bucketing reads a `backend:` with is load-bearing and no
	// cell of the frozen space can witness it while every reference name
	// answers nothing: the boundary class resolves a backend at its AUTHORED
	// default (ToolSurfaceWriteReason's callers), so the counts below must not
	// move on a host that answers one of those names. Answered here with a
	// word that FALLS THROUGH, the one answer that would move them — read
	// against the host instead, the grid's reference primaries would join the
	// fold and every count below would be a different cell set's.
	//
	// Pinned AFTER the rows are read: the verdicts they hold are the catalogue
	// question's (authoredDefaults) and admission's (the process environment,
	// which toolSurfaceGridRows pins empty for the freeze), so this value
	// reaches the bucketing alone.
	if !slices.Contains(toolSurfaceGridEnvNames, gridHostAnsweredBackend) {
		t.Fatalf("%s is not one of the grid's reference names %v: this pin is about a spelling the frozen cell space carries and pins empty",
			gridHostAnsweredBackend, toolSurfaceGridEnvNames)
	}
	t.Setenv(gridHostAnsweredBackend, "auto")
	requireTheLookupsPart(t, rows)
	d := boundaryClassDepartures(rows, lookupOf(t, unnamedRouteIsNoBound))
	misread, onAReferenceNothingAnswers := d.misread, d.onAReferenceNothingAnswers
	fold, absent := d.fold, d.absent
	foldByDefault, absentByPrimary := d.foldByDefault, d.absentByPrimary
	sort.Strings(onAReferenceNothingAnswers)
	if len(misread) > 0 {
		t.Errorf("%d cell(s) are out of the boundary class on a reading that is NOT the one dispatch resolves, so %q does not cover them — a prompt that stops owing an UNTRUSTED INPUT BOUNDARY paragraph must stop owing it because of the route the run takes:\n  %s",
			len(misread), theAutoRouteExceptionReason, strings.Join(misread[:min(len(misread), 20)], "\n  "))
	}
	if len(onAReferenceNothingAnswers) > 0 {
		t.Errorf("%d cell(s) are out of the boundary class because the workflow default answered a `backend:` reference this reading could not: the host answers that spelling first at dispatch, so the default's bound is credited on a route no run need take (#1727):\n  %s",
			len(onAReferenceNothingAnswers), strings.Join(onAReferenceNothingAnswers[:min(len(onAReferenceNothingAnswers), 20)], "\n  "))
	}
	if fold != theAutoFoldIsOutOfTheBoundaryClass {
		t.Errorf("the fold the workflow default answers for leaves %d cell(s) out of the boundary class, the reviewed count is %d (%s): each one is a shipped prompt that may stop owing a paragraph, so the count is reviewed, not inferred",
			fold, theAutoFoldIsOutOfTheBoundaryClass, theAutoRouteExceptionReason)
	}
	if absent != theAbsentFieldIsOutOfTheBoundaryClass {
		t.Errorf("a `backend:` that names nothing leaves %d cell(s) out of the boundary class, the reviewed count is %d: the fall-through of an absent field moves with no ticket at all",
			absent, theAbsentFieldIsOutOfTheBoundaryClass)
	}
	// The derivation each count is reviewed by, held by the same run that
	// holds the totals: a total that stays put while its split moves is two
	// errors cancelling, and the comment on the constant would still read true.
	if !maps.Equal(foldByDefault, theAutoFoldByWorkflowDefault) {
		t.Errorf("the fold leaves cells out of the class under %v, the reviewed decomposition is %v: only a `default_backend:` that BOUNDS a declaration may take a cell out, so a new key is a route credited with a bound it does not have",
			foldByDefault, theAutoFoldByWorkflowDefault)
	}
	if !maps.Equal(absentByPrimary, theAbsentFieldByPrimarySpelling) {
		t.Errorf("the absent field leaves cells out of the class per spelling %v, the reviewed decomposition is %v: the two spellings that name nothing answer for the same cells, which is what makes `auto` read like the empty field a measurement",
			absentByPrimary, theAbsentFieldByPrimarySpelling)
	}
	// The two decompositions above are keyed on different axes — the fold on
	// the `default_backend:` that answered, the absent field on the spelling
	// that fell through — so equal totals say nothing about the cells
	// themselves. What "`auto` is read like a field that names nothing" claims
	// is that the SAME cells leave the class, and this is that claim: the
	// triple of everything but the node's own `backend:`, compared as a set.
	for _, primary := range toolSurfaceGridPrimaries {
		cells, ok := d.absentCells[primary]
		if !ok {
			continue
		}
		if !maps.Equal(cells, d.foldCells) {
			t.Errorf("the %d cell(s) a `backend: %q` leaves out of the class are not the %d the `auto` fold leaves out: `auto` is read like a field that names nothing, so the two take out the same (`default_backend:`, `fallbacks:`, `tools:`) cells or one of the two readings moved\n  only on %q: %v\n  only on the fold: %v",
				len(cells), primary, len(d.foldCells), primary, cellKeysMissingFrom(cells, d.foldCells), cellKeysMissingFrom(d.foldCells, cells))
		}
	}
}

// cellKeysMissingFrom lists the keys of a that b does not hold, sorted — what
// a set comparison owes its reader instead of two counts.
func cellKeysMissingFrom(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out[:min(len(out), 10)]
}

// TestSharedWorktreeAdmissionMovesOnlyWhereCodexSandboxesTheListWritable is the
// other half of the same guarantee, on the question that REFUSES work: a cell
// that moves here stops being eligible for a parallel read-only fan-out, so a
// false positive costs an author a workflow that used to run.
//
// The move is two named classes and the test says which, cell by cell.
//
// One: a cell that names codex, with a declared list codex's own classifier
// does not sandbox read-only (delegate.CodexDeclaredListIsReadOnly — the
// producer the engine reads, never a table of this test's own). Codex runs its
// built-in shell outside the SDK's per-name callbacks, so such a node acts on
// the worktree whatever its list says, and two of them on one worktree is the
// git-index race the guard exists to refuse (the end-to-end pair in
// fan_out_codex_declaration_test.go).
//
// Two: a cell whose `backend:` falls through while naming something — the
// explicit `auto` — read through the workflow's `default_backend:` for
// theAutoRouteExceptionReason. Each such cell is checked against DISPATCH's own
// resolution of it rather than against its spelling: the reading that moved it
// must be the reading taken through the route the run takes
// (TestTheIROnlyReadingTakesTheRouteDispatchResolves holds that over the whole
// cell space, both questions).
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
	var freed, offClass, misread []string
	moved, codexSandboxed, both, autoRouted := 0, 0, 0, 0
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
		// The second class is keyed on the ENGINE's rule rather than on the
		// word: a `backend:` that RESOLVES to something and still falls through
		// is the fold the workflow default answers for, whether the author
		// wrote `auto` or a `${VAR:-auto}` this reading answered. Admission
		// reads a field against the process environment (isMutatingNodeIn
		// passes no lookup), which is how the cell was read above. Each such
		// cell is checked against dispatch's own resolution, so the exception
		// covers the reading that moved it and not the spelling.
		fallsThrough := routeFallsThroughWhileNamed(p.cell.primary, lookupOf(t, unnamedRouteUnread))
		if fallsThrough {
			if irOnly, viaDispatch := routeReadings(p.cell, unnamedRouteUnread, lookupOf(t, unnamedRouteUnread)); irOnly != viaDispatch {
				misread = append(misread, fmt.Sprintf("%s\n      IR-only        %q\n      via dispatch   %q", p.key, irOnly, viaDispatch))
			}
		}
		switch {
		case namesCodex && !sandboxed && fallsThrough:
			both++
		case namesCodex && !sandboxed:
			codexSandboxed++
		case fallsThrough:
			autoRouted++
		default:
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
		t.Errorf("%d cell(s) lost their shared-worktree eligibility outside the two classes this change refuses — a fan-out that used to run now fails at the router:\n  %s",
			len(offClass), strings.Join(offClass[:min(len(offClass), 20)], "\n  "))
	}
	if len(misread) > 0 {
		t.Errorf("%d cell(s) whose `backend:` falls through while naming a route moved on a reading that is NOT the one dispatch resolves, so %q does not cover them:\n  %s",
			len(misread), theAutoRouteExceptionReason, strings.Join(misread[:min(len(misread), 20)], "\n  "))
	}
	if codexSandboxed != toolSurfaceAdmissionMovedForCodex || both != toolSurfaceAdmissionMovedForBoth {
		t.Errorf("the move splits into %d cell(s) codex's sandbox alone explains and %d both classes do, the reviewed split is %d and %d: a cell moving under another class is a cell nobody measured",
			codexSandboxed, both, toolSurfaceAdmissionMovedForCodex, toolSurfaceAdmissionMovedForBoth)
	}
	if autoRouted != toolSurfaceAutoRoutedByTheDefault {
		t.Errorf("%d cell(s) moved because the workflow default routes a `backend:` that falls through while naming a route, and nothing else explains them; the reviewed exception is %d (%s): each one is a fan-out that used to be admitted, so the count is reviewed, not inferred",
			autoRouted, toolSurfaceAutoRoutedByTheDefault, theAutoRouteExceptionReason)
	}
	if moved != toolSurfaceAdmissionMoved {
		t.Errorf("admission moved on %d cells, the measured move is %d (by `tools:` shape: %v): the refusals this change adds are reviewed one by one, not inferred",
			moved, toolSurfaceAdmissionMoved, byShape)
	}
}
