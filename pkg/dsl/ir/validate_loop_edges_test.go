package ir

import (
	"strings"
	"testing"
)

// C011 (DiagAmbiguousCondition) intentionally refuses the "loop
// back-edge + non-loop edge on the same condition" shape too: the
// runtime iterates edges in source order and returns the first
// matching conditional edge (a loop's back-edge falls through the
// loop-block into the same general conditional check as any other
// edge), so the pair is write-order-dependent — the loop wins when
// written first, the sibling wins when written first. #1386's ask is
// legitimate but requires a runtime enhancement that preserves the
// bots-in-the-field contract (modernize's `lot_gate` ships that
// declaration-order dependency on purpose: `mark_done when converged`
// before `upgrade_campaign when not stop as repair_loop(N)` picks
// mark_done over the loop when both hold). Until that lands, the C011
// refusal is honest; the workaround is a bare / `else` exit that the
// wave-1 fallback-slot rule orders correctly regardless of source
// order.
func TestC011RefusesLoopPlusNonLoopOnSameCondition(t *testing.T) {
	head := `dsl: 2

schema out:
  ok: bool
  was_batch: bool

tool select_candidate:
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

workflow w:
  worktree: none
  sandbox: none
  entry: select_candidate
  select_candidate -> write_audit_md
`
	// The write-order-dependent pair from #1386 stays refused.
	src := head + `  write_audit_md -> select_candidate when not was_batch as package_loop(3)
  write_audit_md -> bucket_families when was_batch
  write_audit_md -> phase2_decider when not was_batch
  phase2_decider -> done
  bucket_families -> done
`
	cr := compileText(t, src)
	found := false
	for _, d := range cr.Diagnostics {
		if d.Code == DiagAmbiguousCondition && strings.Contains(d.Message, "not was_batch") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("C011 does not fire on the write-order-dependent loop+sibling pair — the ticket #1386 pair sneaks through:\n%v", cr.Diagnostics)
	}
	// The recommended workaround — a BARE exhaustion exit — has a
	// different condKey from the loop back-edge, so C011 doesn't fire
	// on it (the two edges are on distinct keys, not the "same
	// condition" pair).
	workaround := head + `  write_audit_md -> select_candidate when not was_batch as package_loop(3)
  write_audit_md -> bucket_families when was_batch
  write_audit_md -> phase2_decider
  phase2_decider -> done
  bucket_families -> done
`
	crOK := compileText(t, workaround)
	for _, d := range crOK.Diagnostics {
		if d.Code == DiagAmbiguousCondition {
			t.Fatalf("C011 fires on the BARE-exit workaround — the check is over-broad:\n%s\n%v", d.Error(), crOK.Diagnostics)
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
