package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
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

// parkWorkflow is scratchWorkflow with an LLM router, which runs in the
// sandbox, and a condition router, which does not.
func parkWorkflow() *ir.Workflow {
	wf := scratchWorkflow()
	wf.Nodes["route"] = &ir.RouterNode{BaseNode: ir.BaseNode{ID: "route"}, RouterMode: ir.RouterLLM}
	wf.Nodes["pick"] = &ir.RouterNode{BaseNode: ir.BaseNode{ID: "pick"}, RouterMode: ir.RouterCondition}
	wf.Nodes["fix"] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: "fix"}}
	wf.Edges = append(wf.Edges, &ir.Edge{From: "fix", To: "fix", LoopName: "again"})
	return wf
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
	measured := store.Event{Type: store.EventNodeFinished, NodeID: "measure"}
	failed := store.Event{Type: store.EventNodeFinished, NodeID: "report", Data: map[string]any{"error": "exec: command terminated with exit code 137"}}
	answered := store.Event{Type: store.EventNodeFinished, NodeID: "report", Data: map[string]any{nodeFinishedAnswered: true}}
	routed := store.Event{Type: store.EventNodeFinished, NodeID: "route"}
	picked := store.Event{Type: store.EventNodeFinished, NodeID: "pick"}
	rewound := store.Event{Type: store.EventRunRewound, NodeID: "report", Data: map[string]any{"dropped_nodes": []string{"report"}}}
	fixed := store.Event{Type: store.EventNodeFinished, NodeID: "fix"}
	rewoundLoop := store.Event{Type: store.EventRunRewound, NodeID: "fix", Data: map[string]any{"dropped_nodes": []string{"fix"}}}
	forcedWithout := store.Event{Type: store.EventSandboxScratchRestored, Data: map[string]any{"restored": false, "accepted": true, "reason": "gone"}}
	forcedStale := store.Event{Type: store.EventSandboxScratchRestored, Data: map[string]any{"restored": true, "bytes": 10, "stale": true}}
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
		{"a bank, then a node that failed", []store.Event{banked, failed}, scratchPark{recorded: true, banked: true}, false, false},
		{"a bank, then an LLM router", []store.Event{banked, routed}, scratchPark{recorded: true, banked: true, advanced: true}, true, true},
		{"a bank, then a condition router", []store.Event{banked, picked}, scratchPark{recorded: true, banked: true}, false, false},
		{"a bank, a node, then a rewind that dropped it", []store.Event{banked, ran, rewound}, scratchPark{recorded: true, banked: true}, false, false},
		{"a bank, two nodes, then a rewind that dropped one", []store.Event{banked, measured, ran, rewound}, scratchPark{recorded: true, banked: true, advanced: true}, true, true},
		{"a bank, a rewind, then the node again", []store.Event{banked, ran, rewound, ran}, scratchPark{recorded: true, banked: true, advanced: true}, true, true},
		{"a bank, a looped node, then a rewind that dropped it", []store.Event{banked, fixed, rewoundLoop}, scratchPark{recorded: true, banked: true, advanced: true}, true, true},
		{"a start, then a bank", []store.Event{started, banked}, scratchPark{recorded: true, banked: true}, false, false},
		{"a start, then a refusal", []store.Event{started, refused}, scratchPark{recorded: true, reason: "over the cap"}, true, true},
		{"a bank, then an execution that wrote no record", []store.Event{banked, resumed}, scratchPark{recorded: true, banked: true, superseded: true}, false, false},
		{"a bank, then an execution that ran a node and wrote no record", []store.Event{banked, resumed, ran}, scratchPark{recorded: true, banked: true, advanced: true, superseded: true}, true, false},
		{"a refusal, then an execution that wrote no record", []store.Event{refused, resumed}, scratchPark{recorded: true, reason: "over the cap", superseded: true}, true, false},
		{"a bank, then an execution whose teardown could not read", []store.Event{banked, resumed, ran, unknown}, scratchPark{recorded: true, banked: true, advanced: true}, true, true},
		{"a bank, then an execution that banked again", []store.Event{banked, resumed, ran, banked}, scratchPark{recorded: true, banked: true}, false, false},
		{"a bank forced past, then an unread teardown", []store.Event{banked, resumed, forcedWithout, ran, unknown}, scratchPark{recorded: true, unknown: true, reason: "gone"}, false, false},
		{"a bank forced past, then an execution that wrote no record", []store.Event{banked, resumed, forcedWithout, ran}, scratchPark{recorded: true, forsaken: true, superseded: true}, false, false},
		{"a stale bank forced back", []store.Event{banked, ran, resumed, forcedStale}, scratchPark{recorded: true, banked: true, superseded: true}, false, false},
		{"a stale bank forced back, then a node", []store.Event{banked, ran, resumed, forcedStale, ran, unknown}, scratchPark{recorded: true, banked: true, advanced: true}, true, true},
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
			got, err := lastScratchPark(ctx, s, parkWorkflow(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("park %+v, want %+v", got, tc.want)
			}
			cause, err := scratchRefusal(ctx, s, parkWorkflow(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if (cause != "") != tc.engine {
				t.Fatalf("engine: refusal %q, want refused=%v", cause, tc.engine)
			}
			serr := ValidateResumeScratch(ctx, s, mustLoadRun(t, s, runID), parkWorkflow(), false)
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
	if err := scratchEngine(t, s, x, d).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	host := os.Getenv("TMPDIR")
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	err := scratchEngine(t, s, x, d).Resume(ctx, runID, map[string]any{"ok": true})
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
	if err := scratchEngine(t, s, x, d).Resume(ctx, runID, map[string]any{"ok": true}); err != nil {
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

// racingTar runs commands like podRun; its first races archives come back
// the way GNU tar reports a member that changed while it read it.
type racingTar struct {
	*podRun
	races, tars int
}

func (r *racingTar) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	res, err := r.podRun.Exec(ctx, argv, opts)
	if err != nil || res.ExitCode != 0 || !strings.Contains(strings.Join(argv, " "), "-czf") {
		return res, err
	}
	r.tars++
	if r.tars <= r.races {
		fmt.Fprintf(opts.Stderr, "tar: ./server.log: file changed as we read it\n%s\n", sandbox.KubectlRemoteExit1)
		res.ExitCode = 1
	}
	return res, nil
}

// TestBankScratch_aRaceIsArchivedAgainThenNamed: an archive that caught a
// member changing may hold no state the scratch was ever in. tar runs
// again; a clean archive is banked as such, and one that raced on every
// try is banked with the members it caught named.
func TestBankScratch_aRaceIsArchivedAgainThenNamed(t *testing.T) {
	for _, tc := range []struct {
		name        string
		races, tars int
		raced       []string
	}{
		{"a race, then a clean archive", 1, 2, nil},
		{"a race on every archive", 99, scratchTarAttempts, []string{"./server.log"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tmpStore(t)
			ctx := context.Background()
			const runID = "run-scratch-raced"
			if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "server.log"), []byte("listening\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run := &racingTar{podRun: &podRun{scratch: dir}, races: tc.races}
			got := bankScratch(ctx, run, sandboxScratchContainerPath, store.AsScratchBankStore(s), runID, scratchBankMaxBytes)
			if !got.banked || run.tars != tc.tars || fmt.Sprint(got.raced) != fmt.Sprint(tc.raced) {
				t.Fatalf("banked=%v after %d archive(s), raced %v; want banked after %d, raced %v (%+v)", got.banked, run.tars, got.raced, tc.tars, tc.raced, got)
			}
		})
	}
}

// racingPods starts pods like podDriver whose teardown's tar races on
// every archive.
type racingPods struct{ *podDriver }

func (d racingPods) Start(ctx context.Context, p sandbox.PreparedSpec, info sandbox.RunInfo) (sandbox.Run, error) {
	run, err := d.podDriver.Start(ctx, p, info)
	if err != nil {
		return nil, err
	}
	return &racingTar{podRun: run.(*podRun), races: 99}, nil
}

// TestResume_aRacedBankIsRestoredAndSaysSo: a bank whose members raced on
// every archive is restored, and the restore says it holds them as caught.
func TestResume_aRacedBankIsRestoredAndSaysSo(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-raced-restore"
	d := &podDriver{root: t.TempDir()}
	x := newStubExecutor()
	x.on("measure", func(map[string]any) (map[string]any, error) {
		if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
			return nil, err
		}
		return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), "server.log"), []byte("listening\n"), 0o644)
	})
	x.on("report", func(map[string]any) (map[string]any, error) {
		_, err := os.Stat(filepath.Join(d.scratch(), "server.log"))
		return map[string]any{}, err
	})
	eng := func(drv sandbox.Driver) *Engine {
		return New(scratchWorkflow(), s, x, WithLogger(iterlog.Nop()), WithSandboxDrivers(map[string]sandbox.DriverConstructor{
			"docker": func() (sandbox.Driver, error) { return drv, nil },
		}))
	}
	if err := eng(racingPods{d}).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	banked := eventsOf(t, s, runID, store.EventSandboxScratchBanked)
	if len(banked) != 1 || banked[0].Data["banked"] != true || banked[0].Data["raced"] == nil {
		t.Fatalf("want the raced bank recorded with its members, got %v", dataOf(banked))
	}
	if err := eng(d).Resume(ctx, runID, map[string]any{"ok": true}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	restored := eventsOf(t, s, runID, store.EventSandboxScratchRestored)
	if len(restored) != 1 || restored[0].Data["restored"] != true || restored[0].Data["raced"] != true {
		t.Fatalf("want the restore of a raced bank to say so, got %v", dataOf(restored))
	}
}

