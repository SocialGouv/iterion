package alert

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// mustObs builds a registration observation for runID with the given birth
// marker; two markers of one ID are different observations (a replacement).
func mustObs(t *testing.T, runID, birth string) store.RunObservation {
	t.Helper()
	obs, err := store.CaptureRunObservation(store.RunDataHeader{
		RunID: runID, Format: store.ExactValueFormat, Protocol: store.HistoricalRunData, BirthID: birth,
	}, "", true)
	if err != nil {
		t.Fatalf("capture observation: %v", err)
	}
	return obs
}

// recordingLane records what dispatch published along the captured route.
// Publish optionally gates and/or refuses, so a test can interpose between
// the birth and the consumption, and Close counts exactly once per call.
type recordingLane struct {
	entered    chan struct{}
	release    chan struct{}
	closed     chan struct{}
	afterClose error // returned by Publish once Close ran (nil = accept)
	publishes  []store.Event
	closeCalls int
}

func newLane() *recordingLane { return &recordingLane{closed: make(chan struct{})} }

func newGatedLane() *recordingLane {
	return &recordingLane{entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
}

func (l *recordingLane) Publish(evt store.Event) error {
	if l.entered != nil {
		l.entered <- struct{}{}
		<-l.release
	}
	select {
	case <-l.closed:
		// A closed lane is a closed epoch: the publish refuses.
		if l.afterClose != nil {
			return l.afterClose
		}
		return ErrNotRegistered
	default:
	}
	l.publishes = append(l.publishes, evt)
	return nil
}

func (l *recordingLane) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	l.closeCalls++
	return nil
}

// TA1 (F8 + H5): every holder's Register installs its OWN bail on the live
// epoch, and its Release closes that one bail — a holder's release never
// ends a lane another holder still lives on; the epoch ends with the last.
func TestRegisterOwnershipTransfer(t *testing.T) {
	m := NewManager()
	obs := mustObs(t, "r", "birth-a")
	// The tailer holder bails first.
	live := newLane()
	liveBail, err := m.Register("r", obs, live)
	if err != nil || liveBail == nil {
		t.Fatalf("first Register = (%v, %v), want adopted", liveBail, err)
	}
	if live.closeCalls != 0 {
		t.Fatalf("the adopted lane was closed at adoption")
	}
	// The producer holder bails second (a resume re-adopting the episode):
	// its own bail joins the live one, the delivery lane unchanged.
	fresh := newLane()
	freshBail, err := m.Register("r", obs, fresh)
	if err != nil || freshBail == nil {
		t.Fatalf("holder re-registration = (%v, %v), want adopted", freshBail, err)
	}
	if fresh.closeCalls != 0 || live.closeCalls != 0 {
		t.Fatal("a bail closed at its own adoption")
	}
	if got, registered := m.Observation("r"); !registered || got != obs {
		t.Fatalf("Observation = (%+v, %t), want the live registration", got, registered)
	}
	// The FIRST holder releases its own token: ITS lane closes, the other
	// holder's lane stays live — the episode did not end (H5).
	liveBail.Close()
	if live.closeCalls != 1 || fresh.closeCalls != 0 {
		t.Fatalf("the first holder's release closed live=%d fresh=%d, want live only", live.closeCalls, fresh.closeCalls)
	}
	freshBail.Close() // the second holder releases: the epoch's last bail.
	if live.closeCalls != 1 || fresh.closeCalls != 1 {
		t.Fatalf("after both releases live=%d fresh=%d, want each closed once", live.closeCalls, fresh.closeCalls)
	}
	liveBail.Close() // double release is safe (the token is once-only).
	if live.closeCalls != 1 || fresh.closeCalls != 1 {
		t.Fatalf("double release closed live=%d fresh=%d, want no change", live.closeCalls, fresh.closeCalls)
	}
	// A LATE release by the first holder must not touch a successor's bail:
	// the token is scoped, never "the newest bail of this run ID".
	successor := newLane()
	succBail, err := m.Register("r", obs, successor)
	if err != nil || succBail == nil {
		t.Fatalf("re-adoption after release = (%v, %v), want adopted", succBail, err)
	}
	liveBail.Close()
	if successor.closeCalls != 0 {
		t.Fatalf("a stale holder's release closed the successor's live bail")
	}
	// A refused registration refuses cleanly: an unobserved observation and
	// a stopped manager both leave the fresh handle with the caller.
	if bail, err := m.Register("r", store.RunObservation{}, newLane()); err == nil || bail != nil {
		t.Fatalf("unobserved registration = (%v, %v), want refused", bail, err)
	}
	m.Stop()
	refused := newLane()
	if bail, err := m.Register("r", obs, refused); err == nil || bail != nil {
		t.Fatalf("registration after Stop = (%v, %v), want refused", bail, err)
	}
	if refused.closeCalls != 0 {
		t.Fatal("the manager closed the refused handle")
	}
	if err := refused.Close(); err != nil {
		t.Fatal(err)
	}
	if err := refused.Close(); err != nil {
		t.Fatal(err)
	}
}

