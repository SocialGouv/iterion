package model

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"context"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestTheExecutorResolvesCodemodeByPrecedence pins the ladder the engine
// walks, on the rungs that disagree. The node's own value wins, "" inherits,
// and an explicit `off` under a workflow `on` is a DECISION rather than an
// absence — which is the whole reason the attribute is a string.
func TestTheExecutorResolvesCodemodeByPrecedence(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, node, workflow string
		wantOn               bool
		wantSource           string
	}{
		{"nobody declares", "", "", false, ir.CodemodeFromDefault},
		{"the node alone", "on", "", true, ir.CodemodeFromNode},
		{"the workflow alone", "", "on", true, ir.CodemodeFromWorkflow},
		{"the node beats the workflow", "off", "on", false, ir.CodemodeFromNode},
		{"the node turns it on over an off workflow", "on", "off", true, ir.CodemodeFromNode},
		{"an off workflow is still a declaration", "", "off", false, ir.CodemodeFromWorkflow},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := &ClawExecutor{wfCodemode: c.workflow}
			got, err := e.codemodePolicy(backendFields{id: "n", codemode: c.node}, backendcaps.Claw)
			if err != nil {
				t.Fatalf("on the backend that serves it: %v", err)
			}
			if got.On != c.wantOn || got.Source != c.wantSource {
				t.Errorf("node=%q workflow=%q resolved to %+v, want on=%v source=%q",
					c.node, c.workflow, got, c.wantOn, c.wantSource)
			}
		})
	}
}

// TestTheExecutorRefusesCodemodeOnABackendThatCannotServeIt holds the half of
// the refusal the compiler cannot reach: a node whose route the source does
// not decide has no backend at compile time, and this is the first place the
// resolved one exists. Without it, such a node would run with the author's
// `codemode: on` silently dropped — the model keeps its whole loop in
// context, the bill is the one codemode exists to avoid, and nothing says so.
//
// The error is TYPED so a reader can tell a missing capability from a
// transport failure — NOT so a fallback routes past it, which an adversarial
// round refuted and TestACodemodeRefusalIsTerminal now pins. The message is
// therefore the whole of what the operator gets, and has to carry the way out
// and not only the verdict.
func TestTheExecutorRefusesCodemodeOnABackendThatCannotServeIt(t *testing.T) {
	t.Parallel()
	e := &ClawExecutor{}
	_, err := e.codemodePolicy(backendFields{id: "n", codemode: "on"}, backendcaps.Codex)
	var refusal *delegate.ErrCapabilityUnsupported
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a *delegate.ErrCapabilityUnsupported", err)
	}
	if refusal.NodeID != "n" || refusal.Backend != backendcaps.Codex {
		t.Errorf("the refusal names node=%q backend=%q, want n/codex", refusal.NodeID, refusal.Backend)
	}
	for _, want := range []string{"codemode: on (from node)", backendcaps.Claw} {
		if !strings.Contains(refusal.Error(), want) {
			t.Errorf("the refusal does not carry %q: %s", want, refusal.Error())
		}
	}
}

// TestTheExecutorRefusesOnlyWhatWasDeclared is the guard against the refusal
// that would break every fielded bot: the DEFAULT is off, and off is not
// asked for. A resolver that refused "codemode is not served here" without
// first asking whether anyone wanted it would fail every node of the fleet on
// codex the moment this cell existed.
func TestTheExecutorRefusesOnlyWhatWasDeclared(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, node, workflow string
	}{
		{"nobody declared anything", "", ""},
		{"the node declared off", "off", ""},
		{"the node declared off against an on workflow", "off", "on"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := &ClawExecutor{wfCodemode: c.workflow}
			if _, err := e.codemodePolicy(backendFields{id: "n", codemode: c.node}, backendcaps.Codex); err != nil {
				t.Errorf("codex refused a node that asked for nothing: %v", err)
			}
		})
	}
}

