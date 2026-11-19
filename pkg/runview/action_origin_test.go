package runview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// originCall is one store access the origin store recorded: the method,
// whether its context carried a run data binding (never the value-format
// intent), whether it came after the spawn took the run lock, and whether a
// frame of the engine's resume (engineResumeLoad) was on its stack.
type originCall struct {
	method                  string
	bound, afterRun, engine bool
}

// originStore is the filesystem store with the probes of the carrier-E
// witnesses: a one-shot hook run on ENTRY of a named method, before it
// delegates — of its next call, or of its next call a named function makes —
// and a record of the accesses of the methods it overrides. It forwards
// every call with the caller's context, so the binding policy under test
// stays in the native store, and it endorses itself as that store.
type originStore struct {
	*store.FilesystemRunStore
	mu      sync.Mutex
	before  map[string]originHook
	hookErr error
	locked  bool
	calls   []originCall
}

// originHook is an armed hook: h, for a call a frame of caller makes ("" for
// any call).
type originHook struct {
	caller string
	h      func() error
}

// engineResumeLoad names the frame of the engine's own load of the run it
// resumes, and engineClaimLoad the claim's fresh read after it.
const (
	engineResumeLoad = "pkg/runtime.(*Engine).ResumeWithHostInputs"
	engineClaimLoad  = "pkg/runtime.(*Engine).acquireResumeEntry"
)

// calledFrom reports whether a frame of fn (a qualified function name as the
// runtime spells it, matched as a suffix) is on the caller's stack.
func calledFrom(fn string) bool {
	pcs := make([]uintptr, 64)
	frames := goruntime.CallersFrames(pcs[:goruntime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, fn) {
			return true
		}
		if !more {
			return false
		}
	}
}

func (s *originStore) CheckBoundObservationStore(actual store.RunStore) error {
	if a, ok := actual.(*originStore); !ok || a != s {
		return fmt.Errorf("%w: %T is not this test double", store.ErrRunDataCapability, actual)
	}
	return nil
}

// arm runs h once, on entry of the next call to method.
func (s *originStore) arm(method string, h func() error) {
	s.armFrom(method, "", h)
}

// armFrom runs h once, on entry of the next call to method a frame of caller
// makes.
func (s *originStore) armFrom(method, caller string, h func() error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.before == nil {
		s.before = map[string]originHook{}
	}
	s.before[method] = originHook{caller: caller, h: h}
}

func (s *originStore) enter(ctx context.Context, method string) {
	_, bound := store.RunDataBindingFrom(ctx)
	engine := calledFrom(engineResumeLoad)
	s.mu.Lock()
	hook, ok := s.before[method]
	var h func() error
	if ok && (hook.caller == "" || calledFrom(hook.caller)) {
		h = hook.h
		delete(s.before, method)
	}
	s.calls = append(s.calls, originCall{method: method, bound: bound, afterRun: s.locked, engine: engine})
	s.mu.Unlock()
	if h == nil {
		return
	}
	if err := h(); err != nil {
		s.mu.Lock()
		s.hookErr = errors.Join(s.hookErr, err)
		s.mu.Unlock()
	}
}

func (s *originStore) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hookErr
}

func (s *originStore) recorded() []originCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]originCall(nil), s.calls...)
}

// engineCalls returns the methods of the recorded accesses the engine's
// resume made, in order.
func (s *originStore) engineCalls() []string {
	var out []string
	for _, c := range s.recorded() {
		if c.engine {
			out = append(out, c.method)
		}
	}
	return out
}

func (s *originStore) LockRun(ctx context.Context, id string) (store.RunLock, error) {
	s.enter(ctx, "LockRun")
	l, err := s.FilesystemRunStore.LockRun(ctx, id)
	s.mu.Lock()
	s.locked = true
	s.mu.Unlock()
	return l, err
}

func (s *originStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	s.enter(ctx, "LoadRun")
	return s.FilesystemRunStore.LoadRun(ctx, id)
}

func (s *originStore) SaveRun(ctx context.Context, r *store.Run) error {
	s.enter(ctx, "SaveRun")
	return s.FilesystemRunStore.SaveRun(ctx, r)
}

