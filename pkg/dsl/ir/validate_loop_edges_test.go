package ir

import (
	"os"
	"strings"
	"testing"
)

// The guarded loop-exhaustion exit: a bounded-iteration back-edge
// (`as name(N)`, `as foreach …`) and a non-iteration sibling may share one
// condition when the back-edge is DECLARED FIRST. That is the order
// `evaluateEdgesWithLoopsRS` walks, and it returns the first matching
// conditional edge — so the back-edge wins while the iteration has work and
// the sibling fires once the iteration is declined. Declared the other way
// round the sibling matches first and the iteration edge is never taken,
// which C011 refuses while naming the order that works. The permission is
// that one pair: two non-iteration edges on one condition stay ambiguous,
// two iteration edges stay ambiguous, a third edge on the condition is
// shadowed by the sibling that precedes it, and the sibling has to LEAVE the
// iteration — an exit the body leads back to this node from re-enters the
// body without crossing the back-edge, so the cap stops bounding it, in
// either declaration order.
func TestC011AndTheGuardedLoopExit(t *testing.T) {
	head := `dsl: 2

schema out:
  ok: bool
  was_batch: bool
  items: string[]

tool select_candidate:
  command: ` + "`echo hi`" + `
  output: out

tool stage_work:
  command: ` + "`echo hi`" + `
  output: out

tool write_audit_md:
  command: ` + "`echo hi`" + `
  output: out

tool bucket_families:
  command: ` + "`echo hi`" + `
  output: out

tool phase2_decider:
  command: ` + "`echo hi`" + `
  output: out

tool late_exit:
  command: ` + "`echo hi`" + `
  output: out

workflow w:
  worktree: none
  sandbox: none
  entry: select_candidate
  select_candidate -> stage_work
  stage_work -> write_audit_md
`
	const (
		// Every node stays reachable whichever edges the case declares.
		tail = "  bucket_families -> late_exit\n  late_exit -> phase2_decider\n  phase2_decider -> done\n"

		backEdge = "  write_audit_md -> select_candidate when not was_batch as package_loop(3)\n"
		// A real back-edge, over a collection that resolves: the shape the
		// permission names for `as foreach …`, not a forward edge wearing a
		// foreach it could never iterate.
		foreachBackEdge = "  write_audit_md -> select_candidate when not was_batch as foreach scan(item in \"{{outputs.select_candidate.items}}\")\n"
		otherIteration  = "  write_audit_md -> late_exit when not was_batch as other_loop(2)\n"
		otherPolarity   = "  write_audit_md -> bucket_families when was_batch\n"
		guardedExit     = "  write_audit_md -> phase2_decider when not was_batch\n"
		bareExit        = "  write_audit_md -> phase2_decider\n"
		secondExit      = "  write_audit_md -> late_exit when not was_batch\n"
		// Not exits: both land where the body leads back here from — the
		// back-edge's own target, and a node between it and this one.
		reentryExit      = "  write_audit_md -> select_candidate when not was_batch\n"
		bodyInteriorExit = "  write_audit_md -> stage_work when not was_batch\n"
		// A way back that is itself capped: the walk looking for a route
		// back here steps over bounded iteration edges, their own cap
		// bounding whatever cycle crosses them.
		cappedWayBack = "  phase2_decider -> write_audit_md as recheck(2)\n"
	)
	for _, tc := range []struct {
		name    string
		edges   string
		want    int      // expected number of C011 diagnostics
		must    []string // substrings the C011 message must carry
		mustNot []string // substrings it must not carry
	}{
		{
			name:  "a back-edge declared first permits the exit it guards",
			edges: backEdge + otherPolarity + guardedExit,
		},
		{
			name:  "a foreach back-edge declared first permits it too",
			edges: foreachBackEdge + otherPolarity + guardedExit,
		},
		{
			name:  "the bare exhaustion exit is on another condition entirely",
			edges: backEdge + otherPolarity + bareExit,
		},
		{
			// The only route from this exit back here crosses a second
			// iteration edge, and that one's cap bounds it: an exit all the
			// same.
			name:  "an exit whose way back is itself capped is an exit",
			edges: backEdge + otherPolarity + guardedExit + cappedWayBack,
		},
		{
			name:  "the exit declared first shadows the back-edge",
			edges: guardedExit + otherPolarity + backEdge,
			want:  1,
			must: []string{
				"write_audit_md->phase2_decider",
				"write_audit_md->select_candidate",
				"not was_batch",
				"is declared first",
				"declare the iteration back-edge before the exit it guards",
			},
		},
		{
			name:  "two iteration edges on one condition stay ambiguous",
			edges: backEdge + otherPolarity + otherIteration,
			want:  1,
			must:  []string{"write_audit_md->select_candidate", "write_audit_md->late_exit"},
		},
		{
			name:  "an exit back to the node the back-edge re-enters is no exit",
			edges: backEdge + otherPolarity + reentryExit,
			want:  1,
			must: []string{
				"write_audit_md->select_candidate",
				"not was_batch",
				`leads back to "write_audit_md"`,
				"must leave the iteration",
			},
		},
		{
			// One node further in: the exit crosses into the body, which
			// carries it back here with the cap untouched.
			name:  "an exit into the body is no exit either",
			edges: backEdge + otherPolarity + bodyInteriorExit,
			want:  1,
			must: []string{
				"write_audit_md->stage_work",
				`leads back to "write_audit_md"`,
				"must leave the iteration",
			},
		},
		{
			// Declaring it first does not repair it, so the refusal must not
			// send the author to swap the order: the move is to route the
			// exit out of the iteration.
			name:  "a re-entering exit declared first is refused as a re-entry, not as an order",
			edges: reentryExit + otherPolarity + backEdge,
			want:  1,
			must: []string{
				`leads back to "write_audit_md"`,
				"must leave the iteration",
			},
			mustNot: []string{"declare the iteration back-edge before"},
		},
		{
			name:  "a foreach back-edge is re-entered no more than a loop's",
			edges: foreachBackEdge + otherPolarity + reentryExit,
			want:  1,
			must:  []string{`leads back to "write_audit_md"`},
		},
		{
			name:  "a second exit is shadowed by the first, not by the back-edge",
			edges: backEdge + otherPolarity + guardedExit + secondExit,
			want:  1,
			must:  []string{"write_audit_md->phase2_decider", "write_audit_md->late_exit"},
			// The back-edge is not a party to this ambiguity: the exit
			// before it is what makes the second exit dead.
			mustNot: []string{"write_audit_md->select_candidate"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr := compileText(t, head+tc.edges+tail)
			var got []Diagnostic
			for _, d := range cr.Diagnostics {
				if d.Code == DiagAmbiguousCondition {
					got = append(got, d)
				}
			}
			if len(got) != tc.want {
				t.Fatalf("got %d C011 diagnostic(s), want %d:\n%v", len(got), tc.want, cr.Diagnostics)
			}
			if tc.want == 0 {
				for _, d := range cr.Diagnostics {
					if d.Severity == SeverityError {
						t.Fatalf("the permitted shape does not compile clean: %s", d.Error())
					}
				}
				return
			}
			for _, want := range tc.must {
				if !strings.Contains(got[0].Message, want) {
					t.Errorf("the C011 message does not carry %q: %s", want, got[0].Message)
				}
			}
			for _, unwanted := range tc.mustNot {
				if strings.Contains(got[0].Message, unwanted) {
					t.Errorf("the C011 message carries %q, which does not hold for this pair: %s", unwanted, got[0].Message)
				}
			}
		})
	}
}

