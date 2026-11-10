package credpool

// Round-4 adversarial guarantee tests: the REAL broker under randomized,
// seeded sequences of side-by-side resolutions, releases, takeovers,
// rollbacks, interim/final fine/coarse reports, redeliveries, late reports,
// conditions, cooldowns and clock jumps — checked against invariants.

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

const r4Bot = "docs-refresh"

var r4Kinds = []Credential{
	{Source: SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)},
	{Source: SourceAPIKey, Ref: "anthropic"},
	{Source: SourceAPIKey, Ref: string(secrets.ProviderOpenAICompatible)},
}

type r4Pledge struct {
	id   string
	kind int
	user string
}

type r4Attempt struct {
	n          int
	run        string
	id         string
	marker     time.Time
	pub        time.Time
	published  bool
	grantless  bool
	rolledBack bool
	sealed     []*Grant
	released   []*Grant
	finalSent  bool
	final      Outcome
	interims   int
}

type r4World struct {
	t         *testing.T
	h         *harness
	rng       *rand.Rand
	seed      int64
	attID     string
	draw      DrawLimits
	resetHold bool
	pledges   []r4Pledge
	near      map[string]bool
	markers   map[string]*time.Time
	current   map[string]*r4Attempt
	pending   map[string]*r4Attempt
	attempts  []*r4Attempt
	nAttempt  int
	runs      []string
	trace     []string
	viol      []string
	stats     map[string]int
	// hostile: allow interim reports while a resume of the run is pending.
	hostileInterim bool
	// realistic: a publication happens a few ms after its resolution, and
	// nothing acquires inside a publication's millisecond.
	realistic bool
}

func r4Setup(t *testing.T, seed int64) *r4World {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(seed))
	w := &r4World{t: t, h: newHarnessWithAttachments(t), rng: rng, seed: seed,
		near: map[string]bool{}, markers: map[string]*time.Time{}, current: map[string]*r4Attempt{},
		pending: map[string]*r4Attempt{}, stats: map[string]int{}}
	switch rng.Intn(4) {
	case 0:
		w.h.now = time.Date(2026, 8, 2, 23, 59, 59, 998_000_000, time.UTC) // Sunday, last ms of an ISO week
	case 1:
		w.h.now = time.Date(2026, 8, 3, 23, 59, 59, 999_000_000, time.UTC)
	}
	randLimits := func() Limits {
		pick := func(xs ...float64) float64 { return xs[rng.Intn(len(xs))] }
		return Limits{
			MaxRunsPerDay:     []int{0, 0, 1, 2, 3}[rng.Intn(5)],
			MaxUSDPerDay:      pick(0, 0, 1.5, 3, 6),
			MaxUSDPerWeek:     pick(0, 0, 0, 8),
			MaxConcurrentRuns: []int{0, 0, 1, 2}[rng.Intn(4)],
		}
	}
	counts := []int{2 + rng.Intn(2), 1 + rng.Intn(3), 1 + rng.Intn(2)}
	for kind, n := range counts {
		for i := 0; i < n; i++ {
			user := fmt.Sprintf("d%d%c", kind, 'a'+i)
			prio := rng.Intn(3)
			lim := randLimits()
			var p Pledge
			switch kind {
			case 0:
				p = w.h.donor(t, user, lim, func(p *Pledge) { p.Priority = prio })
				rec, err := w.h.oauth.Get(ctx, user, secrets.OAuthKindClaudeCode)
				if err != nil {
					t.Fatal(err)
				}
				rec.Fingerprint = "fp-oauth-" + user
				if err := w.h.oauth.Upsert(ctx, rec); err != nil {
					t.Fatal(err)
				}
			case 2:
				p = w.h.r4GatewayDonor(t, user, lim, func(p *Pledge) { p.Priority = prio })
			default:
				p, _ = w.h.donorKey(t, user, r4Kinds[kind].Ref, lim, func(p *Pledge) { p.Priority = prio })
			}
			w.pledges = append(w.pledges, r4Pledge{id: p.ID, kind: kind, user: user})
		}
	}
	w.resetHold = rng.Intn(4) != 0
	w.draw = DrawLimits{MaxRunsPerDay: []int{0, 0, 2, 4}[rng.Intn(4)], MaxUSDPerDay: []float64{0, 0, 5, 12}[rng.Intn(4)]}
	_, w.attID, _ = attachForTest(t, w.h, Strategy{Mode: StrategyPriority, ResetHold: w.resetHold}, w.draw)
	nRuns := 2 + rng.Intn(3)
	for i := 0; i < nRuns; i++ {
		w.runs = append(w.runs, fmt.Sprintf("run%d", i))
	}
	return w
}