func (s *originStore) AppendEvent(ctx context.Context, id string, evt store.Event) (*store.Event, error) {
	s.enter(ctx, "AppendEvent")
	return s.FilesystemRunStore.AppendEvent(ctx, id, evt)
}

func (s *originStore) SaveCheckpoint(ctx context.Context, id string, cp *store.Checkpoint) error {
	s.enter(ctx, "SaveCheckpoint")
	return s.FilesystemRunStore.SaveCheckpoint(ctx, id, cp)
}

func (s *originStore) UpdateRunStatus(ctx context.Context, id string, status store.RunStatus, runErr string) error {
	s.enter(ctx, "UpdateRunStatus")
	return s.FilesystemRunStore.UpdateRunStatus(ctx, id, status, runErr)
}

func (s *originStore) SetRunBudgetOverrides(ctx context.Context, id string, o *store.RunBudgetOverrides) error {
	s.enter(ctx, "SetRunBudgetOverrides")
	return s.FilesystemRunStore.SetRunBudgetOverrides(ctx, id, o)
}

func (s *originStore) SetRunBudgetSnapshot(ctx context.Context, id string, b *store.RunBudget) error {
	s.enter(ctx, "SetRunBudgetSnapshot")
	return s.FilesystemRunStore.SetRunBudgetSnapshot(ctx, id, b)
}

func (s *originStore) PatchRunBudgetOverrides(ctx context.Context, id string, o *store.RunBudgetOverrides) error {
	s.enter(ctx, "PatchRunBudgetOverrides")
	return s.FilesystemRunStore.PatchRunBudgetOverrides(ctx, id, o)
}

func (s *originStore) LoadArtifact(ctx context.Context, runID, nodeID string, version int) (*store.Artifact, error) {
	s.enter(ctx, "LoadArtifact")
	return s.FilesystemRunStore.LoadArtifact(ctx, runID, nodeID, version)
}

// originFixture is an in-process service over the origin store with one
// resumable run (a compiled workflow, its hash, a checkpoint) born A.
type originFixture struct {
	svc    *Service
	native *store.FilesystemRunStore
	st     *originStore
	path   string
	source string
	hash   string
	origin store.RunObservation
}

const originRunID = "r"

func newOriginFixture(t *testing.T) *originFixture {
	t.Helper()
	t.Setenv("ITERION_RUNS_DETACHED", "0")
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	dir := t.TempDir()
	native, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := &originStore{FilesystemRunStore: native}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir()), WithStore(st))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		svc.Stop(ctx)
	})
	const source = "workflow resume_probe:\n  entry: done\n"
	path := filepath.Join(dir, "resume_probe.bot")
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	_, hash, err := CompileWorkflowFromSource(path, source, "", "")
	if err != nil {
		t.Fatal(err)
	}
	fx := &originFixture{svc: svc, native: native, st: st, path: path, source: source, hash: hash}
	if err := fx.seed(context.Background(), "A"); err != nil {
		t.Fatal(err)
	}
	if fx.origin, err = CaptureRunOriginIn(context.Background(), native, originRunID); err != nil {
		t.Fatal(err)
	}
	return fx
}

// seed creates the run under originRunID, named marker, cancelled on a
// checkpoint: what an operator resume re-enters in-process.
func (fx *originFixture) seed(ctx context.Context, marker string) error {
	if _, err := fx.native.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: originRunID, WorkflowName: marker, Format: store.LegacyValueFormat}); err != nil {
		return err
	}
	r, err := fx.native.LoadRun(ctx, originRunID)
	if err != nil {
		return err
	}
	r.Name, r.FilePath, r.WorkflowSource, r.WorkflowHash = marker, fx.path, fx.source, fx.hash
	r.Status = store.RunStatusCancelled
	if err := fx.native.SaveRun(ctx, r); err != nil {
		return err
	}
	return fx.native.SaveCheckpoint(ctx, originRunID, &store.Checkpoint{NodeID: "done"})
}

// replace deletes and prunes the run, then seeds its successor B — the only
// way a new birth takes an existing ID.
func (fx *originFixture) replace() error {
	ctx := context.Background()
	if err := fx.native.DeleteRun(ctx, originRunID); err != nil {
		return err
	}
	if _, err := fx.native.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
		return err
	}
	return fx.seed(ctx, "B")
}