// TestTheExecutorAndTheCompilerRefuseTheSameBackends is the guarantee test
// behind the two above, and it reads the registry rather than a list: the two
// halves of this refusal are written in different packages, and a compiler
// that accepts what the engine refuses is the defect that costs an operator a
// green `task dsl:check` and a failed run.
//
// It is what catches either side drifting to its own idea of which backends
// serve codemode — and an earlier version of it could not, because it asked
// the ENGINE's predicate on both sides of the comparison. Two spellings of
// one expression prove nothing; a second adversarial round found it by
// stubbing the compiler's predicate to refuse nothing and watching this test
// pass.
//
// The agreement it asserts is a property of the registry's CONTENTS, not of
// the two predicates: they answer differently for every verdict that is
// neither shipped nor typed-refused. The second loop below RESTATES that as
// a direct assertion on the data — it is subsumed by the first one once the
// first asks the right question, and a closing round proved it is (every
// third verdict reddens loop 1 alone). It is kept because it asserts on the
// cell rather than through a derived comparison, so it still speaks if loop
// 1 ever regresses again, and because its message can name the decision an
// editor owes. It is not a second detector, and an earlier version of this
// comment claimed it was — on an observation made against the tautological
// loop 1 it replaced.
//
// Neither loop catches a cell wrongly set to `shipped`, which is the silent
// direction: TestTheShippedCellsAreExactlyTheLoopsThatInstallTheTool does.
func TestTheExecutorAndTheCompilerRefuseTheSameBackends(t *testing.T) {
	t.Parallel()
	var served, refused int
	for _, backend := range backendcaps.Names() {
		e := &ClawExecutor{}
		_, err := e.codemodePolicy(backendFields{id: "n", codemode: "on"}, backend)
		engineRefuses := err != nil
		compilerRefuses := ir.CodemodeRefusedBy(backend)
		if engineRefuses != compilerRefuses {
			t.Errorf("backend %q (cell %q): the engine refuses=%v, the compiler refuses=%v",
				backend, backendcaps.Verdict(backend, backendcaps.Codemode), engineRefuses, compilerRefuses)
		}
		if engineRefuses {
			refused++
		} else {
			served++
		}
	}
	if served == 0 || refused == 0 {
		t.Fatalf("the registry gave %d serving and %d refusing backend(s); this test needs both arms", served, refused)
	}

	// What makes the agreement above hold, stated as the gate it is: the two
	// predicates are `== shipped` and `== typed-refused`, so any third
	// verdict on a codemode cell splits them. A registry edit that reaches
	// for `partial`, `inert`, `unknown` or an `unwired` verdict here owes
	// this test a decision — which half it joins — rather than a golden
	// regeneration.
	for _, backend := range backendcaps.Names() {
		switch v := backendcaps.Verdict(backend, backendcaps.Codemode); v {
		case backendcaps.Shipped, backendcaps.TypedRefused:
		default:
			t.Errorf("backend %q carries codemode verdict %q: the compiler reads only `typed-refused` as a refusal and the engine only `shipped` as a grant, so this cell compiles clean and dies at dispatch. Decide which half it joins — `shipped` only if this backend's generation loop calls installCodemode, `typed-refused` otherwise", backend, v)
		}
	}
}

