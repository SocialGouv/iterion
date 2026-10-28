package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/clock"
	"github.com/SocialGouv/iterion/pkg/lease"
	"github.com/SocialGouv/iterion/pkg/runwatch"
)

// watchHealthOf answers s's watch-health route for run id, decoded.
func watchHealthOf(t *testing.T, s *Server, runID string) assistantWatchHealthResponse {
	t.Helper()
	return watchHealthWith(t, s, runID, context.Background())
}

func watchHealthWith(t *testing.T, s *Server, runID string, ctx context.Context) assistantWatchHealthResponse {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+runID+"/assistant-watch-health", nil).WithContext(ctx)
	req.SetPathValue("id", runID)
	s.handleAssistantWatchHealth(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: watch-health answered %d: %s", s.replicaID, w.Code, w.Body.String())
	}
	var out assistantWatchHealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode watch-health: %v", err)
	}
	return out
}

func seedHealthRun(t *testing.T, rs *countingRunStore, id string) {
	t.Helper()
	if _, err := rs.CreateRun(context.Background(), id, "chatbot", nil); err != nil {
		t.Fatalf("CreateRun %s: %v", id, err)
	}
}

// sweepAttention is every attention code the holder read can add.
func sweepAttention(codes []string) []string {
	var out []string
	for _, c := range codes {
		if strings.HasPrefix(c, "sweep_") {
			out = append(out, c)
		}
	}
	return out
}

// The answer an operator reads from whichever replica the load balancer
// hits: the holder's name comes from the shared lease store, so a follower
// names the holder, and the name moves to the successor once the holder
// steps down (#2680). Before, nine answers in ten said only "not me".
func TestAssistantWatchHealth_NamesTheLeaseHolderFromAnyReplica(t *testing.T) {
	shared := lease.NewMemoryStore()
	holder, hrs, _ := electedWorker(t, shared, "holder")
	waitForCond(t, 10*time.Second, "the holder elected", func() bool { return holder.assistantWatch.heartbeat().Elected })
	follower, frs, _ := electedWorker(t, shared, "follower")
	seedHealthRun(t, hrs, "run-1")
	seedHealthRun(t, frs, "run-1")

	h := watchHealthOf(t, holder, "run-1")
	if !h.Coordinator.Elected || h.Coordinator.Holder != "holder" || h.Coordinator.HolderState != assistantWatchHolderHeld || h.Coordinator.HolderExpiresAt == nil {
		t.Fatalf("the holder's answer = %+v, want elected, holder=holder held, with an expiry", h.Coordinator)
	}
	if !h.Coordinator.HolderExpiresAt.After(time.Now()) {
		t.Fatalf("the live holder's lease expiry %s is not ahead of now", h.Coordinator.HolderExpiresAt)
	}
	f := watchHealthOf(t, follower, "run-1")
	if f.Coordinator.Elected || f.Coordinator.Holder != "holder" || f.Coordinator.HolderState != assistantWatchHolderHeld {
		t.Fatalf("the follower's answer = %+v, want not elected with holder=holder held — a follower must name who holds the lease", f.Coordinator)
	}
	if f.Coordinator.Stale || len(sweepAttention(f.Attention)) > 0 {
		t.Fatalf("the follower reports a dead sweep while the holder is alive: %+v, attention %v", f.Coordinator, f.Attention)
	}

	// The holder steps down; the follower takes the lease, and its answer
	// names itself.
	holder.assistantWatchCancel(context.Background())
	<-holder.assistantWatchDone
	holder.assistantWatchCancel = nil
	waitForCond(t, 10*time.Second, "the follower elected", func() bool { return follower.assistantWatch.heartbeat().Elected })
	if f := watchHealthOf(t, follower, "run-1"); !f.Coordinator.Elected || f.Coordinator.Holder != "follower" || f.Coordinator.HolderState != assistantWatchHolderHeld {
		t.Fatalf("after the hand-over the successor's answer = %+v, want elected with holder=follower held", f.Coordinator)
	}
}

// scriptedHolderLeases campaigns on the embedded store and answers the holder
// read with a script: the health route's judgment of what the store says.
type scriptedHolderLeases struct {
	lease.Store
	holding lease.Holding
	held    bool
	err     error
}

func (s *scriptedHolderLeases) Holder(context.Context, string) (lease.Holding, bool, error) {
	return s.holding, s.held, s.err
}

