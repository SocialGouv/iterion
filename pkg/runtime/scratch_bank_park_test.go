package runtime

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// askingWorkflow: measure writes the scratch, ask — an agent that pauses
// through ask_user — waits for its answer, report reads the scratch.
func askingWorkflow() *ir.Workflow {
	wf := scratchWorkflow()
	delete(wf.Nodes, "gate")
	wf.Nodes["ask"] = &ir.AgentNode{
		BaseNode:          ir.BaseNode{ID: "ask"},
		InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
	}
	wf.Edges = []*ir.Edge{{From: "measure", To: "ask"}, {From: "ask", To: "report"}, {From: "report", To: "done"}}
	return wf
}

// askingRun parks runID at ask, its scratch banked, and returns an engine
// factory over store s and whether report found the scratch.
func askingRun(t *testing.T, s store.RunStore, runID string) (func(store.RunStore) *Engine, *bool) {
	t.Helper()
	d := &podDriver{root: t.TempDir()}
	x := newStubExecutor()
	x.on("measure", func(map[string]any) (map[string]any, error) {
		if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
			return nil, err
		}
		return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), "facts.json"), []byte("{}"), 0o644)
	})
	asked := false
	x.on("ask", func(map[string]any) (map[string]any, error) {
		if !asked {
			asked = true
			return nil, &model.ErrNeedsInteraction{NodeID: "ask", Questions: map[string]any{delegate.AskUserQuestionKey: "staging or prod?"}, Backend: "claw"}
		}
		return map[string]any{}, nil
	})
	read := new(bool)
	x.on("report", func(map[string]any) (map[string]any, error) {
		_, err := os.Stat(filepath.Join(d.scratch(), "facts.json"))
		*read = err == nil
		return map[string]any{}, err
	})
	eng := func(st store.RunStore) *Engine {
		return New(askingWorkflow(), st, x, WithLogger(iterlog.Nop()), WithSandboxDrivers(map[string]sandbox.DriverConstructor{
			"docker": func() (sandbox.Driver, error) { return d, nil },
		}))
	}
	if err := eng(s).Run(context.Background(), runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	if banked := eventsOf(t, s, runID, store.EventSandboxScratchBanked); len(banked) != 1 || banked[0].Data["banked"] != true {
		t.Fatalf("the pause did not bank the scratch: %v", dataOf(banked))
	}
	return eng, read
}