// Declaration order is not the file's layout: a `use`d group's edges are
// appended after the workflow's own, so a back-edge held by a group is
// declared last however high in the file it sits — and the exit a workflow
// declares for it can never precede it. The refusal is right (the evaluator
// would take that exit first), which is why its message and docs/dsl.md
// carry the move that is always available: the bare exit, which no order
// decides.
func TestAGroupHeldBackEdgeIsDeclaredAfterTheWorkflowsOwnEdges(t *testing.T) {
	head := `dsl: 2

schema out:
  ok: bool

group loopy():
  tool src:
    command: ` + "`echo hi`" + `
    output: out
  tool body:
    command: ` + "`echo hi`" + `
    output: out
  body -> src
  src -> body when not ok as spin(3)

tool entry_node:
  command: ` + "`echo hi`" + `
  output: out

tool give_up:
  command: ` + "`echo hi`" + `
  output: out

tool go_on:
  command: ` + "`echo hi`" + `
  output: out

use loopy as g with { }

workflow w:
  worktree: none
  sandbox: none
  entry: entry_node
  entry_node -> g.src
  g.src -> go_on when ok
  give_up -> done
  go_on -> done
`
	t.Run("the group's back-edge comes after the workflow's own edges", func(t *testing.T) {
		cr := compileText(t, head+"  g.src -> give_up when not ok\n")
		exit, backEdge := -1, -1
		for i, e := range cr.Workflow.Edges {
			if e.From == "g.src" && e.To == "give_up" {
				exit = i
			}
			if e.From == "g.src" && e.To == "g.body" {
				backEdge = i
			}
		}
		if exit < 0 || backEdge < 0 {
			t.Fatalf("the fixture lost an edge: exit=%d back-edge=%d", exit, backEdge)
		}
		if backEdge < exit {
			t.Fatalf("the group's back-edge is at %d, before the workflow's exit at %d — the corner this case documents is gone", backEdge, exit)
		}
		var got []Diagnostic
		for _, d := range cr.Diagnostics {
			if d.Code == DiagAmbiguousCondition {
				got = append(got, d)
			}
		}
		if len(got) != 1 {
			t.Fatalf("got %d C011 diagnostic(s), want 1:\n%v", len(got), cr.Diagnostics)
		}
		if !strings.Contains(got[0].Message, "write the exit as a bare edge") {
			t.Errorf("the refusal names no move the author of a group-held back-edge can make: %s", got[0].Message)
		}
	})
	t.Run("the bare exit is that move", func(t *testing.T) {
		cr := compileText(t, head+"  g.src -> give_up\n")
		for _, d := range cr.Diagnostics {
			if d.Severity == SeverityError {
				t.Fatalf("the bare exit the refusal recommends does not compile: %s", d.Error())
			}
		}
	})
}

