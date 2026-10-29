package lease

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"syscall"
	"testing"
	"time"
)

// passJournal is what the probe of #2685 measured, as a test observer: which
// replica served the first term, how many passes each replica ran, and what
// each of them was worth. It is fed by the campaign goroutines of every
// replica, so every field is read and written under its mutex — the -race
// job has reddened this class twice in one day.
type passJournal struct {
	mu     sync.Mutex
	first  string
	passes map[string][]PassVerdict
}

func newPassJournal() *passJournal {
	return &passJournal{passes: map[string][]PassVerdict{}}
}

// pass records one pass by owner and reports whether this owner is the one
// that served the first term — the sick replica of the probe.
func (j *passJournal) pass(owner string, v PassVerdict) (sick bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.first == "" {
		j.first = owner
	}
	j.passes[owner] = append(j.passes[owner], v)
	return owner == j.first
}

// isSick reports whether owner is the replica that served the first term,
// without recording a pass: the pass function has to know before it decides
// what to report.
func (j *passJournal) isSick(owner string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.first == "" {
		j.first = owner
	}
	return owner == j.first
}

// healthyPasses counts the passes run by any replica other than the first
// elected one: the number the probe measured at ZERO over 20 TTLs.
func (j *passJournal) healthyPasses() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	n := 0
	for owner, vs := range j.passes {
		if owner != j.first {
			n += len(vs)
		}
	}
	return n
}

func (j *passJournal) passesBy(owner string) int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.passes[owner])
}

// total counts every pass by every replica. Which replica is elected first is
// the store's to decide, so a test that waits for "the holder's passes" waits
// on this and never on one name — a condition that only holds when replica-0
// wins the first campaign is flaky, not a witness.
func (j *passJournal) total() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	n := 0
	for _, vs := range j.passes {
		n += len(vs)
	}
	return n
}

// unreachable is the failure a per-node resolver flap reaches a pass as: the
// NXDOMAIN of a name every other node resolves. Wrapped the way an HTTP
// client wraps it, so the test exercises what errors.As has to traverse in
// production rather than a bare sentinel.
func unreachable() error {
	return &url.Error{Op: "Get", URL: "https://forge.example/api/v4", Err: &net.DNSError{Err: "no such host", Name: "forge.example", IsNotFound: true}}
}

// errRefused is the far side answering, and answering no: a credential every
// replica would be refused identically.
var errRefused = errors.New("401 Unauthorized: the installation token is not valid")

// servePasses is the pass loop of a leased net, reduced to what every one of
// them shares: a pass on entry, then one per tick, and a return — which
// releases the lease — when the health says to hand over.
func servePasses(h *Health, every time.Duration, pass func() PassVerdict) func(context.Context) {
	return func(ctx context.Context) {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			if h.Observe(pass()) {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}
}

// The defect of #2685: a holder whose every pass fails for a transport reason
// renews its lease through a store it can still reach, while the healthy
// replicas never run. The probe measured 60 failed passes and 0 healthy
// passes over 20 TTLs; the property is that a healthy replica gets a pass.
func TestRun_aLeaderWhoseEveryPassIsUnreachableHandsOverToAHealthyPeer(t *testing.T) {
	const tick = 50 * time.Millisecond
	const leaseTTL = 3 * tick
	st := NewMemoryStore()
	log := &logSink{}
	j := newPassJournal()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		owner := fmt.Sprintf("replica-%d", i)
		h := &Health{Passes: 2}
		sp := spec(owner, leaseTTL, log)
		sp.Retry, sp.Health = tick, h
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = Run(ctx, st, sp, servePasses(h, tick, func() PassVerdict {
				var p Pass
				if j.isSick(owner) {
					p.Failed(unreachable())
				} else {
					p.Succeeded()
				}
				v := p.Verdict()
				j.pass(owner, v)
				return v
			}))
		}()
	}
	// The budget is generous on purpose: the property is that a healthy
	// replica EVENTUALLY runs, and the mutant — a term that never ends on
	// the work failing — never lets one run, whatever the budget.
	until(t, 10*time.Second, "a healthy replica ran at least one pass", func() bool { return j.healthyPasses() >= 1 })
	if !log.has("handing over") {
		t.Errorf("no handover line in the campaign log: %v", log.all())
	}
	cancel()
	wg.Wait()
	if n := j.healthyPasses(); n < 1 {
		t.Fatalf("healthy passes = %d, want at least one", n)
	}
}