// TA2 (Q3a): a different observation closes the old lane and rebuilds the
// episode — the replacement's own budget warning for the same axis fires.
func TestRegisterReplacementResetsAndClosesTheOldLane(t *testing.T) {
	m := NewManager(WithLegacyAlertPublish(func(store.Event) {}))
	obsA := mustObs(t, "r", "birth-a")
	obsB := mustObs(t, "r", "birth-b")
	laneA := newLane()
	if bail, err := m.Register("r", obsA, laneA); err != nil || bail == nil {
		t.Fatalf("register A = (%v, %v)", bail, err)
	}
	budget := func() store.Event {
		return store.Event{RunID: "r", Type: store.EventBudgetWarning, NodeID: "n",
			Timestamp: time.Now(), Data: map[string]any{"dimension": "tokens"}}
	}
	m.Observe(budget())
	if n := len(laneA.publishes); n != 1 {
		t.Fatalf("A's lane got %d publishes, want the warning", n)
	}
	laneB := newLane()
	if bail, err := m.Register("r", obsB, laneB); err != nil || bail == nil {
		t.Fatalf("register B = (%v, %v)", bail, err)
	}
	if laneA.closeCalls != 1 {
		t.Fatalf("the old lane closed %d times at the replacement, want once", laneA.closeCalls)
	}
	// The same axis fires again for B: the dedup of A's episode is gone.
	m.Observe(budget())
	if n := len(laneB.publishes); n != 1 {
		t.Fatalf("B's lane got %d publishes, want its own warning on the same axis", n)
	}
	if n := len(laneA.publishes); n != 1 {
		t.Fatalf("B's warning reached A's closed episode (%d publishes)", n)
	}
}

// TA3 (F1/M17): the route is frozen at birth. A registration change racing
// the dispatch cannot re-route the in-flight alert: it consumes the
// captured lane, the captured observation and the captured registeredness.
func TestDispatchConsumesTheCapturedRoute(t *testing.T) {
	var twinObs []store.RunObservation
	var twinRegistered []bool
	var legacy []store.Event
	twinEntered := make(chan struct{})
	twinRelease := make(chan struct{})
	m := NewManager(
		WithStoreSink(func(a Alert, obs store.RunObservation, registered bool) {
			twinObs = append(twinObs, obs)
			twinRegistered = append(twinRegistered, registered)
			twinEntered <- struct{}{}
			<-twinRelease
		}),
		WithLegacyAlertPublish(func(evt store.Event) { legacy = append(legacy, evt) }),
	)
	obsA := mustObs(t, "r", "birth-a")
	obsB := mustObs(t, "r", "birth-b")
	laneA := newLane()
	laneB := newLane()
	if bail, err := m.Register("r", obsA, laneA); err != nil || bail == nil {
		t.Fatalf("register A = (%v, %v)", bail, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The TWIN gates the window (M2): Register(B) while the store sink is
		// blocked precedes ANY dispatch-time re-resolution, whose re-read
		// would route B's lane.
		m.Observe(store.Event{RunID: "r", Type: store.EventBudgetWarning, NodeID: "n",
			Timestamp: time.Now(), Data: map[string]any{"dimension": "tokens"}})
	}()
	<-twinEntered
	if bail, err := m.Register("r", obsB, laneB); err != nil || bail == nil {
		t.Fatalf("register B mid-dispatch = (%v, %v)", bail, err)
	}
	twinRelease <- struct{}{}
	<-done
	// The replacement legitimately closed A's epoch mid-dispatch: the
	// captured route refuses and the frame drops with A's episode (R3) —
	// it reaches NO lane, and a dispatch-time re-resolution (M17) is what
	// would land it on B's.
	if n := len(laneA.publishes); n != 0 {
		t.Fatalf("A's closed lane accepted %d frames", n)
	}
	if n := len(laneB.publishes); n != 0 {
		t.Fatalf("B's lane received %d frames — the route was re-resolved by ID", n)
	}
	if len(twinObs) != 1 || twinObs[0] != obsA || !twinRegistered[0] {
		t.Fatalf("twin route = obs %+v registered %t, want A's captured truth", twinObs, twinRegistered)
	}
	if len(legacy) != 1 {
		t.Fatalf("legacy carrier got %d frames, want one", len(legacy))
	}
	if obs, ok := legacy[0].EventObservation(); !ok || obs != obsA {
		t.Fatalf("legacy frame stamped %+v (%t), want A's observation", obs, ok)
	}
}

// TA4 (Q6a/M13): the birth publish is inline — Observe does not return
// while the captured lane holds the frame (the buffering-before-return
// fact only; stream-end ordering stays a non-claim, F7).
func TestBirthPublishIsInline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(WithLegacyAlertPublish(func(store.Event) {}))
		obs := mustObs(t, "r", "birth-a")
		lane := newGatedLane()
		if bail, err := m.Register("r", obs, lane); err != nil || bail == nil {
			t.Fatalf("register = (%v, %v)", bail, err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			m.Observe(store.Event{RunID: "r", Type: store.EventBudgetWarning, NodeID: "n",
				Timestamp: time.Now(), Data: map[string]any{"dimension": "tokens"}})
		}()
		<-lane.entered
		// Every bubble goroutine is parked: Observe is either still inside
		// the publish or already returned.
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("Observe returned before the birth publish completed")
		default:
		}
		lane.release <- struct{}{}
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("Observe never returned after the publish completed")
		}
	})
}