// A guarded exit does not replace the node's coverage of the opposite
// outcome: with no sibling on the other polarity and no fallback, nothing
// matches when the condition is false and C012 still refuses the node.
func TestAGuardedLoopExitDoesNotReplaceTheFallback(t *testing.T) {
	src := `dsl: 2

schema out:
  ok: bool

tool check:
  command: ` + "`echo hi`" + `
  output: out

tool work:
  command: ` + "`echo hi`" + `
  output: out

tool give_up:
  command: ` + "`echo hi`" + `
  output: out

workflow w:
  worktree: none
  sandbox: none
  entry: check
  check -> work
  work -> check when not ok as retry(3)
  work -> give_up when not ok
  give_up -> done
`
	cr := compileText(t, src)
	if !hasCode(cr.Diagnostics, DiagMissingFallback) {
		t.Fatalf("C012 does not fire on a node whose only outcomes are `not ok` twice:\n%v", cr.Diagnostics)
	}
}

// This catalogue is what renders C011's remedy: the `fix:` line of `iterion
// validate`, the MCP result, and the studio badge — which shows the
// diagnostic's own hint, filled from here by the compiler's `emit` for every
// site that passes none. The reference table carries the second copy, and the
// studio's hint map (studio/src/lib/diagnosticHints.ts, held by its vitest)
// the badge's title plus the hint a diagnostic arriving without one falls back
// to. A remedy that only says "remove the duplicate" sends the author whose
// exit sits above its back-edge to delete the exit instead of moving it.
func TestTheC011RemedyNamesTheOrderThatWorks(t *testing.T) {
	hint := HintFor(DiagAmbiguousCondition)
	if hint == "" {
		t.Fatal("C011 must carry a fix line")
	}
	// Both halves of the permission, or an author follows it into the
	// refusal the other half names.
	for _, want := range []string{"back-edge", "declared first", "leaves the iteration"} {
		if !strings.Contains(hint, want) {
			t.Errorf("the C011 catalogue remedy does not name %q: %s", want, hint)
		}
	}
	data, err := os.ReadFile("../../../docs/references/diagnostics.md")
	if err != nil {
		t.Fatalf("read the diagnostic reference: %v", err)
	}
	var row string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "| **C011**") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatal("the diagnostic reference has no C011 row")
	}
	if !strings.Contains(row, "declared first") {
		t.Errorf("the C011 reference row does not name the order that works: %s", row)
	}
	if !strings.Contains(row, "leaves the iteration") {
		t.Errorf("the C011 reference row carries half the permission, nothing on the exit having to leave the iteration: %s", row)
	}
	// The remedy lives in the row's LAST cell, the *Fix* column — the one an
	// author blocked by C011 reads. A row-wide check passes on the phrase the
	// *Cause* column supplies and leaves that column unheld.
	cells := strings.Split(row, "|")
	fix := ""
	for i := len(cells) - 1; i >= 0; i-- {
		if strings.TrimSpace(cells[i]) != "" {
			fix = cells[i]
			break
		}
	}
	for _, want := range []string{"declare the back-edge first", "route the exit out of the iteration"} {
		if !strings.Contains(fix, want) {
			t.Errorf("the C011 row's Fix column does not name %q: %q", want, fix)
		}
	}
}