// The mirror, as its own isolated scenario: a net whose passes find nothing
// to do is the healthiest state the net has, and its holder keeps the lease.
// Two guards, two scenarios, or neither is proven.
func TestRun_aLeaderWhosePassesFindNothingToDoKeepsItsLease(t *testing.T) {
	const tick = 50 * time.Millisecond
	const leaseTTL = 3 * tick
	st := NewMemoryStore()
	log := &logSink{}
	j := newPassJournal()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		owner := fmt.Sprintf("replica-%d", i)
		h := &Health{Passes: 2}
		sp := spec(owner, leaseTTL, log)
		sp.Retry, sp.Health = tick, h
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = Run(ctx, st, sp, servePasses(h, tick, func() PassVerdict {
				var p Pass // nothing was due: the zero pass is an idle one
				v := p.Verdict()
				j.pass(owner, v)
				return v
			}))
		}()
	}
	// Four TTLs of idle passes: twice the run of failures that would end a
	// term if idleness were read as sickness.
	until(t, 10*time.Second, "the holder ran its first passes", func() bool { return j.total() >= 4 })
	time.Sleep(4 * leaseTTL)
	cancel()
	wg.Wait()
	if n := j.healthyPasses(); n != 0 {
		t.Errorf("a peer ran %d passes: the idle holder lost its lease", n)
	}
	if log.has("handing over") {
		t.Errorf("an idle holder handed over: %v", log.all())
	}
}

// The other half of the classification: the far side ANSWERED, and answered
// no. A peer would be refused identically, so handing over would move the
// refusal instead of repairing it — and the answer coming back is proof this
// replica reaches the far side, so it also clears the run of failures.
func TestRun_aLeaderTheFarSideRefusesKeepsItsLease(t *testing.T) {
	const tick = 50 * time.Millisecond
	const leaseTTL = 3 * tick
	st := NewMemoryStore()
	log := &logSink{}
	j := newPassJournal()
	h := &Health{Passes: 2}
	sp := spec("replica-0", leaseTTL, log)
	sp.Retry, sp.Health = tick, h
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = Run(ctx, st, sp, servePasses(h, tick, func() PassVerdict {
			var p Pass
			p.Failed(errRefused)
			v := p.Verdict()
			j.pass("replica-0", v)
			return v
		}))
	}()
	until(t, 10*time.Second, "four refused passes", func() bool { return j.passesBy("replica-0") >= 4 })
	cancel()
	wg.Wait()
	if log.has("handing over") {
		t.Errorf("a refusal handed the lease over: %v", log.all())
	}
	if st := h.Snapshot(); st.FailingPasses != 0 || st.Handovers != 0 {
		t.Errorf("snapshot = %+v, want no failing run and no handover", st)
	}
}

// The handover is only useful with the backoff: the replica that just
// released is the candidate already awake, and it would win the lease it gave
// up. The assertion is a LOWER bound — a loaded scheduler can only make the
// gap longer.
func TestRun_theHandoverBacksTheSickReplicaOffBeforeItCampaignsAgain(t *testing.T) {
	const tick = 20 * time.Millisecond
	const backoff = 400 * time.Millisecond
	st := NewMemoryStore()
	log := &logSink{}
	h := &Health{Passes: 1, Backoff: backoff}
	sp := spec("replica-0", 3*tick, log)
	sp.Retry, sp.Health = tick, h
	var mu sync.Mutex
	var termStarts []time.Time
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = Run(ctx, st, sp, servePasses(h, tick, func() PassVerdict {
			mu.Lock()
			termStarts = append(termStarts, time.Now())
			mu.Unlock()
			var p Pass
			p.Failed(unreachable())
			return p.Verdict()
		}))
	}()
	until(t, 10*time.Second, "two terms, each ended by the handover", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(termStarts) >= 2
	})
	cancel()
	wg.Wait()
	mu.Lock()
	gap := termStarts[1].Sub(termStarts[0])
	mu.Unlock()
	if gap < backoff {
		t.Fatalf("the sick replica campaigned again after %s, want at least the %s backoff", gap, backoff)
	}
}