// TA5 (F13/M18): Stop ends detection and refuses registrations but closes
// no lane; ReleaseAll does — an in-flight publish hits the closed lane and
// is dropped, not re-routed.
func TestStopKeepsTheRoutesUntilReleaseAll(t *testing.T) {
	m := NewManager(WithLegacyAlertPublish(func(store.Event) {}))
	obs := mustObs(t, "r", "birth-a")
	lane := newGatedLane()
	if bail, err := m.Register("r", obs, lane); err != nil || bail == nil {
		t.Fatalf("register = (%v, %v)", bail, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Observe(store.Event{RunID: "r", Type: store.EventBudgetWarning, NodeID: "n",
			Timestamp: time.Now(), Data: map[string]any{"dimension": "tokens"}})
	}()
	<-lane.entered
	m.Stop()
	if lane.closeCalls != 0 {
		t.Fatal("Stop closed the registration lane; only ReleaseAll does")
	}
	if bail, err := m.Register("r", mustObs(t, "r", "birth-b"), newLane()); err == nil || bail != nil {
		t.Fatalf("registration after Stop = (%v, %v), want refused", bail, err)
	}
	m.ReleaseAll()
	if lane.closeCalls != 1 {
		t.Fatalf("ReleaseAll closed the lane %d times, want once", lane.closeCalls)
	}
	lane.afterClose = ErrNotRegistered
	lane.release <- struct{}{}
	<-done
	// The in-flight publish completed against the closed lane; the manager
	// dropped it without a by-ID re-route.
	if n := len(lane.publishes); n != 0 {
		t.Fatalf("the closed lane accepted %d publishes", n)
	}
}

// TA6 (F9/M6-reap): the terminal reap releases the registration through
// releaseLocked — the lane closes without the public API ever re-entering
// the locked sections.
func TestReapReleasesTheRegistration(t *testing.T) {
	base := time.Now()
	clock := base
	m := NewManager(WithLegacyAlertPublish(func(store.Event) {}))
	m.now = func() time.Time { return clock }
	obs := mustObs(t, "r", "birth-a")
	lane := newLane()
	if bail, err := m.Register("r", obs, lane); err != nil || bail == nil {
		t.Fatalf("register = (%v, %v)", bail, err)
	}
	m.Observe(store.Event{RunID: "r", Type: store.EventRunFinished, Timestamp: base})
	if lane.closeCalls != 0 {
		t.Fatal("the lane closed before the reap")
	}
	clock = base.Add(31 * time.Minute)
	m.checkStalls(clock)
	if lane.closeCalls != 1 {
		t.Fatalf("the reap closed the lane %d times, want once", lane.closeCalls)
	}
	if _, registered := m.Observation("r"); registered {
		t.Fatal("the reaped episode still holds a registration")
	}
}

// TA7 (F12/M5b): the same observation re-adopted after a release resets the
// execution state — a second failure fires, stalls re-arm and a budget axis
// warns again — while the episode keeps its name and observation.
func TestReadoptionResetsExecutionState(t *testing.T) {
	base := time.Now()
	clock := base
	m := NewManager(WithLegacyAlertPublish(func(store.Event) {}))
	m.now = func() time.Time { return clock }
	obs := mustObs(t, "r", "birth-a")
	lane1 := newLane()
	if bail, err := m.Register("r", obs, lane1); err != nil || bail == nil {
		t.Fatalf("register = (%v, %v)", bail, err)
	}
	fail := func(ts time.Time) store.Event {
		return store.Event{RunID: "r", Type: store.EventRunFailed, NodeID: "n", Timestamp: ts,
			Data: map[string]any{"error": "boom"}}
	}
	m.Observe(fail(base))
	if n := len(lane1.publishes); n != 1 {
		t.Fatalf("first phase = %d publishes, want the failure", n)
	}
	m.ReleaseRun("r")
	lane2 := newLane()
	if bail, err := m.Register("r", obs, lane2); err != nil || bail == nil {
		t.Fatalf("re-adopt = (%v, %v)", bail, err)
	}
	// The RESUME EVENT is what starts the new execution (F1/F12): a
	// re-registration alone is just a bail — a reconnect to a finished run
	// takes exactly that path and must not reset anything.
	m.Observe(store.Event{RunID: "r", Type: store.EventRunResumed, NodeID: "n", Timestamp: base.Add(30 * time.Second)})
	// A second failure fires again.
	m.Observe(fail(base.Add(time.Minute)))
	if n := len(lane2.publishes); n != 1 {
		t.Fatalf("second phase = %d publishes, want the second failure (dedup survived the resume)", n)
	}
	// A stall re-arms on fresh silence.
	m.ReleaseRun("r")
	lane3 := newLane()
	if _, err := m.Register("r", obs, lane3); err != nil {
		t.Fatal(err)
	}
	// The resume event clears the previous execution's terminal flag; the
	// node-start event then anchors the fresh stall window. The resume MUST
	// be newer than the first one (base+30s): a replay carries the same
	// timestamp and is ignored (round-3 F1).
	resume2 := base.Add(2 * time.Minute)
	clock = resume2
	m.Observe(store.Event{RunID: "r", Type: store.EventRunResumed, NodeID: "n", Timestamp: resume2})
	m.Observe(store.Event{RunID: "r", Type: store.EventNodeStarted, NodeID: "n", Timestamp: resume2})
	clock = resume2.Add(6 * time.Minute)
	if fired := m.checkStalls(clock); len(fired) != 1 {
		t.Fatalf("the stall stayed suppressed after the re-adoption: %+v", fired)
	}
	// And the budget axis warns again on the fresh state; the stalled
	// episode also closes with its recovered edge on that first event.
	m.Observe(store.Event{RunID: "r", Type: store.EventBudgetWarning, NodeID: "n",
		Timestamp: clock, Data: map[string]any{"dimension": "tokens"}})
	if n := len(lane3.publishes); n != 3 {
		t.Fatalf("third phase = %d publishes, want stall, stall_recovered, budget warning", n)
	}
	if lane3.publishes[2].Data["kind"] != string(KindBudgetWarning) {
		t.Fatalf("third phase's last frame = %v, want the budget warning", lane3.publishes[2].Data["kind"])
	}
}

// TA8 (F9): the real closure shapes — the manager hands the closures the
// data it already holds (the captured registration), the first Observe
// completes bounded, and registrations race observations under -race
// without the locked sections re-entering the public API.
func TestFirstObserveCompletesWithTheRealClosureShapes(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(ctx, "r", "wf", nil); err != nil {
		t.Fatal(err)
	}
	var sawObs store.RunObservation
	var sawRegistered bool
	lookupCalled := make(chan struct{})
	m := NewManager(
		WithRunLookup(func(id string, obs store.RunObservation, registered bool) (string, bool) {
			// Realistic shape: a store read, never a call back into the
			// manager (that would deadlock under m.mu).
			sawObs = obs
			sawRegistered = registered
			defer close(lookupCalled)
			r, err := st.LoadRun(context.Background(), id)
			if err != nil || r == nil {
				return "", false
			}
			return r.WorkflowName, true
		}),
		WithHumanWaitLookup(func(ctx context.Context, id string, obs store.RunObservation, registered bool, now time.Time) bool {
			return store.HasBlockingHumanWait(ctx, st, id, now)
		}),
	)
	obs := mustObs(t, "r", "birth-a")
	lane := newLane()
	if bail, err := m.Register("r", obs, lane); err != nil || bail == nil {
		t.Fatalf("register = (%v, %v)", bail, err)
	}
	// Register created the state: the resolver saw the captured registration.
	<-lookupCalled
	if !sawRegistered || sawObs != obs {
		t.Fatalf("the resolver saw obs %+v registered %t, want the captured registration", sawObs, sawRegistered)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Observe(store.Event{RunID: "r", Type: store.EventNodeStarted, NodeID: "n", Timestamp: time.Now()})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the first Observe never completed")
	}
	// The registration races observations and releases; the locked sections
	// stay consistent (deadlocks and data races surface under -race).
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.Observation("r")
			m.Observe(store.Event{RunID: "r", Type: store.EventToolCalled, Timestamp: time.Now()})
		}
	}()
	for i := 0; i < 50; i++ {
		m.ReleaseRun("r")
		lane = newLane()
		if _, err := m.Register("r", obs, lane); err != nil {
			t.Fatalf("re-register %d: %v", i, err)
		}
	}
	m.Observe(store.Event{RunID: "r", Type: store.EventRunFinished, Timestamp: time.Now()})
	close(stop)
	m.ReleaseAll()
	if lane.closeCalls != 1 {
		t.Fatalf("ReleaseAll closed the last lane %d times, want once", lane.closeCalls)
	}
}