// C309 refuses a loop cap on a `round_robin` or `llm` router's
// outgoing edge — the runtime never reads it. Mutation toward the
// forbidden alternative: put the loop on the edge that re-ENTERS the
// router (a normal downstream edge back into the router), where the
// runtime DOES enforce the cap — C309 must disappear.
func TestC309LoopCapOnRoundRobinRouterEdge(t *testing.T) {
	// Bad shape: the loop rides the router's OUTGOING edges.
	bad := `dsl: 2

schema out:
  ok: bool

tool worker_a:
  command: ` + "`echo hi`" + `
  output: out

tool worker_b:
  command: ` + "`echo hi`" + `
  output: out

router pick:
  mode: round_robin

workflow w:
  worktree: none
  sandbox: none
  entry: pick
  pick -> worker_a as spin(1)
  pick -> worker_b as other(1)
  worker_a -> done
  worker_b -> done
`
	cr := compileText(t, bad)
	seen := 0
	for _, d := range cr.Diagnostics {
		if d.Code == DiagLoopOnRouterEdge {
			seen++
		}
	}
	if seen != 2 {
		t.Fatalf("C309 must fire on each of the two loop-bearing outgoing edges (got %d):\n%v", seen, cr.Diagnostics)
	}
	// Mutation: put the loop on the edge that re-enters the router.
	// `worker_a -> pick as spin(2)` is the runtime-recognised bound
	// shape; the router's outgoing edges become plain.
	ok := `dsl: 2

schema out:
  ok: bool

tool worker_a:
  command: ` + "`echo hi`" + `
  output: out

tool worker_b:
  command: ` + "`echo hi`" + `
  output: out

router pick:
  mode: round_robin

workflow w:
  worktree: none
  sandbox: none
  entry: pick
  pick -> worker_a
  pick -> worker_b
  worker_a -> pick as spin(2)
  worker_b -> done
`
	crOK := compileText(t, ok)
	for _, d := range crOK.Diagnostics {
		if d.Code == DiagLoopOnRouterEdge {
			t.Fatalf("C309 fires on a re-entry loop — the check is over-broad:\n%s\n%v", d.Error(), crOK.Diagnostics)
		}
	}
}