// A lease past its expiry is a holder nobody replaced: the sweep is dead for
// the whole fleet, and the answer says so wherever it is read — not only on
// the replica that used to hold it.
func TestAssistantWatchHealth_AnExpiredLeaseIsStaleWhereverItIsRead(t *testing.T) {
	ghost := &scriptedHolderLeases{Store: lease.NewMemoryStore(), holding: lease.Holding{Owner: "ghost", ExpiresAt: time.Now().Add(-time.Hour)}, held: true}
	s, rs, _ := electedWorker(t, ghost, "reader")
	seedHealthRun(t, rs, "run-1")
	out := watchHealthOf(t, s, "run-1")
	if out.Coordinator.Holder != "ghost" || out.Coordinator.HolderState != assistantWatchHolderExpired || !out.Coordinator.Stale {
		t.Fatalf("answer over an expired lease = %+v; want holder=ghost expired and stale", out.Coordinator)
	}
	if got := sweepAttention(out.Attention); !slices.Equal(got, []string{"sweep_holder_expired"}) {
		t.Fatalf("attention over an expired lease = %v, want exactly sweep_holder_expired (not doubled by sweep_stale)", got)
	}
}

// Nobody holding the lease is the gap between a holder's release and its
// successor's next attempt — every graceful hand-over passes through it, so
// it is a state to read, never an attention code: the CLI exits non-zero on
// any attention, and a rollout must not read as an outage.
func TestAssistantWatchHealth_AFreeLeaseIsAStateNotAnAlarm(t *testing.T) {
	none := &scriptedHolderLeases{Store: lease.NewMemoryStore()}
	s, rs, _ := electedWorker(t, none, "reader")
	// The read must not land inside the term's first pass: until a pass has
	// completed, the heartbeat reports Stale for the sweep this replica owes
	// (the same grace the fresh-term test reads), so under CI load the answer
	// flips with where the read catches the sweep. The first pass is completed
	// and the worker stopped — the hand-over gap's own state, no replica
	// elected: the answer below is the free lease's, race-free.
	waitForCond(t, 10*time.Second, "the reader's first sweep completed", func() bool { return !s.assistantWatch.heartbeat().CompletedAt.IsZero() })
	s.assistantWatchCancel(context.Background())
	<-s.assistantWatchDone
	s.assistantWatchCancel = nil
	seedHealthRun(t, rs, "run-1")
	out := watchHealthOf(t, s, "run-1")
	if out.Coordinator.Holder != "" || out.Coordinator.HolderExpiresAt != nil || out.Coordinator.HolderState != assistantWatchHolderFree {
		t.Fatalf("answer over a free lease = %+v; want no holder and holder_state free", out.Coordinator)
	}
	if out.Coordinator.Stale || len(sweepAttention(out.Attention)) > 0 {
		t.Fatalf("a free lease is reported as an outage: %+v, attention %v", out.Coordinator, out.Attention)
	}
}

// A store that does not answer, and a server with no lease store at all, are
// each said — never folded into a healthy-looking answer. A request the
// client abandoned is not the store's failure, and is not logged as one.
func TestAssistantWatchHealth_AnUnreadableOrMissingStoreIsSaid(t *testing.T) {
	down := &scriptedHolderLeases{Store: lease.NewMemoryStore(), err: errors.New("store unreachable")}
	s, rs, log := electedWorker(t, down, "reader")
	seedHealthRun(t, rs, "run-1")
	out := watchHealthOf(t, s, "run-1")
	if out.Coordinator.Holder != "" || out.Coordinator.HolderState != assistantWatchHolderUnreadable || !slices.Contains(out.Attention, "sweep_holder_unreadable") {
		t.Fatalf("answer over an unreadable store = %+v, attention %v; want holder_state unreadable and sweep_holder_unreadable", out.Coordinator, out.Attention)
	}
	if !strings.Contains(log.String(), "read the sweep lease holder: store unreachable") {
		t.Fatalf("the store error was not logged: %q", log.String())
	}

	orphan, ors, olog := electedWorker(t, nil, "orphan")
	seedHealthRun(t, ors, "run-1")
	out = watchHealthOf(t, orphan, "run-1")
	if out.Coordinator.HolderState != assistantWatchHolderUnreadable || !slices.Contains(out.Attention, "sweep_holder_unreadable") {
		t.Fatalf("answer with no lease store = %+v, attention %v; want unreadable", out.Coordinator, out.Attention)
	}
	if !strings.Contains(olog.String(), "no lease store is wired") {
		t.Fatalf("the missing store was not logged: %q", olog.String())
	}

	quiet, qrs, qlog := electedWorker(t, lease.NewMemoryStore(), "quiet")
	seedHealthRun(t, qrs, "run-1")
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	out = watchHealthWith(t, quiet, "run-1", gone)
	if out.Coordinator.HolderState != assistantWatchHolderUnreadable {
		t.Fatalf("answer on an abandoned request = %+v; want holder_state unreadable", out.Coordinator)
	}
	if strings.Contains(qlog.String(), "read the sweep lease holder") {
		t.Fatalf("a client disconnect was logged as a lease-store failure: %q", qlog.String())
	}
}