// A refused publish never re-routes by ID: the dropped frame does not
// surface on the legacy carrier of a REGISTERED run as an unstamped frame
// (the stamped frame does — dual publication, R6).
func TestRefusedLanePublishDropsWithoutRerouting(t *testing.T) {
	var legacy []store.Event
	m := NewManager(WithLegacyAlertPublish(func(evt store.Event) { legacy = append(legacy, evt) }))
	obs := mustObs(t, "r", "birth-a")
	lane := newLane()
	lane.afterClose = ErrNotRegistered
	if bail, err := m.Register("r", obs, lane); err != nil || bail == nil {
		t.Fatalf("register = (%v, %v)", bail, err)
	}
	lane.Close() // the epoch ends mid-episode (a release the manager missed)
	m.Observe(store.Event{RunID: "r", Type: store.EventBudgetWarning, NodeID: "n",
		Timestamp: time.Now(), Data: map[string]any{"dimension": "tokens"}})
	if n := len(lane.publishes); n != 0 {
		t.Fatalf("the closed lane accepted %d publishes", n)
	}
	if !errors.Is(ErrNotRegistered, ErrNotRegistered) {
		t.Fatal("sentinel broken")
	}
	if len(legacy) != 1 {
		t.Fatalf("legacy carrier got %d frames, want the stamped one", len(legacy))
	}
	if _, ok := legacy[0].EventObservation(); !ok {
		t.Fatal("the legacy frame of a registered run lost its stamp")
	}
}

