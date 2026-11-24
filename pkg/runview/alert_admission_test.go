package runview

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// One admission rule decides every alert a bound merge forwards from the
// run's broker lane — in its live loop, at a closed registry lane's drain and
// at the end flush: a frame stamped with another header than the binding's
// never ships, and a held registry lane dedups its own carrier.

// successorObservation is obs's header under another birth: the stamp of a
// run that took the ID after obs.
func successorObservation(t *testing.T, obs store.RunObservation) store.RunObservation {
	t.Helper()
	h := obs.Header()
	h.BirthID = "successor-of-" + h.BirthID
	succ, err := store.CaptureRunObservation(h, obs.TenantID(), true)
	if err != nil {
		t.Fatal(err)
	}
	return succ
}

// alertStampedWith is an ephemeral alert frame named nodeID born on obs.
func alertStampedWith(t *testing.T, nodeID string, obs store.RunObservation) store.Event {
	t.Helper()
	ev, err := store.WithEventObservation(store.Event{RunID: obs.Header().RunID, Type: store.EventAlert, NodeID: nodeID}, obs)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// brokerQueued returns how many frames wait on runID's broker subscriptions,
// and how many subscriptions there are.
func brokerQueued(svc *Service, runID string) (queued, subs int) {
	svc.broker.mu.RLock()
	defer svc.broker.mu.RUnlock()
	for _, sub := range svc.broker.subscribers[runID] {
		queued += len(sub.ch)
	}
	return queued, len(svc.broker.subscribers[runID])
}

// laneSubscribers counts the open merge subscriptions of obs's current
// registry epoch.
func laneSubscribers(svc *Service, obs store.RunObservation) int {
	svc.alertRegistry.mu.Lock()
	defer svc.alertRegistry.mu.Unlock()
	tp := svc.alertRegistry.topics[obs]
	if tp == nil || tp.closed {
		return 0
	}
	n := 0
	for _, sub := range tp.subs {
		if !sub.closed {
			n++
		}
	}
	return n
}

// streamReader accumulates what a subscription delivered, alert frames and
// persisted rows apart, across successive reads.
type streamReader struct {
	sub               runstream.EventSubscription
	alerts, persisted []*store.Event
}

// until reads the subscription until done holds over everything read so far.
func (r *streamReader) until(t *testing.T, d time.Duration, what string, done func(r *streamReader) bool) {
	t.Helper()
	deadline := time.After(d)
	for !done(r) {
		select {
		case batch, ok := <-r.sub.Events():
			if !ok {
				t.Fatalf("%s: the stream ended; alerts %v persisted %v", what, describe(r.alerts), describe(r.persisted))
			}
			for _, ev := range batch {
				if ev.Type == store.EventAlert {
					r.alerts = append(r.alerts, ev)
				} else {
					r.persisted = append(r.persisted, ev)
				}
			}
		case err, ok := <-r.sub.Errors():
			if ok && runstream.IsPermanentError(err) {
				t.Fatalf("%s: the stream failed: %v", what, err)
			}
		case <-deadline:
			t.Fatalf("%s: not reached within %s; alerts %v persisted %v", what, d, describe(r.alerts), describe(r.persisted))
		}
	}
}

// describe names each event by type, node ID and budget axis.
func describe(evs []*store.Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, fmt.Sprintf("%s/%s/%s", ev.Type, ev.NodeID, axisOf(ev)))
	}
	return out
}

// hasNodeID reports whether evs holds a frame whose node ID is nodeID.
func hasNodeID(evs []*store.Event, nodeID string) bool {
	for _, ev := range evs {
		if ev.NodeID == nodeID {
			return true
		}
	}
	return false
}

// axisOf returns an alert frame's or run_health twin's budget axis.
func axisOf(ev *store.Event) string {
	axis, _ := ev.Data["axis"].(string)
	return axis
}

