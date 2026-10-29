package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
)

// ---------------------------------------------------------------------------
// C321 — a node's backend has no CLI in the published sandbox images (#1643)
// ---------------------------------------------------------------------------

// TestSandboxImagePresenceWarnsBeforeTheRunRefuses: kimi, grok and opencode
// run executables no image iterion publishes carries, and codex is in the
// stock sandbox's pinned tree but outside its PATH — so a sandboxed node on
// any of the four dies at `exec: not found` today, and is refused at start
// once the resolved image is known. The compiler says it first, as a WARNING:
// it knows neither the image nor whether the run will be sandboxed, and an
// operator whose own image bakes the CLI must keep a bot that compiles.
//
// Mutation that reddens it: drop the warnfAt from
// validateSandboxImagePresence; or read the per-family scope instead of the
// merged cell, which turns every codex bot into a warning the compile guard
// TestCompileCodexBackendWithoutDiscouragementWarning refuses.
func TestSandboxImagePresenceWarnsBeforeTheRunRefuses(t *testing.T) {
	src := func(body string) string {
		return "agent a:\n  model: \"m\"\n" + body + "workflow w:\n  entry: a\n  a -> done\n"
	}
	for name, tc := range map[string]struct {
		src  string
		want bool // true = C321 fires
	}{
		"kimi":     {src("  backend: \"kimi\"\n"), true},
		"grok":     {src("  backend: \"grok\"\n"), true},
		"opencode": {src("  backend: \"opencode\"\n"), true},
		// codex is NOT warned here, and that is the family split doing its
		// work: it IS in the runner images, a sandboxed codex node is already
		// refused on its own terms (the launch screen, the delegate's
		// dispatch guard), and TestCompileCodexBackendWithoutDiscouragementWarning
		// holds a codex bot to compiling clean. The sandbox side is read
		// where the resolved image is known — the engine's admission, which
		// refuses it there (TestTheSandboxAndRunnerFamiliesAreJudgedApart).
		"codex, served on the runner side": {src("  backend: \"codex\"\n"), false},
		// The three the published sandbox images serve stay silent.
		"claude_code": {src("  backend: \"claude_code\"\n"), false},
		"claw":        {src("  backend: \"claw\"\n"), false},
		"pi":          {src("  backend: \"pi\"\n"), false},
		// The workflow's own opt-out silences it: the run enters no image.
		"kimi under sandbox: none": {
			"agent a:\n  model: \"m\"\n  backend: \"kimi\"\nworkflow w:\n  entry: a\n  sandbox: none\n  a -> done\n", false,
		},
		// A route the source does not decide is dispatch's to resolve.
		"a dial without a default": {src("  backend: \"${C1643_UNSET}\"\n"), false},
		// …but a dial's DEFAULT decides, as on every other backend screen.
		"a dial defaulting to grok": {src("  backend: \"${C1643_UNSET:-grok}\"\n"), true},
		// A node with no backend of its own inherits default_backend:.
		"the workflow default": {
			"agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  default_backend: \"kimi\"\n  a -> done\n", true,
		},
		// A fallback route is a survival path the run may never take: the
		// launch screen judges those, this warning does not double them.
		"a fallback route alone": {
			"agent a:\n  model: \"m\"\n  backend: \"claude_code\"\n  fallbacks:\n    backup:\n      backend: \"kimi\"\n      model: \"m2\"\nworkflow w:\n  entry: a\n  a -> done\n", false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, tc.src)
			var got *Diagnostic
			for i := range r.Diagnostics {
				if r.Diagnostics[i].Code == DiagBackendAbsentFromSandboxImage {
					got = &r.Diagnostics[i]
				}
			}
			if !tc.want {
				if got != nil {
					t.Fatalf("C321 on a served, opted-out or undecided route: %s", got.Message)
				}
				return
			}
			if got == nil || got.Severity != SeverityWarning {
				t.Fatalf("no C321 warning: %+v\n%v", got, r.Diagnostics)
			}
			if got.NodeID != "a" {
				t.Errorf("C321 attributed to %q, want the node", got.NodeID)
			}
			// What the author must be able to act on: the executable, and
			// both ways out.
			for _, want := range []string{"PATH", "sandbox: none", "image"} {
				if !strings.Contains(got.Message, want) {
					t.Errorf("C321 message %q does not name %q", got.Message, want)
				}
			}
		})
	}
}