// TestOnCycle: a node on a loop's or a foreach's cycle may have run more
// than once; one on none ran once.
func TestOnCycle(t *testing.T) {
	wf := &ir.Workflow{Edges: []*ir.Edge{
		{From: "a", To: "b"}, {From: "b", To: "c"}, {From: "c", To: "b", LoopName: "again"}, {From: "c", To: "d"},
		{From: "d", To: "d", ForeachName: "each"},
	}}
	for id, want := range map[string]bool{"a": false, "b": true, "c": true, "d": true, "x": false} {
		if got := onCycle(wf, id); got != want {
			t.Errorf("onCycle(%q) = %v, want %v", id, got, want)
		}
	}
	if !onCycle(nil, "a") {
		t.Error("a workflow not known may loop anywhere")
	}
}

// raceThenFail runs commands like podRun; its first archive races — complete,
// with tar's race warning — and every later one fails as fail says.
type raceThenFail struct {
	*podRun
	fail func(ctx context.Context) (sandbox.ExecResult, error)
	tars int
}

func (r *raceThenFail) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	if !strings.Contains(strings.Join(argv, " "), "-czf") {
		return r.podRun.Exec(ctx, argv, opts)
	}
	r.tars++
	if r.tars > 1 {
		return r.fail(ctx)
	}
	res, err := r.podRun.Exec(ctx, argv, opts)
	if err != nil || res.ExitCode != 0 {
		return res, err
	}
	fmt.Fprintf(opts.Stderr, "tar: ./server.log: file changed as we read it\n%s\n", sandbox.KubectlRemoteExit1)
	res.ExitCode = 1
	return res, nil
}

// TestBankScratch_keepsTheLastCompleteArchiveARaceLeft: a scratch that races
// is archived again; when that try is killed, or the budget ends it, the
// complete archive that raced is banked, raced — not refused.
func TestBankScratch_keepsTheLastCompleteArchiveARaceLeft(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(ctx context.Context) (sandbox.ExecResult, error)
	}{
		{"the pod killed", func(context.Context) (sandbox.ExecResult, error) {
			return sandbox.ExecResult{ExitCode: 137}, nil
		}},
		{"the budget ended", func(ctx context.Context) (sandbox.ExecResult, error) {
			return sandbox.ExecResult{ExitCode: -1}, context.DeadlineExceeded
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tmpStore(t)
			ctx := context.Background()
			const runID = "run-scratch-kept-archive"
			if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "server.log"), []byte("listening\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run := &raceThenFail{podRun: &podRun{scratch: dir}, fail: tc.fail}
			got := bankScratch(ctx, run, sandboxScratchContainerPath, store.AsScratchBankStore(s), runID, scratchBankMaxBytes)
			if run.tars < 2 || !got.banked || got.bytes == 0 || fmt.Sprint(got.raced) != "[./server.log]" {
				t.Fatalf("after %d archive(s): %+v, want the raced archive banked", run.tars, got)
			}
			body, err := store.AsScratchBankStore(s).OpenScratchBank(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()
			if err := checkBankExtracts(body); err != nil {
				t.Fatalf("the banked archive does not extract: %v", err)
			}
		})
	}
}