// withAxis reports whether evs holds an event of type typ on axis.
func withAxis(evs []*store.Event, typ store.EventType, axis string) bool {
	for _, ev := range evs {
		if ev.Type == typ && axisOf(ev) == axis {
			return true
		}
	}
	return false
}

// heldLaneMerge is a bound merge holding its run's registry lane, blocked in
// Ship: its consumer reads nothing until the test does, so frames published
// meanwhile queue on the merge's broker lane behind the hold.
type heldLaneMerge struct {
	svc     *Service
	sub     runstream.EventSubscription
	obs     store.RunObservation
	release func() // the lane's only bail
}

// holdALaneMerge builds a heldLaneMerge on run "r". Once the merge is blocked
// in Ship it arms the laneFirst seam: a merge holding a registry lane then
// serves that lane before its broker lane, so a closed lane's drain — not the
// live loop — reads the frames queued behind the hold.
func holdALaneMerge(t *testing.T) *heldLaneMerge {
	t.Helper()
	svc, native, _ := newLiveEventsService(t, WithAlerts(AlertSettings{StallTimeout: time.Hour}))
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 0); err != nil {
		t.Fatal(err)
	}
	// Active: no ID-keyed tailer takes a bail of its own, so the test's
	// release alone ends the lane's epoch.
	if _, err := svc.manager.Register(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.manager.Deregister("r") })
	release, err := svc.registerRunAlertOrigin(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	obs, err := svc.CaptureRunOrigin(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	var laneFirst atomic.Bool
	svc.liveEvents.laneFirst = laneFirst.Load
	// From seq 0 inclusive: the run's first persisted row is a twin.
	sub, err := runstream.SubscribeEventsCursor(boundOriginCtx(t, svc, "r"), svc.StreamSource(), "r", runstream.EventCursor{Seq: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	if n := laneSubscribers(svc, obs); n != 1 {
		t.Fatalf("the bound merge holds %d registry lanes, want 1", n)
	}
	if _, subs := brokerQueued(svc, "r"); subs != 1 {
		t.Fatalf("%d broker subscriptions on r, want the merge's alone", subs)
	}
	// The hold: unstamped alerts nobody reads fill the pipe one at a time (the
	// run has no persisted event to replay); once it is full, the next alert
	// the merge takes off the broker lane is held in Ship.
	events := sub.Events()
	for i := 0; ; i++ {
		svc.broker.Publish(store.Event{RunID: "r", Type: store.EventAlert, NodeID: fmt.Sprintf("fill-%d", i)})
		if i < cap(events) {
			awaitCondition(t, 5*time.Second, fmt.Sprintf("fill-%d in the pipe", i), func() bool { return len(events) == i+1 })
			continue
		}
		awaitCondition(t, 5*time.Second, fmt.Sprintf("fill-%d taken by the merge", i), func() bool {
			queued, _ := brokerQueued(svc, "r")
			return queued == 0
		})
		break
	}
	laneFirst.Store(true)
	return &heldLaneMerge{svc: svc, sub: sub, obs: obs, release: release}
}

// W6b: the end flush admits a stamped alert only on the binding's origin. No
// registration exists, so the end flush's binding check alone decides: A's
// buffered alert is forwarded, B's never.
func TestLiveEventsEndFlushRefusesAnotherOrigin(t *testing.T) {
	prev := svcEventTerminalPollInterval
	svcEventTerminalPollInterval = time.Hour
	t.Cleanup(func() { svcEventTerminalPollInterval = prev })
	svc, native, _ := newLiveEventsService(t)
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	obsA, err := svc.CaptureRunOrigin(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	successor := alertStampedWith(t, "successor-alert", successorObservation(t, obsA))
	origin := alertStampedWith(t, "origin-alert", obsA)
	// Published once the retained subscription ended: only the end flush can
	// deliver them.
	svc.liveEvents.beforeFinish = func(context.Context) {
		svc.broker.Publish(successor)
		svc.broker.Publish(origin)
	}
	sub, err := runstream.SubscribeEventsCursor(boundOriginCtx(t, svc, "r"), svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if _, err := native.AppendEvent(ctx, "r", store.Event{Type: store.EventRunFinished}); err != nil {
		t.Fatal(err)
	}
	if err := native.UpdateRunStatus(ctx, "r", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	svc.broker.CloseRun("r")
	got, errs := drainEventsWithin(t, sub, 5*time.Second)
	if len(errs) != 0 {
		t.Fatalf("errors = %v, want a clean end", errs)
	}
	if hasNodeID(got, "successor-alert") {
		t.Fatalf("the end flush forwarded a successor-stamped alert: %v", describe(got))
	}
	if len(got) == 0 || got[len(got)-1].NodeID != "origin-alert" {
		t.Fatalf("delivered %v, want the origin's buffered alert last", describe(got))
	}
}

// W6c: a closed registry lane's drain applies the binding too. A successor-
// stamped frame queued while the merge was held in Ship, read by the drain of
// the lane released meanwhile, never ships; the unstamped fence behind it does.
func TestLiveEventsClosedLaneDrainRefusesAnotherOrigin(t *testing.T) {
	h := holdALaneMerge(t)
	h.release()
	h.svc.broker.Publish(alertStampedWith(t, "successor-alert", successorObservation(t, h.obs)))
	h.svc.broker.Publish(store.Event{RunID: "r", Type: store.EventAlert, NodeID: "fence"})

	r := &streamReader{sub: h.sub}
	r.until(t, 10*time.Second, "the fence", func(r *streamReader) bool { return hasNodeID(r.alerts, "fence") })
	if hasNodeID(r.alerts, "successor-alert") {
		t.Fatalf("a successor-stamped alert queued at the lane's close reached the bound stream: %v", describe(r.alerts))
	}
}

// P0(e), measured: an alert of a re-adoption of the SAME observation (a
// resume's new epoch), dispatched while the merge still holds the previous,
// closed epoch, never reaches the stream as an alert frame. Its broker copy
// carries the lane's own observation, which the closed lane's drain drops as
// that lane's duplicate; its registry copy went to the new epoch before this
// merge adopted it. Its persisted run_health twin does reach the stream, and
// the next alert ships once the merge adopted the new epoch.
func TestLiveEventsReadoptedAlertQueuedAtTheLaneCloseIsDropped(t *testing.T) {
	h := holdALaneMerge(t)
	ctx := t.Context()
	h.release()
	if _, err := h.svc.registerRunAlertOrigin(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	if obs, registered := h.svc.alertManager.Observation("r"); !registered || obs != h.obs {
		t.Fatalf("re-adoption registered %+v (%t), want the same observation", obs, registered)
	}
	budget := func(axis string) {
		h.svc.alertManager.Observe(store.Event{RunID: "r", Type: store.EventBudgetWarning, NodeID: "n", Timestamp: time.Now(), Data: map[string]any{"dimension": axis}})
	}
	budget("readopted")
	h.svc.broker.Publish(store.Event{RunID: "r", Type: store.EventAlert, NodeID: "fence"})
	r := &streamReader{sub: h.sub}
	r.until(t, 10*time.Second, "the fence", func(r *streamReader) bool { return hasNodeID(r.alerts, "fence") })

	// Past the fence the merge holds no lane: the next alert's broker copy
	// ships, and both twins land as persisted rows.
	budget("after")
	r.until(t, 10*time.Second, "the next alert and both twins", func(r *streamReader) bool {
		return withAxis(r.alerts, store.EventAlert, "after") &&
			withAxis(r.persisted, store.EventRunHealth, "readopted") && withAxis(r.persisted, store.EventRunHealth, "after")
	})
	if withAxis(r.alerts, store.EventAlert, "readopted") {
		t.Fatalf("the re-adoption's alert frame reached the stream: %v", describe(r.alerts))
	}
}
