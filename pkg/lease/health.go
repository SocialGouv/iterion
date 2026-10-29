package lease

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"time"
)

// DefaultHandoverPasses is K when Health.Passes is unset: the consecutive
// unreachable passes that end a term. One is a blip the next pass retries;
// three paced by a net's own interval is the shortest run that cannot be a
// single flap, and it bounds the blackhole at three intervals.
const DefaultHandoverPasses = 3

// DefaultSickTerms is the fleet circuit breaker's threshold when
// Health.SickTerms is unset: the consecutive terms ended by a handover after
// which this replica stops handing over. Three says the fleet, not this
// replica, is what cannot reach the far side — every peer took the lease and
// gave it back just as sick — and a fourth handover would only keep the net
// bouncing between replicas with nobody working.
const DefaultSickTerms = 3

// defaultBackoffTerms sizes the wait a handover adds before this replica
// campaigns again, in campaign retries: every peer gets a full attempt at the
// lease before the replica that just gave it up tries again. Without it the
// sick holder races its peers for the lease it released a moment ago, and the
// race is one it often wins — it is the candidate already awake.
const defaultBackoffTerms = 2

// PassVerdict is what ONE pass of a leased net reports about itself.
//
// A term ends on the work failing only on this verdict, which travels FROM
// the pass: "no progress" is never read as "sick", because a net whose passes
// find nothing to do is the healthiest state the net has.
type PassVerdict int

const (
	// VerdictIdle is a pass that attempted nothing: no work was due. It says
	// nothing about this replica's reach, so it neither ends a term nor
	// clears a run of failures.
	VerdictIdle PassVerdict = iota
	// VerdictProgressed is a pass at least one of whose items succeeded. The
	// holder works: the run of failures is cleared and the circuit closes.
	VerdictProgressed
	// VerdictUnreachable is a pass that attempted work, none of which
	// succeeded, whose first failure was a transport one — the class a peer
	// on another node may well not share (per-node DNS, a NetworkPolicy,
	// conntrack). K of these in a row end the term.
	VerdictUnreachable
	// VerdictRefused is a pass that attempted work, none of which succeeded,
	// whose failure was not a transport one: the far side answered, and
	// answered no. The answer coming back is itself proof this replica
	// reaches it, so the run of transport failures is cleared — while a
	// refusal is not progress, and does not close the circuit. Handing over
	// would move the refusal, not repair it.
	VerdictRefused
)

func (v PassVerdict) String() string {
	switch v {
	case VerdictIdle:
		return "idle"
	case VerdictProgressed:
		return "progressed"
	case VerdictUnreachable:
		return "unreachable"
	case VerdictRefused:
		return "refused"
	}
	return "unknown"
}

// Pass accumulates what ONE pass of a leased net did, so the verdict comes
// from the pass instead of being inferred by its observer. The zero value is
// an idle pass. Safe for concurrent use: a pass whose items run in parallel
// records from several goroutines.
type Pass struct {
	mu        sync.Mutex
	attempted int
	succeeded int
	firstErr  error
}

// Attempted records n items the pass tried, s of which succeeded, and the
// first failure it met — nil when none did. It is the shape of a worker that
// already counts, like forge.RefreshWorker.RunOnce's (refreshed, firstErr).
func (p *Pass) Attempted(n, s int, firstErr error) {
	if n <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempted += n
	p.succeeded += s
	if p.firstErr == nil {
		p.firstErr = firstErr
	}
}

// Succeeded records ONE item the pass attempted and completed.
func (p *Pass) Succeeded() { p.Attempted(1, 1, nil) }

// Failed records ONE item the pass attempted and could not complete. A nil
// err is still an attempt that did not succeed, and leaves the pass unable to
// classify its failure: Verdict answers VerdictRefused, the direction that
// does not hand the lease over on a cause nobody named.
func (p *Pass) Failed(err error) {
	if err == nil {
		err = errUnnamedFailure
	}
	p.Attempted(1, 0, err)
}

// errUnnamedFailure stands in for a failure recorded without a cause, so an
// all-failed pass is never classified as unreachable by default.
var errUnnamedFailure = errors.New("lease: a pass recorded a failure without naming it")

