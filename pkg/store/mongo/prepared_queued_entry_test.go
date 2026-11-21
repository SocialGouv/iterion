package mongo

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

type queuedMongoStart struct {
	*Store
	peer                 *Store
	cancel               context.CancelFunc
	mode                 string
	acquired             int
	authority, successor store.RunExecutionAuthority
}

func (s *queuedMongoStart) AcquireRunExecutionIf(ctx context.Context, id string, x store.RunExecutionExpectation, intent store.RunExecutionIntent) (store.RunExecutionAuthority, bool, error) {
	s.acquired++
	a, ok, err := s.Store.AcquireRunExecutionIf(ctx, id, x, intent)
	if err != nil || !ok {
		return a, ok, err
	}
	s.authority = a
	if s.mode == "peer-after" {
		r, e := s.LoadRun(ctx, id)
		if e != nil {
			return a, false, e
		}
		s.successor, ok, e = s.peer.AcquireRunExecutionIf(ctx, id, store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status, QueuedAt: r.QueuedAt}, store.ExecutionStart)
		if e != nil || !ok {
			return a, false, e
		}
	}
	if s.cancel != nil {
		s.cancel()
	}
	return a, true, nil
}

type queuedMongoExecutor struct {
	calls      int
	advertised bool
}

func (e *queuedMongoExecutor) Execute(ctx context.Context, _ ir.Node, _ map[string]any) (map[string]any, error) {
	e.calls++
	_, e.advertised = model.OutputCorrectionOwnershipFromContext(ctx)
	return map[string]any{}, nil
}
func TestPreparedQueuedMongoEntry(t *testing.T) {
	for _, mode := range []string{"normal", "cancel", "own-stamp", "peer-after"} {
		t.Run(mode, func(t *testing.T) {
			s, peer := preparedMongo(t)
			tenantCtx := store.WithTenant(t.Context(), "queued-entry")
			p, err := store.PrepareRunInvocation(tenantCtx, s, store.NewLegacyRunInvocation{Spec: store.RunCreateSpec{ID: "run"}})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "own-stamp" {
				r, e := s.LoadRun(p.Context, "run")
				if e != nil {
					t.Fatal(e)
				}
				r.Name = "own routing stamp"
				if e = peer.SaveRun(p.Context, r); e != nil {
					t.Fatal(e)
				}
			}
			ctx, cancel := context.WithCancel(p.Context)
			defer cancel()
			wrapped := &queuedMongoStart{Store: s, peer: peer, mode: mode}
			if mode != "normal" {
				wrapped.cancel = cancel
			}
			wf := &ir.Workflow{Name: "queued-probe", Entry: "probe", Worktree: "none", Nodes: map[string]ir.Node{"probe": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "probe"}, Command: "unused"}, "done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}}, Edges: []*ir.Edge{{From: "probe", To: "done"}}}
			executor := &queuedMongoExecutor{}
			engine := runtime.New(wf, wrapped, executor, runtime.WithWorkDir(t.TempDir()), runtime.WithSandboxOverride("none"))
			err = engine.RunPrepared(ctx, "run", nil, p)
			r, e := s.LoadRun(tenantCtx, "run")
			if e != nil {
				t.Fatal(e)
			}
			t.Logf("mode=%s status=%s error=%v acquire=%d nodes=%d", mode, r.Status, err, wrapped.acquired, executor.calls)
			if wrapped.acquired != 1 || executor.advertised || len(r.InitialUsageReservations) != 0 {
				t.Fatal("start affected accounting", wrapped.acquired, executor.advertised, len(r.InitialUsageReservations))
			}
			switch mode {
			case "normal":
				if err != nil || r.Status != store.RunStatusFinished || executor.calls != 1 {
					t.Fatal(err, r.Status, executor.calls)
				}
			case "peer-after":
				if !errors.Is(err, runtime.ErrAdmissionStopped) || !wrapped.successor.Matches(r) || executor.calls != 0 {
					t.Fatal("successor overwritten", err, r.Status)
				}
			default:
				if !errors.Is(err, runtime.ErrRunCancelled) || r.Status != store.RunStatusCancelled || executor.calls != 0 {
					t.Fatal("queued birth stranded", err, r.Status)
				}
			}
		})
	}
}
