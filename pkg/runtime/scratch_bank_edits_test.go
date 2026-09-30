package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// refuseEventsStore refuses the appends of one event type: the first times of
// them, every one while times is negative.
type refuseEventsStore struct {
	store.RunStore
	typ     store.EventType
	mu      sync.Mutex
	times   int
	refused int
}

func (s *refuseEventsStore) Unwrap() store.RunStore { return s.RunStore }

func (s *refuseEventsStore) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	s.mu.Lock()
	refuse := evt.Type == s.typ && (s.times < 0 || s.refused < s.times)
	if refuse {
		s.refused++
	}
	s.mu.Unlock()
	if refuse {
		return nil, errors.New("store: insert event: connection reset by peer")
	}
	return s.RunStore.AppendEvent(ctx, runID, evt)
}

// heal lets every later append through.
func (s *refuseEventsStore) heal() {
	s.mu.Lock()
	s.times = 0
	s.mu.Unlock()
}

// isScratchRefusal reports err as SCRATCH_NOT_PORTABLE.
func isScratchRefusal(err error) bool {
	var rt *RuntimeError
	return errors.As(err, &rt) && rt.Code == ErrCodeScratchNotPortable
}

// TestResume_aHandoverRecordSurvivesAStoreBlip: the record of a bank restored
// into a host directory is what a later resume on that host decides from. A
// store blip on it is tried again; a store that refuses it for its whole
// budget fails that resume with no code, the bank kept, and the next resume
// restores it again — a later resume never finds the handover missing, so it
// is neither refused nor forced into reverting the host directory.
func TestResume_aHandoverRecordSurvivesAStoreBlip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		times int
	}{{"no blip", 0}, {"one blip", 1}, {"refused throughout, then healed", -1}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ITERION_MODE", "local")
			t.Setenv("ITERION_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			base := tmpStore(t)
			s := &refuseEventsStore{RunStore: base, typ: store.EventSandboxScratchRestored, times: tc.times}
			ctx := context.Background()
			const runID = "run-scratch-handover-record"
			d := &bindScratchDriver{root: t.TempDir()}
			work := t.TempDir()
			x := newStubExecutor()
			file := func() string { return filepath.Join(d.scratch(), "floor.json") }
			x.on("measure", func(map[string]any) (map[string]any, error) {
				if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
					return nil, err
				}
				return map[string]any{}, os.WriteFile(file(), []byte("v1"), 0o644)
			})
			x.on("report", func(map[string]any) (map[string]any, error) {
				b, err := os.ReadFile(file())
				if err != nil || string(b) != "v1" {
					return nil, fmt.Errorf("report: read %q, %v", b, err)
				}
				return map[string]any{}, os.WriteFile(file(), []byte("v2"), 0o644)
			})
			var final string
			x.on("final", func(map[string]any) (map[string]any, error) {
				b, err := os.ReadFile(file())
				if err != nil {
					return nil, fmt.Errorf("final: %w", err)
				}
				final = string(b)
				return map[string]any{}, nil
			})
			eng := func(hostState string) *Engine {
				e := New(handoverWorkflow(), s, x,
					WithLogger(iterlog.Nop()),
					WithWorkDir(work),
					WithSandboxHostStateOverride(hostState),
					WithSandboxDrivers(map[string]sandbox.DriverConstructor{
						"docker": func() (sandbox.Driver, error) { return d, nil },
					}),
				)
				e.recordRetryPause = time.Millisecond
				e.recordWriteLimit = 100 * time.Millisecond
				return e
			}
			if err := eng("none").Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
				t.Fatalf("Run: want ErrRunPaused, got %v", err)
			}
			err := eng("auto").Resume(ctx, runID, map[string]any{"ok": true})
			if tc.times < 0 {
				if err == nil || isScratchRefusal(err) || errors.Is(err, ErrRunPaused) {
					t.Fatalf("the resume whose handover record the store refused: %v, want a failure with no code", err)
				}
				s.heal()
				err = eng("auto").Resume(ctx, runID, map[string]any{"ok": true})
			}
			if !errors.Is(err, ErrRunPaused) {
				t.Fatalf("the resume into a host scratch: want ErrRunPaused at gate2, got %v", err)
			}
			restored := eventsOf(t, base, runID, store.EventSandboxScratchRestored)
			if len(restored) != 1 || restored[0].Data["host_backed"] != true {
				t.Fatalf("the handover's record: %v, want one, host-backed", dataOf(restored))
			}
			if err := eng("auto").Resume(ctx, runID, map[string]any{"ok": true}); err != nil || final != "v2" {
				t.Fatalf("a later resume on that host: err=%v final=%q, want it run on the host directory's v2", err, final)
			}
		})
	}
}

