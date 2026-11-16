package runview

import (
	"context"
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServiceCursorAfterMaxKeepsAlertsAndTerminates(t *testing.T) {
	svc, dir := newStreamTestService(t)
	seedStreamRun(t, svc, "r", 0)
	line, e := json.Marshal(store.Event{RunID: "r", Seq: math.MaxInt64, Type: store.EventNodeStarted})
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "runs", "r", "events.jsonl"), append(line, '\n'), 0600); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sub, e := runstream.SubscribeEventsCursor(ctx, svc.StreamSource(), "r", runstream.EventCursor{Seq: math.MaxInt64, After: true})
	if e != nil {
		t.Fatal(e)
	}
	defer sub.Close()
	// SubscribeEventsCursor registers the actual broker before returning.
	for _, seq := range []int64{0, 1} {
		svc.Broker().Publish(store.Event{RunID: "r", Seq: seq, Type: store.EventAlert})
		got := drainEventBatches(t, sub, nil, time.Second, func(rows []*store.Event) bool { return len(rows) > 0 })
		if len(got) != 1 || got[0].Type != store.EventAlert || got[0].Seq != seq {
			t.Fatal(got)
		}
	}
	run, e := svc.store.LoadRun(ctx, "r")
	if e != nil {
		t.Fatal(e)
	}
	run.Status = store.RunStatusFinished
	if e = svc.store.SaveRun(ctx, run); e != nil {
		t.Fatal(e)
	}
	// Terminal catch-up must not replay Max again. The service poll is 5s by default.
	select {
	case rows, ok := <-sub.Events():
		if ok {
			t.Fatal("terminal catch-up wrapped", rows)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("terminal source did not close")
	}
}
func TestServiceCursorAlertHighwaterPreserved(t *testing.T) {
	svc, _ := newStreamTestService(t)
	seedStreamRun(t, svc, "r", 3)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sub, e := runstream.SubscribeEventsCursor(ctx, svc.StreamSource(), "r", runstream.EventCursor{Seq: 2, After: true})
	if e != nil {
		t.Fatal(e)
	}
	defer sub.Close()
	svc.Broker().Publish(store.Event{RunID: "r", Seq: 5, Type: store.EventAlert})
	rows := drainEventBatches(t, sub, nil, time.Second, func(rows []*store.Event) bool { return len(rows) > 0 })
	if rows[0].Seq != 5 || rows[0].Type != store.EventAlert {
		t.Fatal(rows)
	}
	// An alert carrying Seq 5 must not move the persisted cursor: seq 3 and 4,
	// appended after it, are both delivered.
	for _, node := range []string{"three", "four"} {
		if _, e := svc.store.AppendEvent(ctx, "r", store.Event{Type: store.EventNodeStarted, NodeID: node}); e != nil {
			t.Fatal(e)
		}
	}
	rows = drainEventBatches(t, sub, nil, 5*time.Second, func(rows []*store.Event) bool { return len(rows) >= 2 })
	if len(rows) != 2 || rows[0].Seq != 3 || rows[1].Seq != 4 {
		t.Fatal("live alert highwater changed", rows)
	}
}
