package runview

import (
	"context"
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