// H1: the stall candidate's route is copied under m.mu — the off-lock
// lookup receives the CAPTURED registration even while a concurrent
// Register/Release mutates the state. Pre-fix the lookup read the state's
// fields unsynchronized (TSan: write Register/releaseLocked vs the read).
func TestStallCandidateCarriesTheCapturedRoute(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var gotObs store.RunObservation
	m := NewManager(WithStallTimeout(time.Minute), WithHumanWaitLookup(func(_ context.Context, _ string, obs store.RunObservation, _ bool, _ time.Time) bool {
		gotObs = obs
		close(entered)
		<-release
		return false
	}))
	obsA := mustObs(t, "r", "birth-a")
	laneA := newLane()
	if _, err := m.Register("r", obsA, laneA); err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	m.Observe(store.Event{RunID: "r", Type: store.EventNodeStarted, NodeID: "n", Timestamp: base})
	// Unordered writes during the lookup's window: the hammer's Register
	// mutates the state's fields with no synchronization edge to the
	// candidate read (the same observation, so the captured route's VALUE
	// stays deterministic — the race is in the access, not the value).
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := m.Register("r", obsA, newLane()); err != nil {
				return
			}
			m.ReleaseRun("r")
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.checkStalls(base.Add(10 * time.Minute))
	}()
	<-entered
	close(release)
	<-done
	close(stop)
	// Only the captured OBSERVATION is deterministic here: the hammer's
	// Register/ReleaseRun cycle empties and refills rs.bails, so the
	// `registered` flag depends on the interleaving (B-F3).
	if gotObs != obsA {
		t.Fatalf("the lookup saw obs %+v, want A's captured registration", gotObs)
	}
}

// M1: a replacement's name resolves under its OWN registration — a
// resolver that refuses must not leave the predecessor's name on the new
// episode's alerts.
func TestReplacementResolvesTheNameUnderItsOwnRegistration(t *testing.T) {
	sink := &captureSink{}
	lookups := 0
	m := NewManager(WithSinks(sink), WithRunLookup(func(id string, obs store.RunObservation, registered bool) (string, bool) {
		lookups++
		if lookups == 1 {
			return "alpha-A", true
		}
		// The replacement's own read refuses: its birth is not what the ID
		// names anymore.
		return "", false
	}))
	obsA := mustObs(t, "r", "birth-a")
	if _, err := m.Register("r", obsA, newLane()); err != nil {
		t.Fatal(err)
	}
	obsB := mustObs(t, "r", "birth-b")
	if _, err := m.Register("r", obsB, newLane()); err != nil {
		t.Fatal(err)
	}
	m.Observe(store.Event{RunID: "r", Type: store.EventRunFailed, NodeID: "n",
		Timestamp: time.Now(), Data: map[string]any{"error": "boom"}})
	waitFor(t, func() bool { return len(sink.snapshot()) == 1 })
	got := sink.snapshot()[0]
	if got.RunName != "" {
		t.Fatalf("B's alert carries the predecessor's name %q", got.RunName)
	}
	if lookups < 2 {
		t.Fatalf("the replacement never re-resolved the name (%d lookups)", lookups)
	}
}