func (w *r4World) logf(format string, args ...any) {
	w.trace = append(w.trace, fmt.Sprintf("[%s] ", w.h.now.Format("01-02T15:04:05.000"))+fmt.Sprintf(format, args...))
}

func (w *r4World) violate(inv, format string, args ...any) {
	w.viol = append(w.viol, inv+": "+fmt.Sprintf(format, args...))
	w.stats["VIOLATION "+inv]++
}

func (w *r4World) margin(p Pledge) (time.Time, bool) {
	if w.near[p.ID] {
		return w.h.now.Add(time.Hour), true
	}
	return time.Time{}, false
}

func (w *r4World) req(run string, kind int, attemptID string, side bool, prefer string) Request {
	return Request{RunID: run, OrgID: testOrg, TenantID: "team-1", UserID: "requester", BotID: r4Bot,
		Wants: []Credential{r4Kinds[kind]}, PoolIDs: []string{"pool-1"}, AttachmentID: w.attID, DrawLimits: w.draw,
		RankMode: StrategyPriority, ClosedAtMargin: w.margin, HoldOnSwitch: w.resetHold, PreferPledgeID: prefer,
		SideBySide: side, AttemptID: attemptID}
}

// --- snapshots ---------------------------------------------------------

func (w *r4World) snapLeases() map[string]Lease {
	s := w.h.leases
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Lease, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	return out
}

func (w *r4World) snapLedger() map[string]memBucket {
	l := w.h.ledger
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]memBucket, len(l.m))
	for k, b := range l.m {
		out[k] = *b
	}
	return out
}

func (w *r4World) snapPledges() map[string]Pledge {
	out := map[string]Pledge{}
	for _, rp := range w.pledges {
		p, err := w.h.pledges.Get(context.Background(), rp.id)
		if err != nil {
			w.t.Fatal(err)
		}
		out[rp.id] = p
	}
	return out
}

func r4TimePtrEq(x, y *time.Time) bool {
	if x == nil || y == nil {
		return x == nil && y == nil
	}
	return x.Equal(*y)
}

func r4LeaseEq(a, b Lease) bool {
	return a.ID == b.ID && a.Closed == b.Closed && a.Outcome == b.Outcome && a.CostUSD == b.CostUSD &&
		a.Held == b.Held && r4TimePtrEq(a.ClosedAt, b.ClosedAt) &&
		r4TimePtrEq(a.SupersededByPublishedAt, b.SupersededByPublishedAt) &&
		a.AcquiredAt.Equal(b.AcquiredAt) && a.ExpiresAt.Equal(b.ExpiresAt) && a.AttemptID == b.AttemptID &&
		a.AttachmentID == b.AttachmentID && a.GrantedCostUSD == b.GrantedCostUSD
}

func r4LeaseStr(l Lease) string {
	return fmt.Sprintf("%.8s{att=%s pledge=%s acq=%s closed=%v out=%q cost=%.2f held=%v}", l.ID, l.AttemptID, l.PledgeID,
		l.AcquiredAt.Format("15:04:05.000"), l.Closed, l.Outcome, l.CostUSD, l.Held)
}

// --- hold oracle (I5) ----------------------------------------------------

type r4Cand struct {
	id        string
	prio      int
	eligible  bool
	status    Status
	exhausted string // "" when not really exhausted, else the reason
	busy      bool
	spentDay  float64
	committed float64
	liveRuns  int
}

// r4Readmits is HasServedAttempt as the acquisition will read it: a
// non-side-by-side acquisition supersedes the run's open leases FIRST, and
// a superseded lease is a non-admission.
func (w *r4World) r4Readmits(run, pledgeID string, side bool) bool {
	all, _ := w.h.leases.ListByRun(context.Background(), run)
	for _, l := range all {
		if l.PledgeID != pledgeID {
			continue
		}
		if !l.Closed {
			if side {
				return true
			}
			continue
		}
		if !isNonAdmission(l.Outcome) {
			return true
		}
	}
	return false
}

