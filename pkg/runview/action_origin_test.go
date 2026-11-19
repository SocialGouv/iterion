package runview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// originCall is one store access the origin store recorded: the method,
// whether its context carried a run data binding (never the value-format
// intent), and whether it came after the spawn took the run lock.
type originCall struct {
	method          string
	bound, afterRun bool
}

// originStore is the filesystem store with the probes of the carrier-E
// witnesses: a one-shot hook run on ENTRY of a named method, before it
// delegates, and a record of the accesses of the methods it overrides. It
// forwards every call with the caller's context, so the binding policy under
// test stays in the native store, and it endorses itself as that store.
type originStore struct {
	*store.FilesystemRunStore
	mu      sync.Mutex
	before  map[string]func() error
	hookErr error
	locked  bool
	calls   []originCall
}

func (s *originStore) CheckBoundObservationStore(actual store.RunStore) error {
	if a, ok := actual.(*originStore); !ok || a != s {
		return fmt.Errorf("%w: %T is not this test double", store.ErrRunDataCapability, actual)
	}
	return nil
}

// arm runs h once, on entry of the next call to method.
func (s *originStore) arm(method string, h func() error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.before == nil {
		s.before = map[string]func() error{}
	}
	s.before[method] = h
}

func (s *originStore) enter(ctx context.Context, method string) {
	_, bound := store.RunDataBindingFrom(ctx)
	s.mu.Lock()
	h := s.before[method]
	delete(s.before, method)
	s.calls = append(s.calls, originCall{method: method, bound: bound, afterRun: s.locked})
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
	var (
		mu  sync.Mutex
		pic runPicture
	)
	fx.st.arm(method, func() error {
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
// the spawn takes the run lock — is refused under that lock: the successor's
// status, checkpoint and events are as it was seeded, nothing executed it.
// Control: the same replacement with the zero ActionOrigin resumes the
// successor, as every unchecked caller does.
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
			if !checked {
				if err != nil || after.status == before.status {
					t.Fatalf("unchecked resume of the successor: err=%v, status %s → %s, want it resumed", err, before.status, after.status)
				}
				return
			}
			if !errors.Is(err, store.ErrRunDataIdentityChanged) {
				t.Fatalf("checked resume of a replaced run: %v, want ErrRunDataIdentityChanged", err)
			}
			if after != before {
				t.Fatalf("the successor changed under the spawn lock:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}
}

// W7b: a checked resume that raises the budget writes the run it verified.
// The run is replaced right before the first budget write: both of Resume's
// writes and the spawn's replay write are refused (they are bound), the
// resume with them, and the successor's budget fields, status and events are
// as seeded.
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
