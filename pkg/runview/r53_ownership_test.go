package runview

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
	"time"
)

type r53ViewGrant struct {
	store.RunFilesGrant
	closed int
}

func (g *r53ViewGrant) Close() error { g.closed++; return g.RunFilesGrant.Close() }

type r53ViewStore struct {
	store.RunStore
	base  *store.FilesystemRunStore
	grant *r53ViewGrant
	opens int
}

func (s *r53ViewStore) OpenRunFilesGrant(ctx context.Context, id string) (store.RunFilesGrant, error) {
	g, e := s.base.OpenRunFilesGrant(ctx, id)
	if e != nil {
		return nil, e
	}
	s.opens++
	s.grant = &r53ViewGrant{RunFilesGrant: g}
	return s.grant, nil
}

type r53EventOnly struct{ model.EventEmitter }

func r53ViewSpec(t *testing.T, strict bool) (ExecutorSpec, *r53ViewStore) {
	t.Helper()
	if strict {
		if err := store.RequireRunDataStorage(store.IncarnationRunData); err != nil {
			t.Skip("incarnation admission remains disabled")
		}
	}
	s, e := store.New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	protocol := store.HistoricalRunData
	if strict {
		protocol = store.IncarnationRunData
	}
	r, e := s.CreateRunWithValueFormat(context.Background(), store.RunCreateSpec{ID: "r", WorkflowName: "w", Format: 2, DataProtocol: protocol})
	if e != nil {
		t.Fatal(e)
	}
	b, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: r.ID, Format: 2, Protocol: r.DataProtocol, BirthID: r.BirthID})
	if e != nil {
		t.Fatal(e)
	}
	ctx, e := store.WithRunDataBinding(context.Background(), b)
	if e != nil {
		t.Fatal(e)
	}
	ctx, e = store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: r.ID, Format: 2})
	if e != nil {
		t.Fatal(e)
	}
	wrapped := &r53ViewStore{RunStore: s, base: s}
	wf := &ir.Workflow{Name: "w", Entry: "done", Nodes: map[string]ir.Node{"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}}}
	return ExecutorSpec{Ctx: ctx, Workflow: wf, Store: r53EventOnly{s}, Runs: wrapped, RunID: "r", WorkDir: t.TempDir(), Logger: iterlog.Nop()}, wrapped
}

func TestReviewR53ServiceOwnsGrant(t *testing.T) {
	for _, fail := range []bool{true, false} {
		t.Run(map[bool]string{true: "prelaunch_error", false: "success"}[fail], func(t *testing.T) {
			spec, st := r53ViewSpec(t, true)
			exec, err := BuildExecutor(spec)
			if err != nil {
				t.Fatal(err)
			}
			defer exec.Close()
			svc, err := NewService(st.base.Root(), WithStore(st.base), WithLogger(iterlog.Nop()), WithRecoveryPassive())
			if err != nil {
				t.Fatal(err)
			}
			defer stopService(t, svc)
			id := "r"
			if fail {
				id = "missing"
			}
			res, err := svc.spawnRun(spec.Ctx, id, spec.Workflow, "", "", "", nil, finalizationOpts{}, callbackOpts{}, exec, iterlog.Nop(), nil, 0, false, nil, "", nil, "", false, launchExtras{}, nil, func(context.Context, context.Context, *runtime.Engine) error { return nil })
			if fail {
				if err == nil {
					t.Fatal("expected load error")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-res.Done:
				case <-time.After(10 * time.Second):
					t.Fatal("timeout")
				}
			}
			if st.grant.closed != 1 {
				t.Fatalf("owned grant Close count=%d, want1", st.grant.closed)
			}
		})
	}
}

func TestReviewR53ServiceRegisterFailureOwnsGrant(t *testing.T) {
	spec, st := r53ViewSpec(t, true)
	exec, e := BuildExecutor(spec)
	if e != nil {
		t.Fatal(e)
	}
	defer exec.Close()
	svc, e := NewService(st.base.Root(), WithStore(st.base), WithLogger(iterlog.Nop()), WithRecoveryPassive())
	if e != nil {
		t.Fatal(e)
	}
	defer stopService(t, svc)
	_, e = svc.manager.Register(spec.Ctx, "r")
	if e != nil {
		t.Fatal(e)
	}
	defer svc.manager.Deregister("r")
	_, e = svc.spawnRun(spec.Ctx, "r", spec.Workflow, "", "", "", nil, finalizationOpts{}, callbackOpts{}, exec, iterlog.Nop(), nil, 0, false, nil, "", nil, "", false, launchExtras{}, nil, func(context.Context, context.Context, *runtime.Engine) error { return nil })
	if e == nil {
		t.Fatal("expected register conflict")
	}
	if st.grant.closed != 1 {
		t.Fatalf("late failure Close=%d", st.grant.closed)
	}
}