// bankReadFailsOnce answers its first bank read with a transport error.
type bankReadFailsOnce struct {
	store.RunStore
	failed bool
}

func (s *bankReadFailsOnce) PutScratchBank(ctx context.Context, runID string, body io.Reader, size int64) error {
	return store.AsScratchBankStore(s.RunStore).PutScratchBank(ctx, runID, body, size)
}

func (s *bankReadFailsOnce) OpenScratchBank(ctx context.Context, runID string) (io.ReadCloser, error) {
	if !s.failed {
		s.failed = true
		return nil, errors.New("blob: GET sessions/run/scratch.tgz: 503 Slow Down")
	}
	return store.AsScratchBankStore(s.RunStore).OpenScratchBank(ctx, runID)
}

func (s *bankReadFailsOnce) DeleteScratchBank(ctx context.Context, runID string) error {
	return store.AsScratchBankStore(s.RunStore).DeleteScratchBank(ctx, runID)
}

// TestResume_aHoldEndsWithItsSandbox: the command line resumes a run on one
// engine. A restore that failed holds the bank from that sandbox's teardown
// only: the next sandbox, restored, banks what its nodes wrote.
func TestResume_aHoldEndsWithItsSandbox(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	base := tmpStore(t)
	s := &bankReadFailsOnce{RunStore: base}
	ctx := context.Background()
	const runID = "run-scratch-hold-scope"
	d := &podDriver{root: t.TempDir()}
	wf := scratchWorkflow()
	wf.Nodes["again"] = &ir.HumanNode{BaseNode: ir.BaseNode{ID: "again"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}}
	wf.Edges = []*ir.Edge{{From: "measure", To: "gate"}, {From: "gate", To: "report"}, {From: "report", To: "again"}, {From: "again", To: "done"}}
	x := newStubExecutor()
	x.on("measure", func(map[string]any) (map[string]any, error) {
		if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
			return nil, err
		}
		return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), "floor.json"), []byte("{}"), 0o644)
	})
	x.on("report", func(map[string]any) (map[string]any, error) {
		return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), "report.json"), []byte("{}"), 0o644)
	})
	e := New(wf, s, x, WithLogger(iterlog.Nop()), WithSandboxDrivers(map[string]sandbox.DriverConstructor{
		"docker": func() (sandbox.Driver, error) { return d, nil },
	}))
	if err := e.Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	if err := e.Resume(ctx, runID, map[string]any{"ok": true}); err == nil {
		t.Fatal("the resume whose bank read failed went on — this proves nothing")
	}
	if err := base.SaveRun(ctx, resumable(t, base, runID)); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(ctx, runID, map[string]any{"ok": true}); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("the resume on the same engine: want the second park, got %v", err)
	}
	banked := eventsOf(t, base, runID, store.EventSandboxScratchBanked)
	if len(banked) != 2 || banked[1].Data["banked"] != true {
		t.Fatalf("the second sandbox's teardown did not bank what report wrote: %v", dataOf(banked))
	}
}

// bindPods starts pods like podDriver, on a driver that bind-mounts host
// directories: the scratch is then the host's.
type bindPods struct{ *podDriver }

func (d bindPods) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{SupportsImage: true, SupportsMounts: true, SupportsHostBindMounts: true}
}

// TestStartSandbox_aChildLearnsWhetherTheScratchDiesWithTheSandbox: the
// sandbox a parent hands its children says whether its scratch lives in the
// container, which a child that parks cannot take along.
func TestStartSandbox_aChildLearnsWhetherTheScratchDiesWithTheSandbox(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	t.Setenv("ITERION_HOME", t.TempDir())
	for _, tc := range []struct {
		name           string
		driver         sandbox.Driver
		hostState      string
		containerLocal bool
	}{
		{"a container-local scratch", &podDriver{root: t.TempDir()}, "none", true},
		{"a host-backed scratch", bindPods{&podDriver{root: t.TempDir()}}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf := scratchWorkflow()
			wf.Sandbox.HostState = tc.hostState
			s := tmpStore(t)
			ctx := context.Background()
			const runID = "run-scratch-share"
			if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
				t.Fatal(err)
			}
			e := New(wf, s, newStubExecutor(), WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir()), WithSandboxDrivers(map[string]sandbox.DriverConstructor{
				"docker": func() (sandbox.Driver, error) { return tc.driver, nil },
			}))
			cleanup, err := e.startSandbox(ctx, runID, e.workDir, "", nil)
			if err != nil {
				t.Fatalf("startSandbox: %v", err)
			}
			defer cleanup()
			if e.activeShare == nil || e.activeShare.ScratchContainerLocal != tc.containerLocal {
				t.Fatalf("the share handed to children: %+v, want ScratchContainerLocal=%v", e.activeShare, tc.containerLocal)
			}
		})
	}
}

// isolatedRecorder is a sandbox whose commands run in a process namespace of
// their own: it records them, answers the quiesce itself — never running a
// signal on the host — and runs the rest like podRun.
type isolatedRecorder struct {
	*podRun
	cmds          []string
	quiesceErr    error
	quiesceExit   int
	quiesceStderr string
	// tarBlocks: the archive runs until its context ends, and its client is
	// killed with it (exit -1) — a scratch too big for the budget.
	tarBlocks bool
	// resumeCtxErr: the context's error when the resume was delivered.
	resumeCtxErr error
}

func (r *isolatedRecorder) ProcessIsolated() bool { return true }

func (r *isolatedRecorder) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	cmd := strings.Join(argv, " ")
	r.cmds = append(r.cmds, cmd)
	if strings.Contains(cmd, "kill ") {
		if strings.Contains(cmd, "kill -STOP -1") {
			return sandbox.ExecResult{ExitCode: r.quiesceExit, Stderr: []byte(r.quiesceStderr)}, r.quiesceErr
		}
		r.resumeCtxErr = ctx.Err()
		return sandbox.ExecResult{}, nil
	}
	if r.tarBlocks && strings.Contains(cmd, "-czf") {
		<-ctx.Done()
		return sandbox.ExecResult{ExitCode: -1}, nil
	}
	return r.podRun.Exec(ctx, argv, opts)
}

// hostRecorder is isolatedRecorder on the host: no process namespace of its
// own.
type hostRecorder struct {
	*podRun
	cmds []string
}

func (r *hostRecorder) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	r.cmds = append(r.cmds, strings.Join(argv, " "))
	return r.podRun.Exec(ctx, argv, opts)
}