// R-A1 (round 2, A-F1): a RECONNECT to a finished run re-registers exactly
// like a resume does — a fresh bail on the same observation — and must not
// resurrect the episode: the deduped failure stays deduped and the stale
// replay of events.jsonl fires nothing.
func TestReRegistrationDoesNotResurrectAFinishedEpisode(t *testing.T) {
	var mu sync.Mutex
	var fired []store.Event
	m := NewManager(WithLegacyAlertPublish(func(ev store.Event) {
		mu.Lock()
		defer mu.Unlock()
		fired = append(fired, ev)
	}))
	obs := mustObs(t, "r", "birth-a")
	bail1, err := m.Register("r", obs, newLane())
	if err != nil {
		t.Fatal(err)
	}
	failure := func(ts time.Time) store.Event {
		return store.Event{RunID: "r", Type: store.EventRunFailed, NodeID: "n", Timestamp: ts,
			Data: map[string]any{"error": "boom"}}
	}
	base := time.Now()
	m.Observe(failure(base))
	mu.Lock()
	n := len(fired)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("first failure fired %d frames, want 1", n)
	}
	bail1.Close()
	// The reconnect: same observation, a fresh bail — and the tailer's replay
	// of every row, including the failure that already fired.
	bail2, err := m.Register("r", obs, newLane())
	if err != nil {
		t.Fatal(err)
	}
	defer bail2.Close()
	m.Observe(failure(base.Add(time.Minute)))
	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 1 {
		t.Fatalf("the reconnect replayed the failure: %d frames, want 1 — a bail is not a resume", len(fired))
	}
}

// R-A2 (round 2, A-F3): a subscriber churn on a PAUSED run (last bail leaves,
// a new one joins) must not clear the pause — otherwise the stall window
// re-arms on a run an operator paused on purpose.
func TestSubscriberChurnKeepsAPausedRunPaused(t *testing.T) {
	sink := &captureSink{}
	base := time.Now()
	clock := base
	m := NewManager(WithStallTimeout(5*time.Minute), WithSinks(sink))
	m.now = func() time.Time { return clock }
	obs := mustObs(t, "r", "birth-a")
	bail, err := m.Register("r", obs, newLane())
	if err != nil {
		t.Fatal(err)
	}
	m.Observe(store.Event{RunID: "r", Type: store.EventNodeStarted, NodeID: "n", Timestamp: base})
	m.Observe(store.Event{RunID: "r", Type: store.EventRunPaused, NodeID: "n", Timestamp: base.Add(time.Second)})
	bail.Close()
	churn, err := m.Register("r", obs, newLane())
	if err != nil {
		t.Fatal(err)
	}
	defer churn.Close()
	// An unrelated event re-anchors the stall window (the reset zeroed it,
	// and a zero anchor is skipped): under the old behaviour the erased
	// pause now lets a stall fire on a run an operator paused.
	clock = base.Add(10 * time.Minute)
	m.Observe(store.Event{RunID: "r", Type: store.EventNodeFinished, NodeID: "n", Timestamp: clock})
	clock = clock.Add(9 * time.Minute)
	if fired := m.checkStalls(clock); len(fired) != 0 {
		t.Fatalf("the churn resumed a paused run: %+v", fired)
	}
}

// R-A3 (round 2, A-F4): the predecessor's current node must not label the
// successor's stall alert.
func TestReplacementClearsThePredecessorsCurrentNode(t *testing.T) {
	sink := &captureSink{}
	base := time.Now()
	clock := base
	m := NewManager(WithStallTimeout(5*time.Minute), WithSinks(sink))
	m.now = func() time.Time { return clock }
	obsA := mustObs(t, "r", "birth-a")
	obsB := mustObs(t, "r", "birth-b")
	if _, err := m.Register("r", obsA, newLane()); err != nil {
		t.Fatal(err)
	}
	m.Observe(store.Event{RunID: "r", Type: store.EventNodeStarted, NodeID: "old-node", Timestamp: base})
	bailB, err := m.Register("r", obsB, newLane())
	if err != nil {
		t.Fatal(err)
	}
	defer bailB.Close()
	// The successor goes silent from its own start: its stall must carry no
	// node, never the predecessor's.
	m.Observe(store.Event{RunID: "r", Type: store.EventNodeStarted, NodeID: "", Timestamp: base.Add(time.Second)})
	clock = base.Add(10 * time.Minute)
	fired := m.checkStalls(clock)
	if len(fired) != 1 {
		t.Fatalf("the successor's stall fired %d times, want once", len(fired))
	}
	if got := fired[0].a.NodeID; got == "old-node" {
		t.Fatalf("the successor's stall carries the predecessor's node %q", got)
	}
}