// TestResume_anAgentThatAskedDoesNotAgeItsBank: the resume of an agent that
// paused through ask_user records it finished by its answer, before the new
// sandbox starts. That finish ran nowhere: the restore is not stale.
func TestResume_anAgentThatAskedDoesNotAgeItsBank(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	const runID = "run-scratch-asked"
	eng, read := askingRun(t, s, runID)
	if err := eng(s).Resume(context.Background(), runID, map[string]any{delegate.AskUserQuestionKey: "staging"}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	restored := eventsOf(t, s, runID, store.EventSandboxScratchRestored)
	if len(restored) != 1 || restored[0].Data["restored"] != true || restored[0].Data["stale"] == true {
		t.Fatalf("an unforced resume of the exact bank: want one restore, not stale, got %v", dataOf(restored))
	}
	if !*read {
		t.Fatal("report did not find the scratch the pause banked")
	}
}

// TestResume_anAgentThatAskedIsNotRefusedAfterARetriedRead: the answer's
// finish lands before the restore. A resume whose bank read then fails on
// the way — a read to retry — leaves it in the timeline; the next resume,
// on a healthy store, is not refused SCRATCH_NOT_PORTABLE over it: the bank
// is exact.
func TestResume_anAgentThatAskedIsNotRefusedAfterARetriedRead(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-asked-retried"
	eng, read := askingRun(t, s, runID)
	err := eng(flakyBankStore{s}).Resume(ctx, runID, map[string]any{delegate.AskUserQuestionKey: "staging"})
	var rt *RuntimeError
	if err == nil || errors.As(err, &rt) && rt.Code == ErrCodeScratchNotPortable {
		t.Fatalf("a bank read that fails on the way: want an error to retry, got %v", err)
	}
	if answered := finishesOf(t, s, runID, "ask"); len(answered) == 0 {
		t.Fatal("precondition: the failed resume recorded no finish for ask — this proves nothing")
	}
	if err := s.SaveRun(ctx, resumable(t, s, runID)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResumeScratch(ctx, s, mustLoadRun(t, s, runID), askingWorkflow(), false); err != nil {
		t.Fatalf("the resume surface refused an exact bank: %v", err)
	}
	if err := eng(s).Resume(ctx, runID, map[string]any{delegate.AskUserQuestionKey: "staging"}); err != nil {
		t.Fatalf("the healthy resume: %v", err)
	}
	if !*read {
		t.Fatal("report did not find the scratch the pause banked")
	}
}

// finishesOf is the node_finished events of node id in runID.
func finishesOf(t *testing.T, s store.RunStore, runID, id string) []*store.Event {
	t.Helper()
	var out []*store.Event
	for _, ev := range eventsOf(t, s, runID, store.EventNodeFinished) {
		if ev.NodeID == id {
			out = append(out, ev)
		}
	}
	return out
}

// lostPodDriver starts pods like podDriver; the pod numbered lost is gone by
// its teardown: listing its scratch fails, as a kubectl exec into a deleted
// pod does.
type lostPodDriver struct {
	*podDriver
	lost int
}

func (d lostPodDriver) Start(ctx context.Context, p sandbox.PreparedSpec, info sandbox.RunInfo) (sandbox.Run, error) {
	run, err := d.podDriver.Start(ctx, p, info)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	n := len(d.pods) - 1
	d.mu.Unlock()
	if n == d.lost {
		return lostPod{run.(*podRun)}, nil
	}
	return run, nil
}

type lostPod struct{ *podRun }

func (r lostPod) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	if strings.Contains(strings.Join(argv, " "), "find ") {
		return sandbox.ExecResult{}, errors.New(`pods "iterion-sbx-1" not found`)
	}
	return r.podRun.Exec(ctx, argv, opts)
}

// TestResume_anUnreadTeardownKeepsTheBankBeforeIt: a resume restores the
// bank, then its pod is killed while report runs — report never finishes —
// and the teardown cannot list the scratch: it records unknown. The bank is
// still stored, and is the state the run restarts from: the next resume
// restores it, and report finds its input.
func TestResume_anUnreadTeardownKeepsTheBankBeforeIt(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-unread-teardown"
	d := &podDriver{root: t.TempDir()}
	x := newStubExecutor()
	x.on("measure", func(map[string]any) (map[string]any, error) {
		if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
			return nil, err
		}
		return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), "floor.json"), []byte(`{"files": 31}`), 0o644)
	})
	killed := false
	x.on("report", func(map[string]any) (map[string]any, error) {
		if !killed {
			killed = true
			return nil, errors.New("exec: command terminated with exit code 137")
		}
		_, err := os.Stat(filepath.Join(d.scratch(), "floor.json"))
		return map[string]any{}, err
	})
	eng := func(drv sandbox.Driver) *Engine {
		e := New(scratchWorkflow(), s, x, WithLogger(iterlog.Nop()), WithSandboxDrivers(map[string]sandbox.DriverConstructor{
			"docker": func() (sandbox.Driver, error) { return drv, nil },
		}))
		e.scratchBankRetryPause = time.Millisecond
		return e
	}
	if err := eng(d).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	if err := eng(lostPodDriver{podDriver: d, lost: 1}).Resume(ctx, runID, map[string]any{"ok": true}); err == nil {
		t.Fatal("the resume whose pod was killed did not fail — this proves nothing")
	}
	banked := eventsOf(t, s, runID, store.EventSandboxScratchBanked)
	if len(banked) != 2 || banked[1].Data["unknown"] != true {
		t.Fatalf("precondition: want the killed pod's teardown recorded unknown, got %v", dataOf(banked))
	}
	if err := eng(d).Resume(ctx, runID, nil); err != nil {
		t.Fatalf("the next resume ran without the bank before the unread teardown: %v", err)
	}
	restored := eventsOf(t, s, runID, store.EventSandboxScratchRestored)
	if last := restored[len(restored)-1]; last.Data["restored"] != true || last.Data["stale"] == true {
		t.Fatalf("want the bank restored, not stale, got %v", dataOf(restored))
	}
}