// editableWorkflow writes the scratch in measure, gives mid (its kind the
// test's) a pass after a first gate, and reads the scratch in final after a
// second.
func editableWorkflow(mid ir.Node) *ir.Workflow {
	human := func(id string) *ir.HumanNode {
		return &ir.HumanNode{BaseNode: ir.BaseNode{ID: id}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}}
	}
	id := mid.NodeID()
	return &ir.Workflow{
		Name:  "scratch_edits",
		Entry: "measure",
		Nodes: map[string]ir.Node{
			"measure": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "measure"}},
			"gate":    human("gate"),
			id:        mid,
			"gate2":   human("gate2"),
			"final":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "final"}},
			"done":    &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "measure", To: "gate"}, {From: "gate", To: id}, {From: id, To: "gate2"},
			{From: "gate2", To: "final"}, {From: "final", To: "done"},
		},
		Sandbox: &ir.SandboxSpec{Mode: "inline", Image: "example.invalid/iterion-sandbox:test", HostState: "none"},
	}
}

// lostSecondExecution runs launch: measure writes v1 and parks at gate (bank
// B1); the resume restores B1, runs mid, parks at gate2, and its pod is gone at
// teardown — an unknown record, B1 kept. It returns an engine maker on the same
// store and driver, and where final records what it read.
func lostSecondExecution(t *testing.T, s store.RunStore, runID string, launch *ir.Workflow, mid string, midWrites bool) (func(wf *ir.Workflow, hash string, force bool) *Engine, *string) {
	t.Helper()
	ctx := context.Background()
	d := &podDriver{root: t.TempDir()}
	x := newStubExecutor()
	file := func() string { return filepath.Join(d.scratch(), "floor.json") }
	x.on("measure", func(map[string]any) (map[string]any, error) {
		if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
			return nil, err
		}
		return map[string]any{}, os.WriteFile(file(), []byte("v1"), 0o644)
	})
	x.on(mid, func(map[string]any) (map[string]any, error) {
		if !midWrites {
			return map[string]any{}, nil
		}
		return map[string]any{}, os.WriteFile(file(), []byte("v2"), 0o644)
	})
	final := new(string)
	x.on("final", func(map[string]any) (map[string]any, error) {
		b, err := os.ReadFile(file())
		if err != nil {
			return nil, fmt.Errorf("final: %w", err)
		}
		*final = string(b)
		return map[string]any{}, nil
	})
	engWith := func(wf *ir.Workflow, drv sandbox.Driver, hash string, force bool) *Engine {
		e := New(wf, s, x, WithLogger(iterlog.Nop()), WithForceResume(force), WithSandboxDrivers(map[string]sandbox.DriverConstructor{
			"docker": func() (sandbox.Driver, error) { return drv, nil },
		}))
		e.workflowHash = hash
		e.scratchBankRetryPause = time.Millisecond
		return e
	}
	if err := engWith(launch, d, "sha256:launch", false).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	if err := engWith(launch, lostPodDriver{podDriver: d, lost: 1}, "sha256:launch", false).Resume(ctx, runID, map[string]any{"ok": true}); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("second execution: want ErrRunPaused at gate2, got %v", err)
	}
	banked := eventsOf(t, s, runID, store.EventSandboxScratchBanked)
	if len(banked) != 2 || banked[0].Data["banked"] != true || banked[1].Data["unknown"] != true {
		t.Fatalf("precondition: want B1 then an unknown teardown, got %v", dataOf(banked))
	}
	return func(wf *ir.Workflow, hash string, force bool) *Engine { return engWith(wf, d, hash, force) }, final
}