// TestBankScratch_quiescesTheSandboxBeforeTarReadsIt: in a sandbox of its
// own, every other process is stopped between the listing and the archive —
// a write tar reports nothing of cannot tear it; a quiesce that fails is
// recorded; a sandbox that may run on the host is never signalled.
func TestBankScratch_quiescesTheSandboxBeforeTarReadsIt(t *testing.T) {
	scratch := func(t *testing.T) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "state.db"), []byte("pages"), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	bank := func(t *testing.T, run sandbox.Run) scratchBanked {
		s := tmpStore(t)
		ctx := context.Background()
		if _, err := s.CreateRun(ctx, "run-scratch-quiesce", "wf", nil); err != nil {
			t.Fatal(err)
		}
		return bankScratch(ctx, run, sandboxScratchContainerPath, store.AsScratchBankStore(s), "run-scratch-quiesce", scratchBankMaxBytes)
	}
	order := func(cmds []string) (quiesce, tar, resume int) {
		quiesce, tar, resume = -1, -1, -1
		for i, c := range cmds {
			switch {
			case strings.Contains(c, "kill -STOP -1") && quiesce < 0:
				quiesce = i
			case strings.Contains(c, "-czf") && tar < 0:
				tar = i
			case strings.Contains(c, "kill -CONT -1") && resume < 0:
				resume = i
			}
		}
		return quiesce, tar, resume
	}
	t.Run("a sandbox of its own", func(t *testing.T) {
		run := &isolatedRecorder{podRun: &podRun{scratch: scratch(t)}}
		got := bank(t, run)
		q, tar, resume := order(run.cmds)
		if !got.banked || got.unquiesced != "" || q < 1 || tar < q || resume < tar {
			t.Fatalf("banked=%v unquiesced=%q, commands %q: want the listing, the quiesce, tar, then the processes resumed", got.banked, got.unquiesced, run.cmds)
		}
	})
	t.Run("a quiesce that fails", func(t *testing.T) {
		run := &isolatedRecorder{podRun: &podRun{scratch: scratch(t)}, quiesceErr: errors.New("error dialing backend")}
		got := bank(t, run)
		if !got.banked || !strings.Contains(got.unquiesced, "error dialing backend") || got.event()["unquiesced"] == nil {
			t.Fatalf("want the bank recorded, and why its sandbox was not stopped: %+v", got)
		}
		// The exec may have stopped the processes before it failed.
		if _, _, resume := order(run.cmds); resume < 0 {
			t.Fatalf("the processes a failed quiesce may have stopped were not resumed: %q", run.cmds)
		}
	})
	for _, exit := range []int{-1, 1, 137} {
		t.Run(fmt.Sprintf("a quiesce that ends exit %d", exit), func(t *testing.T) {
			run := &isolatedRecorder{podRun: &podRun{scratch: scratch(t)}, quiesceExit: exit}
			got := bank(t, run)
			if _, _, resume := order(run.cmds); !got.banked || !strings.Contains(got.unquiesced, fmt.Sprintf("exited %d", exit)) || resume < 0 {
				t.Fatalf("banked=%v unquiesced=%q: want the exit recorded and the processes it may have stopped resumed (%q)", got.banked, got.unquiesced, run.cmds)
			}
		})
	}
	t.Run("no host temporary file for the archive", func(t *testing.T) {
		s := tmpStore(t)
		ctx := context.Background()
		if _, err := s.CreateRun(ctx, "run-scratch-quiesce", "wf", nil); err != nil {
			t.Fatal(err)
		}
		run := &isolatedRecorder{podRun: &podRun{scratch: scratch(t)}}
		t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "gone"))
		got := bankScratch(ctx, run, sandboxScratchContainerPath, store.AsScratchBankStore(s), "run-scratch-quiesce", scratchBankMaxBytes)
		if _, _, resume := order(run.cmds); got.banked || !strings.Contains(got.reason, "no host temporary file") || resume < 0 {
			t.Fatalf("banked=%v reason=%q: want the banking to fail by name and the stopped processes resumed (%q)", got.banked, got.reason, run.cmds)
		}
	})
	t.Run("an archive that outlives the budget", func(t *testing.T) {
		s := tmpStore(t)
		if _, err := s.CreateRun(context.Background(), "run-scratch-quiesce", "wf", nil); err != nil {
			t.Fatal(err)
		}
		run := &isolatedRecorder{podRun: &podRun{scratch: scratch(t)}, tarBlocks: true}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		got := bankScratch(ctx, run, sandboxScratchContainerPath, store.AsScratchBankStore(s), "run-scratch-quiesce", scratchBankMaxBytes)
		if _, _, resume := order(run.cmds); got.banked || resume < 0 || run.resumeCtxErr != nil {
			t.Fatalf("banked=%v resume ctx=%v: want the stopped processes resumed on a budget of their own (%q)", got.banked, run.resumeCtxErr, run.cmds)
		}
	})
	t.Run("a quiesce that leaves processes running", func(t *testing.T) {
		run := &isolatedRecorder{podRun: &podRun{scratch: scratch(t)}, quiesceExit: scratchQuiescePartial, quiesceStderr: "not stopped: 2068(sh)"}
		got := bank(t, run)
		if _, _, resume := order(run.cmds); !got.banked || !strings.Contains(got.unquiesced, "2068(sh)") || resume < 0 {
			t.Fatalf("banked=%v unquiesced=%q: want the processes left running named, and the stopped ones resumed (%q)", got.banked, got.unquiesced, run.cmds)
		}
	})
	t.Run("a sandbox in the host's initial process namespace", func(t *testing.T) {
		run := &isolatedRecorder{podRun: &podRun{scratch: scratch(t)}, quiesceExit: 3, quiesceStderr: "the sandbox runs in the host's initial process namespace"}
		got := bank(t, run)
		if _, _, resume := order(run.cmds); !got.banked || !strings.Contains(got.unquiesced, "initial process namespace") || resume >= 0 {
			t.Fatalf("banked=%v unquiesced=%q: want the refusal recorded and nothing resumed (%q)", got.banked, got.unquiesced, run.cmds)
		}
	})
	t.Run("a sandbox that may run on the host", func(t *testing.T) {
		run := &hostRecorder{podRun: &podRun{scratch: scratch(t)}}
		got := bank(t, run)
		if q, _, resume := order(run.cmds); !got.banked || q >= 0 || resume >= 0 {
			t.Fatalf("banked=%v, commands %q: a sandbox not isolated must never be signalled", got.banked, run.cmds)
		}
		// Nothing was stopped: the record says so, as it does a quiesce that
		// failed.
		if !strings.Contains(got.unquiesced, "process namespace of its own") || got.event()["unquiesced"] == nil {
			t.Fatalf("unquiesced=%q: want the bank recorded as archived while nothing was stopped", got.unquiesced)
		}
	})
}