// TestLastScratchPark_readsTheRecordThatDecides: which record decides, what
// ages a bank, and what the resume surface may decide without the run's
// lock, over each order of the events that matter.
func TestLastScratchPark_readsTheRecordThatDecides(t *testing.T) {
	banked := store.Event{Type: store.EventSandboxScratchBanked, Data: map[string]any{"banked": true, "empty": false, "bytes": 10}}
	empty := store.Event{Type: store.EventSandboxScratchBanked, Data: map[string]any{"banked": false, "empty": true}}
	unknown := store.Event{Type: store.EventSandboxScratchBanked, Data: map[string]any{"banked": false, "empty": false, "unknown": true, "reason": "gone"}}
	refused := store.Event{Type: store.EventSandboxScratchBanked, Data: map[string]any{"banked": false, "empty": false, "reason": "over the cap"}}
	ran := store.Event{Type: store.EventNodeFinished, NodeID: "report"}
	answered := store.Event{Type: store.EventNodeFinished, NodeID: "report", Data: map[string]any{nodeFinishedAnswered: true}}
	started := store.Event{Type: store.EventRunStarted}
	resumed := store.Event{Type: store.EventRunResumed}
	cases := []struct {
		name            string
		events          []store.Event
		want            scratchPark
		engine, surface bool // refused by the engine's check, by the resume surface's
	}{
		{"a bank, then an unread teardown", []store.Event{banked, unknown}, scratchPark{recorded: true, banked: true}, false, false},
		{"a bank, a node, then an unread teardown", []store.Event{banked, ran, unknown}, scratchPark{recorded: true, banked: true, advanced: true}, true, true},
		{"a bank, an unread teardown, then a node", []store.Event{banked, unknown, ran}, scratchPark{recorded: true, banked: true, advanced: true}, true, true},
		{"an unread teardown, then a bank", []store.Event{unknown, banked}, scratchPark{recorded: true, banked: true}, false, false},
		{"empty, then an unread teardown", []store.Event{empty, unknown}, scratchPark{recorded: true, unknown: true, reason: "gone"}, false, false},
		{"a refusal, then an unread teardown", []store.Event{refused, unknown}, scratchPark{recorded: true, unknown: true, reason: "gone"}, false, false},
		{"an unread teardown, then a refusal", []store.Event{unknown, refused}, scratchPark{recorded: true, reason: "over the cap"}, true, true},
		{"a bank, then an answered finish", []store.Event{banked, answered}, scratchPark{recorded: true, banked: true}, false, false},
		{"a bank, then a finish that ran", []store.Event{banked, ran}, scratchPark{recorded: true, banked: true, advanced: true}, true, true},
		{"a start, then a bank", []store.Event{started, banked}, scratchPark{recorded: true, banked: true}, false, false},
		{"a start, then a refusal", []store.Event{started, refused}, scratchPark{recorded: true, reason: "over the cap"}, true, true},
		{"a bank, then an execution that wrote no record", []store.Event{banked, resumed}, scratchPark{recorded: true, banked: true, superseded: true}, false, false},
		{"a bank, then an execution that ran a node and wrote no record", []store.Event{banked, resumed, ran}, scratchPark{recorded: true, banked: true, advanced: true, superseded: true}, true, false},
		{"a refusal, then an execution that wrote no record", []store.Event{refused, resumed}, scratchPark{recorded: true, reason: "over the cap", superseded: true}, true, false},
		{"a bank, then an execution whose teardown could not read", []store.Event{banked, resumed, ran, unknown}, scratchPark{recorded: true, banked: true, advanced: true}, true, true},
		{"a bank, then an execution that banked again", []store.Event{banked, resumed, ran, banked}, scratchPark{recorded: true, banked: true}, false, false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tmpStore(t)
			ctx := context.Background()
			runID := fmt.Sprintf("run-park-%d", i)
			if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
				t.Fatal(err)
			}
			for _, ev := range tc.events {
				ev.Data = cloneData(ev.Data)
				if _, err := s.AppendEvent(ctx, runID, ev); err != nil {
					t.Fatal(err)
				}
			}
			got, err := lastScratchPark(ctx, s, scratchWorkflow(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("park %+v, want %+v", got, tc.want)
			}
			cause, err := scratchRefusal(ctx, s, scratchWorkflow(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if (cause != "") != tc.engine {
				t.Fatalf("engine: refusal %q, want refused=%v", cause, tc.engine)
			}
			serr := ValidateResumeScratch(ctx, s, mustLoadRun(t, s, runID), scratchWorkflow(), false)
			if (serr != nil) != tc.surface {
				t.Fatalf("surface: %v, want refused=%v", serr, tc.surface)
			}
		})
	}
}

func cloneData(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// raceThroughKubectl runs commands like podRun, reporting them the way a
// kubernetes sandbox's `kubectl exec` does: the teardown's tar catches a file
// mid-write — a complete archive, exit 1 — and kubectl adds its own line.
type raceThroughKubectl struct{ *podRun }

func (r raceThroughKubectl) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	res, err := r.podRun.Exec(ctx, argv, opts)
	if err != nil || res.ExitCode != 0 || !strings.Contains(strings.Join(argv, " "), "-czf") {
		return res, err
	}
	fmt.Fprintf(opts.Stderr, "tar: ./server.log: file changed as we read it\n%s\n", sandbox.KubectlRemoteExit1)
	res.ExitCode = 1
	return res, nil
}

// TestBankScratch_aFileChangedThroughKubectlIsBanked: tar's archive of a file
// changed while it read it is complete, and kubectl's exit-code line does not
// make it a failure: banked.
func TestBankScratch_aFileChangedThroughKubectlIsBanked(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-kubectl-race"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server.log"), []byte("listening\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := bankScratch(ctx, raceThroughKubectl{&podRun{scratch: dir}}, sandboxScratchContainerPath, store.AsScratchBankStore(s), runID, scratchBankMaxBytes)
	if !got.banked {
		t.Fatalf("through kubectl exec, a complete archive of a file changed while tar read it: want banked, got %+v", got)
	}
}

// failsOnce runs commands like podRun, but the first exec whose argv holds
// match fails the way fail says.
type failsOnce struct {
	*podRun
	match  string
	fail   func(opts sandbox.ExecOpts) (sandbox.ExecResult, error)
	failed bool
}

func (r *failsOnce) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	if !r.failed && strings.Contains(strings.Join(argv, " "), r.match) {
		r.failed = true
		return r.fail(opts)
	}
	return r.podRun.Exec(ctx, argv, opts)
}