func (w *r4World) holdSnapshot(run string, kind int, side bool) []r4Cand {
	ctx := context.Background()
	// The ledger judges on the acquisition's own instant: the run's floor
	// (nextAcquiredAt over its leases), which can cross midnight.
	at := w.h.now
	prev, err := w.h.leases.ListByRun(ctx, run)
	if err != nil {
		w.t.Fatal(err)
	}
	for i := range prev {
		at = nextAcquiredAt(at, &prev[i].AcquiredAt)
	}
	var out []r4Cand
	for _, rp := range w.pledges {
		if rp.kind != kind {
			continue
		}
		p, err := w.h.pledges.Get(ctx, rp.id)
		if err != nil {
			w.t.Fatal(err)
		}
		ok, st := p.AvailableForLaunch(w.h.now, r4Bot)
		if ok && w.near[p.ID] {
			ok, st = false, StatusNearLimit
		}
		day, week, err := w.h.ledger.Usage(ctx, p.ID, at)
		if err != nil {
			w.t.Fatal(err)
		}
		readmit := w.r4Readmits(run, p.ID, side)
		runs, committed, _ := w.h.leases.LiveCommitment(ctx, p.ID, run, w.h.now)
		c := r4Cand{id: p.ID, prio: p.Priority, eligible: ok, status: st, spentDay: day.CostUSD, committed: committed, liveRuns: runs}
		if !ok && switchStatus(st) {
			c.exhausted = string(st)
		}
		if ok {
			switch {
			case !readmit && p.Limits.MaxRunsPerDay > 0 && day.Runs >= p.Limits.MaxRunsPerDay:
				c.exhausted = "runs_per_day"
			case p.Limits.MaxUSDPerDay > 0 && day.CostUSD >= p.Limits.MaxUSDPerDay:
				c.exhausted = "usd_day_spent"
			case p.Limits.MaxUSDPerWeek > 0 && week.CostUSD >= p.Limits.MaxUSDPerWeek:
				c.exhausted = "usd_week_spent"
			}
			c.busy = p.Limits.MaxConcurrentRuns > 0 && runs >= p.Limits.MaxConcurrentRuns
		}
		out = append(out, c)
	}
	return out
}

func (w *r4World) checkHold(run string, kind int, prefer string, snap []r4Cand, g *Grant) {
	prio := -1
	for _, c := range snap {
		if c.id == g.PledgeID {
			prio = c.prio
		}
	}
	desc := func() string {
		var b strings.Builder
		for _, c := range snap {
			fmt.Fprintf(&b, " [%s prio=%d elig=%v st=%s exh=%q busy=%v spent=%.2f committed=%.2f live=%d]",
				c.id, c.prio, c.eligible, c.status, c.exhausted, c.busy, c.spentDay, c.committed, c.liveRuns)
		}
		return b.String()
	}
	for _, c := range snap {
		if c.id == g.PledgeID && !c.eligible {
			w.violate("I5-elig", "run %s kind %d: granted %s which the eligibility filter refuses (%s), prefer=%q", run, kind, g.PledgeID, c.status, prefer)
		}
		if c.id == g.PledgeID && c.id == prefer {
			w.stats["grant on the preferred (held) entry, eligible"]++
		}
	}
	if g.Held {
		if g.PledgeID == prefer {
			w.stats["hold: re-serves the held entry"]++
			return
		}
		for _, c := range snap {
			if c.id != g.PledgeID && c.prio <= prio && c.exhausted != "" {
				w.stats["hold: justified by "+c.exhausted]++
				return
			}
		}
		w.violate("I5", "run %s kind %d: lease on %s HELD with no candidate at least as well ranked really exhausted (prefer=%q):%s", run, kind, g.PledgeID, prefer, desc())
		return
	}
	if !w.resetHold || g.PledgeID == prefer {
		return
	}
	for _, c := range snap {
		if c.id == g.PledgeID || c.exhausted == "" || (c.id == prefer && c.eligible) {
			continue
		}
		if (!c.eligible && c.prio <= prio) || (c.eligible && c.prio < prio) {
			why := "other"
			if c.busy {
				why = "busy+exhausted"
			}
			w.stats["MISSED hold ("+why+", "+c.exhausted+")"]++
			w.logf("MISSED-HOLD run %s kind %d served by %s (prio %d) not held; %s prio %d exhausted=%s busy=%v:%s", run, kind, g.PledgeID, prio, c.id, c.prio, c.exhausted, c.busy, desc())
			return
		}
	}
}

// --- operations -----------------------------------------------------------

