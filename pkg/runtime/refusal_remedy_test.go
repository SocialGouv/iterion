package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestRemedyOf_isTheHintTheErrorCarries: the remedy is read from the
// RuntimeError in the chain, wrapped or not, with the fact that --force is
// needed too; a scratch refusal that lost its hint still names its consent,
// and an error that names no remedy gives none.
func TestRemedyOf_isTheHintTheErrorCarries(t *testing.T) {
	scratch := scratchNotPortable("run-1", "its teardown could not bank the scratch")
	edited := WithSourceChange(scratch)
	for _, tc := range []struct {
		name string
		err  error
		want Remedy
	}{
		{"nil", nil, Remedy{}},
		{"a plain error", errors.New("boom"), Remedy{}},
		{"a wrapped refusal", fmt.Errorf("runtime: sandbox: %w", scratch), Remedy{Hint: scratchLossHint}},
		{"a refusal naming a change --force accepts", edited, Remedy{Hint: scratchLossHint + "; the source changed too: add --force to accept that", AlsoNeedsForce: true}},
		{"a scratch refusal without its hint", &RuntimeError{Code: ErrCodeScratchNotPortable, Message: "lost"}, Remedy{Hint: scratchLossHint}},
		{"a code that names no remedy", &RuntimeError{Code: ErrCodeExecutionFailed, Message: "boom"}, Remedy{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RemedyOf(tc.err); got != tc.want {
				t.Fatalf("RemedyOf = %+v, want %+v", got, tc.want)
			}
		})
	}
	if !strings.Contains(RemedyOf(scratch).Hint, "--accept-scratch-loss") {
		t.Fatalf("the scratch's remedy does not name its consent: %q", RemedyOf(scratch).Hint)
	}
	if got := OperatorMessage(scratch); got != scratch.Error()+" — hint: "+scratchLossHint {
		t.Fatalf("OperatorMessage = %q, want the error followed by its hint", got)
	}
	if got := OperatorMessage(errors.New("boom")); got != "boom" {
		t.Fatalf("OperatorMessage of an error naming no remedy = %q, want its words alone", got)
	}
	data := map[string]any{}
	RemedyOf(edited).Record(data)
	if data["hint"] != RemedyOf(edited).Hint || data["also_needs_force"] != true {
		t.Fatalf("Record = %v, want hint and also_needs_force", data)
	}
	data = map[string]any{}
	Remedy{}.Record(data)
	if len(data) != 0 {
		t.Fatalf("a remedy naming nothing recorded %v", data)
	}
}