// R-A4 (round 3, F1): the tailer replays events.jsonl from offset 0 on every
// reconnect, so a run_resumed line arrives again — with its ORIGINAL
// timestamp. A replay is not a new execution: the dedup must survive it, or
// every reconnect re-fires the failures and budget warnings it already
// delivered.
func TestReplayedResumeDoesNotResurrectTheEpisode(t *testing.T) {
	var mu sync.Mutex
	var fired []store.Event
	m := NewManager(WithLegacyAlertPublish(func(ev store.Event) {
		mu.Lock()
		defer mu.Unlock()
		fired = append(fired, ev)
	}))
	base := time.Now()
	obs := mustObs(t, "r", "birth-a")
	bail, err := m.Register("r", obs, newLane())
	if err != nil {
		t.Fatal(err)
	}
	defer bail.Close()
	failure := func(ts time.Time) store.Event {
		return store.Event{RunID: "r", Type: store.EventRunFailed, NodeID: "n", Timestamp: ts,
			Data: map[string]any{"error": "boom"}}
	}
	resume := store.Event{RunID: "r", Type: store.EventRunResumed, NodeID: "n", Timestamp: base.Add(30 * time.Second)}
	m.Observe(failure(base))
	m.Observe(resume)
	m.Observe(failure(base.Add(time.Minute)))
	mu.Lock()
	before := len(fired)
	mu.Unlock()
	if before != 2 {
		t.Fatalf("the live episode fired %d frames, want 2 (the two failures)", before)
	}
	// The reconnect: the tailer replays the whole file, resume line included.
	m.Observe(store.Event{RunID: "r", Type: store.EventRunFailed, NodeID: "n", Timestamp: base})
	m.Observe(resume)
	m.Observe(store.Event{RunID: "r", Type: store.EventRunFailed, NodeID: "n", Timestamp: base.Add(time.Minute)})
	mu.Lock()
	defer mu.Unlock()
	if len(fired) != before {
		t.Fatalf("the replayed resume resurrected the episode: %d frames, want %d", len(fired), before)
	}
}

// R-A5 (round 3, F2): a replacement must not inherit the predecessor's
// PAUSE (the fourth episode field): a successor that goes silent before its
// first node event would otherwise stay silent forever.
func TestReplacementClearsThePredecessorsPause(t *testing.T) {
	sink := &captureSink{}
	base := time.Now()
	clock := base
	m := NewManager(WithStallTimeout(5*time.Minute), WithSinks(sink))
	m.now = func() time.Time { return clock }
	obsA := mustObs(t, "r", "birth-a")
	obsB := mustObs(t, "r", "birth-b")
	if _, err := m.Register("r", obsA, newLane()); err != nil {
		t.Fatal(err)
	}
	m.Observe(store.Event{RunID: "r", Type: store.EventRunPaused, NodeID: "n", Timestamp: base})
	bailB, err := m.Register("r", obsB, newLane())
	if err != nil {
		t.Fatal(err)
	}
	defer bailB.Close()
	m.Observe(store.Event{RunID: "r", Type: store.EventRunStarted, NodeID: "", Timestamp: base.Add(2 * time.Second)})
	clock = base.Add(10 * time.Minute)
	if fired := m.checkStalls(clock); len(fired) != 1 {
		t.Fatalf("the successor's stall fired %d times, want once (its predecessor's pause leaked)", len(fired))
	}
}

// R-A6 (round 4, F1): the replay carries events from BEFORE the resume too.
// An event older than the execution's own resume is a replay of the previous
// execution: it must not fire, and must not mark the live execution
// terminal (which would silence every later stall and let the reap close a
// live registration).
func TestReplayedPreResumeFailureDoesNotTouchTheLiveExecution(t *testing.T) {
	var mu sync.Mutex
	var fired []store.Event
	sink := &captureSink{}
	base := time.Now()
	clock := base
	m := NewManager(WithStallTimeout(5*time.Minute), WithSinks(sink), WithLegacyAlertPublish(func(ev store.Event) {
		mu.Lock()
		defer mu.Unlock()
		fired = append(fired, ev)
	}))
	m.now = func() time.Time { return clock }
	obs := mustObs(t, "r", "birth-a")
	bail, err := m.Register("r", obs, newLane())
	if err != nil {
		t.Fatal(err)
	}
	defer bail.Close()
	failure := func(ts time.Time) store.Event {
		return store.Event{RunID: "r", Type: store.EventRunFailed, NodeID: "n", Timestamp: ts,
			Data: map[string]any{"error": "boom"}}
	}
	m.Observe(failure(base))                                                                               // execution 1 fails
	m.Observe(store.Event{RunID: "r", Type: store.EventRunResumed, Timestamp: base.Add(30 * time.Second)}) // execution 2 starts
	mu.Lock()
	before := len(fired)
	mu.Unlock()
	if before != 1 {
		t.Fatalf("the live episode fired %d frames, want 1", before)
	}
	// The reconnect replays execution 1's failure (older than the resume).
	m.Observe(failure(base))
	mu.Lock()
	after := len(fired)
	mu.Unlock()
	if after != before {
		t.Fatalf("the replayed PRE-resume failure re-fired: %d frames, want %d", after, before)
	}
	// And the live execution is still live: it stalls on fresh silence.
	clock = base.Add(30*time.Second + 10*time.Minute)
	if got := m.checkStalls(clock); len(got) != 1 {
		t.Fatalf("the live execution was silenced by the replayed predecessor failure (stalls fired %d)", len(got))
	}
}