// TestResume_anEditedNodeKindDoesNotHideAStaleBank: where a node ran travels
// with its finish. An edit that makes the node which aged the bank an
// engine-side kind still has the stale bank refused — at the surface and in
// the engine, before the source check, naming the source change too.
func TestResume_anEditedNodeKindDoesNotHideAStaleBank(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	ctx := context.Background()
	s := tmpStore(t)
	const runID = "run-scratch-edited-kind"
	launch := editableWorkflow(&ir.AgentNode{BaseNode: ir.BaseNode{ID: "report"}})
	edited := editableWorkflow(&ir.ComputeNode{BaseNode: ir.BaseNode{ID: "report"}})
	eng, _ := lostSecondExecution(t, s, runID, launch, "report", true)
	if err := ValidateResumeScratch(ctx, s, mustLoadRun(t, s, runID), edited, false); !isScratchRefusal(err) {
		t.Fatalf("the surface, edited source: %v, want the stale bank refused", err)
	}
	err := eng(edited, "sha256:edited", false).Resume(ctx, runID, map[string]any{"ok": true})
	if !isScratchRefusal(err) || !strings.Contains(err.Error(), "the workflow source has also changed") {
		t.Fatalf("the engine, edited source: %v, want the stale bank refused, naming the source change", err)
	}
}

// TestResume_aRenamedNodeDoesNotRefuseAnExactBank: an edit that renames an
// engine-side node which finished after an exact bank does not make it
// stale: the source change is what the operator is shown.
func TestResume_aRenamedNodeDoesNotRefuseAnExactBank(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	ctx := context.Background()
	s := tmpStore(t)
	const runID = "run-scratch-renamed"
	launch := editableWorkflow(&ir.ComputeNode{BaseNode: ir.BaseNode{ID: "prep"}})
	edited := editableWorkflow(&ir.ComputeNode{BaseNode: ir.BaseNode{ID: "prepare"}})
	eng, _ := lostSecondExecution(t, s, runID, launch, "prep", false)
	if n := len(finishesOf(t, s, runID, "prep")); n != 1 {
		t.Fatalf("precondition: prep finished %d times, want 1", n)
	}
	if err := ValidateResumeScratch(ctx, s, mustLoadRun(t, s, runID), edited, false); err != nil {
		t.Fatalf("the surface, edited source: %v, want the exact bank accepted", err)
	}
	if err := eng(edited, "sha256:edited", false).Resume(ctx, runID, map[string]any{"ok": true}); !IsWorkflowSourceChanged(err) {
		t.Fatalf("the engine, edited source: %v, want the source change shown", err)
	}
}