// runPicture is what a resume could change of a run: its document's
// lifecycle and budget fields, its checkpoint and its events.
type runPicture struct {
	birth, name             string
	status                  store.RunStatus
	budgetOverrides, budget string
	checkpoint              string
	events                  int
}

// takePicture reads the picture; it runs inside a store hook too, where a
// test cannot stop, so it returns its failure.
func (fx *originFixture) takePicture() (runPicture, error) {
	ctx := context.Background()
	r, err := fx.native.LoadRun(ctx, originRunID)
	if err != nil {
		return runPicture{}, err
	}
	evs, err := fx.native.LoadEvents(ctx, originRunID)
	if err != nil {
		return runPicture{}, err
	}
	return runPicture{
		birth: r.BirthID, name: r.Name, status: r.Status,
		budgetOverrides: fmt.Sprintf("%+v", r.BudgetOverrides), budget: fmt.Sprintf("%+v", r.Budget),
		checkpoint: fmt.Sprintf("%+v", r.Checkpoint), events: len(evs),
	}, nil
}

func (fx *originFixture) picture(t *testing.T) runPicture {
	t.Helper()
	p, err := fx.takePicture()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// armReplacement replaces the run on entry of the next call to method and
// pictures the successor at the end of the hook.
func (fx *originFixture) armReplacement(method string) func() runPicture {
	return fx.armReplacementFrom(method, "")
}

// armReplacementFrom is armReplacement on the next call to method a frame of
// caller makes.
func (fx *originFixture) armReplacementFrom(method, caller string) func() runPicture {
	var (
		mu  sync.Mutex
		pic runPicture
	)
	fx.st.armFrom(method, caller, func() error {
		if err := fx.replace(); err != nil {
			return err
		}
		p, err := fx.takePicture()
		if err != nil {
			return err
		}
		mu.Lock()
		pic = p
		mu.Unlock()
		return nil
	})
	return func() runPicture {
		mu.Lock()
		defer mu.Unlock()
		return pic
	}
}

func (fx *originFixture) resume(ctx context.Context, spec ResumeSpec) error {
	spec.RunID, spec.FilePath = originRunID, fx.path
	res, err := fx.svc.Resume(ctx, spec)
	if err != nil {
		return err
	}
	select {
	case <-res.Done:
	case <-time.After(60 * time.Second):
		return errors.New("the resumed run never ended")
	}
	return nil
}

// The ActionOrigin a checked surface hands the service compares a loaded run
// as a bound read compares it: the header and the stored tenant. The zero
// value admits every run.
func TestActionOriginVerifyComparesTheHeaderAndTheTenant(t *testing.T) {
	a := &store.Run{ID: "r", BirthID: "A", FormatVersion: int(store.LegacyValueFormat), TenantID: "t1"}
	o, err := runObservation(a)
	if err != nil {
		t.Fatal(err)
	}
	expect := ExpectRunOrigin(o)
	other := func(edit func(*store.Run)) *store.Run {
		c := *a
		edit(&c)
		return &c
	}
	for _, tc := range []struct {
		name   string
		origin ActionOrigin
		run    *store.Run
		want   error
	}{
		{"the admitted run", expect, a, nil},
		{"a successor's birth", expect, other(func(r *store.Run) { r.BirthID = "B" }), store.ErrRunDataIdentityChanged},
		{"a historical successor", expect, other(func(r *store.Run) { r.BirthID = "" }), store.ErrRunDataIdentityChanged},
		{"another tenant's record", expect, other(func(r *store.Run) { r.TenantID = "t2" }), store.ErrRunDataIdentityChanged},
		{"another format", expect, other(func(r *store.Run) { r.FormatVersion = int(store.ExactValueFormat) }), store.ErrRunDataIdentityChanged},
		{"no run", expect, nil, store.ErrRunNotFound},
		{"unchecked admits a successor", ActionOrigin{}, other(func(r *store.Run) { r.BirthID = "B" }), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.origin.Verify(tc.run)
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("Verify = %v, want %v", err, tc.want)
			}
		})
	}
	if (ActionOrigin{}).Checked() || !expect.Checked() {
		t.Fatal("Checked does not tell the zero value from an admitted origin")
	}
}

