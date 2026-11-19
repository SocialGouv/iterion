// Package polling runs a connector source iterion reads instead of being
// called (ADR-168 §7): each poller's tick calls its trigger's read operation
// through the connection resolver, turns every item it has not seen into an
// intake — idempotent on (poller, item id, item version), numbered by the
// poller's own sequence — and only then moves the checkpoint, under the
// generation its tick took (pkg/connector/poller).
//
// The worker is elected to one replica (the server wraps Pass in a lease
// campaign); the generation is what keeps a stalled holder from writing over
// its successor, and the intake's identity what absorbs the items two
// overlapping holders both accept.
package polling

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/binding"
	"github.com/SocialGouv/iterion/pkg/connector/exec"
	"github.com/SocialGouv/iterion/pkg/connector/ingress"
	"github.com/SocialGouv/iterion/pkg/connector/intake"
	"github.com/SocialGouv/iterion/pkg/connector/poller"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/lease"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// The operator's dials (internal/envtrust classifies the prefix).
const (
	// EnvInterval sets every poller's cadence (a Go duration) in place of
	// its trigger's default, never below the trigger's floor.
	EnvInterval = "ITERION_CONNECTOR_POLL_INTERVAL"
	// EnvMinInterval lowers the floor a trigger's interval.min sets.
	EnvMinInterval = "ITERION_CONNECTOR_POLL_MIN_INTERVAL"
	// EnvSeenRetention is how long an item stays in its poller's seen set
	// after it was last observed (a Go duration; 0 keeps it for ever).
	EnvSeenRetention = "ITERION_CONNECTOR_POLL_SEEN_RETENTION"
	// EnvPassInterval is how often the elected worker looks for due
	// pollers.
	EnvPassInterval = "ITERION_CONNECTOR_POLL_PASS_INTERVAL"
	// EnvTickTimeout bounds one poller's scan (a Go duration): a slow vendor
	// costs its own poller's tick, not every tenant's pass.
	EnvTickTimeout = "ITERION_CONNECTOR_POLL_TICK_TIMEOUT"
	// EnvConcurrency is how many pollers one pass ticks at once.
	EnvConcurrency = "ITERION_CONNECTOR_POLL_CONCURRENCY"
)

// Defaults of the dials.
const (
	DefaultSeenRetention = 7 * 24 * time.Hour
	DefaultPassInterval  = 5 * time.Second
	DefaultTickTimeout   = 2 * time.Minute
	DefaultConcurrency   = 4
	// DefaultBatch bounds the pollers one pass ticks.
	DefaultBatch = 20
	userAgent    = "iterion-poller"
	// dormantRecheck is how long a poller nobody listens to sleeps between
	// two looks; a subscription activated on it wakes it at once
	// (poller.Store.Wake).
	dormantRecheck = 24 * time.Hour
	// refusedRecheck is how soon a poller whose trigger cannot be read is
	// looked at again.
	refusedRecheck = 5 * time.Minute
	// storeTimeout bounds the bookkeeping writes after a scan, on a context
	// of their own: a scan that spent its deadline still records its outcome.
	storeTimeout = 30 * time.Second
)

// Policy is the operator's dials, read once.
type Policy struct {
	// Interval, when set, replaces every trigger's default cadence.
	Interval time.Duration
	// MinInterval, when set, replaces every trigger's floor.
	MinInterval time.Duration
	// SeenRetention is how long a key outlives its last observation; 0
	// keeps every key.
	SeenRetention time.Duration
	// IntakeRetention is how long a polled intake is kept (the intake
	// store's own dial, intake.RetentionFromEnv).
	IntakeRetention time.Duration
	Pass            time.Duration
	// TickTimeout bounds one poller's scan; Concurrency is how many pollers
	// a pass ticks at once.
	TickTimeout time.Duration
	Concurrency int
}