// TestResume_theScratchRefusalNamesAnEditedSource: the scratch refusal comes
// before the source check and names it when the source changed — the one
// --force it asks for accepts both — and only then.
func TestResume_theScratchRefusalNamesAnEditedSource(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	for _, tc := range []struct {
		name  string
		hash  string
		named bool
	}{{"same source", "sha256:launch", false}, {"edited source", "sha256:edited", true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := tmpStore(t)
			ctx := context.Background()
			const runID = "run-scratch-names-source"
			d := &podDriver{root: t.TempDir()}
			x := newStubExecutor()
			x.on("measure", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
			x.on("report", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
			eng := func(hash string) *Engine {
				e := scratchEngine(s, x, d)
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
			err := eng(tc.hash).Resume(ctx, runID, map[string]any{"ok": true})
			if !isScratchRefusal(err) {
				t.Fatalf("resume: %v, want SCRATCH_NOT_PORTABLE", err)
			}
			if named := strings.Contains(err.Error(), "the workflow source has also changed"); named != tc.named {
				t.Fatalf("the refusal names the source change = %v, want %v: %v", named, tc.named, err)
			}
		})
	}
}

// TestResume_aForcedChildResumeRefusedLaterKeepsItsLineage: a forced lone
// resume of a child records the lineage's forsake only once it runs. Refused
// later — here the source override's record fails before the claim — it
// leaves the lineage refusing the child's next, unforced resume; run, it
// records the forsake.
func TestResume_aForcedChildResumeRefusedLaterKeepsItsLineage(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	ctx := context.Background()
	base := tmpStore(t)
	const id = "run-child-forsake"
	if _, err := adoptedChild(t, ctx, base, id, "sha256:launch", &podRun{scratch: t.TempDir()}); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("the child: want it parked, got %v", err)
	}
	var ran bool
	lone := func(st store.RunStore, hash string, force bool) *Engine {
		x := newStubExecutor()
		x.on("read", func(map[string]any) (map[string]any, error) { ran = true; return map[string]any{}, nil })
		e := New(scratchChildWorkflow(), st, x, WithWorkDir(t.TempDir()), WithSandboxOverride("none"), WithForceResume(force))
		e.workflowHash = hash
		return e
	}
	blip := &refuseEventsStore{RunStore: base, typ: store.EventRunResumeOverride, times: 1}
	if err := lone(blip, "sha256:edited", true).Resume(ctx, id, map[string]any{"acknowledge_recovery": "continue"}); err == nil || ran {
		t.Fatalf("the forced lone resume whose override record failed: err=%v ran=%v, want it refused before any node", err, ran)
	}
	if err := lone(base, "sha256:launch", false).Resume(ctx, id, map[string]any{"acknowledge_recovery": "continue"}); !isScratchRefusal(err) || ran {
		t.Fatalf("the next, unforced lone resume: err=%v ran=%v, want it still refused", err, ran)
	}
	if err := lone(base, "sha256:launch", true).Resume(ctx, id, map[string]any{"acknowledge_recovery": "continue"}); err != nil || !ran {
		t.Fatalf("a forced lone resume that runs: err=%v ran=%v", err, ran)
	}
	forsaken := false
	for _, ev := range eventsOf(t, base, id, store.EventSandboxShared) {
		if ev.Data["adopted"] == false && ev.Data["forced"] == true {
			forsaken = true
		}
	}
	if !forsaken {
		t.Fatalf("the forced lone resume that ran recorded no forsake: %v", dataOf(eventsOf(t, base, id, store.EventSandboxShared)))
	}
}

// TestResume_aRewindReadsTheLoopFromTheExecutingSource: whether a node lies
// on a cycle travels with its finish, as where it ran does. A node that
// finished passes of a loop after the bank keeps it stale once a rewind drops
// it, under an edit that removes the loop — even when it finished again under
// that edit; a node that ran off any cycle gives its pass back to the rewind,
// under an edit that adds one.
func TestResume_aRewindReadsTheLoopFromTheExecutingSource(t *testing.T) {
	loopWF := func(loop bool) *ir.Workflow {
		wf := &ir.Workflow{
			Name:  "scratch_loop",
			Entry: "a",
			Nodes: map[string]ir.Node{
				"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
				"b":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
				"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			},
			Edges: []*ir.Edge{{From: "a", To: "b"}, {From: "b", To: "done"}},
		}
		if loop {
			wf.Edges = append(wf.Edges, &ir.Edge{From: "b", To: "a"})
		}
		return wf
	}
	for _, tc := range []struct {
		name         string
		ranInLoop    bool
		thenOffIt    bool
		staleRefused bool
	}{
		{"ran in a loop, resumed without it", true, false, true},
		{"ran in a loop, then off it, resumed without it", true, true, true},
		{"ran off any cycle, resumed with a loop", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tmpStore(t)
			ctx := context.Background()
			const runID = "run-scratch-rewound-loop"
			if _, err := s.CreateRun(ctx, runID, "scratch_loop", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AppendEvent(ctx, runID, store.Event{Type: store.EventSandboxScratchBanked, Data: map[string]any{"banked": true, "empty": false, "bytes": 10}}); err != nil {
				t.Fatal(err)
			}
			executing := New(loopWF(tc.ranInLoop), s, newStubExecutor(), WithLogger(iterlog.Nop()))
			for range 2 {
				if err := executing.emit(ctx, runID, store.EventNodeFinished, "b", map[string]any{"output": map[string]any{}}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.thenOffIt {
				edited := New(loopWF(false), s, newStubExecutor(), WithLogger(iterlog.Nop()))
				if err := edited.emit(ctx, runID, store.EventNodeFinished, "b", map[string]any{"output": map[string]any{}}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.AppendEvent(ctx, runID, store.Event{Type: store.EventRunRewound, Data: map[string]any{"dropped_nodes": []string{"b"}}}); err != nil {
				t.Fatal(err)
			}
			err := ValidateResumeScratch(ctx, s, mustLoadRun(t, s, runID), loopWF(!tc.ranInLoop), false)
			if refused := isScratchRefusal(err); refused != tc.staleRefused || (!refused && err != nil) {
				t.Fatalf("resumed under the edited source: %v, want the stale bank refused = %v", err, tc.staleRefused)
			}
		})
	}
}

// TestResume_aForceGivenBeforeALossWasShownDoesNotWaiveIt: a loss only a
// restore finds — no sandbox to restore into, a bank that is gone — is not
// waived by a --force given before the operator was shown it, as one given
// for an edited source is: that resume is refused and the loss recorded, and
// the next --force, given knowing it, goes on without the scratch. A bank
// that is gone is refused before the claim from then on.
func TestResume_aForceGivenBeforeALossWasShownDoesNotWaiveIt(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	for _, loss := range []string{lossNoSandbox, lossBankGone} {
		t.Run(loss, func(t *testing.T) {
			s := tmpStore(t)
			ctx := context.Background()
			const runID = "run-scratch-unseen-loss"
			d := &podDriver{root: t.TempDir()}
			x := newStubExecutor()
			x.on("measure", func(map[string]any) (map[string]any, error) {
				if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
					return nil, err
				}
				return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), "floor.json"), []byte("v1"), 0o644)
			})
			reportRan := false
			x.on("report", func(map[string]any) (map[string]any, error) {
				reportRan = true
				return map[string]any{}, nil
			})
			launch := scratchEngine(s, x, d)
			launch.workflowHash = "sha256:launch"
			if err := launch.Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
				t.Fatalf("Run: want ErrRunPaused, got %v", err)
			}
			if b := eventsOf(t, s, runID, store.EventSandboxScratchBanked); len(b) != 1 || b[0].Data["banked"] != true {
				t.Fatalf("precondition: want the scratch banked, got %v", dataOf(b))
			}
			if loss == lossBankGone {
				if err := store.AsScratchBankStore(s).DeleteScratchBank(ctx, runID); err != nil {
					t.Fatal(err)
				}
			}
			resume := func(force bool) error {
				e := scratchEngine(s, x, d)
				if loss == lossNoSandbox {
					e = New(scratchWorkflow(), s, x, WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir()), WithSandboxOverride("none"))
				}
				e.forceResume = force
				e.workflowHash = "sha256:edited"
				return e.Resume(ctx, runID, map[string]any{"ok": true})
			}
			if err := resume(false); !IsWorkflowSourceChanged(err) || isScratchRefusal(err) {
				t.Fatalf("precondition: the unforced resume: %v, want only the source change shown", err)
			}
			if err := resume(true); !isScratchRefusal(err) || reportRan {
				t.Fatalf("the --force given for the source: %v, ran=%v, want the unseen loss refused and nothing run", err, reportRan)
			}
			if loss == lossBankGone {
				if err := ValidateResumeScratch(ctx, s, mustLoadRun(t, s, runID), scratchWorkflow(), false); !isScratchRefusal(err) {
					t.Fatalf("the surface, after the refusal: %v, want the gone bank refused", err)
				}
				claims := len(eventsOf(t, s, runID, store.EventRunResumed))
				if err := resume(false); !isScratchRefusal(err) || reportRan {
					t.Fatalf("the next unforced resume: %v, ran=%v, want the gone bank refused", err, reportRan)
				}
				if n := len(eventsOf(t, s, runID, store.EventRunResumed)); n != claims {
					t.Fatalf("the gone bank was refused after the claim (%d run_resumed, was %d), want before it", n, claims)
				}
			}
			if err := resume(true); err != nil || !reportRan {
				t.Fatalf("the --force given knowing the loss: %v, ran=%v, want the run through without its scratch", err, reportRan)
			}
		})
	}
}