// A present coordinator that is stale for its own reasons — the holder's
// passes stopped completing — carries attention too, like the expired lease
// does: a red field the CLI exits 0 on is a silent outage.
func TestAssistantWatchHealth_AStaleHolderCarriesAttention(t *testing.T) {
	forever := &scriptedHolderLeases{Store: lease.NewMemoryStore(), holding: lease.Holding{Owner: "holder", ExpiresAt: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)}, held: true}
	s, rs, _ := electedWorker(t, forever, "holder")
	waitForCond(t, 10*time.Second, "the holder's first sweep completed", func() bool { return !s.assistantWatch.heartbeat().CompletedAt.IsZero() })
	// Freeze the worker, then let the coordinator's clock run far past its
	// last completed pass: an elected holder whose sweep stopped.
	s.assistantWatchCancel(context.Background())
	<-s.assistantWatchDone
	s.assistantWatchCancel = nil
	s.assistantWatch.setElected(true)
	s.assistantWatch.clock = clock.NewFakeClock(time.Now().Add(time.Hour))
	seedHealthRun(t, rs, "run-1")
	out := watchHealthOf(t, s, "run-1")
	if !out.Coordinator.Elected || !out.Coordinator.Stale || out.Coordinator.HolderState != assistantWatchHolderHeld {
		t.Fatalf("answer of a stale holder = %+v; want elected, stale, holder_state held", out.Coordinator)
	}
	if got := sweepAttention(out.Attention); !slices.Equal(got, []string{"sweep_stale"}) {
		t.Fatalf("attention of a stale holder = %v, want exactly sweep_stale", got)
	}
}

// A term's first moments are not an outage: the holder is elected, its
// first pass has started and not completed, and the heartbeat calls that
// stale — the attention code waits for a pass that started three intervals
// ago and never completed. Through the route, on a real coordinator.
func TestAssistantWatchHealth_AFreshTermIsNotAStaleAlarm(t *testing.T) {
	forever := &scriptedHolderLeases{Store: lease.NewMemoryStore(), holding: lease.Holding{Owner: "holder", ExpiresAt: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)}, held: true}
	s, rs, _ := electedWorker(t, forever, "holder")
	waitForCond(t, 10*time.Second, "the holder's first sweep completed", func() bool { return !s.assistantWatch.heartbeat().CompletedAt.IsZero() })
	s.assistantWatchCancel(context.Background())
	<-s.assistantWatchDone
	s.assistantWatchCancel = nil
	seedHealthRun(t, rs, "run-1")
	c := s.assistantWatch
	now := time.Now()
	c.clock = clock.NewFakeClock(now)
	c.setElected(true) // the term starts at the fake now
	c.healthMu.Lock()
	c.lastSweepStartedAt, c.lastSweepCompletedAt = now, time.Time{}
	c.healthMu.Unlock()
	out := watchHealthOf(t, s, "run-1")
	if !out.Coordinator.Elected || !out.Coordinator.Stale {
		t.Fatalf("a fresh term reads %+v, want elected and stale (the heartbeat's own rule)", out.Coordinator)
	}
	if got := sweepAttention(out.Attention); len(got) != 0 {
		t.Fatalf("a term's first moments raise %v — the first pass is still running", got)
	}
	// Three intervals of the term and a second more, still nothing completed.
	c.clock = clock.NewFakeClock(now.Add(3*assistantWatchSweepInterval + time.Second))
	if got := sweepAttention(watchHealthOf(t, s, "run-1").Attention); !slices.Equal(got, []string{"sweep_stale"}) {
		t.Fatalf("a term with no pass completed in three intervals raises %v, want sweep_stale", got)
	}
}

// failingWatchStore fails the first read of every pass, so no pass ever
// completes while every pass starts.
type failingWatchStore struct {
	runwatch.Store
}

func (failingWatchStore) ActiveWatchUpperBound(context.Context) (*runwatch.WatchCursor, error) {
	return nil, errors.New("watch store down")
}