// Verdict classifies the pass. A pass that attempted items and recorded
// neither a success nor a failure for them is idle: a verdict guessed from
// that silence is exactly what this type exists to refuse.
func (p *Pass) Verdict() PassVerdict {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.attempted == 0:
		return VerdictIdle
	case p.succeeded > 0:
		return VerdictProgressed
	case p.firstErr == nil:
		return VerdictIdle
	case TransportFailure(p.firstErr):
		return VerdictUnreachable
	default:
		return VerdictRefused
	}
}

// TransportFailure reports whether err is the class of failure a peer on
// another node may well not share: the pass could not reach what it had to
// reach. It answers the only question a handover turns on — would another
// replica plausibly do better? — so a refusal that came BACK from the far
// side (an HTTP status, an invalid credential, a validation, a TLS identity)
// is false: a peer would be refused identically.
//
// Typed, never by message: the causes the probe of #2685 named — per-node
// DNS, a NetworkPolicy, conntrack — reach a caller as *net.DNSError or a
// syscall errno inside an HTTP client's *url.Error, which errors.As
// traverses.
//
// context.Canceled is false: it is the term ending, not the egress failing.
// context.DeadlineExceeded is true — it is how a call to an unreachable peer
// ends when the client bounds it rather than the kernel — so a pass must not
// record the expiry of its OWN context as a failure, or every shutdown under
// a deadline would read as a handover.
func TransportFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	// A resolver flap is typed, including NXDOMAIN: a half-broken resolver
	// answers "no such host" for a name that resolves on every other node,
	// which is the per-node DNS failure this whole net exists to cover.
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	for _, errno := range []syscall.Errno{
		syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.ECONNABORTED,
		syscall.ETIMEDOUT, syscall.EPIPE, syscall.ENETUNREACH,
		syscall.EHOSTUNREACH, syscall.ENETDOWN, syscall.ENETRESET,
		syscall.EHOSTDOWN,
	} {
		if errors.Is(err, errno) {
			return true
		}
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// Last, and only on Timeout(): a net.Error that is not a timeout is
	// already covered above by its errno, and context.DeadlineExceeded
	// reaches here as the timeout of a bounded call.
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// Health lets a term end on the work failing, backs this replica off before
// it campaigns again, and stops handing over once handing over has
// demonstrably stopped helping. One per replica per lease: the state it keeps
// is this candidate's own reach, not the lease's.
//
// It is wired through Spec.Health and read by the net's pass loop (Observe).
// A nil *Health is the behaviour without it — a term ends on lease loss or
// shutdown, never on the work failing — so every method below is nil-safe.
type Health struct {
	// Passes is K: the consecutive unreachable passes that end a term. Zero
	// or negative means DefaultHandoverPasses.
	Passes int
	// SickTerms is the fleet circuit breaker's threshold: the consecutive
	// terms this replica ended by handing over after which it stops handing
	// over and keeps serving, because every peer gave the lease back just as
	// sick. Zero or negative means DefaultSickTerms.
	SickTerms int
	// Backoff is the wait a handover adds before this replica campaigns
	// again. Zero means defaultBackoffTerms campaign retries, which is the
	// only form that scales with a net's own pace.
	Backoff time.Duration
	// Off disarms the handover: a term ends on lease loss or shutdown only.
	// The operator's escape hatch over a mechanism that gives work up, kept
	// greppable rather than implicit (the server reads
	// ITERION_LEASE_HANDOVER into it).
	Off bool

	mu sync.Mutex
	// unreachable is the run of consecutive unreachable passes in this term.
	unreachable int
	// sickTerms is the run of consecutive terms ended by a handover.
	sickTerms int
	// handedOver says THIS term ended because the work kept failing.
	handedOver bool
	// open is the circuit breaker: handing over has not helped, so this
	// replica keeps serving. Closed again by the first pass that progresses.
	open bool
	// handovers, lastAt and lastVerdict are what an operator reads
	// (Snapshot); they are not read by any decision here.
	handovers   int
	lastAt      time.Time
	lastVerdict PassVerdict
}

// HandoverState is the handover as an operator reads it: armed or not, where
// the current run of failures stands, and whether this replica has stopped
// handing over.
type HandoverState struct {
	// Armed is whether a term can end on the work failing at all: a Health
	// is wired and its override is not off.
	Armed bool `json:"armed"`
	// Passes is K, the run of unreachable passes that ends a term.
	Passes int `json:"passes,omitempty"`
	// FailingPasses is where that run stands in the current term.
	FailingPasses int `json:"failing_passes,omitempty"`
	// Handovers is how many terms this replica has ended by handing over
	// since it started.
	Handovers int `json:"handovers,omitempty"`
	// SickTerms is the current run of consecutive such terms.
	SickTerms int `json:"sick_terms,omitempty"`
	// CircuitOpen says this replica keeps serving despite its passes
	// failing: handing over stopped helping, so the net is not bouncing
	// between replicas with nobody working.
	CircuitOpen bool `json:"circuit_open,omitempty"`
	// LastPassAt and LastPassVerdict are the last pass this replica
	// classified, so a reader can tell a quiet net from a dead one.
	LastPassAt      *time.Time `json:"last_pass_at,omitempty"`
	LastPassVerdict string     `json:"last_pass_verdict,omitempty"`
}

// Observe records one pass's verdict and reports whether the term must end:
// the net returns from its pass loop, which releases the lease (Run), and a
// peer campaigns at its next attempt. Safe for concurrent use.
//
// It reports true once per term: the caller is expected to return promptly,
// as it is for a term context that was cancelled.
func (h *Health) Observe(v PassVerdict) (handOver bool) {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastVerdict, h.lastAt = v, time.Now()
	switch v {
	case VerdictProgressed:
		h.unreachable, h.sickTerms, h.open = 0, 0, false
		return false
	case VerdictRefused:
		// The far side answered: this replica reaches it, whatever the
		// answer was.
		h.unreachable = 0
		return false
	case VerdictUnreachable:
		h.unreachable++
	default: // VerdictIdle: no evidence either way, and no decision.
		return false
	}
	if h.Off || h.open || h.handedOver || h.unreachable < h.passes() {
		return false
	}
	h.handedOver = true
	h.handovers++
	return true
}

// Snapshot is the handover state as an operator reads it.
func (h *Health) Snapshot() HandoverState {
	if h == nil {
		return HandoverState{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := HandoverState{
		Armed:         !h.Off,
		Passes:        h.passes(),
		FailingPasses: h.unreachable,
		Handovers:     h.handovers,
		SickTerms:     h.sickTerms,
		CircuitOpen:   h.open,
	}
	if !h.lastAt.IsZero() {
		at := h.lastAt.UTC()
		out.LastPassAt, out.LastPassVerdict = &at, h.lastVerdict.String()
	}
	return out
}

// beginTerm clears what belongs to one term. The run of sick TERMS is not
// one of them: it is what tells this replica that handing over is not working.
func (h *Health) beginTerm() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unreachable, h.handedOver = 0, false
}

// endTerm closes a term and reports what it was: whether the work ended it,
// the run of such terms, and whether this term is the one that opened the
// circuit — the transition, so the fact is logged once rather than at every
// term that follows.
func (h *Health) endTerm() (handedOver bool, sickTerms int, circuitOpened bool) {
	if h == nil {
		return false, 0, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.handedOver {
		// A term that ended another way — a step-down, a rollout, the lease
		// lost — breaks the run of sick terms AND re-arms the handover: the
		// evidence that handing over was not helping is this replica's own
		// consecutive sick terms, and a term that ended otherwise is not one
		// of them. Without this, a circuit opened by a past outage disarms
		// the handover for the next one.
		h.sickTerms, h.open = 0, false
		return false, 0, false
	}
	h.sickTerms++
	if !h.open && h.sickTerms >= h.maxSickTerms() {
		h.open = true
		circuitOpened = true
	}
	return true, h.sickTerms, circuitOpened
}

// backoff is the wait a handover adds to the campaign's retry, so every peer
// gets an attempt at the lease before this replica tries again.
func (h *Health) backoff(retry time.Duration) time.Duration {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Backoff > 0 {
		return h.Backoff
	}
	return defaultBackoffTerms * retry
}

// passes is K with its default applied. Called under h.mu.
func (h *Health) passes() int {
	if h.Passes > 0 {
		return h.Passes
	}
	return DefaultHandoverPasses
}

// maxSickTerms is the circuit breaker's threshold with its default applied.
// Called under h.mu.
func (h *Health) maxSickTerms() int {
	if h.SickTerms > 0 {
		return h.SickTerms
	}
	return DefaultSickTerms
}