func (w *r4World) resolve(run string) {
	ctx := context.Background()
	w.nAttempt++
	a := &r4Attempt{n: w.nAttempt, run: run, id: fmt.Sprintf("%s#A%d", run, w.nAttempt)}
	a.marker = store.NextQueuedAt(w.h.now, w.markers[run])
	var kinds []int
	for len(kinds) == 0 {
		for k := range r4Kinds {
			if w.rng.Intn(3) != 0 {
				kinds = append(kinds, k)
			}
		}
	}
	side := false
	var got []string
	for _, k := range kinds {
		prefer := ""
		if w.resetHold {
			held, err := w.h.broker.HeldPledge(ctx, run, r4Kinds[k])
			if err != nil {
				w.t.Fatal(err)
			}
			prefer = held
		}
		snap := w.holdSnapshot(run, k, side)
		reqSide := side
		g, err := w.h.broker.Acquire(ctx, w.req(run, k, a.id, side, prefer))
		if g != nil {
			side = true
		}
		if err != nil {
			if !errors.Is(err, ErrNoDonor) {
				w.violate("ACQ", "acquire run %s kind %d: unexpected error %v", run, k, err)
			}
			w.stats[fmt.Sprintf("acquire kind %d: no donor", k)]++
			got = append(got, fmt.Sprintf("k%d:none", k))
			continue
		}
		w.checkHold(run, k, prefer, snap, g)
		w.stats[fmt.Sprintf("acquire kind %d: granted (side=%v)", k, reqSide)]++
		// The walk releases only the gateway grant (a pair without its URL).
		if k == 2 && w.rng.Intn(4) == 0 {
			w.h.broker.ReleaseGrant(ctx, g)
			a.released = append(a.released, g)
			got = append(got, fmt.Sprintf("k%d:%s(released)", k, g.PledgeID))
			w.stats["walk released the gateway grant"]++
		} else {
			a.sealed = append(a.sealed, g)
			got = append(got, fmt.Sprintf("k%d:%s(held=%v acq=%s)", k, g.PledgeID, g.Held, g.AcquiredAt.Format("05.000")))
		}
		if w.rng.Intn(2) == 0 {
			w.h.now = w.h.now.Add(time.Millisecond)
		}
	}
	w.pending[run] = a
	w.attempts = append(w.attempts, a)
	w.logf("RESOLVE %s %s: %s", run, a.id, strings.Join(got, " "))
}

func (w *r4World) publish(run string) {
	ctx := context.Background()
	a := w.pending[run]
	delete(w.pending, run)
	if w.rng.Intn(8) == 0 {
		// Publish refused, flip reverted: the deferred releases (grants[1:]
		// first, then grants[0]) and then the rollback's reopen.
		for i := len(a.sealed) - 1; i >= 0; i-- {
			w.h.broker.ReleaseGrant(ctx, a.sealed[i])
		}
		if len(a.sealed) > 0 {
			w.h.broker.ReopenSuperseded(ctx, a.sealed[0])
		}
		for _, g := range a.released {
			w.h.broker.ReopenSuperseded(ctx, g)
		}
		for i := 1; i < len(a.sealed); i++ {
			w.h.broker.ReopenSuperseded(ctx, a.sealed[i])
		}
		a.rolledBack = true
		w.stats["resume rolled back"]++
		w.logf("ROLLBACK %s %s", run, a.id)
		return
	}
	if w.realistic {
		w.h.now = w.h.now.Add(3 * time.Millisecond)
	}
	pub := store.PublishAt(w.h.now, a.marker)
	if len(a.sealed) > 0 {
		pub = store.PublishAt(pub, a.sealed[0].AcquiredAt)
	}
	a.pub, a.published = pub, true
	m := a.marker
	w.markers[run] = &m
	if len(a.sealed) == 0 {
		a.grantless = true
		if err := w.h.broker.SupersedeRun(ctx, run, "team-1", pub); err != nil {
			w.violate("PUB", "supersede run %s: %v", run, err)
		}
		w.stats["grantless publication"]++
	}
	w.current[run] = a
	if w.realistic && pub.After(w.h.now) {
		w.h.now = pub.Add(time.Millisecond)
	}
	w.logf("PUBLISH %s %s pub=%s grantless=%v", run, a.id, pub.Format("05.000000"), a.grantless)
}

func (w *r4World) outcome(a *r4Attempt, final bool) Outcome {
	rng := w.rng
	total, tin := 0.0, int64(0)
	table := []FingerprintSpend{}
	for _, g := range a.sealed {
		c := float64(rng.Intn(9)) * 0.25
		in := int64(rng.Intn(3)) * 100
		total += c
		tin += in
		table = append(table, FingerprintSpend{Fingerprint: g.Fingerprint, CostUSD: c, InputTokens: in})
	}
	sort.Slice(table, func(i, j int) bool { return table[i].Fingerprint < table[j].Fingerprint })
	out := Outcome{CostUSD: total, InputTokens: tin, Interim: !final}
	switch r := rng.Intn(10); {
	case r < 6:
	case r < 8:
		out.Condition = ConditionUsageWindow
		if rng.Intn(2) == 0 {
			out.CooldownUntil = w.h.now.Add(30 * time.Minute)
		}
	default:
		out.Condition = ConditionAuthFailed
	}
	if rng.Intn(2) == 0 {
		out.ByFingerprint = table
	}
	return out
}