// A holder whose every pass fails from its first one re-stamps "started" at
// the top of each pass: a grace anchored on the last pass would never end,
// and a rollout that ships a sweep regression would never alarm. Anchored on
// the term, the alarm comes after three intervals of failing passes — run
// through the real sweep, one pass per interval.
func TestAssistantWatchHealth_PassesFailingSinceTheTermStartRaiseSweepStale(t *testing.T) {
	forever := &scriptedHolderLeases{Store: lease.NewMemoryStore(), holding: lease.Holding{Owner: "holder", ExpiresAt: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)}, held: true}
	s, rs, _ := electedWorker(t, forever, "holder")
	waitForCond(t, 10*time.Second, "the holder's first sweep completed", func() bool { return !s.assistantWatch.heartbeat().CompletedAt.IsZero() })
	s.assistantWatchCancel(context.Background())
	<-s.assistantWatchDone
	s.assistantWatchCancel = nil
	seedHealthRun(t, rs, "run-1")
	c := s.assistantWatch
	fake := clock.NewFakeClock(time.Now())
	c.clock = fake
	// The runtime switches to a store whose passes fail; the term starts.
	c.setRuntime(s.runs, failingWatchStore{Store: c.store}, c.botPaths)
	c.setElected(true)
	for i := 1; i <= 3; i++ {
		fake.Advance(assistantWatchSweepInterval)
		c.sweep(context.Background())
		out := watchHealthOf(t, s, "run-1")
		if !out.Coordinator.Elected || !out.Coordinator.Stale || out.Coordinator.LastSweepCompletedAt != nil {
			t.Fatalf("pass %d: %+v, want elected, stale, nothing completed", i, out.Coordinator)
		}
		if got := sweepAttention(out.Attention); len(got) != 0 {
			t.Fatalf("pass %d, within three intervals of the term: raises %v", i, got)
		}
	}
	fake.Advance(time.Second)
	c.sweep(context.Background()) // one more failing pass re-stamps "started" at the fake now
	out := watchHealthOf(t, s, "run-1")
	if out.Coordinator.LastSweepStartedAt == nil || !out.Coordinator.LastSweepStartedAt.Equal(fake.Now()) {
		t.Fatalf("the failing pass did not re-stamp started: %+v (now %s)", out.Coordinator, fake.Now())
	}
	if got := sweepAttention(out.Attention); !slices.Equal(got, []string{"sweep_stale"}) {
		t.Fatalf("passes failing since the term start for three intervals raise %v, want sweep_stale", got)
	}
}