// putFailsOnce refuses the first bank upload.
type putFailsOnce struct {
	store.RunStore
	failed bool
}

func (s *putFailsOnce) PutScratchBank(ctx context.Context, runID string, body io.Reader, size int64) error {
	if !s.failed {
		s.failed = true
		return errors.New("blob: PUT sessions/run/scratch.tgz: 503 Slow Down")
	}
	return store.AsScratchBankStore(s.RunStore).PutScratchBank(ctx, runID, body, size)
}

func (s *putFailsOnce) OpenScratchBank(ctx context.Context, runID string) (io.ReadCloser, error) {
	return store.AsScratchBankStore(s.RunStore).OpenScratchBank(ctx, runID)
}

func (s *putFailsOnce) DeleteScratchBank(ctx context.Context, runID string) error {
	return store.AsScratchBankStore(s.RunStore).DeleteScratchBank(ctx, runID)
}

// TestBankScratch_anUploadIsTriedAgainOnTheSameArchive: the store's blip is
// not a reason to archive the scratch again.
func TestBankScratch_anUploadIsTriedAgainOnTheSameArchive(t *testing.T) {
	s := &putFailsOnce{RunStore: tmpStore(t)}
	ctx := context.Background()
	const runID = "run-scratch-upload-again"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "floor.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := &scriptedRun{podRun: &podRun{scratch: dir}, tar: []func(sandbox.ExecOpts) (sandbox.ExecResult, error){nil}}
	got := bankScratch(ctx, run, sandboxScratchContainerPath, store.AsScratchBankStore(s), runID, scratchBankMaxBytes)
	if !s.failed || !got.banked || run.tars != 1 {
		t.Fatalf("banked=%v after %d archive(s): want the one archive stored on the second upload (%+v)", got.banked, run.tars, got)
	}
}

// slowRacingArchive is a sandbox whose scratch holds one file that changes
// while tar reads it: each archive takes took, and races. Built in Go, the
// archive runs nothing on the host.
type slowRacingArchive struct {
	took time.Duration
	tars int
}

func (r *slowRacingArchive) Driver() string                { return "docker" }
func (r *slowRacingArchive) Cleanup(context.Context) error { return nil }
func (r *slowRacingArchive) ProcessIsolated() bool         { return true }
func (r *slowRacingArchive) Command(ctx context.Context, argv []string, _ sandbox.ExecOpts) *exec.Cmd {
	return exec.CommandContext(ctx, "false")
}

func (r *slowRacingArchive) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	cmd := strings.Join(argv, " ")
	switch {
	case strings.Contains(cmd, "find "):
		return sandbox.ExecResult{Stdout: []byte("./server.log\n")}, nil
	case strings.Contains(cmd, "-czf"):
		r.tars++
		select {
		case <-ctx.Done():
			return sandbox.ExecResult{ExitCode: -1}, ctx.Err()
		case <-time.After(r.took):
		}
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		body := []byte("listening\n")
		if err := tw.WriteHeader(&tar.Header{Name: "./server.log", Mode: 0o644, Size: int64(len(body))}); err != nil {
			return sandbox.ExecResult{}, err
		}
		_, _ = tw.Write(body)
		_ = tw.Close()
		_ = gz.Close()
		if _, err := opts.Stdout.Write(buf.Bytes()); err != nil {
			return sandbox.ExecResult{}, err
		}
		fmt.Fprintf(opts.Stderr, "tar: ./server.log: file changed as we read it\n")
		return sandbox.ExecResult{ExitCode: 1}, nil
	}
	return sandbox.ExecResult{}, nil
}

// TestBankScratch_archivesAgainOnlyWhatTheBudgetAllows: another archive of a
// racing scratch must fit, with its upload, what is left of the budget; when
// it cannot, the raced archive is banked at once.
func TestBankScratch_archivesAgainOnlyWhatTheBudgetAllows(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget time.Duration
		tars   int
	}{
		{"a budget for every try", time.Hour, scratchTarAttempts},
		{"a budget for one", 100 * time.Second, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := tmpStore(t)
				const runID = "run-scratch-budget"
				if _, err := s.CreateRun(context.Background(), runID, "wf", nil); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), tc.budget)
				defer cancel()
				run := &slowRacingArchive{took: 40 * time.Second}
				got := bankScratch(ctx, run, sandboxScratchContainerPath, store.AsScratchBankStore(s), runID, scratchBankMaxBytes)
				if !got.banked || len(got.raced) == 0 || run.tars != tc.tars {
					t.Fatalf("banked=%v raced=%v after %d archive(s), want banked raced after %d", got.banked, got.raced, run.tars, tc.tars)
				}
			})
		})
	}
}

// TestBankScratch_aSymlinkedScratchIsListedThroughTheLink: a scratch that is
// a link to a directory is listed as tar archives it — through the link —
// not read as empty, which would drop the previous bank.
func TestBankScratch_aSymlinkedScratchIsListedThroughTheLink(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-symlink"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "floor.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "scratch")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if got := bankScratch(ctx, localRun(link), sandboxScratchContainerPath, store.AsScratchBankStore(s), runID, scratchBankMaxBytes); !got.banked {
		t.Fatalf("a scratch linked to a directory holding a file: %+v, want it banked", got)
	}
}

// partlyQuiescedPods starts pods like podDriver whose quiesce leaves a
// process running — another user's.
type partlyQuiescedPods struct{ *podDriver }

func (d partlyQuiescedPods) Start(ctx context.Context, p sandbox.PreparedSpec, info sandbox.RunInfo) (sandbox.Run, error) {
	run, err := d.podDriver.Start(ctx, p, info)
	if err != nil {
		return nil, err
	}
	return &isolatedRecorder{podRun: run.(*podRun), quiesceExit: scratchQuiescePartial, quiesceStderr: "not stopped: 9(sh)"}, nil
}