func r4Ms(t time.Time) time.Time { return t.Truncate(time.Millisecond) }

func (w *r4World) report(a *r4Attempt, out Outcome, label string) {
	ctx := context.Background()
	bl, bg, bp := w.snapLeases(), w.snapLedger(), w.snapPledges()
	// The runner reports with the attempt id its message carries (a
	// grantless publication carries none). In realistic timing a share of
	// the reports pins by the publication instant instead, as a message
	// older than the field does — the frozen clock's collisions are what
	// the id exists for.
	attemptID := ""
	if !a.grantless && (!w.realistic || w.rng.Intn(3) != 0) {
		attemptID = a.id
	}
	err := w.h.broker.ReportAttemptID(ctx, a.run, attemptID, a.pub, out)
	if err != nil {
		w.violate("REPORT", "report %s %s: %v", a.id, label, err)
	}
	al, ag, ap := w.snapLeases(), w.snapLedger(), w.snapPledges()
	fine := out.ByFingerprint != nil
	w.logf("REPORT %s %s %s cost=%.2f cond=%q fine=%v", a.run, a.id, label, out.CostUSD, out.Condition, fine)

	slice := func(fp string) (float64, int64) {
		for _, fs := range out.ByFingerprint {
			if fs.Fingerprint == fp {
				return fs.CostUSD, fs.InputTokens
			}
		}
		return 0, 0
	}
	expLedger := map[string]int64{}
	attExp := int64(0)
	anySettleable := false
	condPledges := map[string]bool{}
	for id, l := range bl {
		if l.AttemptID != a.id {
			// I1/I4: nothing outside the attempt moves.
			if nl, ok := al[id]; !ok || !r4LeaseEq(l, nl) {
				inv := "I1/I4"
				if r4Ms(l.AcquiredAt).Before(r4Ms(a.pub)) {
					// The foreign lease was acquired BEFORE the reporting
					// attempt's publication instant (a publication pushed
					// into the future by its marker/lease floor, or skew).
					inv = "I4-timing"
				}
				w.violate(inv, "report of %s (%s, pub=%s) changed lease %s of attempt %s: %s -> %s", a.id, label, a.pub.Format("05.000000"), id, l.AttemptID, r4LeaseStr(l), r4LeaseStr(al[id]))
			}
			continue
		}
		takeoverOwnReport := l.SupersededByPublishedAt != nil && r4Ms(*l.SupersededByPublishedAt).Equal(r4Ms(a.pub))
		settleable := !l.Closed || l.Outcome == OutcomeSuperseded ||
			(l.Outcome == OutcomeSupersededGrantless && !takeoverOwnReport)
		nl := al[id]
		if !settleable {
			if !r4LeaseEq(l, nl) {
				w.violate("I3", "report of %s (%s) moved an already-settled lease: %s -> %s", a.id, label, r4LeaseStr(l), r4LeaseStr(nl))
			}
			continue
		}
		anySettleable = true
		condPledges[l.PledgeID] = true
		charge, tok := out.CostUSD, out.InputTokens
		if fine && l.Fingerprint != "" {
			charge, tok = slice(l.Fingerprint)
		}
		if l.Closed && (l.Outcome == OutcomeSuperseded || l.Outcome == OutcomeSupersededGrantless) {
			w.stats["report settles a superseded lease"]++
		}
		if returnedUnused(l) {
			w.violate("ORACLE", "settleable not_launched?")
		}
		// lease state
		wantClosed := l.Closed || !out.Interim
		if nl.Closed != wantClosed {
			w.violate("I1", "lease %s closed=%v after %s report, want %v", r4LeaseStr(nl), nl.Closed, label, wantClosed)
		}
		// A final report stamps a superseded lease; an interim one charges
		// it and leaves the stamp — and the supersede undoable.
		if l.Closed && !out.Interim && nl.Outcome != OutcomeSupersededReported {
			w.violate("I2", "superseded lease %s not stamped by its attempt's report", r4LeaseStr(nl))
		}
		if l.Closed && out.Interim && nl.Outcome != l.Outcome {
			w.violate("I2", "interim report of %s moved superseded lease %s to %q", a.id, r4LeaseStr(nl), nl.Outcome)
		}
		if d := CostToMillis(nl.CostUSD) - CostToMillis(l.CostUSD); d != CostToMillis(charge) {
			w.violate("I2", "lease %s cost moved %+d millis, want %d (%s)", r4LeaseStr(nl), d, CostToMillis(charge), label)
		}
		if charge > 0 || tok > 0 {
			expLedger[ledgerKey(l.PledgeID, periodDay, dayKey(l.AcquiredAt))] += CostToMillis(charge)
			expLedger[ledgerKey(l.PledgeID, periodWeek, weekKey(l.AcquiredAt))] += CostToMillis(charge)
		}
		if fine && l.AttachmentID != "" {
			attExp += CostToMillis(charge)
		}
	}
	if !fine && anySettleable {
		attExp = CostToMillis(out.CostUSD)
	}
	// Donor rows: exact per bucket. att: row: total across its day buckets.
	attPrefix := AttachmentLedgerID(w.attID) + "|"
	attGot := int64(0)
	keys := map[string]bool{}
	for k := range bg {
		keys[k] = true
	}
	for k := range ag {
		keys[k] = true
	}
	for k := range keys {
		d := ag[k].costMillis - bg[k].costMillis
		if ag[k].runs != bg[k].runs {
			w.violate("I2", "report %s moved the run counter of %s", label, k)
		}
		if strings.HasPrefix(k, attPrefix) {
			if strings.Contains(k, "|"+periodDay+"|") {
				attGot += d
			}
			continue
		}
		if d != expLedger[k] {
			w.violate("I2", "report of %s (%s, fine=%v) moved ledger %s by %+d millis, want %+d", a.id, label, fine, k, d, expLedger[k])
		}
	}
	if attGot != attExp {
		w.violate("I2-att", "report of %s (%s, fine=%v, cost=%.2f) booked the att: row %+d millis, want %+d", a.id, label, fine, out.CostUSD, attGot, attExp)
	}
	// Conditions reach exactly the settled leases' pledges.
	for id, p := range ap {
		q := bp[id]
		if !condPledges[id] {
			if p.ConsecutiveAuthFailures != q.ConsecutiveAuthFailures || p.Health != q.Health || !r4TimePtrEq(p.CooldownUntil, q.CooldownUntil) {
				w.violate("COND", "report of %s (%s) changed pledge %s it did not settle", a.id, label, id)
			}
			continue
		}
		switch out.Condition {
		case ConditionAuthFailed:
			if p.ConsecutiveAuthFailures != q.ConsecutiveAuthFailures+1 {
				w.violate("COND", "auth_failed did not count on %s", id)
			}
		case ConditionUsageWindow:
			if p.CooldownUntil == nil || !p.CooldownUntil.After(w.h.now) {
				w.violate("COND", "usage_window did not cool %s", id)
			}
		default:
			if p.ConsecutiveAuthFailures != 0 {
				w.violate("COND", "ok did not clear %s", id)
			}
		}
	}
	if !out.Interim {
		// I1: every lease of the attempt is closed after its final report.
		for _, l := range al {
			if l.AttemptID == a.id && !l.Closed {
				w.violate("I1", "after the final report of %s (%s) lease %s is still open", a.id, label, r4LeaseStr(l))
			}
		}
	}
	key := "report " + label
	if fine {
		key += " fine"
	} else {
		key += " coarse"
	}
	w.stats[key]++
}

