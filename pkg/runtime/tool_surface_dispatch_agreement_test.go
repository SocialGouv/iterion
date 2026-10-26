package runtime

import (
	"fmt"
	"maps"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The IR-only reading of a node's route, read against the ROUTE THE RUN TAKES.
//
// Both questions this package answers about a node — the catalogue's
// prompt-injection boundary (ToolSurfaceWriteReason, unnamedRouteIsNoBound) and
// shared-worktree admission (isMutatingNodeCtx, unnamedRouteUnread) — are asked
// before the run, and one of them is asked with no executor at all: the
// catalogue guard compiles a `.bot` and classifies its nodes. What that reading
// makes of the node's route is therefore a claim about what dispatch will
// resolve, and a claim is only worth the cell space it holds over: `backend:
// auto` under a `default_backend:` was read as a route left to the host while
// dispatch had already resolved it through the workflow default (#2428).

// dispatchRoute is the backend DISPATCH resolves for a cell, from its own chain
// and nothing else: the program-only executor runs resolveBackendName's steps 1
// and 2 — the node's `backend:`, then the workflow's `default_backend:` — and
// stops where the host would answer, which is exactly the half a pre-run
// reading may state.
func dispatchRoute(c toolSurfaceGridCell) string {
	return model.NewProgramExecutor(&ir.Workflow{DefaultBackend: c.dflt}).EffectiveBackendName(c.node())
}

// routeReadings asks the ONE walk both questions share for a cell, twice: as
// the IR-only reading asks it (no resolver), and with a resolver that answers
// dispatch's own resolution of the node's route.
//
// The resolver answers that ONE seam, so the node's route is the only thing
// that differs between the two readings: `answersRoute` implements neither the
// fallback seam — so both walks read the `fallbacks:` routes from the IR, by
// the spelling their author wrote — nor the undecided-route reporter, so
// neither reading carries a clause the other does not.
//
// It is the DECLARED half of each question, which is where a route is read. The
// half that asks what a node will HOLD resolves opt-ins outside the IR and is
// measured over the same cells in
// TestTheEffectiveSurfaceHalfOnlyRefusesAndOnlyThroughANamedArm.
func routeReadings(c toolSurfaceGridCell, question unnamedRouteReading, lookup func(string) string) (irOnly, throughDispatch string) {
	a, b := c.node(), c.node()
	irOnly = llmDeclaredSurfaceWriteReason(a, a.LLMFields, a.Tools, c.dflt, nil, question, lookup)
	throughDispatch = llmDeclaredSurfaceWriteReason(b, b.LLMFields, b.Tools, c.dflt, answersRoute(dispatchRoute(c)), question, lookup)
	return irOnly, throughDispatch
}

// theQuestionsThisPackageAsks are the two readings of a node, each with the
// lookup its production entry point passes: the boundary class resolves a
// backend at its authored default so a catalogue verdict does not depend on the
// host that compiled it (ToolSurfaceWriteReason's callers), admission reads the
// process environment, which is what the run does (isMutatingNodeIn).
var theQuestionsThisPackageAsks = []struct {
	name     string
	question unnamedRouteReading
	lookup   func(string) string
}{
	{"the boundary class", unnamedRouteIsNoBound, authoredDefaults},
	{"shared-worktree admission", unnamedRouteUnread, nil},
}

// lookupOf is the lookup a question's production entry point passes, read from
// the table above — the ONE place the pairing is declared, so no guarantee of
// this package can read a route from an environment its question never reads.
//
// Every reading of the grid goes through it. The argument is invisible to the
// frozen cell space on its own (the grid pins its reference names empty, which
// is what makes the table freezable), so the pairing is where a swap is caught:
// TestTheLookupDecidesWhereAFieldFallsThroughFrom answers one of those names
// and holds what each lookup then makes of it, and
// TestEveryCellTheWorkflowDefaultTookOutOfTheBoundaryClassIsNamed counts its
// cells under that same answer.
func lookupOf(t *testing.T, q unnamedRouteReading) func(string) string {
	t.Helper()
	for _, it := range theQuestionsThisPackageAsks {
		if it.question == q {
			return it.lookup
		}
	}
	t.Fatalf("no question of this package reads %v: the lookup a reading resolves a `backend:` with is the question's, never a test's own choice", q)
	return nil
}

// theReadingsPartWhereTheSourceNamesAReferenceNothingAnswers counts the cells
// where the IR-only reading deliberately REFUSES dispatch's answer, over the
// whole cell space and both questions.
//
// A `backend:` that names a reference and resolves to nothing here has not left
// its route to the workflow default: at dispatch the same spelling is read
// against the HOST's environment first, and `default_backend:` answers only on
// a host that names none. So the route is the host's to decide, and crediting
// it with the workflow default's bound would trust the one route this reading
// cannot read — a `${VAR}` under `default_backend: claw` read as claw while the
// host may answer claude_code.
//
// Only the boundary class parts: it is the question whose cost is a missing
// paragraph in a system prompt, so it reads the reference as a route nothing
// bounds. Admission's cost is a refused fan-out, so it keeps the workflow
// default's reading there and agrees with dispatch on every cell.
//
// The count is the three reference spellings of the grid (`${X}`, `$X` and
// `${X:-}`, all three read at an authored default that does not exist) over the
// five `default_backend:` values dispatch answers something for — the four that
// name a backend, plus the unresolved `{{vars.d}}` template, which step 2 hands
// back as written — times the 72 of each line's 84 cells where the route is
// what the verdict rests on. The other 12 are the `tools:` shape that declares
// a writer: the author's own line answers before any route is read, so both
// readings word it the same. The three defaults dispatch answers nothing for (an
// absent one, `auto` and a reference nothing answers) leave the node's own unanswered
// reference as the only route, which both readings then read alike.
const theReadingsPartWhereTheSourceNamesAReferenceNothingAnswers = 1080

// theReadingsPartByWorkflowDefault is the decomposition the count above is
// reviewed by, asserted rather than described: WHICH `default_backend:` values
// dispatch answers something for is the half of the derivation a reader cannot
// check by arithmetic.
var theReadingsPartByWorkflowDefault = map[string]int{
	"claw": 216, "claude_code": 216, "codex": 216, "pi": 216, "{{vars.d}}": 216,
}

// TestTheIROnlyReadingTakesTheRouteDispatchResolves is the acceptance of #2428,
// read over the whole cell space rather than on the spellings someone thought
// to attack: for every cell and both questions, the reading taken with no
// executor says what the reading taken through dispatch's own resolution says.
//
// Measured on the bytes before the fix: 662 cell readings disagree, every one
// of them a node spelling `backend: auto` under one of the five
// `default_backend:` values that name a route — the IR-only reading left `auto`
// unresolved and read it as a route the host decides, while dispatch had
// already routed the node through the workflow default.
func TestTheIROnlyReadingTakesTheRouteDispatchResolves(t *testing.T) {
	for _, name := range toolSurfaceGridEnvNames {
		t.Setenv(name, "")
	}
	parted := 0
	bySpelling, byDefault := map[string]int{}, map[string]int{}
	var disagree []string
	for _, q := range theQuestionsThisPackageAsks {
		for _, c := range toolSurfaceGridCells() {
			irOnly, throughDispatch := routeReadings(c, q.question, q.lookup)
			if irOnly == throughDispatch {
				continue
			}
			// The one cell the two readings part on, deliberately.
			if strings.Contains(irOnly, unresolvedReferenceClause) && q.question == unnamedRouteIsNoBound {
				parted++
				bySpelling[fmt.Sprintf("%q under default %q", c.primary, c.dflt)]++
				byDefault[c.dflt]++
				continue
			}
			disagree = append(disagree, fmt.Sprintf("%s, %s\n      IR-only        %q\n      via dispatch   %q (route %q)",
				c.String(), q.name, irOnly, throughDispatch, dispatchRoute(c)))
		}
	}
	sort.Strings(disagree)
	if len(disagree) > 0 {
		t.Errorf("%d cell reading(s) disagree with the route dispatch resolves — a pre-run analysis that keys on a node's backend must read the route the run takes (model.ClawExecutor.resolveBackendName), or say in this test why it may not:\n  %s",
			len(disagree), strings.Join(disagree[:min(len(disagree), 20)], "\n  "))
	}
	if parted != theReadingsPartWhereTheSourceNamesAReferenceNothingAnswers {
		t.Errorf("the two readings part on %d cells, the reviewed exception is %d (by the `backend:` the cell spells: %v): a cell where a pre-run reading refuses what dispatch resolves is read one by one, not inferred",
			parted, theReadingsPartWhereTheSourceNamesAReferenceNothingAnswers, bySpelling)
	}
	if !maps.Equal(byDefault, theReadingsPartByWorkflowDefault) {
		t.Errorf("the two readings part under %v, the reviewed decomposition is %v: the exception holds only where the workflow default answers a route the node's own reference did not, so a key that moves is a cell nobody reviewed",
			byDefault, theReadingsPartByWorkflowDefault)
	}
}

// The three shapes of a route this reading cannot NAME, counted over the whole
// cell space: the cells where the unnamedRouteReading flag — and nothing else —
// parts the two questions this package asks.
//
// Both sides are read with the SAME lookup and no resolver, so the flag is the
// only axis; which lookup each question's entry point passes is a different
// claim, pinned in TestTheLookupDecidesWhereAFieldFallsThroughFrom.
const (
	// theFlagPartsOnANodeReferenceNothingAnswers: the three primaries that are
	// references read at an authored default that does not exist, over all
	// eight `default_backend:` values, times the 72 of each line's 84 cells
	// where the route is what the verdict rests on — the other 12 are the
	// `writer` shape, which the author's own line answers before any route is
	// read: 3 x 8 x 72.
	theFlagPartsOnANodeReferenceNothingAnswers = 1728
	// theFlagPartsOnARouteNeitherFieldNames: the three primaries that fall
	// through (absent, whitespace, `auto`) crossed with the three
	// `default_backend:` values that name no route either — absent, `auto`,
	// and a reference nothing answers — over the same 72: 3 x 3 x 72.
	theFlagPartsOnARouteNeitherFieldNames = 648
	// theFlagPartsOnAFallbackReferenceNothingAnswers: the two `fallbacks:`
	// spellings nothing answers, 136 cells each, on the nodes whose own route
	// is read as bounding the declared list — there the chain's unreadable
	// entry is what the verdict turns on.
	theFlagPartsOnAFallbackReferenceNothingAnswers = 272
)

// The three shapes, named once so the fact and the engine's own wording are
// compared by the same vocabulary.
const (
	aNodeReferenceNothingAnswers     = "a `backend:` reference nothing answers"
	aRouteNeitherFieldNames          = "a route neither field names"
	aFallbackReferenceNothingAnswers = "a `fallbacks:` route nothing answers"
)

// theRouteShapeOf names which shape a cell is, from its own fields read through
// the rules the walk itself asks (ir.ExpandWithDefault, ir.BackendFallsThrough)
// and in the precedence unrestrictedRouteWriteReason answers them: the node's
// own unanswered reference first, then the route neither field names, then the
// chain's. "" for a cell that is none of them.
func theRouteShapeOf(c toolSurfaceGridCell, lookup func(string) string) string {
	read := func(s string) string { return strings.TrimSpace(ir.ExpandWithDefault(s, lookup)) }
	switch {
	case strings.TrimSpace(c.primary) != "" && read(c.primary) == "":
		return aNodeReferenceNothingAnswers
	case routeIsUnnamed(read(c.primary)) && routeIsUnnamed(read(c.dflt)):
		return aRouteNeitherFieldNames
	case c.routeSet && strings.TrimSpace(c.route) != "" && read(c.route) == "":
		return aFallbackReferenceNothingAnswers
	}
	return ""
}

// theClauseShapeOf names which shape the ENGINE's own wording says, read off
// the reason it emitted: the clause it carries, and the prefix the `fallbacks:`
// walk writes.
func theClauseShapeOf(reason string) string {
	viaFallback := strings.HasPrefix(reason, fallbackReasonPrefix)
	switch {
	case strings.Contains(reason, unresolvedReferenceClause) && !viaFallback:
		return aNodeReferenceNothingAnswers
	case strings.Contains(reason, unresolvedReferenceClause):
		return aFallbackReferenceNothingAnswers
	case strings.Contains(reason, unnamedRouteDispatchClause):
		return aRouteNeitherFieldNames
	}
	return ""
}

// TestTheFlagPartsTheTwoQuestionsOnEveryRouteThisReadingCannotName holds what
// the unnamedRouteReading flag governs, over the whole cell space: WHICH cells
// the catalogue's prompt-injection boundary and shared-worktree admission read
// differently, and that each one's clause states the fact that put it there.
//
// Both halves matter to a reader of the exported predicate. The cells say the
// flag is three shapes and not one, so a bot author's "does this prompt owe an
// UNTRUSTED INPUT BOUNDARY paragraph" cannot be answered from the unnamed-route
// cell alone. The clause says the message follows the FACT: a wording that
// tells the shapes apart by the `$` in the spelling it is about to quote calls
// 216 of these cells a reference the author never wrote, because after the
// fall-through that spelling is the workflow default's.
func TestTheFlagPartsTheTwoQuestionsOnEveryRouteThisReadingCannotName(t *testing.T) {
	rows := toolSurfaceGridRows(t)
	// ONE lookup, read from the pairing and passed to both readings and to the
	// fact: the flag is the only axis this test varies, and a cell read with
	// two different lookups would part for a reason that is not it.
	lookup := lookupOf(t, unnamedRouteIsNoBound)
	byShape := map[string]int{}
	var unclassed, misworded []string
	for _, r := range rows {
		a, b := r.cell.node(), r.cell.node()
		noBound := llmDeclaredSurfaceWriteReason(a, a.LLMFields, a.Tools, r.cell.dflt, nil, unnamedRouteIsNoBound, lookup)
		unread := llmDeclaredSurfaceWriteReason(b, b.LLMFields, b.Tools, r.cell.dflt, nil, unnamedRouteUnread, lookup)
		if noBound == unread {
			continue
		}
		shape := theRouteShapeOf(r.cell, lookup)
		if shape == "" {
			unclassed = append(unclassed, fmt.Sprintf("%s\n      flag on  %q\n      flag off %q", r.key, noBound, unread))
			continue
		}
		byShape[shape]++
		if said := theClauseShapeOf(noBound); said != shape {
			misworded = append(misworded, fmt.Sprintf("%s is %s, worded as %s:\n      %q", r.key, shape, said, noBound))
		}
	}
	sort.Strings(unclassed)
	sort.Strings(misworded)
	if len(unclassed) > 0 {
		t.Errorf("%d cell(s) are read differently under the two questions on a route shape this test does not name — the flag is a parameter of the QUESTION, so a fourth shape is a divergence nobody reviewed (unnamedRouteReading):\n  %s",
			len(unclassed), strings.Join(unclassed[:min(len(unclassed), 10)], "\n  "))
	}
	if len(misworded) > 0 {
		t.Errorf("%d cell(s) carry the clause of another shape: which clause a cell gets is the fact of which field left the route unnamed, never the `$` in the spelling the message quotes:\n  %s",
			len(misworded), strings.Join(misworded[:min(len(misworded), 10)], "\n  "))
	}
	want := map[string]int{
		aNodeReferenceNothingAnswers:     theFlagPartsOnANodeReferenceNothingAnswers,
		aRouteNeitherFieldNames:          theFlagPartsOnARouteNeitherFieldNames,
		aFallbackReferenceNothingAnswers: theFlagPartsOnAFallbackReferenceNothingAnswers,
	}
	if !maps.Equal(byShape, want) {
		t.Errorf("the flag parts the two questions on %v, the reviewed decomposition is %v: every cell in it is a node the catalogue guard and the fan-out guard read differently, so the count is reviewed, not inferred",
			byShape, want)
	}
}

// TestAnExecutorsAnswerOverridesTheFoldOntoTheWorkflowDefault pins the arm the
// fold's blast radius rests on: wherever an executor names the node's route,
// that name IS the reading, and the walk's own fall-through onto
// `default_backend:` answers only where nothing else does
// (routeThisReadingAnswersFor).
//
// It is why the cells the two guarantees count are moves of the executor-less
// reading alone. An engine call never reaches the walk without a resolver —
// Engine.backendResolver answers a stand-in rather than nil — so a run's
// admission already read the route its executor resolved, which for a node on
// `auto` is the workflow default's: the fold brings the executor-less reading
// onto that same answer. The resolver here names the OPPOSITE route to the
// workflow default, so each row can only pass if the executor's answer won.
func TestAnExecutorsAnswerOverridesTheFoldOntoTheWorkflowDefault(t *testing.T) {
	readOnly := []string{"read_file", "glob"}
	for _, tc := range []struct {
		dflt, route, reason string
		admission           bool
		why                 string
	}{
		{
			dflt: "claude_code", route: "claw", reason: "", admission: false,
			why: "the executor routes the node to claw, which resolves its tool definitions from the list, so the declaration is the bound — the workflow default it falls through to names a route that bounds nothing",
		},
		{
			dflt: "claw", route: "claude_code",
			reason: "claude_code " + declarationNoBoundClause("claude_code", readOnly), admission: true,
			why: "the executor routes the node to claude_code, whose --disallowedTools bounds nothing — the workflow default it falls through to would have bounded it",
		},
	} {
		t.Run(tc.dflt+" overridden by "+tc.route, func(t *testing.T) {
			node := agentNode("a", readOnly, "auto", nil)
			if got := llmDeclaredSurfaceWriteReason(node, node.LLMFields, node.Tools, tc.dflt, answersRoute(tc.route), unnamedRouteIsNoBound, authoredDefaults); got != tc.reason {
				t.Errorf("the boundary class reads %q, want %q: %s", got, tc.reason, tc.why)
			}
			if got := isMutatingNodeCtx(agentNode("a", readOnly, "auto", nil), tc.dflt, answersRoute(tc.route), nil, false); got != tc.admission {
				t.Errorf("admission reads %v, want %v: %s", got, tc.admission, tc.why)
			}
		})
	}
}

// TestAReferenceNothingAnswersStaysTheHostsToDecide pins the exception the
// count above holds over, on the cell an author actually writes: a node dialled
// `backend: "${VAR}"` under a workflow default, on a host that answers the
// reference with nothing.
//
// The boundary class reads it as a route nothing here bounds — the host may
// answer claude_code, whose `--disallowedTools` is no bound on a closed native
// roster — and never as the workflow default it did not fall through to. A
// reading that substituted the default would hand a `tools: [read_file]` node
// claw's bound and drop the UNTRUSTED INPUT BOUNDARY paragraph its prompt owes.
func TestAReferenceNothingAnswersStaysTheHostsToDecide(t *testing.T) {
	const unset = "ITERION_TEST_UNSET_BACKEND"
	t.Setenv(unset, "")
	dialled := agentNode("a", []string{"read_file"}, "${"+unset+"}", nil)
	reason := ToolSurfaceWriteReason(dialled, "claw", authoredDefaults)
	if want := "backend ${" + unset + "} " + unresolvedReferenceClause; reason != want {
		t.Errorf("ToolSurfaceWriteReason = %q, want %q: a reference nothing answers is the host's route to decide, never the workflow default's", reason, want)
	}
	// The same spelling with an authored default IS answered, and the node is
	// read on claw's bound — the escape hatch an author has for the cell above.
	dflt := agentNode("a", []string{"read_file"}, "${"+unset+":-claw}", nil)
	if reason := ToolSurfaceWriteReason(dflt, "claude_code", authoredDefaults); reason != "" {
		t.Errorf("ToolSurfaceWriteReason = %q for a `${VAR:-claw}` node, want \"\": the authored default names the route, and claw bounds the node by its `tools:` list", reason)
	}
}

// TestAnAutoNodeReadsTheWorkflowDefaultOnBothQuestions is #2428's own cell,
// stated once in full rather than only counted: a node spelling `backend: auto`
// is routed by the workflow's `default_backend:`, on both readings, because
// that is what dispatch does with it (resolveBackendName step 1 falls through
// on an absent field and on `auto` alike — ir.BackendFallsThrough).
func TestAnAutoNodeReadsTheWorkflowDefaultOnBothQuestions(t *testing.T) {
	readOnly := []string{"read_file", "glob"}
	for _, tc := range []struct {
		dflt      string
		canWrite  string
		admission bool
		why       string
	}{
		{
			dflt:      "claw",
			canWrite:  "",
			admission: false,
			why:       "claw resolves its tool defs from the list, so the declaration is the node's bound and neither question reads a writer",
		},
		{
			dflt:      "claude_code",
			canWrite:  "claude_code " + declarationNoBoundClause("claude_code", readOnly),
			admission: true,
			why:       "claude_code takes the list as --disallowedTools over a roster iterion does not own, so the declaration bounds nothing",
		},
		{
			dflt:      "pi",
			canWrite:  "pi " + declarationNoBoundClause("pi", readOnly),
			admission: true,
			why:       "pi never receives the list at all",
		},
		{
			dflt:      "",
			canWrite:  `backend "auto" resolves at dispatch, where the compiled-in detection order's first choice is claude_code, which does not bound what the node holds (#1652)`,
			admission: false,
			why:       "neither field names the route, so it is left to the host: the boundary class reads the compiled-in detection order's first choice and names the word the author wrote, admission leaves the route unread",
		},
	} {
		t.Run("default_backend "+tc.dflt, func(t *testing.T) {
			auto := agentNode("a", readOnly, "auto", nil)
			if got := ToolSurfaceWriteReason(auto, tc.dflt, authoredDefaults); got != tc.canWrite {
				t.Errorf("ToolSurfaceWriteReason = %q, want %q: %s", got, tc.canWrite, tc.why)
			}
			if got := isMutatingNodeCtx(agentNode("a", readOnly, "auto", nil), tc.dflt, nil, nil, false); got != tc.admission {
				t.Errorf("isMutatingNodeCtx = %v, want %v: %s", got, tc.admission, tc.why)
			}
			// What the two readings above answer for is the route the run
			// takes: dispatch resolves this node exactly there.
			cell := toolSurfaceGridCell{primary: "auto", dflt: tc.dflt, tools: readOnly}
			if got, want := dispatchRoute(cell), strings.TrimSpace(tc.dflt); got != want {
				t.Errorf("dispatch resolves the node onto %q, want %q — the reading above answers for a route no run takes", got, want)
			}
		})
	}
}

// TestAReferenceAnsweredToAutoFallsThroughLikeTheWordItNames holds the fold on
// the ENGINE's rule rather than on the literal word: a `backend:` dialled
// `${VAR:-auto}` resolves to `auto` and falls through exactly as the word does
// (ir.BackendFallsThrough reads a RESOLVED field), so both questions and
// dispatch must answer for it what they answer for `auto`.
//
// It is the shape the catalogue's own idiom family writes
// (`${ITERION_SEC_AUDIT_BACKEND:-claude_code}`), and the frozen grid carries no
// primary that EXPANDS to `auto` — so the cell space certifies the word and
// this test certifies the rule.
//
// The message is pinned too, on the one cell where the reading has a choice:
// with no `default_backend:` to answer, the route left over is the host's
// detection order, and the reason must say so. Quoting the author's reference
// there would blame the unanswered-reference rule the verdict never used — the
// reading DID answer it, at the author's own default.
func TestAReferenceAnsweredToAutoFallsThroughLikeTheWordItNames(t *testing.T) {
	const unset = "ITERION_TEST_UNSET_AUTO_BACKEND"
	t.Setenv(unset, "")
	dialled := "${" + unset + ":-auto}"
	readOnly := []string{"read_file", "glob"}
	for _, tc := range []struct {
		dflt   string
		reason string
		why    string
	}{
		{
			dflt:   "",
			reason: `backend "auto" resolves at dispatch, where the compiled-in detection order's first choice is claude_code, which does not bound what the node holds (#1652)`,
			why:    "neither field names a route, so the host's detection order decides it — and the word the reading resolved is what the message quotes",
		},
		{
			dflt:   "claw",
			reason: "",
			why:    "claw resolves its tool defs from the list, so the declaration is the node's bound",
		},
		{
			dflt:   "claude_code",
			reason: "claude_code " + declarationNoBoundClause("claude_code", readOnly),
			why:    "the workflow default answers the fall-through, and claude_code takes the list as --disallowedTools",
		},
		{
			dflt:   "codex",
			reason: "",
			why:    "codex sandboxes this declared list read-only",
		},
	} {
		t.Run("default_backend "+tc.dflt, func(t *testing.T) {
			if got := ToolSurfaceWriteReason(agentNode("a", readOnly, dialled, nil), tc.dflt, authoredDefaults); got != tc.reason {
				t.Errorf("ToolSurfaceWriteReason = %q, want %q: %s", got, tc.reason, tc.why)
			}
			// Read against the word the reference resolves to, on both
			// questions and at dispatch: the rule is the fold, never the
			// spelling that reaches it.
			word := agentNode("a", readOnly, "auto", nil)
			if got, want := ToolSurfaceWriteReason(agentNode("a", readOnly, dialled, nil), tc.dflt, authoredDefaults),
				ToolSurfaceWriteReason(word, tc.dflt, authoredDefaults); got != want {
				t.Errorf("the boundary class reads %q as %q and the `auto` it resolves to as %q", dialled, got, want)
			}
			if got, want := isMutatingNodeCtx(agentNode("a", readOnly, dialled, nil), tc.dflt, nil, nil, false),
				isMutatingNodeCtx(agentNode("a", readOnly, "auto", nil), tc.dflt, nil, nil, false); got != want {
				t.Errorf("admission reads %q as %v and the `auto` it resolves to as %v", dialled, got, want)
			}
			ref := toolSurfaceGridCell{primary: dialled, dflt: tc.dflt, tools: readOnly}
			if got, want := dispatchRoute(ref), dispatchRoute(toolSurfaceGridCell{primary: "auto", dflt: tc.dflt, tools: readOnly}); got != want {
				t.Errorf("dispatch resolves %q onto %q and the `auto` it resolves to onto %q — the readings above answer for a route no run takes", dialled, got, want)
			}
		})
	}
}