// The fleet circuit breaker: when every peer takes the lease and gives it
// back just as sick, the far side — not this replica — is what cannot be
// reached, and a net bouncing between replicas with nobody working is worse
// than one replica trying.
func TestHealth_theCircuitStopsTheChurnOnceHandingOverStopsHelping(t *testing.T) {
	h := &Health{Passes: 1, SickTerms: 2}
	for term := 1; term <= 2; term++ {
		h.beginTerm()
		if !h.Observe(VerdictUnreachable) {
			t.Fatalf("term %d: the first unreachable pass did not end the term", term)
		}
		ended, sick, opened := h.endTerm()
		if !ended || sick != term {
			t.Fatalf("term %d: endTerm = (%v, %d), want (true, %d)", term, ended, sick, term)
		}
		if opened != (term == 2) {
			t.Fatalf("term %d: circuit opened = %v, want %v", term, opened, term == 2)
		}
	}
	h.beginTerm()
	for pass := 1; pass <= 5; pass++ {
		if h.Observe(VerdictUnreachable) {
			t.Fatalf("pass %d handed over with the circuit open", pass)
		}
	}
	st := h.Snapshot()
	if !st.CircuitOpen || st.Handovers != 2 {
		t.Fatalf("snapshot = %+v, want the circuit open after exactly 2 handovers", st)
	}
	// And it closes on the first pass that works: the replica is reaching
	// the far side again, so the mechanism is armed for the next outage.
	if h.Observe(VerdictProgressed) {
		t.Fatal("a pass that progressed handed the lease over")
	}
	if st := h.Snapshot(); st.CircuitOpen || st.SickTerms != 0 {
		t.Fatalf("snapshot = %+v, want the circuit closed and the sick run cleared", st)
	}
}

// The circuit is evidence about THIS replica's consecutive sick terms, so a
// term that ended another way re-arms the handover. Without it, one outage
// disarms the mechanism for every outage after it.
func TestHealth_aTermThatEndedAnotherWayRearmsTheHandover(t *testing.T) {
	h := &Health{Passes: 1, SickTerms: 1}
	h.beginTerm()
	if !h.Observe(VerdictUnreachable) {
		t.Fatal("the first unreachable pass did not end the term")
	}
	if _, _, opened := h.endTerm(); !opened {
		t.Fatal("the circuit did not open after the one sick term it takes")
	}
	// A term that ends on a step-down, a rollout or the lease lost: no pass
	// handed over.
	h.beginTerm()
	if ended, _, _ := h.endTerm(); ended {
		t.Fatal("a term nothing handed over reported a handover")
	}
	if st := h.Snapshot(); st.CircuitOpen {
		t.Fatalf("snapshot = %+v, want the circuit closed again", st)
	}
	h.beginTerm()
	if !h.Observe(VerdictUnreachable) {
		t.Fatal("the handover stayed disarmed for the next outage")
	}
}

// The override: a mechanism that gives work up keeps an escape hatch, and it
// is greppable rather than implicit.
func TestHealth_theOverrideDisarmsTheHandover(t *testing.T) {
	h := &Health{Passes: 1, Off: true}
	h.beginTerm()
	for pass := 1; pass <= 5; pass++ {
		if h.Observe(VerdictUnreachable) {
			t.Fatalf("pass %d handed over with the override off", pass)
		}
	}
	ended, _, _ := h.endTerm()
	if ended {
		t.Fatal("endTerm reported a handover with the override off")
	}
	if st := h.Snapshot(); st.Armed {
		t.Fatalf("snapshot = %+v, want Armed false", st)
	}
}

// A run of failures belongs to ONE term: a term that ended for another
// reason — a step-down, a rollout — must not have its successor start K-1
// failures deep, or the next term hands over on its first bad pass.
func TestHealth_aTermStartsWithACleanRunOfFailures(t *testing.T) {
	h := &Health{Passes: 3}
	h.beginTerm()
	for pass := 1; pass <= 2; pass++ {
		if h.Observe(VerdictUnreachable) {
			t.Fatalf("pass %d of 3 handed over", pass)
		}
	}
	h.beginTerm()
	for pass := 1; pass <= 2; pass++ {
		if h.Observe(VerdictUnreachable) {
			t.Fatalf("pass %d of the NEXT term handed over: the previous term's run of failures carried over", pass)
		}
	}
	if st := h.Snapshot(); st.FailingPasses != 2 {
		t.Fatalf("snapshot = %+v, want a run of 2 failing passes in this term", st)
	}
}