// TestResume_aPlainRestoreRecordDoesNotHoldTheResume: the record of a bank
// restored into a container-local scratch decides nothing a later resume
// reads: tried once, best-effort — a store that refuses it neither holds the
// resume on a record's budget nor fails it.
func TestResume_aPlainRestoreRecordDoesNotHoldTheResume(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	base := tmpStore(t)
	s := &refuseEventsStore{RunStore: base, typ: store.EventSandboxScratchRestored, times: -1}
	ctx := context.Background()
	const runID = "run-scratch-plain-record"
	d := &podDriver{root: t.TempDir()}
	x := newStubExecutor()
	x.on("measure", func(map[string]any) (map[string]any, error) {
		if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
			return nil, err
		}
		return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), "floor.json"), []byte("v1"), 0o644)
	})
	x.on("report", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
	if err := scratchEngine(base, x, d).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	e := scratchEngine(s, x, d)
	e.recordRetryPause = time.Millisecond
	e.recordWriteLimit = 100 * time.Millisecond
	if err := e.Resume(ctx, runID, map[string]any{"ok": true}); err != nil {
		t.Fatalf("the resume whose plain restore record the store refused: %v, want it through", err)
	}
	if s.refused != 1 {
		t.Fatalf("the plain restore record was tried %d times, want once", s.refused)
	}
}

