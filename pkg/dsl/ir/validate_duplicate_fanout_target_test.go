package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// C249 — a branch-spawning router that names one target twice.

const dupFanOutPrelude = `
prompt sys:
  System.

prompt usr:
  User.

schema s:
  ok: bool

tool a:
  command: ` + "`echo a`" + `
  output: s

tool b:
  command: ` + "`echo b`" + `
  output: s

tool join:
  command: ` + "`echo join`" + `
  output: s
  await: wait_all
`

// The degenerate shape the diagnostic exists for: fan_out_all spawns one
// goroutine per outgoing edge and derives every branch id from the TARGET, so
// two edges to `a` collapse onto one id, one output slot and one durable
// branch checkpoint.
func TestDuplicateFanOutTarget_Warns(t *testing.T) {
	src := dupFanOutPrelude + `
router fork:
  mode: fan_out_all

workflow test:
  entry: fork
  fork -> a
  fork -> a
  fork -> b
  a -> join
  b -> join
  join -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagDuplicateFanOutTarget)

	var found *Diagnostic
	for i := range r.Diagnostics {
		if r.Diagnostics[i].Code == DiagDuplicateFanOutTarget {
			found = &r.Diagnostics[i]
			break
		}
	}
	if found == nil {
		t.Fatal("no C249 diagnostic to inspect")
	}
	if found.Severity != SeverityWarning {
		t.Errorf("C249 severity = %s, want warning — the shape has always compiled and refusing it would break existing .bot files", found.Severity)
	}
	// The message has to be actionable on its own: which router, which target,
	// how many edges, and the remedy. "2 edges" rather than "2" — the code
	// string C249 contains a 2, so the looser probe would pass on nothing.
	for _, want := range []string{`"fork"`, `"a"`, "2 edges", "fan_out_each"} {
		if !strings.Contains(found.Message, want) {
			t.Errorf("C249 message %q does not mention %q", found.Message, want)
		}
	}
	if found.NodeID != "fork" {
		t.Errorf("C249 NodeID = %q, want %q so the studio can badge the router", found.NodeID, "fork")
	}
	if found.EdgeID != edgeID("fork", "a") {
		t.Errorf("C249 EdgeID = %q, want %q", found.EdgeID, edgeID("fork", "a"))
	}
	// A warning must never become a refusal.
	for _, d := range r.Diagnostics {
		if d.Severity == SeverityError {
			t.Errorf("duplicate fan-out target must still compile, got error %s: %s", d.Code, d.Message)
		}
	}
	if r.Workflow == nil {
		t.Fatal("workflow must still compile — C249 warns, it does not refuse")
	}
}

// One warning per (router, target) pair, not one per surplus edge: three edges
// to the same node are one authoring mistake.
func TestDuplicateFanOutTarget_OneWarningPerTarget(t *testing.T) {
	src := dupFanOutPrelude + `
router fork:
  mode: fan_out_all

workflow test:
  entry: fork
  fork -> a
  fork -> a
  fork -> a
  fork -> b
  a -> join
  b -> join
  join -> done
`
	r := compileFile(t, src)
	count := 0
	var msg string
	for _, d := range r.Diagnostics {
		if d.Code == DiagDuplicateFanOutTarget {
			count++
			msg = d.Message
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 C249 for one duplicated target, got %d", count)
	}
	if !strings.Contains(msg, "3 edges") {
		t.Errorf("C249 message %q does not report the real edge count (3)", msg)
	}
}

// The llm router's multi mode routes through the same planFromEdges /
// launchBranches path, so it collides identically. Its remedy differs: the
// model names a target once, so a second edge to it can never mean "twice".
func TestDuplicateLLMMultiTarget_Warns(t *testing.T) {
	src := dupFanOutPrelude + `
router pick:
  mode: llm
  model: "m"
  system: sys
  multi: true

workflow test:
  entry: pick
  pick -> a
  pick -> a
  pick -> b
  a -> join
  b -> join
  join -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagDuplicateFanOutTarget)
	for _, d := range r.Diagnostics {
		if d.Code != DiagDuplicateFanOutTarget {
			continue
		}
		if strings.Contains(d.Message, "fan_out_each") {
			t.Errorf("llm-multi remedy must not point at fan_out_each (an llm router has no per-item template): %q", d.Message)
		}
		if !strings.Contains(strings.ToLower(d.Message), "remove the duplicate edge") {
			t.Errorf("llm-multi C249 message %q does not name the remedy (remove the duplicate edge)", d.Message)
		}
	}
}