// bankedAtTeardown runs one teardown of runID over run and returns its
// record.
func bankedAtTeardown(t *testing.T, s store.RunStore, runID string, run sandbox.Run) map[string]any {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	e := New(scratchWorkflow(), s, newStubExecutor(), WithLogger(iterlog.Nop()))
	e.scratchBankRetryPause = time.Millisecond
	e.bankScratchOnCleanup(ctx, runID, &activeSandbox{run: run})
	got := eventsOf(t, s, runID, store.EventSandboxScratchBanked)
	if len(got) != 1 {
		t.Fatalf("want one record, got %v", dataOf(got))
	}
	return got[0].Data
}

// TestBankScratchOnCleanup_triesAgainWhatAnotherTryMayCure: a live pod's
// scratch is banked though the first try failed on the way — a blip on the
// exec that lists it, a tar that caught a file being truncated.
func TestBankScratchOnCleanup_triesAgainWhatAnotherTryMayCure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		match string
		fail  func(opts sandbox.ExecOpts) (sandbox.ExecResult, error)
	}{
		{"a blip listing the scratch", "find ", func(sandbox.ExecOpts) (sandbox.ExecResult, error) {
			return sandbox.ExecResult{}, errors.New("error dialing backend: dial tcp 10.0.3.7:10250: i/o timeout")
		}},
		{"tar caught a file being truncated", "-czf", func(opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
			fmt.Fprintf(opts.Stderr, "tar: ./server.log: File shrank by 4096 bytes; padding with zeros\n%s\n", sandbox.KubectlRemoteExit1)
			return sandbox.ExecResult{ExitCode: 1}, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "verify.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			run := &failsOnce{podRun: &podRun{scratch: dir}, match: tc.match, fail: tc.fail}
			rec := bankedAtTeardown(t, tmpStore(t), "run-scratch-tried-again", run)
			if !run.failed || rec["banked"] != true {
				t.Fatalf("after one failure another try may cure: want the scratch banked, got %v (failed=%v)", rec, run.failed)
			}
		})
	}
}