// TestACodemodeRefusalIsTerminal pins what an adversarial round refuted: this
// refusal does NOT route past a `fallbacks:` chain, in either direction.
//
// The comment it replaces claimed the opposite, and the claim was load-bearing
// — it was the reason a typed refusal was argued to be acceptable. A sentence
// that decides a doctrine and has never been executed is the finding of the
// next round, so it is executed here.
//
// Both arms matter and they fail for DIFFERENT structural reasons: a refused
// PRIMARY never enters the chain at all (its task is built outside it), and a
// refused FALLBACK ends the chain rather than being skipped — which is
// exactly what the twin this code mirrors asserts in
// TestAmbientContextRefusesAFallbackThatCannotHonourIt.
func TestACodemodeRefusalIsTerminal(t *testing.T) {
	t.Parallel()

	t.Run("a refused primary never reaches its fallback", func(t *testing.T) {
		t.Parallel()
		primary, fallback := &ambientRecorder{}, &ambientRecorder{}
		reg := delegate.NewRegistry()
		reg.Register(backendcaps.Codex, primary)
		reg.Register(backendcaps.Claw, fallback)
		exec := newFallbackExecutor(reg, EventHooks{})
		node := fallbackAgentNode("reviewer", backendcaps.Codex, "")
		node.Codemode = "on"
		node.Fallbacks = []ir.Fallback{{Name: "backup", Backend: backendcaps.Claw, Model: "m", On: []string{"any"}}}

		_, err := exec.Execute(context.Background(), node, nil)
		var refused *delegate.ErrCapabilityUnsupported
		if !errors.As(err, &refused) || refused.Backend != backendcaps.Codex {
			t.Fatalf("err = %v, want the codex route refused", err)
		}
		if len(primary.tasks) != 0 || len(fallback.tasks) != 0 {
			t.Fatalf("dispatches: primary=%d fallback=%d, want 0 and 0 — the node is refused, not routed",
				len(primary.tasks), len(fallback.tasks))
		}
	})

	t.Run("a refused fallback ends the chain", func(t *testing.T) {
		t.Parallel()
		head := &ambientRecorder{fail: &delegate.ErrTransient{Reason: "unavailable"}}
		tail := &ambientRecorder{}
		reg := delegate.NewRegistry()
		reg.Register(backendcaps.Claw, head)
		reg.Register(backendcaps.Codex, tail)
		exec := newFallbackExecutor(reg, EventHooks{})
		exec.retry.MaxAttempts = 1
		// A model, because a claw node without one asks the DETECTOR for a
		// spec and this harness has none.
		node := fallbackAgentNodeModel("reviewer", backendcaps.Claw, "", "anthropic/m")
		node.Codemode = "on"
		node.Fallbacks = []ir.Fallback{{Name: "backup", Backend: backendcaps.Codex, Model: "m", On: []string{"any"}}}

		_, err := exec.Execute(context.Background(), node, nil)
		var refused *delegate.ErrCapabilityUnsupported
		if !errors.As(err, &refused) || refused.Backend != backendcaps.Codex {
			t.Fatalf("err = %v, want the codex fallback refused", err)
		}
		if len(head.tasks) == 0 {
			t.Fatalf("the claw primary never ran: it serves codemode and must be tried")
		}
		if len(tail.tasks) != 0 {
			t.Fatalf("the refused fallback dispatched %d time(s)", len(tail.tasks))
		}
		// The claw primary DID get the capability: the refusal is the
		// fallback's, not a blanket one.
		if !head.tasks[0].Codemode {
			t.Errorf("the claw task did not carry Codemode")
		}
	})
}

// TestTheStaticExecutorCarriesTheWorkflowRungToo holds the SECOND reader of
// one fact. `NewProgramExecutor` builds the executor that pre-run analyses
// use — `iterion validate --exec`, pkg/bundlelint, pkg/dryrun — and it seeds
// the workflow's fields one by one. An adversarial round found `wfCodemode`
// missing from that list: the two readings of the same `.bot` disagreed, the
// static one answering "off (from default)" for a workflow that says on.
//
// The assertion is the AGREEMENT, not the value. Said exactly: `wfCodemode`
// has NO static consumer today — `codemodePolicy` is reached only from
// buildTask, which a staticProgramOnly executor never runs, and neither
// pkg/bundlelint nor pkg/dryrun reads codemode at all. A second adversarial
// round checked that and confirmed the seeding changes no verdict any tool
// prints, so nothing that passed before fails now. The field is seeded ahead
// of its first reader because a constructor that seeds every workflow rung
// but one is how the two readings of a `.bot` start disagreeing; this test
// calls the policy directly to hold that, which is a site no production
// caller takes.
func TestTheStaticExecutorCarriesTheWorkflowRungToo(t *testing.T) {
	t.Parallel()
	wf := &ir.Workflow{Name: "w", Codemode: "on"}
	fields := backendFields{id: "n"}

	static := NewProgramExecutor(wf)
	engine := &ClawExecutor{wfCodemode: wf.Codemode}

	gotStatic, staticErr := static.codemodePolicy(fields, backendcaps.Claw)
	gotEngine, engineErr := engine.codemodePolicy(fields, backendcaps.Claw)
	if gotStatic != gotEngine || (staticErr == nil) != (engineErr == nil) {
		t.Errorf("the static reading says %+v (err=%v) and the engine says %+v (err=%v) for the same workflow",
			gotStatic, staticErr, gotEngine, engineErr)
	}
	if !gotStatic.On || gotStatic.Source != ir.CodemodeFromWorkflow {
		t.Errorf("static reading = %+v, want on from the workflow rung", gotStatic)
	}

	// And the refusal it would produce agrees too, on a backend that cannot
	// serve it: a pre-run analysis that misses the refusal is the shape that
	// lets an operator launch a node the engine then stops.
	if _, err := static.codemodePolicy(fields, backendcaps.Codex); err == nil {
		t.Error("the static reading does not refuse codex, so a pre-run analysis would miss the refusal the dispatch makes")
	}
}