// TestResume_aBankArchivedUnquiescedIsRestoredAndSaysSo: a bank archived
// while some of the sandbox's processes still ran is restored, and the
// restore says something may have written the scratch meanwhile.
func TestResume_aBankArchivedUnquiescedIsRestoredAndSaysSo(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-unquiesced"
	d := &podDriver{root: t.TempDir()}
	x := newStubExecutor()
	x.on("measure", func(map[string]any) (map[string]any, error) {
		if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
			return nil, err
		}
		return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), "state.db"), []byte("pages"), 0o644)
	})
	x.on("report", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
	eng := func(drv sandbox.Driver) *Engine {
		return New(scratchWorkflow(), s, x, WithLogger(iterlog.Nop()), WithSandboxDrivers(map[string]sandbox.DriverConstructor{
			"docker": func() (sandbox.Driver, error) { return drv, nil },
		}))
	}
	if err := eng(partlyQuiescedPods{d}).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	if banked := eventsOf(t, s, runID, store.EventSandboxScratchBanked); len(banked) != 1 || banked[0].Data["banked"] != true || !strings.Contains(fmt.Sprint(banked[0].Data["unquiesced"]), "9(sh)") {
		t.Fatalf("want the bank recorded with the process left running, got %v", dataOf(banked))
	}
	if err := eng(d).Resume(ctx, runID, map[string]any{"ok": true}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	restored := eventsOf(t, s, runID, store.EventSandboxScratchRestored)
	if len(restored) != 1 || restored[0].Data["restored"] != true || restored[0].Data["unquiesced"] != true {
		t.Fatalf("want the restore to say the bank was archived unquiesced, got %v", dataOf(restored))
	}
}

// dryScratchScript is script with its signal replaced by an echo: the copy a
// test may run on the host side, where the real signal would stop the test's
// own processes. A copy that still signals is never returned.
func dryScratchScript(t *testing.T, script string) string {
	t.Helper()
	dry := strings.NewReplacer("kill -STOP -1", "echo WOULD_STOP", "kill -CONT -1", "echo WOULD_CONT").Replace(script)
	if strings.Contains(dry, "kill") {
		t.Fatal("the dry copy still signals — not run")
	}
	return dry
}

// TestScratchQuiesceScripts_guardRefusesBeforeAnySignal: both scripts exit
// scratchQuiesceRefused before any signal when the process namespace is the
// host's initial one — here the test's own namespace stands in for it, so
// the guard is exercised wherever the test runs — and when it cannot be
// read.
func TestScratchQuiesceScripts_guardRefusesBeforeAnySignal(t *testing.T) {
	own, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		t.Skipf("no readable process namespace here: %v", err)
	}
	noReadlink := t.TempDir()
	if err := os.WriteFile(filepath.Join(noReadlink, "readlink"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{"quiesce": scratchQuiesceScript, "resume": scratchResumeScript} {
		for why, run := range map[string]*exec.Cmd{
			"the host's initial namespace": exec.Command("sh", "-c", strings.Replace(dryScratchScript(t, script), "pid:[4026531836]", own, 1)),
			"an unreadable namespace":      exec.Command("sh", "-c", dryScratchScript(t, script)),
		} {
			if why == "an unreadable namespace" {
				run.Env = append(os.Environ(), "PATH="+noReadlink+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			out, err := run.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != scratchQuiesceRefused || strings.Contains(string(out), "WOULD_") {
				t.Errorf("%s, %s: err=%v out=%q, want exit %d before any signal", name, why, err, out, scratchQuiesceRefused)
			}
		}
	}
}

// TestScratchQuiesceScript_dryScanNamesWhatRuns: in a process namespace of
// its own, the dry quiesce — which stops nothing — finds this test's process
// still running and names it, exit scratchQuiescePartial; in the host's
// initial namespace the guard refuses it first.
func TestScratchQuiesceScript_dryScanNamesWhatRuns(t *testing.T) {
	own, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		t.Skipf("no readable process namespace here: %v", err)
	}
	out, err := exec.Command("sh", "-c", dryScratchScript(t, scratchQuiesceScript)).CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the dry quiesce: err=%v out=%q, want it to exit non-zero", err, out)
	}
	if own == "pid:[4026531836]" {
		if exit.ExitCode() != scratchQuiesceRefused || strings.Contains(string(out), "WOULD_") {
			t.Fatalf("in the host's initial namespace: exit %d out=%q, want %d before any signal", exit.ExitCode(), out, scratchQuiesceRefused)
		}
		return
	}
	if exit.ExitCode() != scratchQuiescePartial || !strings.Contains(string(out), fmt.Sprintf(" %d(", os.Getpid())) {
		t.Fatalf("exit %d out=%q, want %d naming this test's process %d", exit.ExitCode(), out, scratchQuiescePartial, os.Getpid())
	}
}

// TestSharedChildParked_isRefusedAloneUnderAContainerLocalScratch: a child
// that declares no pause is adopted into a parent whose ${PROJECT_SCRATCH_DIR}
// lives in the container, and parks all the same — on a recovery pause, on a
// permission ask it does not declare. The external resume, an engine with no
// parent handle, refuses it SCRATCH_NOT_PORTABLE before any node runs without
// that scratch.
func TestSharedChildParked_isRefusedAloneUnderAContainerLocalScratch(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	ctx := context.Background()
	child := func() *ir.Workflow {
		return &ir.Workflow{
			Name:  "child",
			Entry: "write",
			Nodes: map[string]ir.Node{
				"write": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "write"}},
				"read":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "read"}},
				"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			},
			Edges: []*ir.Edge{{From: "write", To: "read"}, {From: "read", To: "done"}},
		}
	}
	pauseOnAuth := func(_ context.Context, err error, _ func(ErrorCode) int) (RecoveryAction, ErrorCode) {
		var rt *RuntimeError
		if errors.As(err, &rt) && rt.Code == ErrCodeAuthFailed {
			return RecoveryAction{Kind: RecoveryPauseForHuman, Reason: "model provider rejected credentials"}, ErrCodeAuthFailed
		}
		return RecoveryAction{Kind: RecoveryFailTerminal}, ErrCodeExecutionFailed
	}
	for _, tc := range []struct {
		name   string
		opts   []EngineOption
		read   func(map[string]any) (map[string]any, error)
		answer map[string]any
	}{
		{
			name: "a recovery pause",
			opts: []EngineOption{WithRecoveryDispatch(pauseOnAuth)},
			read: func(map[string]any) (map[string]any, error) {
				return nil, &RuntimeError{Code: ErrCodeAuthFailed, NodeID: "read", Message: "401 invalid token"}
			},
			answer: map[string]any{"acknowledge_recovery": "continue"},
		},
		{
			name: "a permission ask the child does not declare",
			read: func(map[string]any) (map[string]any, error) {
				return nil, &model.ErrNeedsInteraction{NodeID: "read", Backend: "claw", Questions: map[string]any{
					delegate.AskUserQuestionKey:     "Allow Bash(cat floor.json)?",
					permission.InteractionMarkerKey: permission.Marker("Bash", map[string]any{"command": "cat floor.json"}, "Bash(cat floor.json)"),
				}}
			},
			answer: map[string]any{delegate.AskUserQuestionKey: "allow"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := &podRun{scratch: t.TempDir()}
			wf := child()
			if workflowHasPausingNode(wf) {
				t.Fatal("precondition: the child declares no pause")
			}
			const id = "run-child"
			e, x, st := sharedTestEngine(t, wf, t.TempDir(), &SharedSandbox{Run: parent, ScratchContainerLocal: true}, tc.opts...)
			x.on("write", func(map[string]any) (map[string]any, error) {
				res, err := x.sandbox.Exec(ctx, []string{"sh", "-c", "mkdir -p " + sandboxScratchContainerPath + " && printf 31 > " + sandboxScratchContainerPath + "/floor.json"}, sandbox.ExecOpts{})
				if err != nil || res.ExitCode != 0 {
					return nil, errors.New("write: the scratch could not be written")
				}
				return map[string]any{}, nil
			})
			x.on("read", tc.read)
			if err := e.Run(ctx, id, nil); !errors.Is(err, ErrRunPaused) {
				t.Fatalf("the child: want it parked, got %v", err)
			}
			if x.sandbox != sandbox.Run(parent) {
				t.Fatalf("precondition: the child was not adopted into the parent's sandbox (%v)", x.sandbox)
			}
			alone := newStubExecutor()
			ran := false
			alone.on("read", func(map[string]any) (map[string]any, error) { ran = true; return map[string]any{}, nil })
			err := New(wf, st, alone, WithWorkDir(t.TempDir()), WithSandboxOverride("none")).Resume(ctx, id, tc.answer)
			var rt *RuntimeError
			if !errors.As(err, &rt) || rt.Code != ErrCodeScratchNotPortable {
				t.Fatalf("the child resumed on its own: want SCRATCH_NOT_PORTABLE, got %v", err)
			}
			if ran {
				t.Fatal("read ran outside the parent's sandbox, without its scratch")
			}
		})
	}
}