// TestBankScratch_pastTheCapIsNotTriedAgain: a scratch past the cap is past
// it on every try: the teardown records it without archiving it again.
func TestBankScratch_pastTheCapIsNotTriedAgain(t *testing.T) {
	dir := t.TempDir()
	noise := make([]byte, 64<<10)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "noise.bin"), noise, 0o644); err != nil {
		t.Fatal(err)
	}
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-past-the-cap"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	got := bankScratch(ctx, localRun(dir), sandboxScratchContainerPath, store.AsScratchBankStore(s), runID, 1<<10)
	if got.banked || got.retry || !strings.Contains(got.reason, "cap") {
		t.Fatalf("past the cap: want a refusal not tried again, got %+v", got)
	}
}

// slowTeardown starts pods like podDriver; the teardown of the pod numbered
// slow blocks its tar — a large scratch on its way to the store — until
// release is closed.
type slowTeardown struct {
	*podDriver
	slow             int
	blocked, release chan struct{}
}

func (d slowTeardown) Start(ctx context.Context, p sandbox.PreparedSpec, info sandbox.RunInfo) (sandbox.Run, error) {
	run, err := d.podDriver.Start(ctx, p, info)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	n := len(d.pods) - 1
	d.mu.Unlock()
	if n == d.slow {
		return slowTar{podRun: run.(*podRun), blocked: d.blocked, release: d.release}, nil
	}
	return run, nil
}

type slowTar struct {
	*podRun
	blocked, release chan struct{}
}

func (r slowTar) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	if strings.Contains(strings.Join(argv, " "), "-czf") {
		close(r.blocked)
		<-r.release
	}
	return r.podRun.Exec(ctx, argv, opts)
}

// TestValidateResumeScratch_waitsOutATeardownStillBanking: a run parks a
// second time and reads paused while that park's teardown still banks the
// scratch. The resume surface does not refuse an answer then: that
// execution wrote no record yet, and only the engine, under the run's lock,
// sees its teardown end. Once the bank lands, the run resumes with it.
func TestValidateResumeScratch_waitsOutATeardownStillBanking(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-teardown-window"
	d := &podDriver{root: t.TempDir()}
	wf := func() *ir.Workflow {
		w := scratchWorkflow()
		w.Nodes["work"] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}}
		w.Nodes["again"] = &ir.HumanNode{BaseNode: ir.BaseNode{ID: "again"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}}
		w.Edges = []*ir.Edge{{From: "measure", To: "gate"}, {From: "gate", To: "work"}, {From: "work", To: "again"}, {From: "again", To: "report"}, {From: "report", To: "done"}}
		return w
	}
	x := newStubExecutor()
	write := func(name string) func(map[string]any) (map[string]any, error) {
		return func(map[string]any) (map[string]any, error) {
			if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
				return nil, err
			}
			return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), name), []byte("{}"), 0o644)
		}
	}
	x.on("measure", write("floor.json"))
	x.on("work", write("ledger.json"))
	read := false
	x.on("report", func(map[string]any) (map[string]any, error) {
		_, err := os.Stat(filepath.Join(d.scratch(), "ledger.json"))
		read = err == nil
		return map[string]any{}, err
	})
	slow := slowTeardown{podDriver: d, slow: 1, blocked: make(chan struct{}), release: make(chan struct{})}
	eng := func(drv sandbox.Driver) *Engine {
		return New(wf(), s, x, WithLogger(iterlog.Nop()), WithSandboxDrivers(map[string]sandbox.DriverConstructor{
			"docker": func() (sandbox.Driver, error) { return drv, nil },
		}))
	}
	if err := eng(d).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- eng(slow).Resume(ctx, runID, map[string]any{"ok": true}) }()
	<-slow.blocked
	r := mustLoadRun(t, s, runID)
	if r.Status != store.RunStatusPausedWaitingHuman {
		close(slow.release)
		<-done
		t.Fatalf("precondition: the run reads %s while its teardown banks, want paused_waiting_human — this proves nothing", r.Status)
	}
	cause, err := scratchRefusal(ctx, s, wf(), runID)
	surface := ValidateResumeScratch(ctx, s, r, wf(), false)
	close(slow.release)
	if err := <-done; !errors.Is(err, ErrRunPaused) {
		t.Fatalf("the second park: want ErrRunPaused, got %v", err)
	}
	if err != nil || cause == "" {
		t.Fatalf("precondition: the timeline in the window reads as a lost sandbox to the engine (%q, %v) — this proves nothing", cause, err)
	}
	if surface != nil {
		t.Fatalf("the resume surface refused an answer while the teardown was still banking: %v", surface)
	}
	if err := ValidateResumeScratch(ctx, s, mustLoadRun(t, s, runID), wf(), false); err != nil {
		t.Fatalf("the resume surface after the teardown: %v", err)
	}
	if err := eng(d).Resume(ctx, runID, map[string]any{"ok": true}); err != nil {
		t.Fatalf("the resume after the teardown: %v", err)
	}
	if !read {
		t.Fatal("report did not find what work wrote before the second park")
	}
}