// TestTheEngineAsksThePositiveQuestion kills a mutant that survived the
// bench: swapping the executor's `ir.CodemodeServedBy` for the COMPILER's
// `!ir.CodemodeRefusedBy` changed nothing any test could see.
//
// The two are not each other's negation, and the difference is this class
// alone: a backend name the registry does not carry — an operator's own,
// which delegate.Registry deliberately allows. The compiler must stay silent
// about it (the registry publishes "unknown names acquire neither
// capabilities nor compiler refusals"); the engine must refuse it, because
// the only loop that installs the tool is the one whose cell says shipped,
// and granting here hands a node a Codemode nothing will ever read.
//
// The ir-side test of the same class cannot see this: it exercises the two
// predicates, not which one the executor calls.
func TestTheEngineAsksThePositiveQuestion(t *testing.T) {
	t.Parallel()
	const unknown = "an_operators_own_backend"
	// The premise, executed rather than assumed: this name is in neither
	// camp of the registry. A name that WAS typed-refused would make the two
	// predicates agree and the test blind.
	if ir.CodemodeServedBy(unknown) || ir.CodemodeRefusedBy(unknown) {
		t.Fatalf("%q is not the unregistered name this test needs (served=%v refused=%v)",
			unknown, ir.CodemodeServedBy(unknown), ir.CodemodeRefusedBy(unknown))
	}

	e := &ClawExecutor{}
	_, err := e.codemodePolicy(backendFields{id: "n", codemode: "on"}, unknown)
	var refusal *delegate.ErrCapabilityUnsupported
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a refusal: a backend the registry does not carry installs no codemode tool, so granting it is the silent drop this slice exists to prevent", err)
	}
	if refusal.Backend != unknown {
		t.Errorf("the refusal names %q, want %q", refusal.Backend, unknown)
	}
}

// TestTheSchemaReaskDropsCodemodeWithTheRestOfTheTools holds a line that
// became LOAD-BEARING in an earlier round and had no test: the re-ask plans a
// deliberately tool-less turn, and since a codemode node runs a tool loop
// whatever its `tools:` say, `retryTask.Codemode = false` is now the only
// thing keeping the program host off that turn.
//
// The assertion is on the WIRE, not on the field: what matters is that the
// re-ask request carries no codemode tool, and that it still takes the
// structured arm it was planned for.
func TestTheSchemaReaskDropsCodemodeWithTheRestOfTheTools(t *testing.T) {
	t.Parallel()
	exec := NewClawExecutor(NewRegistry(), verdictWorkflow())
	// The CONTINUE-CONVERSATION arm is the only tool-less one, and it is the
	// one this test is about: the restart and resume arms want a full turn by
	// design. It is reached through an ANSWERED pause — a resume conversation
	// plus the pending tool_use id — which is what completedClawConversation
	// reads when no session is in the run context.
	task := &delegate.Task{
		NodeID:                 "n",
		Model:                  "test/test-model",
		UserPrompt:             "go",
		OutputSchema:           json.RawMessage(`{"type":"object"}`),
		Codemode:               true,
		HasTools:               true,
		AllowedTools:           []string{"Bash"},
		ResumeConversation:     []byte(`[{"role":"user","content":[{"type":"text","text":"hi"}]}]`),
		ResumePendingToolUseID: "tu_1",
		ResumeAnswer:           "yes",
	}
	r := exec.planSchemaReask(context.Background(), delegate.BackendClaw, task, delegate.Result{}, "schema-feedback")
	if r.mode != ReaskContinueConversation {
		t.Fatalf("the fixture stopped taking the tool-less arm (mode=%q): this test would pass over the thing it exists to hold", r.mode)
	}
	if r.task.HasTools {
		t.Error("the tool-less arm kept HasTools")
	}
	if r.task.Codemode {
		t.Error("the re-ask kept codemode: the turn is planned tool-less, and a program composing tool calls is the one thing it must not have")
	}

	// And on the wire: the planned task must reach the model with no
	// codemode tool. Asserting the field alone would pass over a backend
	// that installs the tool for another reason.
	backend, cap := newCapturingBackend()
	if _, err := backend.Execute(context.Background(), r.task); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(cap.requests) == 0 {
		t.Fatal("no request reached the wire")
	}
	for _, tool := range cap.requests[0].Tools {
		if tool.Name == ScriptHostToolName {
			t.Errorf("the re-ask offered %q to a turn planned without tools", ScriptHostToolName)
		}
	}
}