// scratchChildWorkflow is a subbot child that writes ${PROJECT_SCRATCH_DIR}
// in write, and needs it in read.
func scratchChildWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "child",
		Entry: "write",
		Nodes: map[string]ir.Node{
			"write": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "write"}},
			"read":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "read"}},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "write", To: "read"}, {From: "read", To: "done"}},
	}
}

// parkOnAuth pauses a node whose credentials the provider rejected, as the
// default recipes do.
func parkOnAuth(_ context.Context, err error, _ func(ErrorCode) int) (RecoveryAction, ErrorCode) {
	var rt *RuntimeError
	if errors.As(err, &rt) && rt.Code == ErrCodeAuthFailed {
		return RecoveryAction{Kind: RecoveryPauseForHuman, Reason: "model provider rejected credentials"}, ErrCodeAuthFailed
	}
	return RecoveryAction{Kind: RecoveryFailTerminal}, ErrCodeExecutionFailed
}

// adoptedChild runs child id adopted into parent, a sandbox whose scratch
// lives in the container, until read parks it on a recovery pause.
func adoptedChild(t *testing.T, ctx context.Context, st store.RunStore, id, hash string, parent *podRun) (*Engine, error) {
	t.Helper()
	x := &sandboxCapturingExecutor{stubExecutor: newStubExecutor()}
	x.on("write", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
	x.on("read", func(map[string]any) (map[string]any, error) {
		return nil, &RuntimeError{Code: ErrCodeAuthFailed, NodeID: "read", Message: "401 invalid token"}
	})
	e := New(scratchChildWorkflow(), st, x, WithWorkDir(t.TempDir()), WithSandboxOverride("none"), WithParentRunID("run-parent"),
		WithSharedSandbox(&SharedSandbox{Run: parent, WorkspaceFolder: t.TempDir(), ScratchContainerLocal: true}),
		WithRecoveryDispatch(parkOnAuth))
	e.workflowHash = hash
	e.recordRetryPause = time.Millisecond
	return e, e.Run(ctx, id, nil)
}

// shareRecordRefused refuses a child's sandbox_shared appends: the first
// times of them, every one when times is negative.
type shareRecordRefused struct {
	store.RunStore
	mu      sync.Mutex
	times   int
	refused int
}

func (s *shareRecordRefused) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	s.mu.Lock()
	refuse := evt.Type == store.EventSandboxShared && (s.times < 0 || s.refused < s.times)
	if refuse {
		s.refused++
	}
	s.mu.Unlock()
	if refuse {
		return nil, errors.New("store: insert event: connection reset by peer")
	}
	return s.RunStore.AppendEvent(ctx, runID, evt)
}

// TestAdoption_writesItsLineageRecordOrDoesNotAdopt: a lone resume of the
// child is refused from the adoption's record. A store blip on it is tried
// again — the record lands, and the parked child's lone resume is refused; a
// store that refuses it for the record's whole budget fails the adoption by
// name, and no node runs in the parent's sandbox without it.
func TestAdoption_writesItsLineageRecordOrDoesNotAdopt(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	const id = "run-child"
	t.Run("a store blip on the record", func(t *testing.T) {
		ctx := context.Background()
		st := &shareRecordRefused{RunStore: tmpStore(t), times: 1}
		if _, err := adoptedChild(t, ctx, st, id, "", &podRun{scratch: t.TempDir()}); !errors.Is(err, ErrRunPaused) {
			t.Fatalf("the child: want it parked, got %v", err)
		}
		if recs := eventsOf(t, st, id, store.EventSandboxShared); st.refused != 1 || len(recs) != 1 || recs[0].Data["scratch_container_local"] != true {
			t.Fatalf("refused=%d records=%v: want the record written on a later try", st.refused, dataOf(recs))
		}
		err := New(scratchChildWorkflow(), st, newStubExecutor(), WithWorkDir(t.TempDir()), WithSandboxOverride("none")).Resume(ctx, id, map[string]any{"acknowledge_recovery": "continue"})
		var rt *RuntimeError
		if !errors.As(err, &rt) || rt.Code != ErrCodeScratchNotPortable {
			t.Fatalf("the child resumed on its own: want SCRATCH_NOT_PORTABLE, got %v", err)
		}
	})
	t.Run("a store that refuses it throughout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		st := &shareRecordRefused{RunStore: tmpStore(t), times: -1}
		parent := &podRun{scratch: t.TempDir()}
		_, err := adoptedChild(t, ctx, st, id, "", parent)
		if err == nil || !strings.Contains(err.Error(), "could not be written") || st.refused < 2 {
			t.Fatalf("err=%v refused=%d: want the adoption failed by name, after tries", err, st.refused)
		}
		if entries, _ := os.ReadDir(parent.scratch); len(entries) != 0 {
			t.Fatalf("a node ran in the parent's sandbox without the record: %v", entries)
		}
	})
}

