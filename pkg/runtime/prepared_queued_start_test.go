package runtime

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"strings"
	"testing"
)

type queuedStartProbe struct {
	*store.FilesystemRunStore
	cancel                          context.CancelFunc
	mode                            string
	acquisitions, legacyTransitions int
	first, successor                store.RunExecutionAuthority
	infra                           error
}

func (s *queuedStartProbe) UpdateRunStatusIf(ctx context.Context, id string, status store.RunStatus, msg string, from []store.RunStatus) (bool, error) {
	s.legacyTransitions++
	ok, err := s.FilesystemRunStore.UpdateRunStatusIf(ctx, id, status, msg, from)
	if ok && s.cancel != nil {
		s.cancel()
	}
	return ok, err
}
func (s *queuedStartProbe) AcquireRunExecutionIf(ctx context.Context, id string, x store.RunExecutionExpectation, intent store.RunExecutionIntent) (store.RunExecutionAuthority, bool, error) {
	s.acquisitions++
	if s.infra != nil {
		return store.RunExecutionAuthority{}, false, s.infra
	}
	if s.mode == "peer-before" {
		r, err := s.LoadRun(ctx, id)
		if err != nil {
			return store.RunExecutionAuthority{}, false, err
		}
		s.successor, _, err = s.FilesystemRunStore.AcquireRunExecutionIf(ctx, id, store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status, QueuedAt: r.QueuedAt}, store.ExecutionStart)
		if err != nil {
			return store.RunExecutionAuthority{}, false, err
		}
	}
	a, ok, err := s.FilesystemRunStore.AcquireRunExecutionIf(ctx, id, x, intent)
	if err != nil || !ok {
		return a, ok, err
	}
	s.first = a
	if s.mode == "peer-after" {
		r, err := s.LoadRun(ctx, id)
		if err != nil {
			return a, false, err
		}
		s.successor, _, err = s.FilesystemRunStore.AcquireRunExecutionIf(ctx, id, store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status, QueuedAt: r.QueuedAt}, store.ExecutionStart)
		if err != nil {
			return a, false, err
		}
	}
	if s.cancel != nil {
		s.cancel()
	}
	return a, true, nil
}

type queuedNoAccountingExecutor struct {
	NodeExecutor
	advertised bool
}

func (s *queuedNoAccountingExecutor) Execute(ctx context.Context, n ir.Node, in map[string]any) (map[string]any, error) {
	if _, ok := model.OutputCorrectionOwnershipFromContext(ctx); ok {
		s.advertised = true
	}
	return s.NodeExecutor.Execute(ctx, n, in)
}

type queuedNoCapability struct{ store.RunStore }

func TestPreparedQueuedStart(t *testing.T) {
	for _, mode := range []string{"normal", "cancel", "own-stamp", "peer-before", "peer-after", "mutated-return", "existing", "historical", "missing-capability", "infra", "running-control"} {
		t.Run(mode, func(t *testing.T) {
			fs, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			initialStatus := store.RunStatusQueued
			if mode == "running-control" {
				initialStatus = store.RunStatusRunning
			}
			p, err := store.PrepareRunInvocation(t.Context(), fs, store.NewLegacyRunInvocation{Spec: store.RunCreateSpec{ID: "run", Status: initialStatus}})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "own-stamp" {
				r, e := fs.LoadRun(p.Context, "run")
				if e != nil {
					t.Fatal(e)
				}
				r.Name = "policy already stamped"
				if e = fs.SaveRun(p.Context, r); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "mutated-return" {
				p.Run.ID = "foreign"
				p.Run.ExecutionEpoch = "foreign"
				if p.Run.QueuedAt != nil {
					*p.Run.QueuedAt = p.Run.QueuedAt.Add(1000000000)
				}
			}
			if mode == "existing" {
				p, err = store.PrepareRunInvocation(p.Context, fs, store.ExistingRunInvocation{ID: "run"})
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(p.Context)
			defer cancel()
			spy := &queuedStartProbe{FilesystemRunStore: fs, mode: mode}
			if mode == "cancel" || mode == "own-stamp" || mode == "peer-after" || mode == "existing" || mode == "historical" {
				spy.cancel = cancel
			}
			if mode == "infra" {
				spy.infra = errors.New("acquire unavailable")
			}
			var runs store.RunStore = spy
			if mode == "missing-capability" {
				runs = &queuedNoCapability{RunStore: spy}
			}
			calls := 0
			e := admissionSnapshotEngine(t, runs, nil, &calls)
			executor := &queuedNoAccountingExecutor{NodeExecutor: e.executor}
			e.executor = executor
			if mode == "historical" {
				err = e.Run(ctx, "run", nil)
			} else {
				err = e.RunPrepared(ctx, "run", nil, p)
			}
			r, re := fs.LoadRun(t.Context(), "run")
			if re != nil {
				t.Fatal(re)
			}
			t.Logf("mode=%s error=%v status=%s acquire=%d legacyCAS=%d nodes=%d", mode, err, r.Status, spy.acquisitions, spy.legacyTransitions, calls)
			if executor.advertised || len(r.InitialUsageReservations) != 0 {
				t.Fatal("queued start activated accounting")
			}
			switch mode {
			case "normal", "mutated-return":
				if err != nil || r.Status != store.RunStatusFinished || spy.acquisitions != 1 || spy.legacyTransitions != 0 || calls != 1 {
					t.Fatal("normal", err, r.Status, calls)
				}
			case "running-control":
				if err != nil || r.Status != store.RunStatusFinished || spy.acquisitions != 0 || spy.legacyTransitions != 0 || calls != 1 {
					t.Fatal("running broadened", err, r.Status, calls)
				}
			case "cancel", "own-stamp":
				if !errors.Is(err, ErrRunCancelled) || r.Status != store.RunStatusCancelled || spy.acquisitions != 1 || spy.legacyTransitions != 0 || calls != 0 {
					t.Fatal("queued birth stranded", err, r.Status, calls)
				}
			case "peer-before", "peer-after":
				if (!errors.Is(err, ErrAdmissionStopped) && !errors.Is(err, store.ErrRunConflict)) || !spy.successor.Matches(r) || calls != 0 {
					t.Fatal("peer overwritten", err, r.Status, calls)
				}
			case "existing", "historical":
				if !errors.Is(err, context.Canceled) || r.Status != store.RunStatusRunning || spy.acquisitions != 0 || spy.legacyTransitions != 1 || calls != 0 {
					t.Fatal("existing behavior changed", err, r.Status, calls)
				}
			case "missing-capability":
				if err == nil || !strings.Contains(err.Error(), "lacks RunExecutionStore") || r.Status != store.RunStatusQueued || calls != 0 {
					t.Fatal("missing capability degraded", err, r.Status)
				}
			case "infra":
				if !errors.Is(err, spy.infra) || r.Status != store.RunStatusQueued || calls != 0 {
					t.Fatal("infra adopted", err, r.Status)
				}
			}
		})
	}
}