// TestResume_aStaleRestoreRecordSurvivesAStoreBlip: a stale bank --force
// restored is the run's scratch again, and a later resume reads that from
// the restore's record: a store blip on it is tried again, not lost.
func TestResume_aStaleRestoreRecordSurvivesAStoreBlip(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	ctx := context.Background()
	base := tmpStore(t)
	s := &refuseEventsStore{RunStore: base, typ: store.EventSandboxScratchRestored}
	const runID = "run-scratch-stale-record"
	launch := editableWorkflow(&ir.AgentNode{BaseNode: ir.BaseNode{ID: "report"}})
	eng, _ := lostSecondExecution(t, s, runID, launch, "report", true)
	s.mu.Lock()
	s.times = s.refused + 1
	s.mu.Unlock()
	e := eng(launch, "sha256:launch", true)
	e.recordRetryPause = time.Millisecond
	if err := e.Resume(ctx, runID, map[string]any{"ok": true}); err != nil {
		t.Fatalf("the forced resume of a stale bank: %v", err)
	}
	var stale int
	for _, ev := range eventsOf(t, base, runID, store.EventSandboxScratchRestored) {
		if ev.Data["stale"] == true {
			stale++
		}
	}
	if stale != 1 || s.refused == 0 {
		t.Fatalf("the stale restore's record: %d written after %d refusals, want it written past the blip", stale, s.refused)
	}
}

// TestResume_aLossShownDoesNotWaiveAnother: the --force a shown loss allows
// is for that loss. A run refused for resuming without a sandbox, resumed with
// one while its bank is gone, is refused over that loss too before a --force
// goes on without the scratch.
func TestResume_aLossShownDoesNotWaiveAnother(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-another-loss"
	d := &podDriver{root: t.TempDir()}
	x := newStubExecutor()
	x.on("measure", func(map[string]any) (map[string]any, error) {
		if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
			return nil, err
		}
		return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), "floor.json"), []byte("v1"), 0o644)
	})
	reportRan := false
	x.on("report", func(map[string]any) (map[string]any, error) {
		reportRan = true
		return map[string]any{}, nil
	})
	if err := scratchEngine(s, x, d).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	forced := func(sandboxed bool) error {
		e := scratchEngine(s, x, d)
		if !sandboxed {
			e = New(scratchWorkflow(), s, x, WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir()), WithSandboxOverride("none"))
		}
		e.forceResume = true
		return e.Resume(ctx, runID, map[string]any{"ok": true})
	}
	if err := forced(false); !errors.Is(err, errResumedWithoutSandbox) || reportRan {
		t.Fatalf("a first --force without a sandbox: %v, ran=%v, want the missing sandbox refused", err, reportRan)
	}
	if err := store.AsScratchBankStore(s).DeleteScratchBank(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if err := forced(true); !isScratchRefusal(err) || errors.Is(err, errResumedWithoutSandbox) || reportRan {
		t.Fatalf("a --force with a sandbox, the bank gone: %v, ran=%v, want the gone bank refused — the operator was shown another loss", err, reportRan)
	}
	if err := forced(true); err != nil || !reportRan {
		t.Fatalf("the --force given knowing the gone bank: %v, ran=%v, want the run through without its scratch", err, reportRan)
	}
}