// R-A7 (round 4, F2): a predecessor that was OBSERVED but never registered
// leaves its pause/name/current node behind — the successor must not
// inherit them even though `hadPrior` is false.
func TestUnregisteredPredecessorLeavesNoEpisodeFields(t *testing.T) {
	sink := &captureSink{}
	base := time.Now()
	clock := base
	m := NewManager(WithStallTimeout(5*time.Minute), WithSinks(sink),
		WithRunLookup(func(string, store.RunObservation, bool) (string, bool) { return "alpha-A", true }))
	m.now = func() time.Time { return clock }
	// Observed only: the runtime observer fed the pause, no registration.
	m.Observe(store.Event{RunID: "r", Type: store.EventRunPaused, NodeID: "n", Timestamp: base})
	obsB := mustObs(t, "r", "birth-b")
	bail, err := m.Register("r", obsB, newLane())
	if err != nil {
		t.Fatal(err)
	}
	defer bail.Close()
	m.Observe(store.Event{RunID: "r", Type: store.EventRunStarted, Timestamp: base.Add(2 * time.Second)})
	clock = base.Add(10 * time.Minute)
	if got := m.checkStalls(clock); len(got) != 1 {
		t.Fatalf("the successor inherited the unregistered predecessor's pause (stalls fired %d)", len(got))
	}
}

// R-A8 (round-4 H-10): the same branch must also drop the predecessor's
// EXECUTION state — an inherited terminal/failed would silence the
// successor's alerts and stalls, and its stale terminalAt would reap a live
// registration.
func TestSuccessorOfAnUnregisteredPredecessorFiresAndStalls(t *testing.T) {
	var mu sync.Mutex
	var fired []store.Event
	sink := &captureSink{}
	base := time.Now()
	clock := base
	m := NewManager(WithStallTimeout(5*time.Minute), WithSinks(sink), WithLegacyAlertPublish(func(ev store.Event) {
		mu.Lock()
		defer mu.Unlock()
		fired = append(fired, ev)
	}))
	m.now = func() time.Time { return clock }
	// The predecessor fails while nobody registered it.
	m.Observe(store.Event{RunID: "r", Type: store.EventRunFailed, NodeID: "n", Timestamp: base,
		Data: map[string]any{"error": "boom"}})
	obsB := mustObs(t, "r", "birth-b")
	bail, err := m.Register("r", obsB, newLane())
	if err != nil {
		t.Fatal(err)
	}
	defer bail.Close()
	mu.Lock()
	before := len(fired)
	mu.Unlock()
	// The successor's own failure must fire.
	clock = base.Add(time.Minute)
	m.Observe(store.Event{RunID: "r", Type: store.EventRunFailed, NodeID: "n", Timestamp: clock,
		Data: map[string]any{"error": "boom2"}})
	mu.Lock()
	after := len(fired)
	mu.Unlock()
	if after != before+1 {
		t.Fatalf("the successor's failure fired %d new frames, want 1 (it inherited the predecessor's failed flag)", after-before)
	}
	// And a fresh successor that stays silent still stalls.
	obsC := mustObs(t, "r", "birth-c")
	bailC, err := m.Register("r", obsC, newLane())
	if err != nil {
		t.Fatal(err)
	}
	defer bailC.Close()
	start := clock.Add(time.Minute)
	m.Observe(store.Event{RunID: "r", Type: store.EventRunStarted, Timestamp: start})
	clock = start.Add(10 * time.Minute)
	if got := m.checkStalls(clock); len(got) != 1 {
		t.Fatalf("the successor never stalls (inherited terminal state): fired %d", len(got))
	}
}

// R-A9 (round-4 H-10, the other side): a run genuinely parked on a human
// gate that gets its FIRST registration must stay paused — the tailer's
// replay of `run_paused` is the execution's own record and re-applies it.
func TestReplayedPauseRestoresAParkedRunsPauseAfterRegistration(t *testing.T) {
	sink := &captureSink{}
	base := time.Now()
	clock := base
	m := NewManager(WithStallTimeout(5*time.Minute), WithSinks(sink))
	m.now = func() time.Time { return clock }
	m.Observe(store.Event{RunID: "r", Type: store.EventNodeStarted, NodeID: "n", Timestamp: base})
	m.Observe(store.Event{RunID: "r", Type: store.EventRunPaused, NodeID: "n", Timestamp: base.Add(time.Second)})
	obs := mustObs(t, "r", "birth-a")
	bail, err := m.Register("r", obs, newLane())
	if err != nil {
		t.Fatal(err)
	}
	defer bail.Close()
	// The tailer's replay re-reads the file from the top: the pause comes back.
	m.Observe(store.Event{RunID: "r", Type: store.EventRunPaused, NodeID: "n", Timestamp: base.Add(time.Second)})
	clock = base.Add(30 * time.Minute)
	if got := m.checkStalls(clock); len(got) != 0 {
		t.Fatalf("a parked run stalled after its first registration: %+v", got)
	}
}
