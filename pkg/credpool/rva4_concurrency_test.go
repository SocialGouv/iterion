package credpool

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// r4Clock is a goroutine-safe test clock.
type r4Clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *r4Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *r4Clock) Add(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

// r4Observer is fed by every goroutine: it takes a mutex, always.
type r4Observer struct {
	mu        sync.Mutex
	grants    int
	refusals  int
	rollbacks int
	reports   int
	maxOpen   map[string]int
	errs      []string
}

func (o *r4Observer) note(f func(o *r4Observer)) { o.mu.Lock(); defer o.mu.Unlock(); f(o) }

func TestPoolGuarantee_guarantee_capsUnderConcurrency(t *testing.T) {
	seed := int64(4242)
	if s := os.Getenv("RVA4_CSEED"); s != "" {
		seed, _ = strconv.ParseInt(s, 10, 64)
	}
	rounds := 30
	if s := os.Getenv("RVA4_CROUNDS"); s != "" {
		rounds, _ = strconv.Atoi(s)
	}
	fineOnly := os.Getenv("RVA4_FINE_ONLY") != ""
	rollbacks := os.Getenv("RVA4_NO_ROLLBACK") == ""
	t.Logf("seed=%d rounds=%d fineOnly=%v rollbacks=%v", seed, rounds, fineOnly, rollbacks)
	totalViol := 0
	for round := 0; round < rounds; round++ {
		totalViol += r4ConcurrentRound(t, seed+int64(round), fineOnly, rollbacks)
	}
	if totalViol > 0 {
		t.Errorf("%d invariant violations over %d rounds", totalViol, rounds)
	}
}

func r4ConcurrentRound(t *testing.T, seed int64, fineOnly, withRollbacks bool) int {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	clock := &r4Clock{t: time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)}
	h.broker = NewBroker(BrokerConfig{
		Pools: h.pools, Pledges: h.pledges, Leases: h.leases, Ledger: h.ledger,
		OAuth: h.oauth, APIKeys: h.apiKeys, Sealer: h.sealer,
		Logger: iterlog.New(iterlog.LevelError, nil), Now: clock.Now,
	})
	type pl struct {
		id   string
		kind int
		lim  Limits
	}
	var pls []pl
	add := func(kind int, user string, lim Limits, prio int) {
		var p Pledge
		switch kind {
		case 0:
			p = h.donor(t, user, lim, func(p *Pledge) { p.Priority = prio })
			rec, _ := h.oauth.Get(ctx, user, secrets.OAuthKindClaudeCode)
			rec.Fingerprint = "fp-oauth-" + user
			_ = h.oauth.Upsert(ctx, rec)
		case 1:
			p, _ = h.donorKey(t, user, "anthropic", lim, func(p *Pledge) { p.Priority = prio })
		case 2:
			p = h.r4GatewayDonor(t, user, lim, func(p *Pledge) { p.Priority = prio })
		}
		pls = append(pls, pl{id: p.ID, kind: kind, lim: lim})
	}
	add(0, "fa", Limits{MaxRunsPerDay: 3, MaxConcurrentRuns: 2, MaxUSDPerDay: 4}, 0)
	add(0, "fb", Limits{MaxRunsPerDay: 2, MaxUSDPerDay: 3}, 1)
	add(1, "ka", Limits{MaxRunsPerDay: 3, MaxConcurrentRuns: 1, MaxUSDPerDay: 5}, 0)
	add(1, "kb", Limits{MaxRunsPerDay: 1}, 1)
	add(2, "ga", Limits{MaxRunsPerDay: 2, MaxUSDPerDay: 2}, 0)
	draw := DrawLimits{MaxRunsPerDay: 5, MaxUSDPerDay: 6}
	_, attID, _ := attachForTest(t, h, Strategy{Mode: StrategyPriority, ResetHold: true}, draw)

	obs := &r4Observer{maxOpen: map[string]int{}}
	G := 10
	if v := os.Getenv("RVA4_G"); v != "" {
		G, _ = strconv.Atoi(v)
	}
	var wg sync.WaitGroup
	for g := 0; g < G; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed*100 + int64(g)))
			for r := 0; r < 3; r++ {
				run := fmt.Sprintf("g%d-r%d", g, r)
				var marker *time.Time
				for attempt := 0; attempt < 1+rng.Intn(3); attempt++ {
					attemptID := fmt.Sprintf("%s-A%d", run, attempt)
					now := clock.Now()
					m := store.NextQueuedAt(now, marker)
					var sealed, released []*Grant
					side := false
					for k := 0; k < 3; k++ {
						if rng.Intn(3) == 0 {
							continue
						}
						held, _ := h.broker.HeldPledge(ctx, run, r4Kinds[k])
						req := Request{RunID: run, OrgID: testOrg, TenantID: "team-1", UserID: "requester", BotID: r4Bot,
							Wants: []Credential{r4Kinds[k]}, PoolIDs: []string{"pool-1"}, AttachmentID: attID, DrawLimits: draw,
							RankMode: StrategyPriority, HoldOnSwitch: true, PreferPledgeID: held, SideBySide: side, AttemptID: attemptID}
						gr, err := h.broker.Acquire(ctx, req)
						if gr != nil {
							side = true
						}
						if err != nil {
							if !errors.Is(err, ErrNoDonor) {
								obs.note(func(o *r4Observer) { o.errs = append(o.errs, "acquire: "+err.Error()) })
							}
							obs.note(func(o *r4Observer) { o.refusals++ })
							continue
						}
						obs.note(func(o *r4Observer) { o.grants++ })
						if k == 2 && rng.Intn(4) == 0 {
							h.broker.ReleaseGrant(ctx, gr)
							released = append(released, gr)
						} else {
							sealed = append(sealed, gr)
						}
					}
					if withRollbacks && rng.Intn(6) == 0 {
						for i := len(sealed) - 1; i >= 0; i-- {
							h.broker.ReleaseGrant(ctx, sealed[i])
						}
						if len(sealed) > 0 {
							h.broker.ReopenSuperseded(ctx, sealed[0])
						}
						for _, gr := range released {
							h.broker.ReopenSuperseded(ctx, gr)
						}
						for i := 1; i < len(sealed); i++ {
							h.broker.ReopenSuperseded(ctx, sealed[i])
						}
						obs.note(func(o *r4Observer) { o.rollbacks++ })
						continue
					}
					clock.Add(time.Duration(rng.Intn(3)) * time.Millisecond)
					pub := store.PublishAt(clock.Now(), m)
					if len(sealed) > 0 {
						pub = store.PublishAt(pub, sealed[0].AcquiredAt)
					}
					marker = &m
					if len(sealed) == 0 {
						_ = h.broker.SupersedeRun(ctx, run, "team-1", pub)
						continue
					}
					// The attempt spends at most each grant's allowance on its own credential.
					var table []FingerprintSpend
					total := 0.0
					for _, gr := range sealed {
						c := float64(rng.Intn(5)) * 0.5
						if gr.RemainingUSD > 0 && c > gr.RemainingUSD {
							c = gr.RemainingUSD
						}
						total += c
						table = append(table, FingerprintSpend{Fingerprint: gr.Fingerprint, CostUSD: c})
					}
					out := Outcome{CostUSD: total}
					if fineOnly || rng.Intn(2) == 0 {
						out.ByFingerprint = table
					}
					if rng.Intn(4) == 0 {
						// A drained delivery spent HALF of each slice; the
						// redelivery spends the other half (sum = the slice).
						it := Outcome{CostUSD: total / 2, Interim: true}
						fin := Outcome{CostUSD: total / 2}
						if out.ByFingerprint != nil {
							for _, fs := range table {
								it.ByFingerprint = append(it.ByFingerprint, FingerprintSpend{Fingerprint: fs.Fingerprint, CostUSD: fs.CostUSD / 2})
								fin.ByFingerprint = append(fin.ByFingerprint, FingerprintSpend{Fingerprint: fs.Fingerprint, CostUSD: fs.CostUSD / 2})
							}
						}
						_ = h.broker.ReportAttempt(ctx, run, pub, it)
						out = fin
					}
					if err := h.broker.ReportAttempt(ctx, run, pub, out); err != nil {
						obs.note(func(o *r4Observer) { o.errs = append(o.errs, "report: "+err.Error()) })
					}
					if rng.Intn(3) == 0 { // a redelivered final, racing whatever runs now
						_ = h.broker.ReportAttempt(ctx, run, pub, out)
					}
					obs.note(func(o *r4Observer) { o.reports++ })
				}
			}
		}(g)
	}
	wg.Wait()

	viol := 0
	bad := func(format string, args ...any) {
		viol++
		t.Errorf("seed %d: "+format, append([]any{seed}, args...)...)
	}
	for _, e := range obs.errs {
		bad("unexpected error %s", e)
	}
	all := h.leases.snapshotForTest()
	day := dayKey(clock.Now())
	units := map[string]int{}
	attUnits := 0
	drewA, unitA := map[string]bool{}, map[string]bool{}
	for _, l := range all {
		if l.Outcome == OutcomeNotLaunched {
			continue
		}
		if l.ConsumedRunUnit && dayKey(l.AcquiredAt) == day {
			units[l.PledgeID]++
		}
		if l.AttachmentID != "" {
			drewA[l.RunID] = true
			if l.AttConsumedRunUnit {
				unitA[l.RunID] = true
				if dayKey(l.AcquiredAt) == day {
					attUnits++
				}
			}
		}
		if !l.Closed {
			bad("lease left open after every attempt reported or rolled back: %s", r4LeaseStr(l))
		}
	}
	for _, p := range pls {
		if p.lim.MaxRunsPerDay > 0 && units[p.id] > p.lim.MaxRunsPerDay {
			bad("pledge %s admitted %d run units today (MaxRunsPerDay %d)", p.id, units[p.id], p.lim.MaxRunsPerDay)
		}
		d, _, _ := h.ledger.Usage(ctx, p.id, clock.Now())
		if d.Runs != units[p.id] {
			bad("pledge %s ledger counts %d runs, its unreleased unit-consuming leases %d", p.id, d.Runs, units[p.id])
		}
		if fineOnly && p.lim.MaxUSDPerDay > 0 && d.CostUSD > p.lim.MaxUSDPerDay+1e-9 {
			t.Logf("seed %d: SOFT $ cap: pledge %s spent $%.2f today (MaxUSDPerDay %.2f)", seed, p.id, d.CostUSD, p.lim.MaxUSDPerDay)
		}
	}
	if attUnits > draw.MaxRunsPerDay {
		bad("the attachment admitted %d draw units today (MaxRunsPerDay %d)", attUnits, draw.MaxRunsPerDay)
	}
	var freeRiders []string
	for run := range drewA {
		if !unitA[run] {
			freeRiders = append(freeRiders, run)
		}
	}
	sort.Strings(freeRiders)
	if len(freeRiders) > 0 {
		bad("runs drawing under the attachment with no att: unit (refunded): %v (att units today %d, distinct drawing runs %d, cap %d)", freeRiders, attUnits, len(drewA), draw.MaxRunsPerDay)
	}
	if len(drewA) > draw.MaxRunsPerDay {
		t.Logf("seed %d: %d distinct runs drew under the attachment today (draw MaxRunsPerDay %d)", seed, len(drewA), draw.MaxRunsPerDay)
	}
	return viol
}

func (s *MemoryLeaseStore) snapshotForTest() []Lease {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Lease, 0, len(s.m))
	for _, l := range s.m {
		out = append(out, l)
	}
	return out
}