// TestResume_aLossOnlyTheRestoreFindsNamesItsConsentOnTheRun: a refusal met
// after the claim — a bank that is gone, a resume without a sandbox — parks
// the run; its document and its run_failed carry the refusal's remedy, which
// names the consent that clears it, on the failure arm and the pause arm
// alike.
func TestResume_aLossOnlyTheRestoreFindsNamesItsConsentOnTheRun(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	for _, tc := range []struct {
		name string
		park func(t *testing.T, s store.RunStore, runID string) error
	}{
		{"failure arm: the bank is gone", func(t *testing.T, s store.RunStore, runID string) error {
			ctx := context.Background()
			d := &podDriver{root: t.TempDir()}
			wf := scratchWorkflow()
			delete(wf.Nodes, "gate")
			wf.Edges = []*ir.Edge{{From: "measure", To: "report"}, {From: "report", To: "done"}}
			x := newStubExecutor()
			x.on("measure", func(map[string]any) (map[string]any, error) {
				if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
					return nil, err
				}
				return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), "verify.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
			})
			x.on("report", func(map[string]any) (map[string]any, error) { return nil, errors.New("the provider went away") })
			eng := func() *Engine {
				e := New(wf, s, x, WithLogger(iterlog.Nop()), WithSandboxDrivers(map[string]sandbox.DriverConstructor{
					"docker": func() (sandbox.Driver, error) { return d, nil },
				}))
				e.forceResume = true
				return e
			}
			if err := eng().Run(ctx, runID, nil); err == nil {
				t.Fatal("the run did not fail on the report node")
			}
			if err := store.AsScratchBankStore(s).DeleteScratchBank(ctx, runID); err != nil {
				t.Fatal(err)
			}
			return eng().Resume(ctx, runID, nil)
		}},
		{"pause arm: a resume without a sandbox", func(t *testing.T, s store.RunStore, runID string) error {
			ctx := context.Background()
			d := &podDriver{root: t.TempDir()}
			x := newStubExecutor()
			x.on("measure", func(map[string]any) (map[string]any, error) {
				if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
					return nil, err
				}
				return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), "facts.json"), []byte("{}"), 0o644)
			})
			if err := scratchEngine(s, x, d).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
				t.Fatalf("Run: want ErrRunPaused, got %v", err)
			}
			e := New(scratchWorkflow(), s, x, WithLogger(iterlog.Nop()), WithSandboxOverride("none"))
			e.forceResume = true
			return e.Resume(ctx, runID, map[string]any{"ok": true})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tmpStore(t)
			const runID = "run-restore-remedy"
			err := tc.park(t, s, runID)
			var rt *RuntimeError
			if !errors.As(err, &rt) || rt.Code != ErrCodeScratchNotPortable {
				t.Fatalf("a forced resume the restore refuses: want SCRATCH_NOT_PORTABLE, got %v", err)
			}
			r := mustLoadRun(t, s, runID)
			if r.Status != store.RunStatusFailedResumable || r.FailureCode != store.FailureScratchNotPortable {
				t.Fatalf("doc = %s/%s, want failed_resumable/SCRATCH_NOT_PORTABLE", r.Status, r.FailureCode)
			}
			if !strings.HasSuffix(r.Error, " — hint: "+rt.Hint) || !strings.Contains(r.Error, "--accept-scratch-loss") {
				t.Errorf("the run's error does not carry the refusal's remedy: %q", r.Error)
			}
			failed := eventsOf(t, s, runID, store.EventRunFailed)
			if len(failed) == 0 {
				t.Fatal("the park wrote no run_failed")
			}
			last := failed[len(failed)-1].Data
			if last["hint"] != rt.Hint || last["code"] != string(store.FailureScratchNotPortable) {
				t.Errorf("run_failed = %v, want the refusal's code and hint %q", last, rt.Hint)
			}
			if msg, _ := last["error"].(string); strings.Contains(msg, "hint:") {
				t.Errorf("run_failed.error repeats the hint it carries in its own field: %q", msg)
			}
		})
	}
}

// TestMarkFailedBestEffort_carriesTheCausesRemedyOnlyAsItsVerdict: a setup
// failure recorded as the cause's own verdict names the cause's remedy on
// the document and on run_failed; a failure setupFailureStatus reads as a
// drain is the drain's, and the cause's remedy is not offered for it.
func TestMarkFailedBestEffort_carriesTheCausesRemedyOnlyAsItsVerdict(t *testing.T) {
	cause := &RuntimeError{Code: ErrCodeSandboxDriverUnavailable, Message: "no container runtime", Hint: "install docker or podman"}
	drained, drain := context.WithCancelCause(context.Background())
	drain(ErrRunInterrupted)
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		wantCode store.FailureCode
		wantHint bool
	}{
		{"the cause's verdict", context.Background(), ErrCodeSandboxDriverUnavailable, true},
		{"a drain", drained, store.FailureInterrupted, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tmpStore(t)
			const runID = "run-setup-remedy"
			if _, err := s.CreateRun(context.Background(), runID, "wf", nil); err != nil {
				t.Fatal(err)
			}
			New(devboxTestWorkflow(), s, newStubExecutor(), WithLogger(iterlog.Nop())).
				markFailedBestEffort(tc.ctx, runID, "sandbox start", cause)
			r := mustLoadRun(t, s, runID)
			if r.FailureCode != tc.wantCode {
				t.Fatalf("failure_code = %q, want %q", r.FailureCode, tc.wantCode)
			}
			if got := strings.Contains(r.Error, "install docker or podman"); got != tc.wantHint {
				t.Errorf("the run's error names the cause's remedy = %v, want %v: %q", got, tc.wantHint, r.Error)
			}
			failed := eventsOf(t, s, runID, store.EventRunFailed)
			if len(failed) != 1 {
				t.Fatalf("run_failed events = %d, want 1", len(failed))
			}
			if _, got := failed[0].Data["hint"]; got != tc.wantHint {
				t.Errorf("run_failed carries a hint = %v, want %v: %v", got, tc.wantHint, failed[0].Data)
			}
		})
	}
}