// ---------------------------------------------------------------------------
// Negative cases — the shapes C249 must stay silent on.
// ---------------------------------------------------------------------------

// Several edges to DIFFERENT targets is the whole point of a fan-out.
func TestDuplicateFanOutTarget_DistinctTargetsSilent(t *testing.T) {
	src := dupFanOutPrelude + `
router fork:
  mode: fan_out_all

workflow test:
  entry: fork
  fork -> a
  fork -> b
  a -> join
  b -> join
  join -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagDuplicateFanOutTarget)
}

// round_robin selects ONE edge per visit (counter % len(edges)), so two edges
// to the same node are a 2:1 rotation weight, not a collision.
func TestDuplicateRoundRobinTarget_Silent(t *testing.T) {
	src := dupFanOutPrelude + `
router rr:
  mode: round_robin

workflow test:
  entry: rr
  rr -> a
  rr -> a
  rr -> b
  a -> join
  b -> join
  join -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagDuplicateFanOutTarget)
}

// An llm router WITHOUT multi: selects a single target and continues on the
// trunk — no branch is spawned, so no branch id can collide.
func TestDuplicateLLMSingleTarget_Silent(t *testing.T) {
	src := dupFanOutPrelude + `
router pick:
  mode: llm
  model: "m"
  system: sys

workflow test:
  entry: pick
  pick -> a
  pick -> a
  pick -> b
  a -> join
  b -> join
  join -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagDuplicateFanOutTarget)
}

// fan_out_each derives its branch ids from the ITEM INDEX
// (branch_<router>_<i>), which is exactly why it is the remedy C249 points at.
// Its own C115 already forbids a second outgoing edge, so the duplicate shape
// cannot even be expressed — assert C249 adds no second complaint.
func TestFanOutEachSingleTemplateEdge_Silent(t *testing.T) {
	src := dupFanOutPrelude + `
router fork:
  mode: fan_out_each
  over: "{{outputs.a.ok}}"
  as: item

workflow test:
  entry: a
  a -> fork
  fork -> b
  b -> join
  join -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagDuplicateFanOutTarget)
}

// A condition router picks one matching edge; duplicates there are C010/C011
// territory (ambiguous routing), never a branch-id collision.
func TestDuplicateConditionTarget_Silent(t *testing.T) {
	src := dupFanOutPrelude + `
router pick:
  mode: condition

workflow test:
  entry: a
  a -> pick
  pick -> b when ok
  pick -> b else
  b -> join
  join -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagDuplicateFanOutTarget)
}

// Two DIFFERENT fan-out routers each targeting the same node do not collide:
// the branch id carries the router id, so `branch_fork_a` != `branch_fork2_a`.
func TestSameTargetFromDistinctRouters_Silent(t *testing.T) {
	src := dupFanOutPrelude + `
router fork:
  mode: fan_out_all

router fork2:
  mode: fan_out_all

workflow test:
  entry: fork
  fork -> a
  fork -> fork2
  fork2 -> a
  fork2 -> b
  a -> join
  b -> join
  join -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagDuplicateFanOutTarget)
}

// Canary for the check itself: a workflow assembled directly as IR (bypassing
// the parser) still warns, so the predicate reads the compiled edge set rather
// than any surface-syntax accident.
func TestDuplicateFanOutTarget_FiresOnIRShape(t *testing.T) {
	c := &compiler{file: &ast.File{}}
	w := &Workflow{
		Name:  "ir",
		Entry: "fork",
		Nodes: map[string]Node{
			"fork": &RouterNode{BaseNode: BaseNode{ID: "fork"}, RouterMode: RouterFanOutAll},
			"a":    &AgentNode{BaseNode: BaseNode{ID: "a"}},
		},
		Edges: []*Edge{
			{From: "fork", To: "a"},
			{From: "fork", To: "a"},
		},
	}
	c.validateDuplicateFanOutTargets(w)
	if len(c.diags) != 1 || c.diags[0].Code != DiagDuplicateFanOutTarget {
		t.Fatalf("expected exactly one C249, got %v", c.diags)
	}
}