// W8: a checked resume whose run is replaced after Resume's load — here as
// the spawn takes the run lock — is refused under that lock, before the
// engine: no store access comes from the engine's resume (whose own check
// would refuse the successor one load later and hide this one), and the
// successor's status, checkpoint and events are as it was seeded, nothing
// executed it. Control: the same replacement with the zero ActionOrigin
// resumes the successor through the engine's resume, as every unchecked
// caller does.
func TestResumeExpectOriginUnderTheSpawnLock(t *testing.T) {
	for _, checked := range []bool{true, false} {
		t.Run(fmt.Sprintf("checked=%t", checked), func(t *testing.T) {
			fx := newOriginFixture(t)
			successor := fx.armReplacement("LockRun")
			spec := ResumeSpec{}
			if checked {
				spec.ExpectOrigin = ExpectRunOrigin(fx.origin)
			}
			err := fx.resume(context.Background(), spec)
			if herr := fx.st.failure(); herr != nil {
				t.Fatalf("the replacement failed: %v", herr)
			}
			before := successor()
			if before.birth == "" || before.birth == fx.origin.Header().BirthID {
				t.Fatalf("the replacement did not run: successor birth %q", before.birth)
			}
			after := fx.picture(t)
			engine := fx.st.engineCalls()
			if !checked {
				if err != nil || after.status == before.status {
					t.Fatalf("unchecked resume of the successor: err=%v, status %s → %s, want it resumed", err, before.status, after.status)
				}
				if len(engine) == 0 || engine[0] != "LoadRun" {
					t.Fatalf("unchecked resume of the successor: the engine's resume made %v, want its load first", engine)
				}
				return
			}
			if !errors.Is(err, store.ErrRunDataIdentityChanged) {
				t.Fatalf("checked resume of a replaced run: %v, want ErrRunDataIdentityChanged", err)
			}
			if len(engine) != 0 {
				t.Fatalf("checked resume of a replaced run reached the engine's resume (%v): want it refused under the spawn lock, before the engine", engine)
			}
			if after != before {
				t.Fatalf("the successor changed under the spawn lock:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}
}

// W8e: a checked resume whose run is replaced after the spawn lock's check —
// on entry of the engine's own load — is refused by the engine, which checks
// the run it loads against the origin: ErrRunDataIdentityChanged, and the
// successor's status, checkpoint and events are as it was seeded. Replaced
// on entry of the claim's read instead, after the engine's load, the claim
// anchored to the run the engine loaded refuses it. Control: the zero
// ActionOrigin resumes the successor replaced at the engine's load.
func TestResumeExpectOriginAtTheEngineLoad(t *testing.T) {
	for _, tc := range []struct {
		name, caller string
		checked      bool
	}{
		{"engine-load", engineResumeLoad, true},
		{"claim-read", engineClaimLoad, true},
		{"engine-load-unchecked", engineResumeLoad, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newOriginFixture(t)
			successor := fx.armReplacementFrom("LoadRun", tc.caller)
			spec := ResumeSpec{}
			if tc.checked {
				spec.ExpectOrigin = ExpectRunOrigin(fx.origin)
			}
			err := fx.resume(context.Background(), spec)
			if herr := fx.st.failure(); herr != nil {
				t.Fatalf("the replacement failed: %v", herr)
			}
			before := successor()
			if before.birth == "" || before.birth == fx.origin.Header().BirthID {
				t.Fatalf("the replacement did not run: successor birth %q", before.birth)
			}
			after := fx.picture(t)
			if !tc.checked {
				if err != nil || after.status == before.status {
					t.Fatalf("unchecked resume of the successor: err=%v, status %s → %s, want it resumed", err, before.status, after.status)
				}
				return
			}
			if err == nil || tc.caller == engineResumeLoad && !errors.Is(err, store.ErrRunDataIdentityChanged) {
				t.Fatalf("checked resume of a run replaced at %s: %v, want a refusal", tc.name, err)
			}
			if after != before {
				t.Fatalf("the checked resume of the admitted run changed the successor:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}
}

// W7b: a checked resume that raises the budget writes the run it verified.
// The run is replaced right before the first budget write: the write is
// refused (it is bound), the resume with it, every budget write it made was
// bound, and the successor's budget fields, status and events are as seeded.
func TestResumeBudgetWritesBoundToTheAdmittedRun(t *testing.T) {
	fx := newOriginFixture(t)
	successor := fx.armReplacement("SetRunBudgetOverrides")
	err := fx.resume(context.Background(), ResumeSpec{ExpectOrigin: ExpectRunOrigin(fx.origin), Budget: &ir.BudgetOverrides{MaxTokens: 1000}})
	if herr := fx.st.failure(); herr != nil {
		t.Fatalf("the replacement failed: %v", herr)
	}
	before := successor()
	if before.birth == "" || before.birth == fx.origin.Header().BirthID {
		t.Fatalf("the replacement did not run: successor birth %q", before.birth)
	}
	if !errors.Is(err, store.ErrRunDataIdentityChanged) {
		t.Fatalf("checked resume whose run was replaced before its budget writes: %v, want ErrRunDataIdentityChanged", err)
	}
	if after := fx.picture(t); after != before {
		t.Fatalf("the successor changed:\nbefore %+v\nafter  %+v", before, after)
	}
	for _, c := range fx.st.recorded() {
		switch c.method {
		case "SetRunBudgetOverrides", "SetRunBudgetSnapshot", "PatchRunBudgetOverrides":
			if !c.bound {
				t.Fatalf("a budget write of a checked resume was unbound: %+v", c)
			}
		}
	}
}

// W7d: in detached mode a checked resume that raises the budget, its run
// replaced on entry of either budget write, returns the refusal of that bound
// write: ErrRunDataIdentityChanged, and no `iterion resume` subprocess is
// spawned to re-read the run by ID — the successor is as it was seeded.
func TestDetachedResumeRefusedByItsBoundBudgetWrite(t *testing.T) {
	for _, method := range []string{"SetRunBudgetOverrides", "SetRunBudgetSnapshot"} {
		t.Run(method, func(t *testing.T) {
			fx := newOriginFixture(t)
			t.Setenv(envDetached, "1")
			dir := t.TempDir()
			argv := filepath.Join(dir, "argv")
			bin := filepath.Join(dir, "iterion")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"$@\" > "+argv+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ITERION_BIN", bin)
			successor := fx.armReplacement(method)
			err := fx.resume(context.Background(), ResumeSpec{ExpectOrigin: ExpectRunOrigin(fx.origin), Budget: &ir.BudgetOverrides{MaxTokens: 1000}})
			if herr := fx.st.failure(); herr != nil {
				t.Fatalf("the replacement failed: %v", herr)
			}
			before := successor()
			if before.birth == "" || before.birth == fx.origin.Header().BirthID {
				t.Fatalf("the replacement did not run: successor birth %q", before.birth)
			}
			if !errors.Is(err, store.ErrRunDataIdentityChanged) {
				t.Fatalf("detached checked resume whose run was replaced at %s: %v, want ErrRunDataIdentityChanged", method, err)
			}
			if raw, rerr := os.ReadFile(argv); !errors.Is(rerr, os.ErrNotExist) {
				t.Fatalf("a resume subprocess was spawned for the successor: %q (%v)", raw, rerr)
			}
			if after := fx.picture(t); after != before {
				t.Fatalf("the successor changed:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}
}

// W9: a checked resume of the run it was admitted on executes unbound: no
// store access the execution makes after the spawn takes the run lock
// carries a run data binding (the value-format intent its loads carry is
// another key and is allowed). The resume reaches the run's end.
func TestExpectedMutationExecutionStaysUnbound(t *testing.T) {
	fx := newOriginFixture(t)
	if err := fx.resume(context.Background(), ResumeSpec{ExpectOrigin: ExpectRunOrigin(fx.origin)}); err != nil {
		t.Fatalf("checked resume of the admitted run: %v", err)
	}
	after := 0
	for _, c := range fx.st.recorded() {
		if !c.afterRun {
			continue
		}
		after++
		if c.bound {
			t.Fatalf("the execution made a bound store access: %+v", c)
		}
	}
	if after == 0 {
		t.Fatal("fixture: no recorded access after the spawn took the run lock")
	}
	if got := fx.picture(t); got.status == store.RunStatusCancelled || got.birth != fx.origin.Header().BirthID {
		t.Fatalf("the admitted run was not resumed: %+v", got)
	}
}

// seedReport re-creates the run as a report-mode run named marker whose
// checkpoint names an artifact revision that does not exist: the in-process
// resume's artifact pass reports it with an artifact_contract_violation
// event on the run ID.
func (fx *originFixture) seedReport(ctx context.Context, marker string) error {
	if err := fx.native.DeleteRun(ctx, originRunID); err != nil {
		return err
	}
	if _, err := fx.native.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
		return err
	}
	if err := fx.seed(ctx, marker); err != nil {
		return err
	}
	r, err := fx.native.LoadRun(ctx, originRunID)
	if err != nil {
		return err
	}
	r.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyReport}
	if err := fx.native.SaveRun(ctx, r); err != nil {
		return err
	}
	return fx.native.SaveCheckpoint(ctx, originRunID, &store.Checkpoint{NodeID: "done",
		ArtifactRevisions: map[string]store.ArtifactRevisionRef{"x": {NodeID: "ghost", Version: 0}}})
}

// violationEvents counts the artifact_contract_violation events of the run
// the ID holds now, and returns its birth.
func (fx *originFixture) violationEvents(t *testing.T) (int, string) {
	t.Helper()
	ctx := context.Background()
	evs, err := fx.native.LoadEvents(ctx, originRunID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if e.Type == store.EventArtifactContractViolation {
			n++
		}
	}
	r, err := fx.native.LoadRun(ctx, originRunID)
	if err != nil {
		t.Fatal(err)
	}
	return n, r.BirthID
}

// W7r: a checked in-process resume of a report-mode run, the run replaced by
// a report-mode successor while the resume's artifact pass reads the run's
// artifacts. The pass is bound to the admitted origin: the violation event it
// reports never lands on the successor, and the resume is refused. Control:
// on the admitted run the pass reports the violation once.
func TestResumeArtifactPassBoundToTheAdmittedRun(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replaced=%t", replace), func(t *testing.T) {
			fx := newOriginFixture(t)
			ctx := context.Background()
			if err := fx.seedReport(ctx, "A"); err != nil {
				t.Fatal(err)
			}
			var err error
			if fx.origin, err = CaptureRunOriginIn(ctx, fx.native, originRunID); err != nil {
				t.Fatal(err)
			}
			var (
				mu        sync.Mutex
				successor string
			)
			if replace {
				fx.st.arm("LoadArtifact", func() error {
					if err := fx.seedReport(ctx, "B"); err != nil {
						return err
					}
					b, err := fx.native.LoadRun(ctx, originRunID)
					if err != nil {
						return err
					}
					mu.Lock()
					successor = b.BirthID
					mu.Unlock()
					return nil
				})
			}
			rerr := fx.resume(ctx, ResumeSpec{ExpectOrigin: ExpectRunOrigin(fx.origin)})
			if herr := fx.st.failure(); herr != nil {
				t.Fatalf("the replacement failed: %v", herr)
			}
			n, birth := fx.violationEvents(t)
			if !replace {
				if rerr != nil || n != 1 {
					t.Fatalf("control: resume err=%v, %d violation events on the admitted run, want one", rerr, n)
				}
				return
			}
			mu.Lock()
			b := successor
			mu.Unlock()
			if b == "" || birth != b || b == fx.origin.Header().BirthID {
				t.Fatalf("the replacement did not run: the run holds %q, successor %q", birth, b)
			}
			if n != 0 {
				t.Fatalf("the refused resume of the admitted run reported %d artifact_contract_violation event(s) on the successor", n)
			}
			if !errors.Is(rerr, store.ErrRunDataIdentityChanged) {
				t.Fatalf("checked resume of a run replaced during its artifact pass: %v, want ErrRunDataIdentityChanged", rerr)
			}
		})
	}
}
