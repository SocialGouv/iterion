package runview

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
	"time"
)

type lockOrderWitness struct {
	t     *testing.T
	st    store.RunStore
	ctx   context.Context
	calls int
}

func (w *lockOrderWitness) Close() error {
	w.calls++
	l, e := w.st.LockRun(w.ctx, "r")
	if e == nil {
		l.Unlock()
		w.t.Error("executor cleanup ran after run lock release")
	}
	return errors.New("close error must not prevent unlock")
}
func TestCloseUnderLockAcrossSuccessAndPersistenceFailures(t *testing.T) {
	for _, path := range []string{"success", "register", "route", "pool", "budget"} {
		t.Run(path, func(t *testing.T) {
			spec, st := r53ViewSpec(t, false)
			ex, e := BuildExecutor(spec)
			if e != nil {
				t.Fatal(e)
			}
			w := &lockOrderWitness{t: t, st: st.base, ctx: spec.Ctx}
			model.WithExtraClosers(w)(ex)
			var serviceStore store.RunStore = st.base
			if path != "success" && path != "register" {
				serviceStore = &closingFailStore{RunStore: st.base}
			}
			svc, e := NewService(st.base.Root(), WithStore(serviceStore), WithLogger(iterlog.Nop()), WithRecoveryPassive())
			if e != nil {
				t.Fatal(e)
			}
			defer stopService(t, svc)
			if path == "register" {
				if _, e := svc.manager.Register(spec.Ctx, "r"); e != nil {
					t.Fatal(e)
				}
				defer svc.manager.Deregister("r")
			}
			extra := launchExtras{}
			switch path {
			case "route":
				extra.routePolicy = &store.RunLLMRoutePolicy{}
			case "pool":
				extra.poolAttachment = &store.RunPoolAttachment{}
			case "budget":
				extra.budgetOverrides = &store.RunBudgetOverrides{}
			}
			res, e := svc.spawnRun(spec.Ctx, "r", spec.Workflow, "", "", "", nil, finalizationOpts{}, callbackOpts{}, ex, iterlog.Nop(), nil, 0, false, nil, "", nil, "", false, extra, nil, func(context.Context, context.Context, *runtime.Engine) error { return nil })
			if path == "success" {
				if e != nil {
					t.Fatal(e)
				}
				select {
				case <-res.Done:
				case <-time.After(5 * time.Second):
					t.Fatal("cleanup failed to finish")
				}
			} else if e == nil {
				t.Fatal("injected persistence failure missing")
			}
			if w.calls != 1 {
				t.Fatalf("Close calls=%d", w.calls)
			}
			l, e := st.base.LockRun(spec.Ctx, "r")
			if e != nil {
				t.Fatal("lock not released after cleanup", e)
			}
			if e = l.Unlock(); e != nil {
				t.Fatal(e)
			}
		})
	}
}

type panicCloseWitness struct{ calls int }

func (w *panicCloseWitness) Close() error { w.calls++; panic("injected closer panic") }
func TestPrelaunchCloserPanicStillReleasesAcquiredLock(t *testing.T) {
	spec, st := r53ViewSpec(t, false)
	ex, e := BuildExecutor(spec)
	if e != nil {
		t.Fatal(e)
	}
	pc := &panicCloseWitness{}
	model.WithExtraClosers(pc)(ex)
	svc, e := NewService(st.base.Root(), WithStore(&closingFailStore{RunStore: st.base}), WithLogger(iterlog.Nop()), WithRecoveryPassive())
	if e != nil {
		t.Fatal(e)
	}
	defer stopService(t, svc)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected closer panic")
			}
		}()
		_, _ = svc.spawnRun(spec.Ctx, "r", spec.Workflow, "", "", "", nil, finalizationOpts{}, callbackOpts{}, ex, iterlog.Nop(), nil, 0, false, nil, "", nil, "", false, launchExtras{routePolicy: &store.RunLLMRoutePolicy{}}, nil, func(context.Context, context.Context, *runtime.Engine) error { return nil })
	}()
	if pc.calls != 1 {
		t.Fatalf("Close retried after panic: %d", pc.calls)
	}
	l, e := st.base.LockRun(spec.Ctx, "r")
	if e != nil {
		t.Fatal("run lock leaked after recovered Close panic", e)
	}
	l.Unlock()
}