// TestNodeGateCanAsk: a permission gate reads as a pause when the policy it
// arms can ask — its mode asks, whatever its spelling or where it is
// declared, or its ask rules apply under a gate that is on — and only on the
// LLM nodes it governs: a tool node's permission is inert.
func TestNodeGateCanAsk(t *testing.T) {
	const schema = "schema empty:\n  ok: bool\n\n"
	for _, tc := range []struct {
		name string
		src  string
		want bool
	}{
		{"ask spelled Ask", schema + "agent act:\n  model: \"m\"\n  output: empty\n  permission: Ask\n\nworkflow child:\n  entry: act\n  act -> done\n", true},
		{"ask on the workflow", schema + "agent act:\n  model: \"m\"\n  output: empty\n\nworkflow child:\n  entry: act\n  permission: ask\n  act -> done\n", true},
		{"deny with an ask rule", schema + "judge review:\n  model: \"m\"\n  output: empty\n  ask: [\"Bash(git push:*)\"]\n\nworkflow child:\n  entry: review\n  permission: deny\n  review -> done\n", true},
		{"deny without an ask rule", schema + "agent act:\n  model: \"m\"\n  output: empty\n  permission: deny\n\nworkflow child:\n  entry: act\n  act -> done\n", false},
		{"ask on a tool node", schema + "tool lint:\n  command: \"true\"\n  output: empty\n  permission: ask\n\nworkflow child:\n  entry: lint\n  lint -> done\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := workflowHasPausingNode(compileBotText(t, tc.src)); got != tc.want {
				t.Fatalf("workflowHasPausingNode = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResume_forcedPastAnUnbankedScratchIsRecorded: --force past a scratch
// its teardown could not bank goes on without it, and the timeline says so.
func TestResume_acceptedPastAnUnbankedScratchIsRecorded(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-unbanked-forced"
	d := &podDriver{root: t.TempDir()}
	x := newStubExecutor()
	x.on("measure", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
	x.on("report", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
	if err := scratchEngine(t, s, x, d).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	if _, err := s.AppendEvent(ctx, runID, store.Event{Type: store.EventSandboxScratchBanked, Data: map[string]any{
		"banked": false, "empty": false, "reason": "the scratch compresses past the 256 MiB cap",
	}}); err != nil {
		t.Fatal(err)
	}
	accepting := scratchEngine(t, s, x, d)
	accepting.acceptScratchLoss = true
	if err := accepting.Resume(ctx, runID, map[string]any{"ok": true}); err != nil {
		t.Fatalf("Resume accepting the scratch's loss: %v", err)
	}
	restored := eventsOf(t, s, runID, store.EventSandboxScratchRestored)
	if len(restored) != 1 || restored[0].Data["restored"] != false || restored[0].Data["accepted"] != true || !strings.Contains(fmt.Sprint(restored[0].Data["reason"]), "could not bank") {
		t.Fatalf("the timeline after a resume past an unbanked scratch, its loss accepted: %v, want one record naming it", dataOf(restored))
	}
}

// TestResume_scratchAndLineageRefusalsComeBeforeTheSourceCheck: an edited
// source is refused only once the scratch and the lineage travel — the force
// the source refusal asks for must not waive a loss the operator was never
// shown.
func TestResume_scratchAndLineageRefusalsComeBeforeTheSourceCheck(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	refusedFirst := func(t *testing.T, err error) {
		t.Helper()
		var rt *RuntimeError
		if !errors.As(err, &rt) || rt.Code != ErrCodeScratchNotPortable || IsWorkflowSourceChanged(err) {
			t.Fatalf("an edited source over a loss: got %v, want SCRATCH_NOT_PORTABLE before the source check", err)
		}
	}
	t.Run("a scratch its teardown could not bank", func(t *testing.T) {
		s := tmpStore(t)
		ctx := context.Background()
		const runID = "run-scratch-edited"
		d := &podDriver{root: t.TempDir()}
		x := newStubExecutor()
		x.on("measure", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
		x.on("report", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
		eng := func(hash string) *Engine {
			e := scratchEngine(t, s, x, d)
			e.workflowHash = hash
			return e
		}
		if err := eng("sha256:launch").Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
			t.Fatalf("Run: want ErrRunPaused, got %v", err)
		}
		if _, err := s.AppendEvent(ctx, runID, store.Event{Type: store.EventSandboxScratchBanked, Data: map[string]any{
			"banked": false, "empty": false, "reason": "the scratch compresses past the 256 MiB cap",
		}}); err != nil {
			t.Fatal(err)
		}
		refusedFirst(t, eng("sha256:edited").Resume(ctx, runID, map[string]any{"ok": true}))
	})
	t.Run("a child whose parent's scratch lived in its container", func(t *testing.T) {
		ctx := context.Background()
		st := tmpStore(t)
		const id = "run-child-edited"
		if _, err := adoptedChild(t, ctx, st, id, "sha256:launch", &podRun{scratch: t.TempDir()}); !errors.Is(err, ErrRunPaused) {
			t.Fatalf("the child: want it parked, got %v", err)
		}
		lone := New(scratchChildWorkflow(), st, newStubExecutor(), WithWorkDir(t.TempDir()), WithSandboxOverride("none"))
		lone.workflowHash = "sha256:edited"
		refusedFirst(t, lone.Resume(ctx, id, map[string]any{"acknowledge_recovery": "continue"}))
	})
}