// tarEnvRecorder runs commands like podRun and keeps the environment the
// teardown's tar was given.
type tarEnvRecorder struct {
	*podRun
	env map[string]string
}

func (r *tarEnvRecorder) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	if strings.Contains(strings.Join(argv, " "), "-czf") {
		r.env = opts.Env
	}
	return r.podRun.Exec(ctx, argv, opts)
}

// TestBankScratch_runsTarUntranslated: the teardown reads tar's stderr for
// its race warnings, which a translated tar would not write in those words.
func TestBankScratch_runsTarUntranslated(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-tar-locale"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "verify.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := &tarEnvRecorder{podRun: &podRun{scratch: dir}}
	if got := bankScratch(ctx, run, sandboxScratchContainerPath, store.AsScratchBankStore(s), runID, scratchBankMaxBytes); !got.banked {
		t.Fatalf("bankScratch: %+v", got)
	}
	if run.env["LC_ALL"] != sandbox.TarLocale {
		t.Fatalf("the teardown's tar ran with env %v, want LC_ALL=%s", run.env, sandbox.TarLocale)
	}
}

// TestResume_aHostWithoutItsTemporaryDirectoryIsRetried: the bank is read
// through a host temporary file. A host whose temporary directory is missing
// has not lost the bank: the resume fails without a code, to be retried, and
// once the host is whole the bank is restored.
func TestResume_aHostWithoutItsTemporaryDirectoryIsRetried(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-host-tmp"
	d := &podDriver{root: t.TempDir()}
	x := newStubExecutor()
	x.on("measure", func(map[string]any) (map[string]any, error) {
		if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
			return nil, err
		}
		return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), "floor.json"), []byte("{}"), 0o644)
	})
	read := false
	x.on("report", func(map[string]any) (map[string]any, error) {
		_, err := os.Stat(filepath.Join(d.scratch(), "floor.json"))
		read = err == nil
		return map[string]any{}, err
	})
	if err := scratchEngine(s, x, d).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	host := os.Getenv("TMPDIR")
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	err := scratchEngine(s, x, d).Resume(ctx, runID, map[string]any{"ok": true})
	if err == nil || !strings.Contains(err.Error(), "no host temporary file for the bank") {
		t.Fatalf("precondition: want the resume stopped on the host's temporary file, got %v — this proves nothing", err)
	}
	var rt *RuntimeError
	if errors.As(err, &rt) && rt.Code == ErrCodeScratchNotPortable {
		t.Fatalf("the host's missing temporary directory was read as a bank that is gone: %v", err)
	}
	if r := mustLoadRun(t, s, runID); r.FailureCode == store.FailureScratchNotPortable {
		t.Fatalf("the run was parked SCRATCH_NOT_PORTABLE on a host fault: %s", r.Error)
	}
	t.Setenv("TMPDIR", host)
	if err := s.SaveRun(ctx, resumable(t, s, runID)); err != nil {
		t.Fatal(err)
	}
	if err := scratchEngine(s, x, d).Resume(ctx, runID, map[string]any{"ok": true}); err != nil {
		t.Fatalf("the resume on a whole host: %v", err)
	}
	if !read {
		t.Fatal("report did not find the scratch the park banked")
	}
}

// scriptedRun answers the teardown's listing (find) and archive (tar -czf)
// execs from their scripts, in order; the last step repeats, and a nil step
// runs the command for real.
type scriptedRun struct {
	*podRun
	list, tar   []func(opts sandbox.ExecOpts) (sandbox.ExecResult, error)
	lists, tars int
}

func (r *scriptedRun) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	var steps []func(opts sandbox.ExecOpts) (sandbox.ExecResult, error)
	var n *int
	switch cmd := strings.Join(argv, " "); {
	case strings.Contains(cmd, "find "):
		steps, n = r.list, &r.lists
	case strings.Contains(cmd, "-czf"):
		steps, n = r.tar, &r.tars
	}
	if len(steps) > 0 {
		step := steps[min(*n, len(steps)-1)]
		*n++
		if step != nil {
			return step(opts)
		}
	}
	return r.podRun.Exec(ctx, argv, opts)
}