// TestBudgetExceeded_theRecordsNameHowToRaiseTheCap: a spent budget stops
// the run with a remedy — raise the cap and resume — that its document and
// run_failed carry, both when the node loop meets it and when a resume that
// raised nothing meets it again before any node.
func TestBudgetExceeded_theRecordsNameHowToRaiseTheCap(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "budget_remedy",
		Entry: "a",
		Nodes: map[string]ir.Node{
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"c":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "c"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "a", To: "b"}, {From: "b", To: "c"}, {From: "c", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxIterations: 2},
	}
	x := newStubExecutor()
	for _, id := range []string{"a", "b", "c"} {
		x.on(id, func(map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil })
	}
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-budget-remedy"
	check := func(step string, err error) {
		t.Helper()
		var rt *RuntimeError
		if !errors.As(err, &rt) || rt.Code != ErrCodeBudgetExceeded || rt.Hint == "" {
			t.Fatalf("%s: want BUDGET_EXCEEDED with its remedy, got %v", step, err)
		}
		r := mustLoadRun(t, s, runID)
		if !strings.HasSuffix(r.Error, " — hint: "+rt.Hint) {
			t.Errorf("%s: the run's error does not carry the remedy %q: %q", step, rt.Hint, r.Error)
		}
		failed := eventsOf(t, s, runID, store.EventRunFailed)
		if len(failed) == 0 || failed[len(failed)-1].Data["hint"] != rt.Hint {
			t.Errorf("%s: run_failed does not carry the remedy %q: %v", step, rt.Hint, dataOf(failed))
		}
	}
	check("the node loop", New(wf, s, x, WithLogger(iterlog.Nop())).Run(ctx, runID, nil))
	check("a resume that raised nothing", New(wf, s, x, WithLogger(iterlog.Nop())).Resume(ctx, runID, nil))
}

// TestFailRunWriters_putTheRemedyOnTheRecords: the terminal failure writers
// — a failure the checkpoint could not keep falls back to them — record the
// remedy the failure names on the document and on run_failed.
func TestFailRunWriters_putTheRemedyOnTheRecords(t *testing.T) {
	const hint = "raise budget.cost and resume"
	for _, tc := range []struct {
		name  string
		write func(e *Engine, runID string) error
	}{
		{"failRunErr", func(e *Engine, runID string) error {
			return e.failRunErr(context.Background(), runID, "n", fmt.Errorf("wrapped: %w", &RuntimeError{Code: ErrCodeBudgetExceeded, Message: "budget exceeded", Hint: hint}))
		}},
		{"failRunWithCode", func(e *Engine, runID string) error {
			return e.failRunWithCode(context.Background(), runID, "n", "budget exceeded", ErrCodeBudgetExceeded, hint)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tmpStore(t)
			const runID = "run-fail-writer"
			if _, err := s.CreateRun(context.Background(), runID, "wf", nil); err != nil {
				t.Fatal(err)
			}
			err := tc.write(New(devboxTestWorkflow(), s, newStubExecutor(), WithLogger(iterlog.Nop())), runID)
			if RemedyOf(err).Hint != hint {
				t.Fatalf("the returned error lost its remedy: %v", err)
			}
			if r := mustLoadRun(t, s, runID); r.Error != "budget exceeded — hint: "+hint {
				t.Errorf("the run's error = %q, want the message and its hint", r.Error)
			}
			failed := eventsOf(t, s, runID, store.EventRunFailed)
			if len(failed) != 1 || failed[0].Data["hint"] != hint || failed[0].Data["error"] != "budget exceeded" {
				t.Errorf("run_failed = %v, want the message and its hint apart", dataOf(failed))
			}
		})
	}
}
