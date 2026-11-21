package runtime

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
	"time"
)

type preparedCASProbe struct {
	*store.FilesystemRunStore
	beforeCAS          func(context.Context, string) error
	writes, reads      int
	cancelAfterAcquire context.CancelFunc
}

func (s *preparedCASProbe) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	s.reads++
	return s.FilesystemRunStore.LoadRun(ctx, id)
}
func (s *preparedCASProbe) UpdateRunOutcomeIf(ctx context.Context, id string, x store.RunOutcomeExpectation, status store.RunStatus, msg string, meta store.RunOutcomeMeta, from []store.RunStatus) (bool, error) {
	s.writes++
	if s.beforeCAS != nil {
		f := s.beforeCAS
		s.beforeCAS = nil
		if err := f(ctx, id); err != nil {
			return false, err
		}
	}
	return s.FilesystemRunStore.UpdateRunOutcomeIf(ctx, id, x, status, msg, meta, from)
}
func (s *preparedCASProbe) AcquireRunExecutionIf(ctx context.Context, id string, x store.RunExecutionExpectation, intent store.RunExecutionIntent) (store.RunExecutionAuthority, bool, error) {
	a, ok, err := s.FilesystemRunStore.AcquireRunExecutionIf(ctx, id, x, intent)
	if ok && s.cancelAfterAcquire != nil {
		s.cancelAfterAcquire()
	}
	return a, ok, err
}
func preparedFixture(t *testing.T) (*preparedCASProbe, store.PreparedRunInvocation) {
	t.Helper()
	fs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &preparedCASProbe{FilesystemRunStore: fs}
	p, err := store.PrepareRunInvocation(t.Context(), s, store.NewLegacyRunInvocation{Spec: store.RunCreateSpec{ID: "run"}})
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}
func preparedTakeover(s *preparedCASProbe, ctx context.Context, id string) error {
	r, err := s.FilesystemRunStore.LoadRun(ctx, id)
	if err != nil {
		return err
	}
	_, ok, err := s.FilesystemRunStore.AcquireRunExecutionIf(ctx, id, store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status}, store.ExecutionStart)
	if err == nil && !ok {
		return errors.New("probe takeover declined")
	}
	return err
}
func TestPreparedAbortCAS(t *testing.T) {
	for _, kind := range []string{"normal", "own-stamp", "concurrent-metadata", "peer-before-read", "peer-at-CAS", "changed-format", "ordinary-cause", "typed-priority", "existing", "forged", "mutated-return", "ambiguous", "deadline", "drain"} {
		t.Run(kind, func(t *testing.T) {
			s, p := preparedFixture(t)
			ctx, cancel := context.WithCancel(p.Context)
			defer cancel()
			cause := context.Canceled
			wantStatus := store.RunStatusCancelled
			wantErr := ErrRunCancelled
			wantWrites := 1
			switch kind {
			case "own-stamp":
				r, err := s.LoadRun(p.Context, "run")
				if err != nil {
					t.Fatal(err)
				}
				r.Name = "own policy stamp"
				if err = s.SaveRun(p.Context, r); err != nil {
					t.Fatal(err)
				}
			case "concurrent-metadata":
				wantWrites = 2
				s.beforeCAS = func(ctx context.Context, id string) error {
					r, err := s.LoadRun(ctx, id)
					if err != nil {
						return err
					}
					r.Name = "concurrent metadata"
					return s.SaveRun(ctx, r)
				}
			case "peer-before-read":
				if err := preparedTakeover(s, p.Context, "run"); err != nil {
					t.Fatal(err)
				}
				wantErr = ErrAdmissionStopped
				wantStatus = store.RunStatusRunning
				wantWrites = 0
			case "peer-at-CAS":
				s.beforeCAS = func(ctx context.Context, id string) error { return preparedTakeover(s, ctx, id) }
				wantErr = ErrAdmissionStopped
				wantStatus = store.RunStatusRunning
			case "changed-format":
				if err := s.DeleteRun(p.Context, "run"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.PruneDeletionMarkers(p.Context, time.Now().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				if _, err := store.CreateRunWithValueFormat(t.Context(), s, store.RunCreateSpec{ID: "run", Format: store.ExactValueFormat}); err != nil {
					t.Fatal(err)
				}
				wantErr = store.ErrValueFormatChanged
				wantStatus = store.RunStatusRunning
				wantWrites = 0
			case "ordinary-cause":
				cause = errors.New("business refusal")
				wantErr = cause
				wantStatus = store.RunStatusRunning
				wantWrites = 0
			case "typed-priority":
				cause = errors.Join(store.ErrValueFormatChanged, context.Canceled)
				wantErr = cause
				wantStatus = store.RunStatusRunning
				wantWrites = 0
			case "existing":
				var err error
				p, err = store.PrepareRunInvocation(p.Context, s, store.ExistingRunInvocation{ID: "run"})
				if err != nil {
					t.Fatal(err)
				}
				wantErr = context.Canceled
				wantStatus = store.RunStatusRunning
				wantWrites = 0
			case "forged":
				p = store.PreparedRunInvocation{Context: p.Context, Run: p.Run, OwnedBirth: true}
				wantErr = context.Canceled
				wantStatus = store.RunStatusRunning
				wantWrites = 0
			case "mutated-return":
				p.Run.ID = "another"
				p.Run.ExecutionEpoch = "manufactured"
				p.Run.CASVersion = 999
			case "ambiguous":
				cause = errors.Join(&RuntimeError{Code: ErrCodeAmbiguousEffect, Message: "uncertain"}, context.Canceled)
				wantErr = cause
				wantStatus = store.RunStatusRunning
				wantWrites = 0
			case "deadline":
				var c context.CancelFunc
				ctx, c = context.WithDeadline(p.Context, time.Now().Add(-time.Second))
				defer c()
				cause = context.DeadlineExceeded
				wantStatus = store.RunStatusFailed
				wantErr = context.DeadlineExceeded
			case "drain":
				var c context.CancelCauseFunc
				ctx, c = context.WithCancelCause(p.Context)
				c(ErrRunInterrupted)
				wantStatus = store.RunStatusFailedResumable
				wantErr = ErrRunInterrupted
			}
			cancel()
			err := AbortPreparedRun(ctx, s, p, cause)
			r, e := s.FilesystemRunStore.LoadRun(t.Context(), "run")
			if e != nil {
				t.Fatal(e)
			}
			t.Logf("kind=%s status=%s error=%v CAS=%d", kind, r.Status, err, s.writes)
			if (wantErr != nil && !errors.Is(err, wantErr)) || r.Status != wantStatus || s.writes != wantWrites {
				t.Fatalf("kind=%s got %s %v writes%d, want %s %v writes%d", kind, r.Status, err, s.writes, wantStatus, wantErr, wantWrites)
			}
			if wantErr == nil {
				var rt *RuntimeError
				if !errors.As(err, &rt) || rt.Code != ErrCodeTimeout {
					t.Fatalf("deadline returned %v", err)
				}
			}
		})
	}
}

type preparedAccountingExecutor struct{ NodeExecutor }

func (preparedAccountingExecutor) ExecutionAccountingVersion() int {
	return model.ExecutionAccountingProtocol
}
func TestPreparedEngineEntry(t *testing.T) {
	for _, kind := range []string{"cancel-before-RunPrepared", "historical-Run", "normal", "acquired-then-cancel", "acquired-then-peer", "rebound-successor", "peer-at-CAS"} {
		t.Run(kind, func(t *testing.T) {
			s, p := preparedFixture(t)
			ctx, cancel := context.WithCancel(p.Context)
			defer cancel()
			calls := 0
			e := admissionSnapshotEngine(t, s, nil, &calls)
			if kind == "acquired-then-cancel" || kind == "acquired-then-peer" {
				e.executor = preparedAccountingExecutor{e.executor}
				s.cancelAfterAcquire = cancel
			}
			if kind == "peer-at-CAS" || kind == "acquired-then-peer" {
				s.beforeCAS = func(ctx context.Context, id string) error { return preparedTakeover(s, ctx, id) }
			}
			if kind != "normal" && kind != "acquired-then-cancel" && kind != "acquired-then-peer" && kind != "rebound-successor" {
				cancel()
			}
			if kind == "rebound-successor" {
				if err := s.DeleteRun(p.Context, "run"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.PruneDeletionMarkers(p.Context, time.Now().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				r, err := store.CreateRunWithValueFormat(t.Context(), s, store.RunCreateSpec{ID: "run", Format: store.ExactValueFormat})
				if err != nil {
					t.Fatal(err)
				}
				ctx, err = store.BindRunValueFormat(t.Context(), r)
				if err != nil {
					t.Fatal(err)
				}
			}
			var err error
			if kind == "historical-Run" {
				err = e.Run(ctx, "run", nil)
			} else {
				err = e.RunPrepared(ctx, "run", nil, p)
			}
			r, re := s.FilesystemRunStore.LoadRun(t.Context(), "run")
			if re != nil {
				t.Fatal(re)
			}
			t.Logf("kind=%s calls=%d status=%s error=%v CAS=%d", kind, calls, r.Status, err, s.writes)
			switch kind {
			case "normal":
				if err != nil || calls != 1 || r.Status != store.RunStatusFinished {
					t.Fatal("normal changed", err, r.Status, calls)
				}
			case "historical-Run":
				if !errors.Is(err, context.Canceled) || calls != 0 || r.Status != store.RunStatusRunning || s.writes != 0 {
					t.Fatal("historical changed", err, r.Status, calls)
				}
			case "rebound-successor":
				if !errors.Is(err, store.ErrValueFormatChanged) || calls != 0 || r.Status != store.RunStatusRunning || s.writes != 0 {
					t.Fatal("rebound successor adopted", err, r.Status, calls)
				}
			case "peer-at-CAS", "acquired-then-peer":
				if !errors.Is(err, ErrAdmissionStopped) || calls != 0 || r.Status != store.RunStatusRunning {
					t.Fatal("peer overwritten", err, r.Status, calls)
				}
			default:
				if !errors.Is(err, ErrRunCancelled) || calls != 0 || r.Status != store.RunStatusCancelled {
					t.Fatal("prepared stranded", err, r.Status, calls)
				}
			}
		})
	}
}

func TestPreparedAbortPolicy(t *testing.T) {
	for _, kind := range []string{"single-conflict", "unbounded", "queued-original", "existing-queued", "initial-final"} {
		t.Run(kind, func(t *testing.T) {
			fs, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s := &preparedCASProbe{FilesystemRunStore: fs}
			status := store.RunStatusRunning
			if kind == "queued-original" || kind == "existing-queued" {
				status = store.RunStatusQueued
			}
			p, err := store.PrepareRunInvocation(t.Context(), s, store.NewLegacyRunInvocation{Spec: store.RunCreateSpec{ID: "run", Status: status}})
			if err != nil {
				t.Fatal(err)
			}
			if kind == "initial-final" {
				if ok, e := s.UpdateRunOutcome(p.Context, "run", store.RunStatusFinished, "finished by peer", store.RunOutcomeMeta{}, []store.RunStatus{store.RunStatusRunning}); e != nil || !ok {
					t.Fatal(ok, e)
				}
			}
			if kind == "existing-queued" {
				p, err = store.PrepareRunInvocation(p.Context, s, store.ExistingRunInvocation{ID: "run"})
				if err != nil {
					t.Fatal(err)
				}
			}
			opts := PreparedStopOptions{MaxAttempts: 1}
			if kind == "unbounded" {
				opts = PreparedStopOptions{MaxAttempts: -1, Timeout: -1}
			}
			if kind == "single-conflict" || kind == "unbounded" {
				s.beforeCAS = func(ctx context.Context, id string) error {
					r, e := s.LoadRun(ctx, id)
					if e != nil {
						return e
					}
					r.Name = "metadata"
					return s.SaveRun(ctx, r)
				}
			}
			ctx, cancel := context.WithCancel(p.Context)
			cancel()
			err = AbortPreparedRun(ctx, s, p, context.Canceled, opts)
			r, e := s.FilesystemRunStore.LoadRun(t.Context(), "run")
			if e != nil {
				t.Fatal(e)
			}
			t.Logf("kind=%s status=%s error=%v CAS=%d", kind, r.Status, err, s.writes)
			switch kind {
			case "single-conflict":
				if !errors.Is(err, store.ErrRunConflict) || errors.Is(err, ErrAdmissionStopped) || r.Status != store.RunStatusRunning {
					t.Fatal("metadata misclassified", err, r.Status)
				}
			case "existing-queued":
				if err != context.Canceled || r.Status != store.RunStatusQueued || s.writes != 0 {
					t.Fatal("existing withdrawn", err, r.Status)
				}
			case "initial-final":
				if !errors.Is(err, ErrAdmissionStopped) || r.Status != store.RunStatusFinished || s.writes != 0 {
					t.Fatal("final touched", err, r.Status)
				}
			default:
				if !errors.Is(err, ErrRunCancelled) || r.Status != store.RunStatusCancelled {
					t.Fatal("birth stranded", err, r.Status)
				}
			}
		})
	}
}