// PolicyFromEnv reads the dials; a value that does not parse is an error,
// never a default — the operator stated a bound this build will not guess at.
func PolicyFromEnv(getenv func(string) string) (Policy, error) {
	p := Policy{SeenRetention: DefaultSeenRetention, Pass: DefaultPassInterval, TickTimeout: DefaultTickTimeout, Concurrency: DefaultConcurrency}
	read := func(name string, positive bool, into *time.Duration) error {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			return nil
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 || (positive && d == 0) {
			want := "a Go duration, 0 or more"
			if positive {
				want = "a positive Go duration"
			}
			return fmt.Errorf("%s=%q: want %s", name, v, want)
		}
		*into = d
		return nil
	}
	for _, r := range []struct {
		name     string
		positive bool
		into     *time.Duration
	}{
		{EnvInterval, true, &p.Interval},
		{EnvMinInterval, true, &p.MinInterval},
		{EnvSeenRetention, false, &p.SeenRetention},
		{EnvPassInterval, true, &p.Pass},
		{EnvTickTimeout, true, &p.TickTimeout},
	} {
		if err := read(r.name, r.positive, r.into); err != nil {
			return Policy{}, err
		}
	}
	if v := strings.TrimSpace(getenv(EnvConcurrency)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Policy{}, fmt.Errorf("%s=%q: want a positive integer", EnvConcurrency, v)
		}
		p.Concurrency = n
	}
	retention, err := intake.RetentionFromEnv(getenv)
	if err != nil {
		return Policy{}, err
	}
	p.IntakeRetention = retention
	return p, nil
}

// Worker ticks the pollers that are due.
type Worker struct {
	Pollers       poller.Store
	Intakes       intake.Store
	Subscriptions trigger.SubscriptionStore
	// Catalog is the tenant's connector catalog, asked under the poller's
	// tenant (the connection's).
	Catalog func(tenantID string) connection.Catalog
	// Resolver builds the tenant's resolver; the worker states its trust
	// (connection.TrustOfTenantConfiguration).
	Resolver func(tenantID string) *connection.Resolver
	// Client is the guarded client the operation goes out on.
	Client *http.Client
	// Owner names this worker on the pollers it takes; unique per process.
	Owner  string
	Policy Policy
	// Batch bounds the pollers one pass ticks; 0 is DefaultBatch.
	Batch  int
	Logger *iterlog.Logger
	// Now is the clock; nil reads the wall clock.
	Now func() time.Time
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) warn(format string, args ...any) {
	if w.Logger != nil {
		w.Logger.Warn(format, args...)
	}
}

// Validate refuses a worker that could not tick.
func (w *Worker) Validate() error {
	switch {
	case w.Pollers == nil || w.Intakes == nil || w.Subscriptions == nil:
		return errors.New("polling: the worker needs its poller, intake and subscription stores")
	case w.Catalog == nil || w.Resolver == nil:
		return errors.New("polling: the worker needs a catalog and a resolver")
	case w.Client == nil:
		return errors.New("polling: the worker needs the guarded client its calls go out on")
	case w.Owner == "":
		return errors.New("polling: the worker needs an owner name")
	}
	return nil
}