// A nil *Pass accounts into nothing and crashes nothing: the same repair
// functions run outside an elected term, where no pass is open.
func TestPass_aNilPassAccountsIntoNothing(t *testing.T) {
	var p *Pass
	p.Succeeded()
	p.Failed(unreachable())
	p.Attempted(3, 0, unreachable())
	if got := p.Verdict(); got != VerdictIdle {
		t.Fatalf("verdict = %s, want %s", got, VerdictIdle)
	}
}

// A nil *Health is the behaviour before #2685, in full: nothing ends a term
// but lease loss and shutdown, and the campaign's pace is untouched.
func TestHealth_aNilHealthIsTheBehaviourWithoutIt(t *testing.T) {
	var h *Health
	h.beginTerm()
	if h.Observe(VerdictUnreachable) {
		t.Fatal("a nil health handed the lease over")
	}
	if ended, sick, opened := h.endTerm(); ended || sick != 0 || opened {
		t.Fatalf("endTerm = (%v, %d, %v), want the zero answer", ended, sick, opened)
	}
	if b := h.backoff(time.Second); b != 0 {
		t.Fatalf("backoff = %s, want 0", b)
	}
	if st := h.Snapshot(); st.Armed || st.Passes != 0 {
		t.Fatalf("snapshot = %+v, want the zero state", st)
	}
}

// What the pass did, and nothing else: the verdict never comes from the
// absence of progress.
func TestPass_classifiesWhatThePassDidAndNothingElse(t *testing.T) {
	for _, c := range []struct {
		name   string
		record func(p *Pass)
		want   PassVerdict
	}{
		{"nothing was due", func(p *Pass) {}, VerdictIdle},
		{"one item worked", func(p *Pass) { p.Succeeded() }, VerdictProgressed},
		{"one of three worked", func(p *Pass) { p.Succeeded(); p.Failed(unreachable()); p.Failed(unreachable()) }, VerdictProgressed},
		{"a counting worker that refreshed some", func(p *Pass) { p.Attempted(4, 2, unreachable()) }, VerdictProgressed},
		{"a counting worker that refreshed none", func(p *Pass) { p.Attempted(4, 0, unreachable()) }, VerdictUnreachable},
		{"a counting worker with nothing due", func(p *Pass) { p.Attempted(0, 0, nil) }, VerdictIdle},
		{"every item unreachable", func(p *Pass) { p.Failed(unreachable()); p.Failed(unreachable()) }, VerdictUnreachable},
		{"every item refused", func(p *Pass) { p.Failed(errRefused) }, VerdictRefused},
		{"the first failure decides", func(p *Pass) { p.Failed(errRefused); p.Failed(unreachable()) }, VerdictRefused},
		{"a failure nobody named", func(p *Pass) { p.Failed(nil) }, VerdictRefused},
		{"items attempted, nothing reported of them", func(p *Pass) { p.Attempted(3, 0, nil) }, VerdictIdle},
	} {
		t.Run(c.name, func(t *testing.T) {
			var p Pass
			c.record(&p)
			if got := p.Verdict(); got != c.want {
				t.Fatalf("verdict = %s, want %s", got, c.want)
			}
		})
	}
}

// Would another replica plausibly do better? That is the only question a
// handover turns on.
func TestTransportFailure_separatesWhatAPeerMightNotShareFromWhatItWould(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"nothing failed", nil, false},
		{"the term ended", context.Canceled, false},
		{"a per-node resolver flap", unreachable(), true},
		{"a NetworkPolicy refusing the connection", &url.Error{Op: "Get", URL: "https://forge.example", Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}}, true},
		{"conntrack dropping the established flow", fmt.Errorf("read tcp: %w", syscall.ECONNRESET), true},
		{"the far side unreachable from this node", fmt.Errorf("dial: %w", syscall.EHOSTUNREACH), true},
		{"a stream cut mid-flight", fmt.Errorf("read body: %w", io.ErrUnexpectedEOF), true},
		{"a client-bounded call that never answered", fmt.Errorf("refresh: %w", context.DeadlineExceeded), true},
		{"the far side answering no", errRefused, false},
		{"a credential the far side rejects", errors.New("invalid_grant"), false},
		{"a certificate every replica would be shown", fmt.Errorf("tls: %w", x509.UnknownAuthorityError{}), false},
		{"a store answering with a decode failure", errors.New("bson: cannot decode"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := TransportFailure(c.err); got != c.want {
				t.Fatalf("TransportFailure(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