func podGone(sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{ExitCode: 1, Stderr: []byte(`Error from server (NotFound): pods "iterion-sbx-1" not found`)}, nil
}

func listedEmpty(sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}

func tarKilled(opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	fmt.Fprintln(opts.Stderr, "command terminated with exit code 137")
	return sandbox.ExecResult{ExitCode: 137}, nil
}

// dropFailsOnce refuses the first drop of a previous bank.
type dropFailsOnce struct {
	store.RunStore
	failed bool
}

func (s *dropFailsOnce) PutScratchBank(ctx context.Context, runID string, body io.Reader, size int64) error {
	return store.AsScratchBankStore(s.RunStore).PutScratchBank(ctx, runID, body, size)
}

func (s *dropFailsOnce) OpenScratchBank(ctx context.Context, runID string) (io.ReadCloser, error) {
	return store.AsScratchBankStore(s.RunStore).OpenScratchBank(ctx, runID)
}

func (s *dropFailsOnce) DeleteScratchBank(ctx context.Context, runID string) error {
	if !s.failed {
		s.failed = true
		return errors.New("blob: DELETE sessions/run/scratch.tgz: 503 Slow Down")
	}
	return store.AsScratchBankStore(s.RunStore).DeleteScratchBank(ctx, runID)
}

// TestBankScratchOnCleanup_aLaterTryDoesNotForgetWhatAnEarlierOneSaw: a try
// that saw the scratch is not replaced by one that saw less of it. The pod
// killed while tar read its files cannot be listed by the next try, nor is
// the scratch it held empty: the record keeps the failure that refuses the
// resume. A scratch seen empty is not replaced by a pod that is gone.
func TestBankScratchOnCleanup_aLaterTryDoesNotForgetWhatAnEarlierOneSaw(t *testing.T) {
	for _, tc := range []struct {
		name      string
		run       func(dir string) *scriptedRun
		dropFails bool
		check     func(t *testing.T, rec map[string]any)
	}{
		{"tar killed, then the pod gone", func(dir string) *scriptedRun {
			return &scriptedRun{podRun: &podRun{scratch: dir}, list: []func(sandbox.ExecOpts) (sandbox.ExecResult, error){nil, podGone}, tar: []func(sandbox.ExecOpts) (sandbox.ExecResult, error){tarKilled}}
		}, false, func(t *testing.T, rec map[string]any) {
			if rec["banked"] != false || rec["unknown"] == true || !strings.Contains(fmt.Sprint(rec["reason"]), "137") {
				t.Fatalf("want the killed tar recorded, got %v", rec)
			}
		}},
		{"tar killed, then an empty listing", func(dir string) *scriptedRun {
			return &scriptedRun{podRun: &podRun{scratch: dir}, list: []func(sandbox.ExecOpts) (sandbox.ExecResult, error){nil, listedEmpty}, tar: []func(sandbox.ExecOpts) (sandbox.ExecResult, error){tarKilled}}
		}, false, func(t *testing.T, rec map[string]any) {
			if rec["banked"] != false || rec["empty"] != false || !strings.Contains(fmt.Sprint(rec["reason"]), "137") {
				t.Fatalf("want the killed tar recorded, got %v", rec)
			}
		}},
		{"empty, then the pod gone", func(dir string) *scriptedRun {
			return &scriptedRun{podRun: &podRun{scratch: dir}, list: []func(sandbox.ExecOpts) (sandbox.ExecResult, error){listedEmpty, podGone}}
		}, true, func(t *testing.T, rec map[string]any) {
			if rec["empty"] != true || rec["unknown"] == true {
				t.Fatalf("want the scratch recorded empty, got %v", rec)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "floor.json"), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
			s := tmpStore(t)
			if tc.dropFails {
				s = &dropFailsOnce{RunStore: s}
			}
			run := tc.run(dir)
			rec := bankedAtTeardown(t, s, "run-scratch-later-try", run)
			if run.lists < 2 {
				t.Fatalf("precondition: the teardown listed the scratch %d time(s), want a second try — this proves nothing", run.lists)
			}
			tc.check(t, rec)
		})
	}
}