// Run passes every Policy.Pass until ctx is done, telling observe each
// pass's verdict; it returns when observe says to hand the term over (a
// lease's Health) — nil observe never does.
func (w *Worker) Run(ctx context.Context, observe func(lease.PassVerdict) bool) error {
	if err := w.Validate(); err != nil {
		return err
	}
	every := w.Policy.Pass
	if every <= 0 {
		every = DefaultPassInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		verdict, err := w.Pass(ctx)
		if err != nil && ctx.Err() == nil {
			w.warn("polling: pass: %v", err)
		}
		if observe != nil && observe(verdict) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Pass ticks every poller due now, Policy.Concurrency at a time, in the
// store's due order. One poller's failure is its own record's (LastError);
// the verdict says whether this replica reached the vendors it called — a
// pass that called nobody is idle — and the error is the store's.
func (w *Worker) Pass(ctx context.Context) (lease.PassVerdict, error) {
	batch := w.Batch
	if batch <= 0 {
		batch = DefaultBatch
	}
	due, err := w.Pollers.ListDue(ctx, w.now(), batch)
	if err != nil {
		var pass lease.Pass
		pass.FailedUnlessDone(ctx, err)
		return pass.Verdict(), fmt.Errorf("polling: list due pollers: %w", err)
	}
	width := w.Policy.Concurrency
	if width <= 0 {
		width = DefaultConcurrency
	}
	var (
		pass lease.Pass
		wg   sync.WaitGroup
		sem  = make(chan struct{}, width)
	)
	for _, p := range due {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(p poller.Poller) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := w.tick(ctx, p, &pass); err != nil {
				w.warn("polling: poller %s: %v", p.ID, err)
			}
		}(p)
	}
	wg.Wait()
	return pass.Verdict(), nil
}

// Interval is a trigger's cadence under the operator's dials: its default,
// or the deployment's interval, never below its floor unless the dials
// lower that.
func Interval(trig spec.Trigger, policy Policy) time.Duration {
	def, floor, err := spec.PollIntervals(trig.Poll)
	if err != nil {
		// Validated at load; a package that changed under the tick is read
		// again at the next one.
		return time.Minute
	}
	if policy.MinInterval > 0 {
		floor = policy.MinInterval
	}
	every := def
	if policy.Interval > 0 {
		every = policy.Interval
	}
	return max(every, floor)
}

// Tick takes the poller and runs one scan. Its error is the stores'; what
// the scan met — a vendor's failure, an unreadable item — is recorded on the
// poller (LastError), and a tick another holder fenced off returns nil.
func (w *Worker) Tick(ctx context.Context, p poller.Poller) error {
	return w.tick(ctx, p, nil)
}

func (w *Worker) tick(ctx context.Context, p poller.Poller, pass *lease.Pass) error {
	now := w.now()
	p, err := w.Pollers.Acquire(ctx, p.ID, p.Generation, w.Owner, now)
	if errors.Is(err, poller.ErrFenced) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	t := &tick{w: w, p: p, now: now, pass: pass}
	return t.run(ctx)
}

// tick is one scan of one poller under the generation it took.
type tick struct {
	w    *Worker
	p    poller.Poller
	now  time.Time
	pass *lease.Pass
	// seqEnd is the last sequence this tick reserved, or the poller's.
	seqEnd int64
	// subs are the subscriptions the tick found listening.
	subs []trigger.Subscription
}

// complete records the tick's outcome; a fence met here means another
// holder took the poller, and what this tick accepted stands.
func (t *tick) complete(ctx context.Context, r poller.Tick) error {
	r.At = t.now
	err := t.w.Pollers.Complete(ctx, t.p.ID, t.p.Generation, r)
	if errors.Is(err, poller.ErrFenced) {
		return nil
	}
	if err == nil && r.Error != t.p.LastError {
		t.report(ctx, r.Error)
	}
	return err
}

// report says a change of the poller's verdict where an operator reads it:
// the server's log, and each listening subscription's last error — the
// field the trigger routes and `iterion remote triggers list` render — so a
// poller that stopped reading is not a subscription that silently stopped
// firing. A recovery clears it.
func (t *tick) report(ctx context.Context, why string) {
	if why != "" {
		t.w.warn("polling: poller %s (%s on connection %s): %s", t.p.ID, t.p.TriggerID, t.p.ConnectionID, why)
	}
	msg := ""
	if why != "" {
		msg = "poller: " + why
	}
	for _, sub := range t.subs {
		if err := t.w.Subscriptions.MarkLaunchError(ctx, sub.ID, msg, t.now); err != nil {
			t.w.warn("polling: poller %s: record its verdict on subscription %s: %v", t.p.ID, sub.ID, err)
		}
	}
}

// refused records a tick whose trigger or subscriptions could not be read.
func (t *tick) refused(ctx context.Context, why string) error {
	return t.complete(ctx, poller.Tick{NextTickAt: t.now.Add(refusedRecheck), Error: why})
}

// observed records, for the lease's health, whether the vendor answered.
func (t *tick) observed(err error) {
	if t.pass == nil {
		return
	}
	if err != nil {
		t.pass.Failed(err)
		return
	}
	t.pass.Succeeded()
}

func (t *tick) run(ctx context.Context) error {
	p := t.p
	t.seqEnd = p.Sequence
	subs, err := t.w.Subscriptions.ListCandidates(ctx, trigger.Event{
		Source: trigger.SourceConnector, TenantID: binding.SubscriptionTenant(p.TenantID),
		Kind: p.TriggerID, ConnectionID: p.ConnectionID, PollerID: p.ID,
	})
	if err != nil {
		_ = t.refused(ctx, fmt.Sprintf("list the subscriptions: %v", err))
		return fmt.Errorf("list subscriptions: %w", err)
	}
	t.subs = subs
	trig, refusal, err := (&binding.Matcher{Catalog: t.w.Catalog}).Trigger(intake.Intake{TenantID: p.TenantID, TriggerID: p.TriggerID, PollerID: p.ID})
	if len(subs) == 0 {
		return t.dormant(ctx, trig, err == nil && refusal == "")
	}
	if err != nil {
		_ = t.refused(ctx, err.Error())
		return err
	}
	if refusal != "" {
		return t.refused(ctx, refusal)
	}
	next := t.now.Add(Interval(trig, t.w.Policy))

	scanCtx, cancel := context.WithTimeout(ctx, t.w.tickTimeout())
	scan, err := t.scan(scanCtx, trig)
	cancel()
	// The bookkeeping after the scan runs on its own bound: a scan that
	// spent its deadline still records what it accepted and why it stopped.
	ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	if err != nil {
		return t.complete(ctx, poller.Tick{NextTickAt: next, Error: err.Error()})
	}
	accepted, stop := t.accept(ctx, trig, scan)
	if stop != nil {
		if errors.Is(stop, poller.ErrFenced) {
			return nil
		}
		return t.complete(ctx, poller.Tick{NextTickAt: next, Error: stop.Error()})
	}
	if err := t.w.Pollers.MarkSeen(ctx, p.TenantID, p.ID, accepted.keys, t.now); err != nil {
		// The intakes stand; the next scan meets them as already accepted.
		return t.complete(ctx, poller.Tick{NextTickAt: next, Error: fmt.Sprintf("record the seen set: %v", err)})
	}
	t.prune(ctx)
	r := poller.Tick{NextTickAt: next, Error: strings.Join(accepted.problems, "; ")}
	if !scan.complete {
		// An incomplete scan keeps what it accepted and never primes; it
		// moves the checkpoint only as far as its operation's order lets it
		// (partial), so the next scan reads on from there or from the old
		// place.
		r.Error = joinProblems(scan.why, r.Error)
		r.Checkpoint = t.partial(trig, scan)
		return t.complete(ctx, r)
	}
	r.Checkpoint = t.checkpoint(trig, scan)
	if !p.Primed {
		r.Primed, r.PrimedThrough = true, t.seqEnd
	}
	if err := t.complete(ctx, r); err != nil {
		return err
	}
	t.settle(ctx, subs, max(p.PrimedThrough, r.PrimedThrough))
	return nil
}

func (w *Worker) tickTimeout() time.Duration {
	if w.Policy.TickTimeout > 0 {
		return w.Policy.TickTimeout
	}
	return DefaultTickTimeout
}

// prune drops what the poller last observed past the seen retention.
func (t *tick) prune(ctx context.Context) {
	if t.w.Policy.SeenRetention <= 0 {
		return
	}
	if _, err := t.w.Pollers.PruneSeen(ctx, t.p.ID, t.now.Add(-t.w.Policy.SeenRetention)); err != nil {
		t.w.warn("polling: poller %s: prune the seen set: %v", t.p.ID, err)
	}
}

// dormant records a tick that found no subscription: nothing is read, and
// the poller is unprimed — the scan a returning subscription triggers
// primes what changed meanwhile instead of launching it. A timestamp
// checkpoint moves to now, where nobody listened up to, so that scan reads
// only what changed since; the seen set keeps what was accepted, past its
// retention. The poller sleeps until a subscription wakes it, or a day.
func (t *tick) dormant(ctx context.Context, trig spec.Trigger, readable bool) error {
	r := poller.Tick{NextTickAt: t.now.Add(dormantRecheck), Idle: true}
	if readable && trig.Poll != nil && trig.Poll.Cursor != nil && trig.Poll.Cursor.Kind == spec.CursorTimestamp {
		at := t.now
		if c := t.p.Checkpoint; c != nil && c.Kind == poller.KindTimestamp && c.At != nil && c.At.After(at) {
			at = *c.At
		}
		r.Checkpoint = &poller.Checkpoint{Kind: poller.KindTimestamp, At: &at}
	}
	t.prune(ctx)
	return t.complete(ctx, r)
}

func joinProblems(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

// item is one observed item with its identity and cursor.
type item struct {
	obj   map[string]any
	key   poller.ItemKey
	at    time.Time
	token string
}

// scanned is what one call of the operation observed.
type scanned struct {
	items    []item
	complete bool
	// why says why a scan is incomplete.
	why string
	// problems are the items it could not identify.
	problems []string
}

// scan calls the operation and identifies every item it answered.
func (t *tick) scan(ctx context.Context, trig spec.Trigger) (scanned, error) {
	p := t.p
	r := t.w.Resolver(p.TenantID)
	if r == nil {
		return scanned{}, errors.New("no resolver for the poller's tenant")
	}
	r.Trust = connection.TrustOfTenantConfiguration()
	// What a poller reads is believed on the connection's word, as a
	// delivery an ingress takes is: the trigger grant the save checked is
	// checked again, so a grant the operator withdrew feeds nothing more.
	if conn, err := r.Store.Get(ctx, p.TenantID, p.ConnectionID); err == nil && !conn.Has(connection.CapTrigger) {
		return scanned{}, fmt.Errorf("connection %q no longer holds the trigger grant: nothing it feeds is believed", conn.Alias)
	}
	pkg, op, cred, baseURL, err := r.ResolveActionByConnection(ctx, trig.Poll.Operation, p.ConnectionID)
	if err != nil {
		return scanned{}, err
	}
	query, err := binding.PollQueryValues(trig, p.Query)
	if err != nil {
		return scanned{}, err
	}
	params, err := spec.PollParams(trig.Poll, query, t.paramsCheckpoint(trig))
	if err != nil {
		return scanned{}, err
	}
	ex := &exec.Executor{Client: t.w.Client, BaseURL: baseURL, UserAgent: userAgent, PreciseNumbers: true}
	raw, complete, last, callErr := ex.CallPaged(ctx, pkg, op, params, cred)
	switch {
	case callErr != nil:
		t.observed(callErr)
	case last.Err != nil:
		// A transport failure carries its Go cause — a refused dial, a DNS
		// failure — which is what tells the lease a peer may reach the
		// vendor where this replica does not; a vendor's answer has none.
		var xe *exec.Error
		if errors.As(last.Err, &xe) && xe.Cause != nil {
			t.observed(xe.Cause)
		} else {
			t.observed(last.Err)
		}
	default:
		t.observed(nil)
	}
	out := scanned{complete: complete && callErr == nil && last.OK()}
	switch {
	case callErr != nil:
		out.why = fmt.Sprintf("the scan stopped: %v", callErr)
	case last.Err != nil:
		out.why = fmt.Sprintf("the scan stopped: %v", last.Err)
	case !complete:
		out.why = "the scan stopped before the last page (the operation's page bound)"
	}
	var response any
	if op.Pagination == nil {
		response = last.Data
		if out.complete {
			raw, err = spec.PollItems(op, trig.Poll, response)
			if err != nil {
				return scanned{}, err
			}
		}
	}
	seen := map[poller.ItemKey]bool{}
	for i, v := range raw {
		obj, ok := v.(map[string]any)
		if !ok {
			out.problems = append(out.problems, fmt.Sprintf("item %d is %T, not an object", i, v))
			continue
		}
		id, version, err := spec.PollItemIdentity(trig.Poll, response, obj)
		if err != nil {
			out.problems = append(out.problems, fmt.Sprintf("item %d: %v", i, err))
			continue
		}
		at, token, err := spec.PollItemCursor(trig.Poll, response, obj)
		if err != nil {
			out.problems = append(out.problems, fmt.Sprintf("item %s: %v", id, err))
			continue
		}
		key := poller.ItemKey{ID: id, Version: version}
		if seen[key] {
			// A page boundary that moved between two requests lists an item
			// twice: one observation.
			continue
		}
		seen[key] = true
		out.items = append(out.items, item{obj: obj, key: key, at: at, token: token})
	}
	return out, nil
}

// paramsCheckpoint is the checkpoint a tick's params read: the stored one,
// a timestamp moved back by the cursor's overlap.
func (t *tick) paramsCheckpoint(trig spec.Trigger) *spec.PollCheckpoint {
	c := t.p.Checkpoint
	if c == nil || trig.Poll.Cursor == nil || c.Kind != trig.Poll.Cursor.Kind {
		return nil
	}
	if c.Kind == poller.KindTimestamp && c.At != nil {
		return &spec.PollCheckpoint{Kind: spec.CursorTimestamp, At: c.At.Add(-spec.PollOverlap(trig.Poll))}
	}
	return &spec.PollCheckpoint{Kind: spec.CursorOpaque, Token: c.Token}
}

// checkpoint is the position a complete scan reached: the latest instant it
// observed, under instant's bounds, or the last item's token; the stored one
// when the scan saw nothing to move it.
func (t *tick) checkpoint(trig spec.Trigger, s scanned) *poller.Checkpoint {
	if trig.Poll.Cursor == nil || len(s.items) == 0 {
		return nil
	}
	if trig.Poll.Cursor.Kind == spec.CursorTimestamp {
		return t.instant(latestAt(s.items, time.Time{}))
	}
	return &poller.Checkpoint{Kind: poller.KindOpaque, Token: s.items[len(s.items)-1].token}
}

// partial is the position a scan that stopped before its last page reached
// (a page bound, a later page failing, its deadline). Only an operation that
// answers oldest first (poll.order) has one, and only for a timestamp
// cursor: the pages read are then the oldest of the answer, so every instant
// before the last one read is read in full, while the last one's other items
// may sit on the page never read. The latest of those complete instants,
// under instant's bounds, is where the next scan reads on from — whatever
// its overlap, and whether its param reads the cursor inclusively or not.
// When every item read shares one instant, or the answer is in no declared
// order (an unread page may hold an item older than every one read), nothing
// moves.
func (t *tick) partial(trig spec.Trigger, s scanned) *poller.Checkpoint {
	if trig.Poll.Order != spec.PollOrderOldestFirst || trig.Poll.Cursor == nil || trig.Poll.Cursor.Kind != spec.CursorTimestamp {
		return nil
	}
	full := latestAt(s.items, latestAt(s.items, time.Time{}))
	if full.IsZero() {
		return nil
	}
	return t.instant(full)
}

// latestAt is the latest item instant strictly before below, or of all the
// items when below is zero; zero when no item has one.
func latestAt(items []item, below time.Time) time.Time {
	var latest time.Time
	for _, it := range items {
		if !below.IsZero() && !it.at.Before(below) {
			continue
		}
		if it.at.After(latest) {
			latest = it.at
		}
	}
	return latest
}

// instant is a timestamp checkpoint at at, bounded: never behind the stored
// one, since the overlap re-reads items before it, and never past the tick's
// own clock, so one item a vendor stamped in the future does not blind every
// later scan. nil when there is no instant to stand on.
func (t *tick) instant(at time.Time) *poller.Checkpoint {
	if at.After(t.now) {
		at = t.now
	}
	if c := t.p.Checkpoint; c != nil && c.Kind == poller.KindTimestamp && c.At != nil && c.At.After(at) {
		at = *c.At
	}
	if at.IsZero() {
		return nil
	}
	return &poller.Checkpoint{Kind: poller.KindTimestamp, At: &at}
}

// acceptance is what accept wrote.
type acceptance struct {
	// keys are every observed key: the ones accepted now, the ones already
	// seen, and the ones the trigger's loop guard or an unreadable item
	// dropped — each observed, so the seen set records it.
	keys         []poller.ItemKey
	problems     []string
	lastSequence int64
}

// accept writes an intake for every item the seen set does not hold, each
// numbered by the poller's sequence: primed while the poller has not
// completed its first scan. It stops at the first store failure: what it
// accepted before stands, the rest is read again by the next scan.
func (t *tick) accept(ctx context.Context, trig spec.Trigger, s scanned) (acceptance, error) {
	p := t.p
	out := acceptance{problems: s.problems}
	keys := make([]poller.ItemKey, 0, len(s.items))
	for _, it := range s.items {
		keys = append(keys, it.key)
	}
	seen, err := t.w.Pollers.Seen(ctx, p.ID, keys)
	if err != nil {
		return out, fmt.Errorf("read the seen set: %w", err)
	}
	var fresh []item
	for _, it := range s.items {
		if seen[it.key] {
			out.keys = append(out.keys, it.key)
			continue
		}
		fresh = append(fresh, it)
	}
	if len(fresh) == 0 {
		return out, nil
	}
	first, err := t.w.Pollers.Reserve(ctx, p.ID, p.Generation, len(fresh))
	if err != nil {
		return out, err
	}
	t.seqEnd = first + int64(len(fresh)) - 1
	for i, it := range fresh {
		seq := first + int64(i)
		event, filtered, err := ingress.ReadItem(trig, it.obj)
		switch {
		case err != nil:
			// Recorded and remembered: an item the package cannot read
			// would otherwise stop the poller on every tick.
			out.problems = append(out.problems, fmt.Sprintf("item %s: %v", it.key.ID, err))
			out.keys = append(out.keys, it.key)
			continue
		case filtered != "":
			out.keys = append(out.keys, it.key)
			continue
		}
		in := intake.Intake{
			TenantID: p.TenantID, ConnectionID: p.ConnectionID, PollerID: p.ID, TriggerID: trig.ID,
			DedupKey: DedupKey(it.key), Event: event, ReceivedAt: t.now,
			ExpiresAt: intake.ExpiryFor(t.now, t.w.Policy.IntakeRetention), Sequence: seq,
		}
		if !p.Primed {
			in.State = intake.IntakePrimed
		}
		if _, _, err := t.w.Intakes.Accept(ctx, in); err != nil {
			return out, fmt.Errorf("accept item %s: %w", it.key.ID, err)
		}
		out.keys = append(out.keys, it.key)
		out.lastSequence = seq
	}
	return out, nil
}

// DedupKey is a polled item's intake identity: its id and version,
// length-prefixed so no two keys share one.
func DedupKey(k poller.ItemKey) string {
	return fmt.Sprintf("%d:%s|%d:%s", len(k.ID), k.ID, len(k.Version), k.Version)
}

// settle owes each subscription activated since it was last settled
// (BackfillDone unset) two things, then records it settled:
//
//   - the catch-up: the intakes its poller accepted above its PollerSince
//     that it holds no pair for — an item accepted between the save that
//     read the poller's sequence and the subscription landing is matched
//     without it, and is owed to it all the same; primed intakes are owed
//     to nobody;
//   - its backfill: the last N intakes at or below the sequence its
//     activation and the priming reached (ADR-168 §7), primed ones
//     included, for it alone.
//
// Each is judged as the matcher judges any intake. Pairs are written once,
// so a repeat — a failure retried at the next tick, the matcher writing the
// same pair — adds nothing.
func (t *tick) settle(ctx context.Context, subs []trigger.Subscription, primedThrough int64) {
	m := &binding.Matcher{Subscriptions: t.w.Subscriptions, Catalog: t.w.Catalog}
	for _, sub := range subs {
		if sub.BackfillDone {
			continue
		}
		var owed []intake.Intake
		if gap := t.seqEnd - sub.PollerSince; gap > 0 {
			rows, err := t.w.Intakes.ListPollerIntakes(ctx, t.p.TenantID, t.p.ID, t.seqEnd, int(gap))
			if err != nil {
				t.w.warn("polling: settle %s: list the poller's intakes: %v", sub.ID, err)
				continue
			}
			for _, in := range rows {
				if in.Sequence > sub.PollerSince && in.State != intake.IntakePrimed {
					owed = append(owed, in)
				}
			}
		}
		if sub.Backfill > 0 {
			rows, err := t.w.Intakes.ListPollerIntakes(ctx, t.p.TenantID, t.p.ID, max(sub.PollerSince, primedThrough), sub.Backfill)
			if err != nil {
				t.w.warn("polling: settle %s: list the poller's intakes: %v", sub.ID, err)
				continue
			}
			owed = append(owed, rows...)
		}
		ok := true
		for _, in := range owed {
			v, _, err := m.Rejudge(ctx, in, sub)
			if err != nil {
				t.w.warn("polling: settle %s: judge intake %s: %v", sub.ID, in.ID, err)
				ok = false
				break
			}
			pairs := intake.PairsOf(in, []intake.Verdict{{SubscriptionID: sub.ID, Matched: v.Matched, Reason: v.Reason, Refusal: v.Refusal}}, t.now)
			if err := t.w.Intakes.WritePairs(ctx, pairs); err != nil {
				t.w.warn("polling: settle %s: write the pair of intake %s: %v", sub.ID, in.ID, err)
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		cur, err := t.w.Subscriptions.Get(ctx, sub.ID)
		if err != nil {
			t.w.warn("polling: settle %s: read the subscription: %v", sub.ID, err)
			continue
		}
		if cur.Poller != sub.Poller || cur.PollerSince != sub.PollerSince || cur.BackfillDone {
			// Re-bound or re-activated since: its next activation is settled
			// on its own.
			continue
		}
		cur.BackfillDone = true
		if err := t.w.Subscriptions.Update(ctx, cur); err != nil {
			t.w.warn("polling: settle %s: record it settled: %v", sub.ID, err)
		}
	}
}
