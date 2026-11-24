package runview

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type closingBarrierGrant struct {
	store.RunFilesGrant
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *closingBarrierGrant) Close() error {
	g.once.Do(func() { close(g.entered); <-g.release })
	return g.RunFilesGrant.Close()
}
func TestIndependentR53ClosePrecedesDoneAndHandoff(t *testing.T) {
	spec, st := r53ViewSpec(t, true)
	ex, err := BuildExecutor(spec)
	if err != nil {
		t.Fatal(err)
	}
	barrier := &closingBarrierGrant{RunFilesGrant: st.grant.RunFilesGrant, entered: make(chan struct{}), release: make(chan struct{})}
	st.grant.RunFilesGrant = barrier
	svc, err := NewService(st.base.Root(), WithStore(st.base), WithLogger(iterlog.Nop()), WithRecoveryPassive())
	if err != nil {
		t.Fatal(err)
	}
	defer stopService(t, svc)
	bodyEnter := make(chan struct{})
	bodyRelease := make(chan struct{})
	defer func() {
		select {
		case <-barrier.release:
		default:
			close(barrier.release)
		}
		select {
		case <-bodyRelease:
		default:
			close(bodyRelease)
		}
	}()
	res, err := svc.spawnRun(spec.Ctx, "r", spec.Workflow, "", "", "", nil, finalizationOpts{}, callbackOpts{}, ex, iterlog.Nop(), nil, nil, 0, false, nil, "", nil, "", false, launchExtras{}, nil, func(context.Context, context.Context, *runtime.Engine) error {
		close(bodyEnter)
		<-bodyRelease
		return errors.New("body failure")
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-bodyEnter:
	case <-time.After(5 * time.Second):
		t.Fatal("no body")
	}
	select {
	case <-barrier.entered:
		t.Fatal("closed during body")
	default:
	}
	close(bodyRelease)
	select {
	case <-barrier.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Close not reached")
	}
	select {
	case <-res.Done:
		t.Fatal("Done before Close completed")
	default:
	}
	if !svc.manager.Active("r") {
		t.Fatal("handoff released before Close completed")
	}
	close(barrier.release)
	select {
	case <-res.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("Done not closed")
	}
	if st.grant.closed != 1 {
		t.Fatal("close count", st.grant.closed)
	}
}

type closingFailStore struct {
	store.RunStore
	lockFail bool
	saves    int
}

func (s *closingFailStore) LockRun(c context.Context, id string) (store.RunLock, error) {
	if s.lockFail {
		return nil, errors.New("injected lock refusal")
	}
	return s.RunStore.LockRun(c, id)
}
func (s *closingFailStore) SaveRun(context.Context, *store.Run) error {
	s.saves++
	return errors.New("injected save refusal")
}
func TestIndependentR53AllPrelaunchBranches(t *testing.T) {
	for _, name := range []string{"lock", "route", "pool", "budget"} {
		t.Run(name, func(t *testing.T) {
			spec, st := r53ViewSpec(t, true)
			ex, err := BuildExecutor(spec)
			if err != nil {
				t.Fatal(err)
			}
			failed := &closingFailStore{RunStore: st.base, lockFail: name == "lock"}
			svc, err := NewService(st.base.Root(), WithStore(failed), WithLogger(iterlog.Nop()), WithRecoveryPassive())
			if err != nil {
				t.Fatal(err)
			}
			defer stopService(t, svc)
			extra := launchExtras{}
			switch name {
			case "route":
				extra.routePolicy = &store.RunLLMRoutePolicy{}
			case "pool":
				extra.poolAttachment = &store.RunPoolAttachment{}
			case "budget":
				extra.budgetOverrides = &store.RunBudgetOverrides{}
			}
			_, err = svc.spawnRun(spec.Ctx, "r", spec.Workflow, "", "", "", nil, finalizationOpts{}, callbackOpts{}, ex, iterlog.Nop(), nil, nil, 0, false, nil, "", nil, "", false, extra, nil, func(context.Context, context.Context, *runtime.Engine) error { t.Error("body launched"); return nil })
			if err == nil {
				t.Fatal("missing injected error")
			}
			if st.grant.closed != 1 {
				t.Fatal("close count", st.grant.closed)
			}
			if svc.manager.Active("r") {
				t.Fatal("manager leaked")
			}
			if name != "lock" && failed.saves != 1 {
				t.Fatal("did not reach save", failed.saves)
			}
		})
	}
}

type independentCloseBarrier struct{ entered, release chan struct{} }

func (b *independentCloseBarrier) Close() error { close(b.entered); <-b.release; return nil }
func TestIndependentR53FileLockProtectsHandoffCleanup(t *testing.T) {
	spec, st := r53ViewSpec(t, false)
	ex, err := BuildExecutor(spec)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := st.base.EnsureRunFilesDir(spec.Ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	rec := model.NewHandoffRecorder(dir, nil)
	rec.Observe(store.EventAssistantText, "old", map[string]any{"text": "old work"})
	if rec.Seal("old", nil) == "" {
		t.Fatal("seal setup")
	}
	model.WithHandoffRecorder(rec)(ex)
	barrier := &independentCloseBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	model.WithExtraClosers(barrier)(ex)
	svc, err := NewService(st.base.Root(), WithStore(st.base), WithLogger(iterlog.Nop()), WithRecoveryPassive())
	if err != nil {
		t.Fatal(err)
	}
	defer stopService(t, svc)
	defer func() {
		select {
		case <-barrier.release:
		default:
			close(barrier.release)
		}
	}()
	res, err := svc.spawnRun(spec.Ctx, "r", spec.Workflow, "", "", "", nil, finalizationOpts{}, callbackOpts{}, ex, iterlog.Nop(), nil, nil, 0, false, nil, "", nil, "", false, launchExtras{}, nil, func(context.Context, context.Context, *runtime.Engine) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-barrier.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("not closing")
	}
	successor, lockErr := st.base.LockRun(spec.Ctx, "r")
	if lockErr == nil {
		defer successor.Unlock()
		foreign := filepath.Join(dir, "routing-handoff", "successor.md")
		if err = os.WriteFile(foreign, []byte("successor"), 0600); err != nil {
			t.Fatal(err)
		}
		close(barrier.release)
		<-res.Done
		_, statErr := os.Stat(foreign)
		t.Fatalf("successor acquired run lock before old executor cleanup; successor file stat after cleanup=%v", statErr)
	}
	close(barrier.release)
	select {
	case <-res.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("not done")
	}
}

func TestIndependentR53PrelaunchUncreatedHandoffPreservesIncumbent(t *testing.T) {
	spec, st := r53ViewSpec(t, false)
	ex, err := BuildExecutor(spec)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := st.base.EnsureRunFilesDir(spec.Ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	rec := model.NewHandoffRecorder(dir, nil)
	model.WithHandoffRecorder(rec)(ex)
	incumbent := filepath.Join(dir, "routing-handoff", "incumbent")
	if err = os.MkdirAll(filepath.Dir(incumbent), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(incumbent, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(st.base.Root(), WithStore(st.base), WithLogger(iterlog.Nop()), WithRecoveryPassive())
	if err != nil {
		t.Fatal(err)
	}
	defer stopService(t, svc)
	_, err = svc.spawnRun(spec.Ctx, "missing", spec.Workflow, "", "", "", nil, finalizationOpts{}, callbackOpts{}, ex, iterlog.Nop(), nil, nil, 0, false, nil, "", nil, "", false, launchExtras{}, nil, func(context.Context, context.Context, *runtime.Engine) error { return nil })
	if err == nil {
		t.Fatal("expected error")
	}
	if b, err := os.ReadFile(incumbent); err != nil || string(b) != "keep" {
		t.Fatal("uncreated recorder removed incumbent", string(b), err)
	}
}