// global invariants after every operation
func (w *r4World) checkGlobal(op string) {
	all := w.snapLeases()
	byRun := map[string]map[string]bool{}
	for _, l := range all {
		if l.Closed {
			continue
		}
		if byRun[l.RunID] == nil {
			byRun[l.RunID] = map[string]bool{}
		}
		byRun[l.RunID][l.AttemptID] = true
	}
	for run, atts := range byRun {
		if len(atts) > 1 {
			var ids []string
			for id := range atts {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			w.violate("ONE-ATTEMPT", "after %s run %s holds open leases of %d attempts: %v", op, run, len(atts), ids)
		}
	}
	for _, a := range w.attempts {
		if a.rolledBack {
			for _, l := range all {
				if l.AttemptID == a.id && !l.Closed {
					w.violate("ROLLBACK", "rolled-back attempt %s keeps lease %s open", a.id, r4LeaseStr(l))
				}
			}
		}
	}
	// I6 (sequential): every run that DREW (a lease not given back) holds a
	// consumed unit — donor-side per pledge, borrower-side on the att: row —
	// and no day admits more units than its ceiling.
	type rp struct{ run, pledge string }
	drewP, unitP := map[rp]bool{}, map[rp]bool{}
	drewA, unitA := map[string]bool{}, map[string]bool{}
	unitsByDay := map[string]int{}
	attByDay := map[string]int{}
	for _, l := range all {
		if l.Outcome == OutcomeNotLaunched {
			continue
		}
		drewP[rp{l.RunID, l.PledgeID}] = true
		if l.ConsumedRunUnit {
			unitP[rp{l.RunID, l.PledgeID}] = true
			unitsByDay[l.PledgeID+"|"+dayKey(l.AcquiredAt)]++
		}
		if l.AttachmentID != "" {
			drewA[l.RunID] = true
			if l.AttConsumedRunUnit {
				unitA[l.RunID] = true
				attByDay[dayKey(l.AcquiredAt)]++
			}
		}
	}
	for k := range drewP {
		if !unitP[k] {
			w.violate("I6-unit", "after %s run %s draws on %s with no donor run unit consumed", op, k.run, k.pledge)
		}
	}
	for run := range drewA {
		if !unitA[run] {
			w.violate("I6-att-unit", "after %s run %s draws under the attachment with no att: run unit consumed (its only unit was refunded by a released lease)", op, run)
		}
	}
	lims := map[string]Limits{}
	for _, p := range w.snapPledges() {
		lims[p.ID] = p.Limits
	}
	for k, n := range unitsByDay {
		pid := k[:strings.LastIndex(k, "|")]
		if m := lims[pid].MaxRunsPerDay; m > 0 && n > m {
			w.violate("I6-runs/day", "after %s pledge %s admitted %d units on %s (cap %d)", op, pid, n, k[strings.LastIndex(k, "|")+1:], m)
		}
	}
	for d, n := range attByDay {
		if m := w.draw.MaxRunsPerDay; m > 0 && n > m {
			w.violate("I6-att-runs/day", "after %s the attachment admitted %d draw units on %s (cap %d)", op, n, d, m)
		}
	}
	for run, a := range w.current {
		if a.finalSent || w.pending[run] != nil || a.grantless {
			continue
		}
		for _, g := range a.sealed {
			l := all[g.releaseGuard.lease.ID]
			if l.Closed && l.Outcome != OutcomeAbandoned {
				w.violate("LIVE", "after %s the live attempt %s of %s runs on a CLOSED lease %s", op, a.id, run, r4LeaseStr(l))
			}
		}
	}
}

func (w *r4World) step() {
	ctx := context.Background()
	rng := w.rng
	run := w.runs[rng.Intn(len(w.runs))]
	switch r := rng.Intn(100); {
	case r < 22:
		if w.pending[run] == nil {
			w.resolve(run)
			w.checkGlobal("resolve")
		}
	case r < 34:
		if w.pending[run] != nil {
			w.publish(run)
			w.checkGlobal("publish")
		}
	case r < 70:
		// A report: the live attempt (interim or final), or a late final /
		// a redelivery of any published attempt with grants.
		var cands []*r4Attempt
		for _, a := range w.attempts {
			if a.published && !a.grantless {
				cands = append(cands, a)
			}
		}
		if len(cands) == 0 {
			return
		}
		a := cands[rng.Intn(len(cands))]
		live := w.current[a.run] == a && (w.pending[a.run] == nil || w.hostileInterim)
		switch {
		case a.finalSent:
			if rng.Intn(4) == 0 && live {
				w.report(a, w.outcome(a, false), "stale-interim-after-final")
			} else {
				w.report(a, a.final, "redelivered-final")
			}
		case live && rng.Intn(3) == 0:
			a.interims++
			w.report(a, w.outcome(a, false), "interim")
		default:
			out := w.outcome(a, true)
			lbl := "final"
			if w.current[a.run] != a {
				lbl = "late-final"
			}
			w.report(a, out, lbl)
			a.finalSent, a.final = true, out
		}
		w.checkGlobal("report")
	case r < 82:
		d := []time.Duration{time.Millisecond, time.Second, 10 * time.Minute, 2 * time.Hour, 13 * time.Hour, 24 * time.Hour, 7 * 24 * time.Hour}[rng.Intn(7)]
		w.h.now = w.h.now.Add(d)
		w.logf("CLOCK +%s", d)
	case r < 90:
		rp := w.pledges[rng.Intn(len(w.pledges))]
		p, _ := w.h.pledges.Get(ctx, rp.id)
		switch rng.Intn(5) {
		case 0:
			until := w.h.now.Add(time.Duration(10+rng.Intn(110)) * time.Minute)
			p.CooldownUntil = &until
		case 1:
			p.CooldownUntil = nil
		case 2:
			if p.Health == HealthOK {
				p.Health = HealthAuthFailed
			} else {
				p.Health, p.ConsecutiveAuthFailures = HealthOK, 0
			}
		case 3:
			p.Enabled = !p.Enabled
		case 4:
			w.near[p.ID] = !w.near[p.ID]
		}
		_ = w.h.pledges.Upsert(ctx, p)
		w.logf("COND %s enabled=%v health=%s cooling=%v near=%v", p.ID, p.Enabled, p.Health, p.Cooling(w.h.now), w.near[p.ID])
	case r < 93:
		rp := w.pledges[rng.Intn(len(w.pledges))]
		n, _ := w.h.broker.ClearHolds(ctx, rp.id)
		w.logf("CLEARHOLDS %s -> %d", rp.id, n)
	case r < 96:
		n, _ := w.h.broker.ReleaseExpired(ctx, 1+rng.Intn(3))
		w.logf("SWEEP freed %d", n)
		w.checkGlobal("sweep")
	default:
		// Repair a parked donor (an operator reconnect).
		rp := w.pledges[rng.Intn(len(w.pledges))]
		p, _ := w.h.pledges.Get(ctx, rp.id)
		p.Health, p.ConsecutiveAuthFailures, p.Enabled = HealthOK, 0, true
		_ = w.h.pledges.Upsert(ctx, p)
	}
}

func r4Seeds(t *testing.T, def int) (int64, int) {
	base := int64(20261110)
	if s := os.Getenv("RVA4_SEED"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return v, 1
	}
	n := def
	if s := os.Getenv("RVA4_ITER"); s != "" {
		v, _ := strconv.Atoi(s)
		n = v
	}
	return base, n
}

func r4Run(t *testing.T, hostile bool) {
	base, iters := r4Seeds(t, 400)
	steps := 120
	if s := os.Getenv("RVA4_STEPS"); s != "" {
		steps, _ = strconv.Atoi(s)
	}
	total := map[string]int{}
	failedSeeds := map[string][]int64{}
	firstTrace := map[string][]string{}
	for i := 0; i < iters; i++ {
		seed := base + int64(i)
		w := r4Setup(t, seed)
		w.hostileInterim = hostile
		// Production-like timing by default (publication a few ms after
		// its resolution, no acquisition inside a publication's ms) — the
		// shape every invariant holds on. RVA4_RACE=1 freezes the clock to
		// force millisecond collisions — a publication floored past the
		// next acquisition — which the attempt id carried by the message
		// settles; every report then carries it.
		w.realistic = os.Getenv("RVA4_RACE") == ""
		for s := 0; s < steps; s++ {
			w.step()
			if len(w.viol) > 0 {
				break
			}
		}
		for k, v := range w.stats {
			total[k] += v
		}
		if len(w.viol) > 0 {
			inv := strings.SplitN(w.viol[0], ":", 2)[0]
			failedSeeds[inv] = append(failedSeeds[inv], seed)
			if _, ok := firstTrace[inv]; !ok {
				tr := w.trace
				firstTrace[inv] = append([]string{fmt.Sprintf("seed=%d resetHold=%v draw=%+v", seed, w.resetHold, w.draw)}, tr...)
				for _, v := range w.viol {
					firstTrace[inv] = append(firstTrace[inv], "=> "+v)
				}
			}
		}
	}
	var keys []string
	for k := range total {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("%6d  %s", total[k], k)
	}
	var invs []string
	for inv := range failedSeeds {
		invs = append(invs, inv)
	}
	sort.Strings(invs)
	for _, inv := range invs {
		seeds := failedSeeds[inv]
		t.Errorf("invariant %s violated on %d/%d seeds (first: %v)", inv, len(seeds), iters, seeds[:min(5, len(seeds))])
		tr := firstTrace[inv]
		if len(tr) > 60 {
			tr = append(tr[:1], tr[len(tr)-59:]...)
		}
		for _, line := range tr {
			t.Log("   " + line)
		}
	}
}

func TestPoolGuarantee_guarantee_sideBySideAccountingAndHolds(t *testing.T) { r4Run(t, false) }

func TestPoolGuarantee_guarantee_hostileInterimDuringPendingResume(t *testing.T) {
	if os.Getenv("RVA4_HOSTILE") == "" {
		t.Skip("set RVA4_HOSTILE=1")
	}
	r4Run(t, true)
}