// C309 also fires on an `llm` router's outgoing edge. Same rule; the
// llm executor picks its target from `selected_route` and never
// reads a cap on the edge.
func TestC309LoopCapOnLLMRouterEdge(t *testing.T) {
	bad := `dsl: 2

schema pout:
  ok: bool

tool worker_a:
  command: ` + "`echo hi`" + `
  output: pout

tool worker_b:
  command: ` + "`echo hi`" + `
  output: pout

router pick:
  mode: llm
  model: "m"

workflow w:
  worktree: none
  sandbox: none
  entry: pick
  pick -> worker_a as spin(1)
  pick -> worker_b
  worker_a -> done
  worker_b -> done
`
	cr := compileText(t, bad)
	found := false
	for _, d := range cr.Diagnostics {
		if d.Code == DiagLoopOnRouterEdge && strings.Contains(d.Message, "llm router") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("C309 did not fire on the llm router's loop-bearing edge:\n%v", cr.Diagnostics)
	}
}

// C309 stays silent on an `llm` router with `multi: true` — that shape
// is C244's rejection (a loop-bearing edge crossing a fan-out
// boundary), and firing both diagnostics on the same edge is noise the
// classifier does not need. Mutation toward the forbidden alternative:
// remove `multi: true` and C309 fires again.
func TestC309StaysSilentOnLLMMulti(t *testing.T) {
	multiBad := `dsl: 2

schema pout:
  ok: bool

tool worker_a:
  command: ` + "`echo hi`" + `
  output: pout

tool worker_b:
  command: ` + "`echo hi`" + `
  output: pout

router pick:
  mode: llm
  model: "m"
  multi: true

workflow w:
  worktree: none
  sandbox: none
  entry: pick
  pick -> worker_a as spin(1)
  pick -> worker_b
  worker_a -> done
  worker_b -> done
`
	cr := compileText(t, multiBad)
	for _, d := range cr.Diagnostics {
		if d.Code == DiagLoopOnRouterEdge {
			t.Fatalf("C309 fires on an `llm multi: true` router — C244 owns that shape, C309 must be silent:\n%s\n%v", d.Error(), cr.Diagnostics)
		}
	}
	// The multi shape is not LEFT silent: C244 must be the one firing.
	c244 := false
	for _, d := range cr.Diagnostics {
		if d.Code == DiagLoopInExecBranch {
			c244 = true
			break
		}
	}
	if !c244 {
		t.Fatalf("C244 did not fire on the llm-multi shape C309 skips — the edge would be silent altogether:\n%v", cr.Diagnostics)
	}
	// Mutation: same graph without multi — C309 must fire.
	singleBad := strings.Replace(multiBad, "  multi: true\n", "", 1)
	cr2 := compileText(t, singleBad)
	found := false
	for _, d := range cr2.Diagnostics {
		if d.Code == DiagLoopOnRouterEdge {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("C309 did not fire once `multi: true` was removed — the guard is over-broad:\n%v", cr2.Diagnostics)
	}
}

// The foreach form is the loop cap's sibling on the same blind edge: the
// router's own selector (`execRoundRobin`'s alternation, `execLLMRouter`'s
// pick) is the only reader of neither, so a `foreach` on the router's
// outgoing edge would iterate nothing — the body runs once. C244 owns the
// llm-multi shape for foreach too (IsBoundedIteration covers it), so the
// skip carries over unchanged.
func TestC309FiresOnAForeachOnARouterEdge(t *testing.T) {
	for name, mode := range map[string]string{
		"round_robin": "round_robin",
		"llm single":  "llm",
	} {
		t.Run(name, func(t *testing.T) {
			src := `dsl: 2

schema pout:
  ok: bool

tool worker_a:
  command: ` + "`echo hi`" + `
  output: pout

tool worker_b:
  command: ` + "`echo hi`" + `
  output: pout

router pick:
  mode: ` + mode + `
  model: "m"

workflow w:
  worktree: none
  sandbox: none
  entry: pick
  pick -> worker_a as foreach spin(item in "['x','y']")
  pick -> worker_b
  worker_a -> done
  worker_b -> done
`
			cr := compileText(t, src)
			for _, d := range cr.Diagnostics {
				if d.Code == DiagLoopOnRouterEdge {
					return
				}
			}
			t.Fatalf("C309 did not fire on a foreach carried by a %s router's outgoing edge:\n%v", mode, cr.Diagnostics)
		})
	}
}
