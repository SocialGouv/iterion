package runview

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/SocialGouv/iterion/pkg/alert"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestAlertManagerWiresPersistedHumanWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		s, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"parent", "child"} {
			if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.SetSubbotChild(ctx, "parent", "subbot", "child"); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateRunStatus(ctx, "child", store.RunStatusPausedWaitingHuman, ""); err != nil {
			t.Fatal(err)
		}
		got := make(chan alert.Alert, 8)
		svc := &Service{store: s, logger: iterlog.Nop()}
		m := svc.buildAlertManager(AlertSettings{StallTimeout: time.Second, DesktopSink: alert.FuncSink(func(_ context.Context, a alert.Alert) { got <- a })})
		m.Start(ctx)
		defer func() { m.Stop(); synctest.Wait() }()
		m.Observe(store.Event{RunID: "parent", Type: store.EventNodeStarted, NodeID: "subbot", Timestamp: time.Now()})
		// The manager polls at least every five seconds, even with a shorter
		// stall threshold. Cross that first poll while the child is paused.
		time.Sleep(6 * time.Second)
		synctest.Wait()
		select {
		case a := <-got:
			t.Fatalf("paused child raised an alert: %+v", a)
		default:
		}
		if err := s.UpdateRunStatus(ctx, "child", store.RunStatusRunning, ""); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		select {
		case a := <-got:
			if a.Kind != alert.KindStall || a.RunID != "parent" {
				t.Fatalf("alert=%+v", a)
			}
		default:
			t.Fatal("genuine parent silence was muted after child resumed")
		}
	})
}

// Enforce Mongo's context contract over the real filesystem store. The alert
// manager has no request context; its internal callbacks must recover identity
// from the known root run and scope descendant reads and event writes to it.
type tenantAlertStore struct{ store.RunStore }

func (s tenantAlertStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	r, err := s.RunStore.LoadRun(ctx, id)
	if err != nil {
		return nil, err
	}
	tenant, ok := store.TenantFromContext(ctx)
	if ok && tenant == r.TenantID || !ok && store.IsWithoutTenantFilter(ctx) {
		return r, nil
	}
	return nil, fmt.Errorf("run %s is outside the context tenant", id)
}

func (s tenantAlertStore) AppendEvent(ctx context.Context, id string, event store.Event) (*store.Event, error) {
	r, err := s.RunStore.LoadRun(ctx, id)
	if err != nil {
		return nil, err
	}
	tenant, _ := store.TenantFromContext(ctx)
	if tenant == "" || tenant != r.TenantID {
		return nil, fmt.Errorf("health event has no run tenant")
	}
	event.TenantID = tenant
	return s.RunStore.AppendEvent(ctx, id, event)
}

func TestAlertHumanWaitRecoversTenantAndScopesDescendants(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		s, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"parent", "child", "foreign"} {
			r, err := s.CreateRun(ctx, id, "wf", nil)
			if err != nil {
				t.Fatal(err)
			}
			r.TenantID = "team-a"
			if id == "foreign" {
				r.TenantID = "team-b"
			}
			if err := s.SaveRun(ctx, r); err != nil {
				t.Fatal(err)
			}
			if id != "parent" {
				if err := s.SetSubbotChild(ctx, "parent", id, id); err != nil {
					t.Fatal(err)
				}
				if err := s.UpdateRunStatus(ctx, id, store.RunStatusPausedWaitingHuman, ""); err != nil {
					t.Fatal(err)
				}
			}
		}
		got := make(chan alert.Alert, 8)
		svc := &Service{store: tenantAlertStore{s}, logger: iterlog.Nop()}
		m := svc.buildAlertManager(AlertSettings{StallTimeout: time.Second, DesktopSink: alert.FuncSink(func(_ context.Context, a alert.Alert) { got <- a })})
		m.Start(ctx)
		defer func() { m.Stop(); synctest.Wait() }()
		m.Observe(store.Event{RunID: "parent", Type: store.EventNodeStarted, Timestamp: time.Now()})
		time.Sleep(6 * time.Second)
		synctest.Wait()
		if len(got) != 0 {
			t.Fatal("missing tenant identity raised a false stall")
		}
		if err := s.UpdateRunStatus(ctx, "child", store.RunStatusRunning, ""); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		select {
		case a := <-got:
			if a.Kind != alert.KindStall {
				t.Fatalf("alert=%+v", a)
			}
		default:
			t.Fatal("a foreign paused descendant muted this tenant's real stall")
		}
		events, err := s.LoadEvents(ctx, "parent")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range events {
			if e.Type == store.EventRunHealth {
				found = true
				if e.TenantID != "team-a" {
					t.Fatalf("health event tenant=%q", e.TenantID)
				}
			}
		}
		if !found {
			t.Fatal("stall health event was not persisted under the run tenant")
		}
	})
}
