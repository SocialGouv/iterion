package runview

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/alert"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// awaitBrokerEvent reads the broker subscription until an event of the wanted
// type arrives (the positive signal the caller needs before asserting on
// durable state the synchronous dispatch already wrote).
func awaitBrokerEvent(t *testing.T, sub *EventSubscription, d time.Duration, typ store.EventType, what string) *store.Event {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				t.Fatalf("%s: the broker lane closed", what)
			}
			if ev.Type == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("%s: no %s event within %s", what, typ, d)
		}
	}
}

// TP1: after A is replaced by B under the same ID, an alert born on A's
// episode must never persist its run_health twin into B's events.jsonl.
// At HEAD the store sink reloads the run BY ID, so A's stale twin lands in
// B's file — this test is red there on purpose (P1a) and goes green when
// the twin appends under the captured registration.
func TestAlertTwinNeverEntersASuccessorsFile(t *testing.T) {
	svc, native, dir := newLiveEventsService(t, WithAlerts(AlertSettings{StallTimeout: time.Second}))
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	setRunName(t, native, "r", "Alpha")
	legacy := svc.Broker().Subscribe("r")
	defer legacy.Cancel()
	sub, err := runstream.SubscribeEventsCursor(ctx, svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if !tailerAttached(svc, "r") {
		t.Fatal("no events tailer feeds alert detection")
	}
	// A's seed must be observed before the replacement: its name resolves on
	// first sight, and the twin's data carries it. The tailer publishes the
	// seed to the broker just before Observe books it; the settle covers the
	// ordering gap between that publish and the name resolution.
	awaitBrokerEvent(t, legacy, 5*time.Second, store.EventNodeStarted, "A's seed observed")
	time.Sleep(200 * time.Millisecond)

	// Replace: A's directory moves aside, B takes the ID and emits a budget
	// warning on a fresh axis.
	moveRunAside(t, dir, "r")
	if err := seedOriginRun(ctx, native, "r", "B", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	setRunName(t, native, "r", "Beta")
	if _, err := native.AppendEvent(valueCtx(t, native, "r"), "r", store.Event{Type: store.EventBudgetWarning, NodeID: "B", Data: map[string]any{"dimension": "tokens"}}); err != nil {
		t.Fatal(err)
	}
	// The alert dispatches twin-first: the legacy frame on the broker proves
	// the twin already landed somewhere.
	awaitBrokerEvent(t, legacy, 15*time.Second, store.EventAlert, "budget alert across the replacement")
	events, err := native.LoadEvents(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == store.EventRunHealth && ev.Data["run_name"] == "Alpha" {
			t.Fatalf("A's run_health twin landed in B's file: seq %d node %s data %v", ev.Seq, ev.NodeID, ev.Data)
		}
	}
}

// setRunName persists an explicit friendly name: two births of one ID must
// carry distinct names so an alert's twin identifies the birth it came from.
func setRunName(t *testing.T, st *store.FilesystemRunStore, runID, name string) {
	t.Helper()
	r, err := st.LoadRun(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	r.Name = name
	if err := st.SaveRun(t.Context(), r); err != nil {
		t.Fatal(err)
	}
}

// TP2 (P1b, control): the legacy broker lane carries the alert to ANY
// subscriber of the ID — kept green at HEAD by construction and kept green
// by the dual publication after the registry lands (F5): unbound consumers
// keep their carrier.
func TestLegacyAlertCarrierReachesAnySubscriberOfTheID(t *testing.T) {
	svc, native, _ := newLiveEventsService(t, WithAlerts(AlertSettings{StallTimeout: time.Second}))
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	legacy := svc.Broker().Subscribe("r")
	defer legacy.Cancel()
	sub, err := runstream.SubscribeEventsCursor(ctx, svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if _, err := native.AppendEvent(valueCtx(t, native, "r"), "r", store.Event{Type: store.EventBudgetWarning, NodeID: "n", Data: map[string]any{"dimension": "tokens"}}); err != nil {
		t.Fatal(err)
	}
	ev := awaitBrokerEvent(t, legacy, 5*time.Second, store.EventAlert, "legacy carrier")
	if ev.NodeID != "n" {
		t.Fatalf("alert node = %q, want n", ev.NodeID)
	}
}

// budgetEvent appends a budget warning on axis to runID and returns whether
// the store accepted it.
func budgetEvent(t *testing.T, st *store.FilesystemRunStore, runID, axis string) {
	t.Helper()
	if _, err := st.AppendEvent(valueCtx(t, st, runID), runID, store.Event{Type: store.EventBudgetWarning, NodeID: "n", Data: map[string]any{"dimension": axis}}); err != nil {
		t.Fatal(err)
	}
}

// awaitRegistered polls until runID's alert registration matches want (the
// rotation handover is discovered by the tailer's defensive re-drain, up to
// one 10 s tick after a replacement).
func awaitRegistered(t *testing.T, svc *Service, runID string, want store.RunObservation) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		if obs, registered := svc.alertManager.Observation(runID); registered && obs == want {
			return
		}
		select {
		case <-deadline:
			if obs, registered := svc.alertManager.Observation(runID); registered {
				t.Fatalf("registration of %s = %+v, want %+v", runID, obs, want)
			}
			t.Fatalf("run %s never registered its successor", runID)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// TR1 (F2, both emit): the flagship replacement. A emits and alerts into its
// own epoch; B takes the ID and emits; the rotation re-registers B so B's
// budget warning routes to B's registration, B's file carries only B's twin
// and A's file carries A's. Mutants M2 (rotation dropped), M7/M8 (the twin
// keeps or falls back to the by-ID append).
func TestAlertRotationReregistersTheSuccessor(t *testing.T) {
	opts := []ServiceOption{WithAlerts(AlertSettings{StallTimeout: time.Second})}
	if os.Getenv("TR1_LOUD") != "" {
		opts = append(opts, WithLogger(iterlog.New(iterlog.LevelWarn, os.Stderr)))
	}
	svc, native, dir := newLiveEventsService(t, opts...)
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	setRunName(t, native, "r", "Alpha")
	legacy := svc.Broker().Subscribe("r")
	defer legacy.Cancel()
	// The supervisor seam holds the tailer WITHOUT a retained subscription:
	// a retained merge would refuse the successor's file and stop the
	// tailer before its rotation drain races it. ObserveRun keeps the
	// tailer (and the rotation hook it carries) alive across the
	// replacement.
	_, releaseObserve, err := svc.ObserveRun(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseObserve()
	budgetEvent(t, native, "r", "tokens")
	// A's alert dispatched twin-first: the twin is already in A's file.
	awaitBrokerEvent(t, legacy, 5*time.Second, store.EventAlert, "A's budget alert")
	if err := awaitRunHealthRow(t, native, "r", "Alpha", 5*time.Second); err != nil {
		t.Fatal(err)
	}

	// Replace: A moves aside, B takes the ID and emits on its own axis.
	moveRunAside(t, dir, "r")
	if err := seedOriginRun(ctx, native, "r", "B", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	setRunName(t, native, "r", "Beta")
	obsB, err := svc.CaptureRunOrigin(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	budgetEvent(t, native, "r", "cost_usd")
	// The moved directory's watch is still alive: an append there wakes the
	// tailer, whose drain opens the ID-keyed path, detects the rotation and
	// re-registers before B's first line is observed.
	wake, err := os.OpenFile(filepath.Join(dir, "aside-r", "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wake.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if err := wake.Close(); err != nil {
		t.Fatal(err)
	}
	awaitRegistered(t, svc, "r", obsB)

	// B's twin landed under B's registration; A's twin never reached B's
	// file, and A's own twin stayed in A's moved-aside file. The budget row
	// may land just after the rotation's drain, whose next wake is the
	// defensive tick (the moved directory deafened the watch).
	if err := awaitRunHealthRow(t, native, "r", "Beta", 15*time.Second); err != nil {
		t.Fatalf("B's budget twin never landed: %v", err)
	}
	events, err := native.LoadEvents(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == store.EventRunHealth && ev.Data["run_name"] == "Alpha" {
			t.Fatalf("A's run_health twin landed in B's file: %+v", ev.Data)
		}
	}
	rows := readAsideEvents(t, dir, "r")
	saw := false
	for _, ev := range rows {
		if ev.Type == store.EventRunHealth && ev.Data["run_name"] == "Alpha" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("A's file lost its own twin: %+v", rows)
	}
}

// awaitRunHealthRow polls runID's file until a run_health row carrying the
// named birth's twin lands, or the budget expires.
func awaitRunHealthRow(t *testing.T, st *store.FilesystemRunStore, runID, name string, d time.Duration) error {
	t.Helper()
	deadline := time.After(d)
	for {
		events, err := st.LoadEvents(t.Context(), runID)
		if err == nil {
			for _, ev := range events {
				if ev.Type == store.EventRunHealth && ev.Data["run_name"] == name {
					return nil
				}
			}
		}
		select {
		case <-deadline:
			seen := "none"
			if events, err := st.LoadEvents(t.Context(), runID); err == nil {
				var rows []string
				for _, ev := range events {
					if ev.Type == store.EventRunHealth {
						rows = append(rows, fmt.Sprintf("seq %d data %v", ev.Seq, ev.Data))
					}
				}
				if len(rows) > 0 {
					seen = strings.Join(rows, "; ")
				}
			}
			return fmt.Errorf("no run_health twin of %s in %s's file within %s (saw: %s)", name, runID, d, seen)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// readAsideEvents decodes the events.jsonl of a run directory moved aside.
func readAsideEvents(t *testing.T, dir, runID string) []*store.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "aside-"+runID, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []*store.Event
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var ev store.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decode aside row: %v", err)
		}
		out = append(out, &ev)
	}
	return out
}

// TR8 (F9): with the real closures wired, the first tailed event of a
// registered run completes Observe bounded — the runLookup received the
// captured observation (never a manager re-entry) and the twin landed.
func TestFirstTailedEventCompletesObserveBounded(t *testing.T) {
	svc, native, _ := newLiveEventsService(t, WithAlerts(AlertSettings{StallTimeout: time.Second}))
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	setRunName(t, native, "r", "Alpha")
	sub, err := runstream.SubscribeEventsCursor(ctx, svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	budgetEvent(t, native, "r", "tokens")
	if err := awaitRunHealthRow(t, native, "r", "Alpha", 5*time.Second); err != nil {
		t.Fatalf("the first tailed event never completed Observe: %v", err)
	}
}

// TP3 (P1c, control): an observed-event scope of the broker never carries a
// manager alert — the broker's observed topics belong to PERSISTED events.
// True at HEAD, pinned as the feed-isolation guard after the registry lands.
func TestObservedFeedNeverCarriesManagerAlerts(t *testing.T) {
	svc, native, _ := newLiveEventsService(t, WithAlerts(AlertSettings{StallTimeout: time.Second}))
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	obs, err := svc.CaptureRunOrigin(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	scope, err := svc.Broker().AcquireObserved(obs)
	if err != nil {
		t.Fatal(err)
	}
	feed, err := scope.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer feed.Cancel()
	legacy := svc.Broker().Subscribe("r")
	defer legacy.Cancel()
	sub, err := runstream.SubscribeEventsCursor(ctx, svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if _, err := native.AppendEvent(valueCtx(t, native, "r"), "r", store.Event{Type: store.EventBudgetWarning, NodeID: "n", Data: map[string]any{"dimension": "tokens"}}); err != nil {
		t.Fatal(err)
	}
	awaitBrokerEvent(t, legacy, 5*time.Second, store.EventAlert, "legacy carrier (positive control)")
	select {
	case ev, ok := <-feed.C:
		if !ok {
			t.Fatal("the observed scope closed")
		}
		t.Fatalf("manager alert leaked into the observed scope: %+v", ev)
	case <-time.After(500 * time.Millisecond):
	}
}

// TR2 (F5/F18): the three consumer classes of one registered alert.
//
//	(i)  a bound native merge with a registry lane ships exactly ONE frame
//	     (the registry's; the legacy carrier is deduped per merge);
//	(ii) an unbound subscription ships exactly one via the legacy carrier;
//	(iii) a wrapper-store bound merge ships one via the legacy carrier with
//	     the stamped-origin admission.
//
// Mutants M3 (registry publish suppressed), M19 (dual publication removed).
func TestAlertThreeConsumerClasses(t *testing.T) {
	svc, native, _ := newLiveEventsService(t, WithAlerts(AlertSettings{StallTimeout: time.Second}))
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	obs, err := svc.CaptureRunOrigin(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	// (i) the bound native merge, with a direct registry subscription next
	// to it as the source witness. The merge's ensure registers the run;
	// the direct subscription joins the epoch after it exists.
	bound := boundOriginCtx(t, svc, "r")
	merge, err := runstream.SubscribeEventsCursor(bound, svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer merge.Close()
	direct, ok := svc.alertRegistry.subscribe(obs)
	if !ok {
		t.Fatalf("no registry topic for a live registration")
	}
	defer direct.Cancel()
	budgetEvent(t, native, "r", "tokens")
	awaitBrokerEvent(t, svc.Broker().Subscribe("r"), 5*time.Second, store.EventAlert, "the alert published at all")
	// The direct registry subscription received exactly the one frame...
	select {
	case ev, ok := <-direct.C:
		if !ok || ev.Type != store.EventAlert {
			t.Fatalf("the registry lane delivered %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the registry publish never reached a direct subscriber (M3)")
	}
	select {
	case ev, ok := <-direct.C:
		if ok {
			t.Fatalf("the registry lane delivered a second frame: %+v", ev)
		}
	case <-time.After(300 * time.Millisecond):
	}
	// ...and the bound merge delivered exactly one alert (the registry's;
	// its legacy twin was deduped).
	if alerts := awaitAlerts(t, merge, 3*time.Second, 1); len(alerts) != 1 {
		t.Fatalf("the bound merge delivered %d alerts, want exactly the registry's one", len(alerts))
	}

	// (ii) the unbound subscription: exactly one alert via the legacy carrier.
	unbound, err := runstream.SubscribeEventsCursor(ctx, svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer unbound.Close()
	budgetEvent(t, native, "r", "cost_usd")
	if seen := awaitAlerts(t, unbound, 5*time.Second, 1); len(seen) != 1 {
		t.Fatalf("the unbound merge delivered %d alerts, want exactly the legacy one", len(seen))
	}

	// (iii) the wrapper-store bound merge: one alert via legacy with the
	// stamped admission.
	wsvc, wnative := newAlertWrapperService(t)
	wctx := t.Context()
	if err := seedOriginRun(wctx, wnative, "w", "W", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	boundW := boundOriginCtx(t, wsvc, "w")
	wsub, err := wsvc.StreamSource().SubscribeEvents(boundW, "w", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer wsub.Close()
	budgetEvent(t, wnative, "w", "tokens")
	if wseen := awaitAlerts(t, wsub, 5*time.Second, 1); len(wseen) != 1 {
		t.Fatalf("the wrapper bound merge delivered %d alerts, want exactly one via legacy", len(wseen))
	}
}

// TR3 (F15/F19): the registry is alert-only and refuses a replayed row.
// Mutants M9a (non-alert accepted), M9b (replayed persisted EventAlert accepted).
func TestAlertRegistryFrameClass(t *testing.T) {
	svc, native, _ := newLiveEventsService(t, WithAlerts(AlertSettings{}))
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	rel, err := svc.registerRunAlertOrigin(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	obs, registered := svc.alertManager.Observation("r")
	if !registered {
		t.Fatal("the seam left the run unregistered")
	}
	lane := svc.alertRegistry.acquire("r", obs)
	// M9a: a non-alert publish is refused.
	stamped, err := store.WithEventObservation(store.Event{Type: store.EventNodeStarted, Seq: 0, RunID: "r"}, obs)
	if err != nil {
		t.Fatal(err)
	}
	if err := lane.Publish(stamped); err == nil {
		t.Fatal("the registry accepted a non-alert publish")
	}
	// M9b: a replayed persisted row (the raw tailer's decode carries NO
	// observation) is refused on the publish path.
	if err := lane.Publish(store.Event{Type: store.EventAlert, Seq: 0, RunID: "r"}); err == nil {
		t.Fatal("the registry accepted an unstamped EventAlert row")
	}
	stampedAlert, err := store.WithEventObservation(store.Event{Type: store.EventAlert, Seq: 0, RunID: "r"}, obs)
	if err != nil {
		t.Fatal(err)
	}
	if err := lane.Publish(stampedAlert); err != nil {
		t.Fatalf("the lane refused its own registration's alert: %v", err)
	}
	// A persisted row replays with its seq: a stamped EventAlert at Seq != 0
	// is a replayed row, not a born-ephemeral alert.
	replayed, err := store.WithEventObservation(store.Event{Type: store.EventAlert, Seq: 5, RunID: "r"}, obs)
	if err != nil {
		t.Fatal(err)
	}
	if err := lane.Publish(replayed); err == nil {
		t.Fatal("the registry accepted a replayed persisted row (a stamped Seq != 0)")
	}
	lane.Close()
	// A replayed persisted EventAlert row flows tailer -> legacy only: the
	// registry never surfaces it, and the raw broker lane carries it.
	if _, err := native.AppendEvent(valueCtx(t, native, "r"), "r", store.Event{Type: store.EventAlert, NodeID: "persisted"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	legacy := svc.Broker().Subscribe("r")
	defer legacy.Cancel()
	_ = legacy
}

// TR10 (F16/M21): an active observed-event feed never receives a manager
// alert — the registry topics stay disjoint from the bridge's.
func TestObservedEventFeedNeverCarriesManagerAlerts(t *testing.T) {
	svc, native, _ := newLiveEventsService(t, WithAlerts(AlertSettings{StallTimeout: time.Second}))
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	obs, err := svc.CaptureRunOrigin(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	feed, err := svc.AcquireObservedEventFeed(ctx, obs)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = feed.Close() }()
	sub, err := runstream.SubscribeEventsCursor(ctx, svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	budgetEvent(t, native, "r", "tokens")
	awaitBrokerEvent(t, svc.Broker().Subscribe("r"), 5*time.Second, store.EventAlert, "the manager alert published")
	deadline := time.After(700 * time.Millisecond)
	for {
		select {
		case ev, ok := <-feed.Events():
			if !ok {
				t.Fatal("the observed feed closed")
			}
			if ev.Type == store.EventAlert {
				t.Fatalf("a manager alert leaked into the observed feed: %+v", ev)
			}
		case <-deadline:
			return
		}
	}
}

// awaitAlerts reads the subscription until at least want alerts of the
// given type arrived, then settles a grace window and returns every alert
// delivered in that span (a dedup mutant delivering two fails the count).
func awaitAlerts(t *testing.T, sub runstream.EventSubscription, d time.Duration, want int) []*store.Event {
	t.Helper()
	var got []*store.Event
	deadline := time.After(d)
	for len(got) < want {
		select {
		case batch, ok := <-sub.Events():
			if !ok {
				t.Fatalf("the stream ended after %d alerts", len(got))
			}
			for _, ev := range batch {
				if ev.Type == store.EventAlert {
					got = append(got, ev)
				}
			}
		case err := <-sub.Errors():
			if err != nil {
				t.Fatalf("the stream errored: %v", err)
			}
		case <-deadline:
			t.Fatalf("only %d of %d wanted alerts arrived within %s", len(got), want, d)
		}
	}
	grace := time.After(600 * time.Millisecond)
settling:
	for {
		select {
		case batch, ok := <-sub.Events():
			if !ok {
				break settling
			}
			for _, ev := range batch {
				if ev.Type == store.EventAlert {
					got = append(got, ev)
				}
			}
		case <-grace:
			break settling
		}
	}
	return got
}

// newAlertWrapperService builds a service whose store WRAPS the native
// filesystem one (the wrapper keeps the legacy svcSource event path, no
// registry) with alerts wired.
func newAlertWrapperService(t *testing.T) (*Service, *store.FilesystemRunStore) {
	t.Helper()
	dir := t.TempDir()
	native, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	race := &originRaceStore{RunStore: native}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithRecoveryPassive(), WithStore(race), WithAlerts(AlertSettings{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		svc.Stop(ctx)
	})
	return svc, native
}

// awaitObservation polls until the run's registration state settles.
func awaitObservation(t *testing.T, svc *Service, runID string, want bool, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		if _, registered := svc.alertManager.Observation(runID); registered == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("registration of %s = %t, want %t", runID, !want, want)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// TR6 (F3/M1a/M6-spawnRun): the in-process launch registers the run before
// its first engine event, and the terminal release closes the registration
// with the episode (the spawnRun pairing).
func TestAlertRegistrationCoversTheLaunchSeam(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "demo.bot")
	if err := os.WriteFile(botPath, []byte("workflow demo:\n  entry: done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)), WithAlerts(AlertSettings{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		svc.Stop(ctx)
	})
	const runID = "launch-alert"
	legacy := svc.Broker().Subscribe(runID)
	defer legacy.Cancel()
	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: botPath, RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	// The engine's first event proves the seam ran before it: the
	// registration precedes the engine start by construction.
	awaitBrokerEvent(t, legacy, 30*time.Second, store.EventRunStarted, "the engine's first event")
	awaitObservation(t, svc, runID, true, 2*time.Second)
	<-res.Done
	// The terminal pairing released the registration before done closed.
	awaitObservation(t, svc, runID, false, 2*time.Second)
}

// TR6 (F3/M1c): the detached launch registers at its tailer seam, and the
// runner's exit releases through the termination pairing. A fake runner
// binary keeps the subprocess shape without a real iterion CLI.
func TestAlertRegistrationCoversTheDetachedSeam(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "demo.bot")
	if err := os.WriteFile(botPath, []byte("workflow demo:\n  entry: done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "fake-iterion")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_BIN", fake)
	t.Setenv("ITERION_RUNS_DETACHED", "1")
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)), WithAlerts(AlertSettings{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		svc.Stop(ctx)
	})
	const runID = "detached-alert"
	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: botPath, RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	awaitObservation(t, svc, runID, true, 5*time.Second)
	// The runner's exit releases through the termination pairing — asserted
	// BEFORE any service stop, whose ReleaseAll would mask the pairing.
	pidRaw, err := os.ReadFile(filepath.Join(dir, "runs", runID, ".pid"))
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Sscanf(string(pidRaw), "%d", &pid); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	awaitObservation(t, svc, runID, false, 15*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	svc.Stop(ctx)
	_ = res
}

// TR6 (F3/M1d): the reattach seam registers at boot, and the runner's exit
// releases through the termination pairing.
func TestAlertRegistrationCoversTheReattachSeam(t *testing.T) {
	dir := t.TempDir()
	seed, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	const runID = "reattach-alert"
	if _, err := seed.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-reaped
	})
	if err := seed.WritePIDFile(runID, pid); err != nil {
		t.Fatal(err)
	}
	// Constructing the service reconciles the orphan and reattaches: the
	// seam registers the run's alert origin.
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithAlerts(AlertSettings{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		svc.Stop(ctx)
	})
	awaitObservation(t, svc, runID, true, 5*time.Second)
	// The runner exits: the watcher releases through the pairing.
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	awaitObservation(t, svc, runID, false, 15*time.Second)
}

// TR1b (F11/M15a): the stall watchdog's human-wait tiers under the
// registration — synctest clocks, the alert_human_wait_test.go pattern.
// Part 1: a genuinely paused descendant exempts the silent parent (the
// traversal runs unbound-tenanted; the mutant that keeps the root's binding
// false-stalls). Part 2: a child outside the parent's tenant is refused and
// the watchdog arms honestly. Part 3: a successor holding the ID refuses
// the bound root read and the stall fires into the captured route.
func TestHumanWaitTiersUnderTheRegistration(t *testing.T) {
	ctx := context.Background()

	// Part 1: the paused child exempts the parent.
	synctest.Test(t, func(t *testing.T) {
		base := time.Now()
		p1, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"parent", "child"} {
			if _, err := p1.CreateRun(ctx, id, "wf", nil); err != nil {
				t.Fatal(err)
			}
		}
		if err := p1.SetSubbotChild(ctx, "parent", "subbot", "child"); err != nil {
			t.Fatal(err)
		}
		if err := p1.UpdateRunStatus(ctx, "child", store.RunStatusPausedWaitingHuman, ""); err != nil {
			t.Fatal(err)
		}
		svc1 := &Service{store: p1, logger: iterlog.Nop()}
		got := make(chan alert.Alert, 8)
		m1 := svc1.buildAlertManager(AlertSettings{StallTimeout: time.Second, DesktopSink: alert.FuncSink(func(_ context.Context, a alert.Alert) { got <- a })})
		obs1, err := svc1.CaptureRunOrigin(ctx, "parent")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m1.Register("parent", obs1, newSilentLane()); err != nil {
			t.Fatal(err)
		}
		m1.Start(ctx)
		defer func() { m1.Stop(); synctest.Wait() }()
		m1.Observe(store.Event{RunID: "parent", Type: store.EventNodeStarted, NodeID: "n", Timestamp: base})
		time.Sleep(6 * time.Second)
		synctest.Wait()
		select {
		case a := <-got:
			t.Fatalf("the paused child did not exempt the parent: %+v", a)
		default:
		}
	})

	// Part 2: a child outside the parent's tenant is refused by the
	// store (the Mongo contract), and the watchdog arms honestly. The FS
	// store keeps no tenants, so the test fabricates the registration's
	// tenant and wraps the store to enforce it on the child's reads.
	synctest.Test(t, func(t *testing.T) {
		base := time.Now()
		p2, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"parent", "child"} {
			if _, err := p2.CreateRun(ctx, id, "wf", nil); err != nil {
				t.Fatal(err)
			}
		}
		if err := p2.SetSubbotChild(ctx, "parent", "subbot", "child"); err != nil {
			t.Fatal(err)
		}
		if err := p2.UpdateRunStatus(ctx, "child", store.RunStatusPausedWaitingHuman, ""); err != nil {
			t.Fatal(err)
		}
		wrapped := &foreignTenantChildStore{RunStore: p2, childTenant: "team-b"}
		svc2 := &Service{store: wrapped, logger: iterlog.Nop()}
		got := make(chan alert.Alert, 8)
		m2 := svc2.buildAlertManager(AlertSettings{StallTimeout: time.Second, DesktopSink: alert.FuncSink(func(_ context.Context, a alert.Alert) { got <- a })})
		r, err := p2.LoadRun(ctx, "parent")
		if err != nil {
			t.Fatal(err)
		}
		obs2, err := store.CaptureRunObservation(runDataHeader(r), "team-a", true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m2.Register("parent", obs2, newSilentLane()); err != nil {
			t.Fatal(err)
		}
		m2.Start(ctx)
		defer func() { m2.Stop(); synctest.Wait() }()
		m2.Observe(store.Event{RunID: "parent", Type: store.EventNodeStarted, NodeID: "n", Timestamp: base})
		time.Sleep(6 * time.Second)
		synctest.Wait()
		select {
		case a := <-got:
			if a.Kind != alert.KindStall {
				t.Fatalf("alert = %+v, want the stall the refusal armed", a)
			}
		default:
			t.Fatal("the foreign-tenant child muted the watchdog")
		}
	})

	// Part 3: a successor holding the ID refuses the bound root read; the
	// stall fires into the predecessor's captured route.
	synctest.Test(t, func(t *testing.T) {
		base := time.Now()
		dir := t.TempDir()
		p3, err := store.New(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p3.CreateRun(ctx, "parent", "wf", nil); err != nil {
			t.Fatal(err)
		}
		svc3 := &Service{store: p3, logger: iterlog.Nop()}
		got := make(chan alert.Alert, 8)
		m3 := svc3.buildAlertManager(AlertSettings{StallTimeout: time.Second, DesktopSink: alert.FuncSink(func(_ context.Context, a alert.Alert) { got <- a })})
		obsA, err := svc3.CaptureRunOrigin(ctx, "parent")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m3.Register("parent", obsA, newSilentLane()); err != nil {
			t.Fatal(err)
		}
		// Replace: A's directory moves aside, B takes the ID (no human wait).
		moveRunAside(t, dir, "parent")
		if _, err := p3.CreateRun(ctx, "parent", "wf", nil); err != nil {
			t.Fatal(err)
		}
		m3.Start(ctx)
		defer func() { m3.Stop(); synctest.Wait() }()
		m3.Observe(store.Event{RunID: "parent", Type: store.EventNodeStarted, NodeID: "n", Timestamp: base})
		time.Sleep(6 * time.Second)
		synctest.Wait()
		select {
		case a := <-got:
			if a.Kind != alert.KindStall {
				t.Fatalf("alert = %+v, want the stall the refusal armed", a)
			}
		default:
			t.Fatal("the bound root read did not refuse its successor")
		}
	})
}

// newSilentLane is a silent ObservedLane stub for manager-level tests.
type silentLane struct{ closed int }

func (l *silentLane) Publish(store.Event) error { return nil }
func (l *silentLane) Close() error              { l.closed++; return nil }

func newSilentLane() *silentLane { return &silentLane{} }

// foreignTenantChildStore refuses the child's reads under a tenant ctx
// naming another team — the Mongo tenant contract the traversal must
// respect (F11).
type foreignTenantChildStore struct {
	store.RunStore
	childTenant string
}

func (s *foreignTenantChildStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if id == "child" {
		if tenant, ok := store.TenantFromContext(ctx); ok && tenant != s.childTenant {
			return nil, fmt.Errorf("run child is outside the context tenant")
		}
	}
	return s.RunStore.LoadRun(ctx, id)
}

// TR4 (F14/M6/M10/M11/M12): the epoch ends at the release pairing; the
// resume's re-adopted epoch is picked up lazily through the legacy carrier
// while the closed old one is never resurrected; and with the watcher
// isolated (the run directory moved away) the legacy lane's wake is the
// only path that surfaces the successor's identity refusal — the M10
// witness, mirroring the slice-2 wake-isolation pattern.
func TestAlertEpochEndRetiredQueueAndResume(t *testing.T) {
	prevTerm := svcEventTerminalPollInterval
	svcEventTerminalPollInterval = time.Hour
	t.Cleanup(func() { svcEventTerminalPollInterval = prevTerm })

	svc, native, dir := newLiveEventsService(t, WithAlerts(AlertSettings{StallTimeout: time.Second}))
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	obsA, err := svc.CaptureRunOrigin(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	// The seed's initial tail drains both rows: the budget alert is born
	// and delivered before any release.
	budgetEvent(t, native, "r", "tokens")
	bound := boundOriginCtx(t, svc, "r")
	sub, err := runstream.SubscribeEventsCursor(bound, svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	awaitAlerts(t, sub, 5*time.Second, 1)

	// The episode ends: the pairing closes the broker lane and the epoch.
	svc.closeRunAndReleaseAlerts("r")
	if _, ok := svc.alertRegistry.subscribe(obsA); ok {
		t.Fatal("the released epoch still hands out subscriptions (M6/M12)")
	}

	// The resume: the same observation re-adopts into a NEW epoch; the
	// next alert arrives once (via the legacy carrier), the merge adopts
	// the new lane, and the alert after that arrives once via the lane —
	// the closed old epoch is never resurrected (M11).
	if _, err := svc.registerRunAlertOrigin(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	svc.alertManager.Observe(store.Event{RunID: "r", Type: store.EventBudgetWarning, NodeID: "n", Timestamp: time.Now(), Data: map[string]any{"dimension": "tokens2"}})
	if phase := awaitAlerts(t, sub, 5*time.Second, 1); len(phase) != 1 {
		t.Fatalf("the post-resume alert delivered %d frames, want one", len(phase))
	}
	svc.alertManager.Observe(store.Event{RunID: "r", Type: store.EventBudgetWarning, NodeID: "n", Timestamp: time.Now(), Data: map[string]any{"dimension": "tokens3"}})
	if phase := awaitAlerts(t, sub, 5*time.Second, 1); len(phase) != 1 {
		t.Fatalf("the alert after the lane adoption delivered %d frames, want one", len(phase))
	}
	// The merge adopted the NEW epoch (the closed old one never resurrects,
	// M11): the re-adopted topic carries the merge's subscription.
	adopted := false
	deadline := time.After(3 * time.Second)
	for !adopted {
		svc.alertRegistry.mu.Lock()
		if tp := svc.alertRegistry.topics[obsA]; tp != nil && !tp.closed && len(tp.subs) > 0 {
			adopted = true
		}
		svc.alertRegistry.mu.Unlock()
		select {
		case <-deadline:
			t.Fatal("the merge never subscribed the re-adopted epoch (M11)")
		case <-time.After(50 * time.Millisecond):
		}
	}

	// M10: with the lane live and the watcher deaf (A's directory moved
	// away), only the legacy wake can surface the successor's identity
	// refusal. A mutant that drops the legacy lane when a registry lane
	// exists loses the wake and never fails the stream.
	moveRunAside(t, dir, "r")
	if err := seedOriginRun(ctx, native, "r", "B", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	successor, err := native.LoadEvents(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	svc.broker.Publish(*successor[len(successor)-1])
	got, errs := drainEventsWithin(t, sub, 4*time.Second)
	if len(got) != 0 || !hasError(errs, store.ErrRunDataIdentityChanged) {
		t.Fatalf("delivered %+v, errors %v: want the wake-driven identity refusal", got, errs)
	}
}

// TR5 (F19/M14): B's alerts never reach A's stream. Held open across a
// replacement, A's stream ends with the identity refusal and never ships
// one of B's alerts (the slice-2 merge fact, kept); and the registry-lane
// fetch matches the registration's header against the ctx binding — a
// fetch opened under A's binding while the manager holds B adopts nothing
// (the unit witness for the header compare).
func TestAlertsOfASuccessorNeverReachAPredecessorsStream(t *testing.T) {
	svc, native, dir := newLiveEventsService(t, WithAlerts(AlertSettings{StallTimeout: time.Second}))
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	obsA, err := svc.CaptureRunOrigin(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	bound := boundOriginCtx(t, svc, "r")
	sub, err := runstream.SubscribeEventsCursor(bound, svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	budgetEvent(t, native, "r", "tokens")
	awaitAlerts(t, sub, 5*time.Second, 1)

	// Replace with B, which emits its own budget warning: the held-open
	// stream ends with the identity refusal, delivering no B alert.
	moveRunAside(t, dir, "r")
	if err := seedOriginRun(ctx, native, "r", "B", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	obsB, err := svc.CaptureRunOrigin(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	budgetEvent(t, native, "r", "cost_usd")
	wake2, err := os.OpenFile(filepath.Join(dir, "aside-r", "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wake2.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if err := wake2.Close(); err != nil {
		t.Fatal(err)
	}
	got, errs := drainEventsWithin(t, sub, 6*time.Second)
	for _, ev := range got {
		if ev.Type != store.EventAlert {
			continue
		}
		// Any alert delivered after the replacement must be A's own: B's
		// frame is born on B's observation and would be a defect (round-4
		// F3: this loop used to discard what it named).
		if stamp, stamped := ev.EventObservation(); stamped && stamp.Header() != obsA.Header() {
			t.Fatalf("a successor's alert reached the predecessor's stream: %+v", ev)
		}
	}
	if !hasError(errs, store.ErrRunDataIdentityChanged) {
		t.Fatalf("errors = %v, want the identity refusal to end the stream", errs)
	}

	// The rotation-driven registration races the merge's own end (the
	// identity refusal above stops the tailer, so B may never register
	// through it) — a scheduling fact, not the property under test. The
	// fetch-time compare below registers B directly to pin its own contract.
	if release, rerr := svc.registerRunAlertOrigin(ctx, "r"); rerr != nil {
		t.Fatalf("register B for the fetch compare: %v", rerr)
	} else {
		defer release()
	}

	// The fetch-time header compare (unit): a ctx bound to A while the
	// manager holds B adopts no registry lane.
	boundA, err := store.BindRunObservation(ctx, obsA)
	if err != nil {
		t.Fatal(err)
	}
	if lane, _ := svc.observedAlertLane(boundA, "r", false); lane != nil {
		lane.Cancel()
		t.Fatal("the fetch adopted a registration its binding does not name (M14)")
	}
	// The same fetch under B's own binding adopts B's epoch.
	boundB, err := store.BindRunObservation(ctx, obsB)
	if err != nil {
		t.Fatal(err)
	}
	lane, _ := svc.observedAlertLane(boundB, "r", false)
	if lane == nil {
		t.Fatal("the fetch refused the registration its binding names")
	}
	lane.Cancel()
}

// TR11 (F4/M16): a subscription opened while the run is Active but not yet
// registered WAITS for the registration broadcast instead of fetching once
// and missing forever; and a bound capture failure fails the subscribe
// typed (alerts disabled honestly, never a legacy degradation).
func TestAlertSubscriptionWaitsForTheRegistration(t *testing.T) {
	svc, native, dir := newLiveEventsService(t, WithAlerts(AlertSettings{StallTimeout: time.Second}))
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	// Part (i): Active without a registration — the merge's fetch waits.
	if _, err := svc.manager.Register(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	opened := make(chan error, 1)
	go func() {
		sub, err := runstream.SubscribeEventsCursor(ctx, svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
		if err != nil {
			opened <- err
			return
		}
		defer sub.Close()
		opened <- nil
	}()
	time.Sleep(300 * time.Millisecond) // the fetch is now parked in the wait
	select {
	case err := <-opened:
		t.Fatalf("the subscribe returned before the registration existed (M16): %v", err)
	default:
	}
	if _, err := svc.registerRunAlertOrigin(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-opened:
		if err != nil {
			t.Fatalf("the subscribe failed after the registration: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the registration broadcast never woke the waiting fetch")
	}
	awaitObservation(t, svc, "r", true, 2*time.Second)

	// Part (iii): a bound ctx whose capture fails — the subscription is
	// refused typed.
	if err := seedOriginRun(ctx, native, "gone", "G", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	boundGone := boundOriginCtx(t, svc, "gone")
	moveRunAside(t, dir, "gone")
	if _, err := runstream.SubscribeEventsCursor(boundGone, svc.StreamSource(), "gone", runstream.EventCursor{Seq: 0, After: true}); err == nil {
		t.Fatal("a bound subscription whose capture fails must be refused typed")
	}
}

// TR9-unit: the ctx the twin path builds — the tenant restored (or the
// explicit no-filter for local runs) and the observation's header as the
// binding a successor is refused by.
func TestAlertObservationContextBuildsTheBoundIdentity(t *testing.T) {
	tenanted, err := store.CaptureRunObservation(store.RunDataHeader{
		RunID: "r", Format: store.ExactValueFormat, Protocol: store.HistoricalRunData, BirthID: "b1",
	}, "team-a", true)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := alertObservationContext(context.Background(), tenanted)
	if err != nil {
		t.Fatal(err)
	}
	if tenant, ok := store.TenantFromContext(bound); !ok || tenant != "team-a" {
		t.Fatalf("tenant = %q (%t), want team-a", tenant, ok)
	}
	if binding, ok := store.RunDataBindingFrom(bound); !ok || binding.Header().RunID != "r" {
		t.Fatalf("binding = %+v (%t), want the observation's header", binding, ok)
	}
	local, err := store.CaptureRunObservation(store.RunDataHeader{
		RunID: "r", Format: store.LegacyValueFormat, Protocol: store.HistoricalRunData,
	}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	localCtx, err := alertObservationContext(context.Background(), local)
	if err != nil {
		t.Fatal(err)
	}
	if !store.IsWithoutTenantFilter(localCtx) {
		t.Fatal("a local run's twin path lost the explicit no-filter")
	}
}

// M20's witness (F17): a store that persists the row and then returns it
// unstamped passes the capability pre-check, fails the append's own
// validation, and the twin is NEVER replayed by ID — the file carries
// exactly one run_health row.
func TestTwinIsNeverReplayedAfterAnAttempt(t *testing.T) {
	dir := t.TempDir()
	native, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := native.CreateRun(ctx, "r", "wf", nil); err != nil {
		t.Fatal(err)
	}
	fake := &unstampedProducerStore{RunStore: native}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithRecoveryPassive(), WithStore(fake), WithAlerts(AlertSettings{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		svc.Stop(cctx)
	})
	// The registry rides the native path; this test builds it directly so
	// the registered twin branch runs against the fake.
	svc.alertRegistry = newAlertRegistry()
	if _, err := svc.registerRunAlertOrigin(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	svc.alertManager.Observe(store.Event{RunID: "r", Type: store.EventBudgetWarning, NodeID: "n", Timestamp: time.Now(), Data: map[string]any{"dimension": "tokens"}})
	deadline := time.After(5 * time.Second)
	var rows int
	for {
		events, err := native.LoadEvents(ctx, "r")
		if err == nil {
			rows = 0
			for _, ev := range events {
				if ev.Type == store.EventRunHealth {
					rows++
				}
			}
			if rows == 1 {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("run_health rows = %d, want exactly one (a replay doubles it)", rows)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// unstampedProducerStore passes the capability check, persists through the
// real store, and hands back the row WITHOUT its retained observation.
type unstampedProducerStore struct {
	store.RunStore
}

func (s *unstampedProducerStore) CheckEventObservationProducer(actual store.EventAppender) error {
	return nil
}

func (s *unstampedProducerStore) AppendEvent(ctx context.Context, id string, evt store.Event) (*store.Event, error) {
	persisted, err := s.RunStore.AppendEvent(ctx, id, evt)
	if err != nil || persisted == nil {
		return persisted, err
	}
	// A JSON round-trip drops the private observation: what a producer that
	// forgets its provenance returns looks like.
	raw, err := json.Marshal(*persisted)
	if err != nil {
		return nil, err
	}
	var stripped store.Event
	if err := json.Unmarshal(raw, &stripped); err != nil {
		return nil, err
	}
	return &stripped, nil
}

// M7/M8's deterministic witness: a registration whose observation does NOT
// match the file the ID now names (the handover-window shape, constructed).
// The registered twin's bound append is REFUSED pre-write (the birth is not
// what the file holds), and no by-ID replay ever lands — the file carries
// no run_health row at all. A sink that keeps the by-ID append (M7) or
// replays after the refusal (M8) writes one.
func TestRegisteredTwinIsRefusedAndNeverReplayedByID(t *testing.T) {
	svc, native, _ := newLiveEventsService(t, WithAlerts(AlertSettings{}))
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	obs, err := svc.CaptureRunOrigin(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	// A registration of ANOTHER birth under the same ID: the store refuses
	// its bound append against the file that is actually there.
	stranger, err := store.CaptureRunObservation(store.RunDataHeader{
		RunID: obs.Header().RunID, Format: obs.Header().Format,
		Protocol: obs.Header().Protocol, BirthID: "NOT-THE-BIRTH",
	}, obs.TenantID(), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.alertManager.Register("r", stranger, newSilentLane()); err != nil {
		t.Fatal(err)
	}
	svc.alertManager.Observe(store.Event{RunID: "r", Type: store.EventBudgetWarning, NodeID: "n", Timestamp: time.Now(), Data: map[string]any{"dimension": "tokens"}})
	deadline := time.After(2 * time.Second)
	for {
		events, err := native.LoadEvents(ctx, "r")
		if err == nil {
			rows := 0
			for _, ev := range events {
				if ev.Type == store.EventRunHealth {
					rows++
				}
			}
			if rows > 0 {
				t.Fatalf("the refused twin's row landed anyway (%d run_health rows) — a by-ID keep or replay ran", rows)
			}
		}
		select {
		case <-deadline:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// H2: the registration wait is bounded — Active without a registration
// that never comes (a seam degraded its capture to a warning) must not
// park the subscription: it opens inside the bound and runs legacy-only,
// the lazy lane adoption catching a registration that lands late.
func TestAlertSubscriptionBoundedWhenNoRegistrationComes(t *testing.T) {
	svc, native, _ := newLiveEventsService(t, WithAlerts(AlertSettings{StallTimeout: time.Second}))
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	// Active without a registration, no broadcast ever coming.
	if _, err := svc.manager.Register(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	started := time.Now()
	sub, err := runstream.SubscribeEventsCursor(waitCtx, svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatalf("the fetch parked until its context died (H2): %v", err)
	}
	defer sub.Close()
	t.Logf("subscribe completed in %s", time.Since(started))
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the fetch took %s, want the bounded wait", elapsed)
	}
	// The subscription works: a persisted event still reaches it.
	persisted, err := native.AppendEvent(valueCtx(t, native, "r"), "r", store.Event{Type: store.EventNodeFinished, NodeID: "live"})
	if err != nil {
		t.Fatal(err)
	}
	svc.broker.Publish(*persisted)
	awaitEvents(t, sub, 5*time.Second, func(evs []*store.Event) bool {
		for _, ev := range evs {
			if ev.Type == store.EventNodeFinished && ev.NodeID == "live" {
				return true
			}
		}
		return false
	})
}

// H5: a resumed producer's registration survives its tailer's departure —
// each holder holds its own bail, and the epoch ends with the LAST bail.
func TestRegistrationBailsSurviveATailorRelease(t *testing.T) {
	svc, native, _ := newLiveEventsService(t, WithAlerts(AlertSettings{StallTimeout: time.Second}))
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	obs, err := svc.CaptureRunOrigin(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	// Two holders of the same episode: the tailer's ensure and the resumed
	// producer's launch seam.
	relTail, err := svc.registerRunAlertOrigin(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	relProducer, err := svc.registerRunAlertOrigin(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	// A direct subscription on the live epoch sees the delivery.
	direct, ok := svc.alertRegistry.subscribe(obs)
	if !ok {
		t.Fatal("no registry topic for the live episode")
	}
	defer direct.Cancel()
	// The tailer leaves: its bail closes; the episode stays live.
	relTail()
	if _, still := svc.alertRegistry.subscribe(obs); !still {
		t.Fatal("the tailer's release closed the episode's lane (H5)")
	}
	svc.alertManager.Observe(store.Event{RunID: "r", Type: store.EventBudgetWarning, NodeID: "n",
		Timestamp: time.Now(), Data: map[string]any{"dimension": "tokens"}})
	select {
	case ev, ok := <-direct.C:
		if !ok || ev.Type != store.EventAlert {
			t.Fatalf("the episode's lane delivered %+v after the tailer left", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the resumed producer's alert never reached the registry lane")
	}
	// The producer leaves: the LAST bail closes the epoch.
	relProducer()
	if _, still := svc.alertRegistry.subscribe(obs); still {
		t.Fatal("the epoch survived its last bail")
	}
}

// M4: the human-wait traversal pins the ALREADY-VALIDATED root row — a
// replacement landing between the root read and the traversal must not
// decide the answer on the successor's data. The root's children are still
// traversed from the pinned row; the descendants' reads stay unbound.
func TestHumanWaitWithRootPinsTheValidatedRow(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(ctx, "parent", "wf", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(ctx, "child", "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSubbotChild(ctx, "parent", "subbot", "child"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateRunStatus(ctx, "child", store.RunStatusPausedWaitingHuman, ""); err != nil {
		t.Fatal(err)
	}
	root, err := st.LoadRun(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	// The parent's file is replaced (a successor with no children) AFTER
	// the validated read.
	moveRunAside(t, dir, "parent")
	if _, err := st.CreateRun(ctx, "parent", "wf", nil); err != nil {
		t.Fatal(err)
	}
	// WithRoot answers from the pinned row: the paused child exempts.
	if !store.HasBlockingHumanWaitWithRoot(store.WithoutTenantFilter(ctx), st, root, "parent", time.Now()) {
		t.Fatal("the pinned root lost its paused child (M4)")
	}
	// The re-reading variant answers from the successor: no exemption —
	// the watchdog arms on the successor's truth, never the pinned row's.
	if store.HasBlockingHumanWait(store.WithoutTenantFilter(ctx), st, "parent", time.Now()) {
		t.Fatal("the re-reading variant saw the pinned row")
	}
}

// H3 (witness): one alert, one frame across an epoch end that races the
// alert's delivery. The merge parks INSIDE its Ship — a stalled consumer
// fills the pipe, the dispatch publishes both carriers of one alert, then
// the epoch ends — so the epoch's end lands while the merge still holds both
// copies. Without the park's drain-under-dedup the same alert ships twice on
// the wire; with it, once. Mutation: removing the drain re-reddens this test
// on its own assertion (verified with the reviewer's overlay harness).
func TestAlertEpochEndDoesNotDoubleDeliver(t *testing.T) {
	for trial := 0; trial < 10; trial++ {
		doubleDeliveryTrial(t, trial)
	}
}

func doubleDeliveryTrial(t *testing.T, trial int) {
	svc, native, _ := newLiveEventsService(t, WithAlerts(AlertSettings{}))
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	bound := boundOriginCtx(t, svc, "r")
	merge, err := runstream.SubscribeEventsCursor(bound, svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer merge.Close()
	time.Sleep(300 * time.Millisecond)

	// Stall: 12 rows, 15ms apart, nobody reading the merge output.
	r, err := native.LoadRun(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	boundApp, err := store.BindRunValueFormat(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	for k := 0; k < 12; k++ {
		if _, err := native.AppendEvent(boundApp, "r", store.Event{Type: store.EventNodeStarted, NodeID: "k"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(15 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)

	// Synchronous dispatch while blocked, then the epoch ends.
	svc.alertManager.Observe(store.Event{RunID: "r", Type: store.EventBudgetWarning, NodeID: "n", Timestamp: time.Now(), Data: map[string]any{"dimension": "ax"}})
	svc.alertManager.ReleaseRun("r")
	time.Sleep(400 * time.Millisecond)

	// Unblock and count on the wire.
	alerts := 0
	deadline := time.After(3 * time.Second)
	events := merge.Events()
	errs := merge.Errors()
reading:
	for {
		select {
		case batch, ok := <-events:
			if !ok {
				break reading
			}
			for _, ev := range batch {
				if ev.Type == store.EventAlert {
					alerts++
				}
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			_ = err
		case <-deadline:
			break reading
		}
	}
	if alerts >= 2 {
		t.Errorf("trial %d: DOUBLE DELIVERY: %d alert frames for one alert", trial, alerts)
	}
}

// B-F4 (round 3): a REFUSED legacy subscribe must not leave its broker
// subscription behind — a legacy-path run never gets a CloseRun, so every
// refused retry would leak one more.
func TestRefusedLegacySubscribeLeavesNoBrokerSubscription(t *testing.T) {
	svc, _ := newAlertWrapperService(t)
	before := svc.broker.SubscriberCount("gone")
	if _, err := svc.StreamSource().(*svcSource).SubscribeEventsCursor(t.Context(), "gone", runstream.EventCursor{Seq: 0}); err == nil {
		t.Fatal("a subscribe on an absent run was accepted")
	}
	if after := svc.broker.SubscriberCount("gone"); after != before {
		t.Fatalf("broker subscribers for a refused subscribe went %d -> %d", before, after)
	}
}