// TestTheGapListIsOrderedAndSaysOneThing: Workflow.Nodes is a map, so an
// unsorted walk words the same run's warnings differently on every launch —
// and the compiler and a launch must say the SAME sentence about one gap,
// because the author and the operator read each other's reports.
//
// Five nodes, read twenty times: Go randomises map iteration per range, so an
// unsorted implementation has one chance in 5! of looking sorted once and
// essentially none of surviving twenty reads. A single two-node read would be
// a coin flip — a test that can only redden in one order is flaky, not a
// witness.
func TestTheGapListIsOrderedAndSaysOneThing(t *testing.T) {
	src := "agent zeta:\n  model: \"m\"\n  backend: \"kimi\"\n" +
		"agent alpha:\n  model: \"m\"\n  backend: \"grok\"\n" +
		"agent mu:\n  model: \"m\"\n  backend: \"opencode\"\n" +
		"agent beta:\n  model: \"m\"\n  backend: \"kimi\"\n" +
		"agent omega:\n  model: \"m\"\n  backend: \"grok\"\n" +
		"workflow w:\n  entry: zeta\n  zeta -> alpha\n  alpha -> mu\n  mu -> beta\n  beta -> omega\n  omega -> done\n"
	r := compileText(t, src)
	want := []string{"alpha", "beta", "mu", "omega", "zeta"}
	for i := 0; i < 20; i++ {
		gaps := SandboxImageGaps(r.Workflow, SourceSandboxRoute(r.Workflow))
		if len(gaps) != len(want) {
			t.Fatalf("read %d: gaps = %d, want %d (one per unserved node): %+v", i, len(gaps), len(want), gaps)
		}
		for j, id := range want {
			if gaps[j].NodeID != id {
				t.Fatalf("read %d: gaps[%d] = %q, want %q — the list is not sorted by node id", i, j, gaps[j].NodeID, id)
			}
		}
	}
	gaps := SandboxImageGaps(r.Workflow, SourceSandboxRoute(r.Workflow))
	if gaps[0].Backend != backendcaps.Grok || gaps[0].Binary != "grok" {
		t.Errorf("alpha's gap = %+v, want grok and its executable", gaps[0])
	}
	// Each node's warning carries that node's own sentence, and the sentence
	// the compiler emitted is the one the gap renders.
	for i := range r.Diagnostics {
		d := r.Diagnostics[i]
		if d.Code != DiagBackendAbsentFromSandboxImage {
			continue
		}
		var reason string
		for _, gap := range gaps {
			if gap.NodeID == d.NodeID {
				reason = gap.Reason()
			}
		}
		if reason == "" || !strings.Contains(d.Message, reason) {
			t.Errorf("C321 on %q does not carry the gap's own sentence: %q", d.NodeID, d.Message)
		}
	}
}

// TestNoGapIsReportedForANameNobodyMeasured: the refusal and the warning both
// read a MEASURED absence. A custom registration's image is the operator's
// too, and reading "not in the registry" as "not in the image" would warn
// about every backend an operator adds.
func TestNoGapIsReportedForANameNobodyMeasured(t *testing.T) {
	w := &Workflow{
		Name:  "w",
		Nodes: map[string]Node{"a": &AgentNode{BaseNode: BaseNode{ID: "a"}, LLMFields: LLMFields{Backend: "my_own_cli"}}},
	}
	if gaps := SandboxImageGaps(w, SourceSandboxRoute(w)); len(gaps) != 0 {
		t.Fatalf("gaps = %+v on an unregistered backend, want none", gaps)
	}
	if backendcaps.UnservedByPublishedImages("my_own_cli") {
		t.Fatal("an unregistered name reads as measured-absent: the negation of a silence is not a measurement")
	}
}