// The decision itself, branch by branch: only an elected holder, stale past
// its term's first moments, under a lease that is not already reported
// expired, is the sweep_stale alarm. A follower stale for a project moved
// under the request (the heartbeat's generation guard) owes no sweep.
func TestSweepStaleIsAttention_OnlyAHoldersStoppedSweep(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	old := now.Add(-3*assistantWatchSweepInterval - time.Second)
	recent := now.Add(-time.Second)
	termOld := now.Add(-time.Hour)
	termYoung := now.Add(-time.Second)
	older := now.Add(-2 * time.Hour)
	holder := func(term time.Time, started, completed *time.Time) assistantWatchCoordinatorHealth {
		return assistantWatchCoordinatorHealth{Present: true, Elected: true, Stale: true, HolderState: assistantWatchHolderHeld, LastSweepStartedAt: started, LastSweepCompletedAt: completed, termStartedAt: term}
	}
	cases := []struct {
		name string
		c    assistantWatchCoordinatorHealth
		want bool
	}{
		{"holder, last completion of this term old", holder(termOld, &old, &old), true},
		{"holder, a pass started long ago and never completed", holder(termOld, &old, nil), true},
		{"holder whose every pass fails: started re-stamped just now, term old, nothing completed", holder(termOld, &recent, nil), true},
		{"holder, first pass still running", holder(termYoung, &recent, nil), false},
		{"holder, no pass started yet", holder(termYoung, nil, nil), false},
		{"re-elected holder, a completion from its previous term, term young", holder(termYoung, &recent, &old), false},
		{"re-elected holder, a completion from its previous term, term old and nothing completed in it", holder(termOld, &recent, &older), true},
		{"holder, not stale", assistantWatchCoordinatorHealth{Present: true, Elected: true, HolderState: assistantWatchHolderHeld, LastSweepCompletedAt: &recent, termStartedAt: termOld}, false},
		{"follower stale for a project switch", assistantWatchCoordinatorHealth{Present: true, Elected: false, Stale: true, HolderState: assistantWatchHolderHeld, LastSweepCompletedAt: &old, termStartedAt: termOld}, false},
		{"expired lease carries its own code", assistantWatchCoordinatorHealth{Present: true, Elected: true, Stale: true, HolderState: assistantWatchHolderExpired, LastSweepCompletedAt: &old, termStartedAt: termOld}, false},
	}
	for _, tc := range cases {
		if got := sweepStaleIsAttention(tc.c, now); got != tc.want {
			t.Errorf("%s: sweepStaleIsAttention = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// The term's first moments are counted from the election or the runtime
// switch, whichever came last: a runtime set long before the election, and a
// project switched long after it, each start the grace afresh — through the
// real coordinator and the route.
func TestAssistantWatchHealth_TheGraceRestartsAtElectionAndAtARuntimeSwitch(t *testing.T) {
	forever := &scriptedHolderLeases{Store: lease.NewMemoryStore(), holding: lease.Holding{Owner: "holder", ExpiresAt: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)}, held: true}
	s, rs, _ := electedWorker(t, forever, "holder")
	waitForCond(t, 10*time.Second, "the holder's first sweep completed", func() bool { return !s.assistantWatch.heartbeat().CompletedAt.IsZero() })
	s.assistantWatchCancel(context.Background())
	<-s.assistantWatchDone
	s.assistantWatchCancel = nil
	seedHealthRun(t, rs, "run-1")
	c := s.assistantWatch
	fake := clock.NewFakeClock(time.Now())
	c.clock = fake
	c.setElected(false)
	c.setRuntime(s.runs, c.store, c.botPaths) // stamps cleared at T, nobody elected
	fake.Advance(time.Hour)
	c.setElected(true) // elected at T+1h: the term starts now, not at T
	out := watchHealthOf(t, s, "run-1")
	if !out.Coordinator.Elected || !out.Coordinator.Stale {
		t.Fatalf("just elected, nothing completed: %+v, want elected and stale", out.Coordinator)
	}
	if got := sweepAttention(out.Attention); len(got) != 0 {
		t.Fatalf("a term elected an hour after its runtime was set raises %v at its first moment", got)
	}
	fake.Advance(time.Hour)
	c.setRuntime(s.runs, c.store, c.botPaths) // the project switches under the holder at T+2h
	if got := sweepAttention(watchHealthOf(t, s, "run-1").Attention); len(got) != 0 {
		t.Fatalf("a runtime switched an hour into the term raises %v at its first moment", got)
	}
	fake.Advance(3*assistantWatchSweepInterval + time.Second)
	if got := sweepAttention(watchHealthOf(t, s, "run-1").Attention); !slices.Equal(got, []string{"sweep_stale"}) {
		t.Fatalf("three intervals after the switch with nothing completed raises %v, want sweep_stale", got)
	}
}

// A re-elected holder's first moments carry a completion from its previous
// term; the heartbeat hands the term start to the route for that to be
// known. Read through the real heartbeat, not a hand-built input: a
// heartbeat that dropped the term start would alarm here.
func TestAssistantWatchHealth_AReElectedHoldersFirstMomentsAreNotAnAlarm(t *testing.T) {
	forever := &scriptedHolderLeases{Store: lease.NewMemoryStore(), holding: lease.Holding{Owner: "holder", ExpiresAt: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)}, held: true}
	s, rs, _ := electedWorker(t, forever, "holder")
	waitForCond(t, 10*time.Second, "the holder's first sweep completed", func() bool { return !s.assistantWatch.heartbeat().CompletedAt.IsZero() })
	s.assistantWatchCancel(context.Background())
	<-s.assistantWatchDone
	s.assistantWatchCancel = nil
	seedHealthRun(t, rs, "run-1")
	c := s.assistantWatch
	completed := c.heartbeat().CompletedAt
	fake := clock.NewFakeClock(completed.Add(2 * time.Minute))
	c.clock = fake
	c.setElected(false) // deposed
	c.setElected(true)  // re-elected two minutes later: its completion is from the previous term
	out := watchHealthOf(t, s, "run-1")
	if !out.Coordinator.Elected || !out.Coordinator.Stale || out.Coordinator.LastSweepCompletedAt == nil {
		t.Fatalf("a re-elected holder reads %+v, want elected, stale, with its previous term's completion", out.Coordinator)
	}
	if got := sweepAttention(out.Attention); len(got) != 0 {
		t.Fatalf("a re-elected holder's first moments raise %v", got)
	}
	fake.Advance(3*assistantWatchSweepInterval + time.Second)
	if got := sweepAttention(watchHealthOf(t, s, "run-1").Attention); !slices.Equal(got, []string{"sweep_stale"}) {
		t.Fatalf("three intervals into the new term with nothing completed raises %v, want sweep_stale", got)
	}
}
