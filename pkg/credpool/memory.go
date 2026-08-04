package credpool

import (
	"context"
	"sort"
	"sync"
	"time"
)

// The in-process stores back local mode and tests. Their semantics are the
// contract the Mongo implementations must match — when the two drift, this
// file is the one that is right.

// ---------------------------------------------------------------------------
// Pools
// ---------------------------------------------------------------------------

type MemoryPoolStore struct {
	mu sync.Mutex
	m  map[string]Pool
}

func NewMemoryPoolStore() *MemoryPoolStore { return &MemoryPoolStore{m: map[string]Pool{}} }

func (s *MemoryPoolStore) GetByOrg(_ context.Context, orgID string) (Pool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.m {
		if p.OrgID == orgID {
			return p, nil
		}
	}
	return Pool{}, ErrNotFound
}

func (s *MemoryPoolStore) ListEnabled(_ context.Context) ([]Pool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Pool
	for _, p := range s.m {
		if p.Enabled {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *MemoryPoolStore) Upsert(_ context.Context, p Pool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if existing, ok := s.m[p.ID]; ok {
		p.CreatedAt = existing.CreatedAt
	} else if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	s.m[p.ID] = p
	return nil
}

// ---------------------------------------------------------------------------
// Pledges
// ---------------------------------------------------------------------------

type MemoryPledgeStore struct {
	mu sync.Mutex
	m  map[string]Pledge
}

func NewMemoryPledgeStore() *MemoryPledgeStore { return &MemoryPledgeStore{m: map[string]Pledge{}} }

func (s *MemoryPledgeStore) Get(_ context.Context, id string) (Pledge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[id]
	if !ok {
		return Pledge{}, ErrNotFound
	}
	return p, nil
}

func (s *MemoryPledgeStore) ListByPool(_ context.Context, poolID string) ([]Pledge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Pledge
	for _, p := range s.m {
		if p.PoolID == poolID {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *MemoryPledgeStore) ListByUser(_ context.Context, userID string) ([]Pledge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Pledge
	for _, p := range s.m {
		if p.UserID == userID {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *MemoryPledgeStore) Upsert(_ context.Context, p Pledge) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.ID == "" {
		p.ID = PledgeID(p.UserID, p.Source, p.Ref)
	}
	now := time.Now().UTC()
	if existing, ok := s.m[p.ID]; ok {
		p.CreatedAt = existing.CreatedAt
	} else if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	s.m[p.ID] = p
	return nil
}

func (s *MemoryPledgeStore) TouchLastServed(_ context.Context, id string, when time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[id]
	if !ok {
		return ErrNotFound
	}
	t := when.UTC()
	p.LastServedAt = &t
	p.UpdatedAt = t
	s.m[id] = p
	return nil
}

func (s *MemoryPledgeStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[id]; !ok {
		return ErrNotFound
	}
	delete(s.m, id)
	return nil
}

// ---------------------------------------------------------------------------
// Leases
// ---------------------------------------------------------------------------

type MemoryLeaseStore struct {
	mu sync.Mutex
	m  map[string]Lease
}

func NewMemoryLeaseStore() *MemoryLeaseStore { return &MemoryLeaseStore{m: map[string]Lease{}} }

func (s *MemoryLeaseStore) Put(_ context.Context, l Lease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.ID == "" {
		return ErrNotFound
	}
	s.m[l.ID] = l
	return nil
}

func (s *MemoryLeaseStore) Get(_ context.Context, leaseID string) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.m[leaseID]
	if !ok {
		return Lease{}, ErrNotFound
	}
	return l, nil
}

func (s *MemoryLeaseStore) GetOpenByRun(ctx context.Context, runID string) (Lease, error) {
	open, err := s.ListOpenByRun(ctx, runID)
	if err != nil {
		return Lease{}, err
	}
	if len(open) == 0 {
		return Lease{}, ErrNotFound
	}
	return open[0], nil // newest first
}

func (s *MemoryLeaseStore) ListOpenByRun(_ context.Context, runID string) ([]Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Lease
	for _, l := range s.m {
		if l.RunID == runID && !l.Closed {
			out = append(out, l)
		}
	}
	// Newest first: a map's iteration order is randomised, and picking
	// arbitrarily here would charge an arbitrary donor.
	sort.Slice(out, func(i, j int) bool { return out[i].AcquiredAt.After(out[j].AcquiredAt) })
	return out, nil
}

func (s *MemoryLeaseStore) HasServedAttempt(_ context.Context, runID, pledgeID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.m {
		if l.RunID == runID && l.PledgeID == pledgeID && !isNonAdmission(l.Outcome) {
			return true, nil
		}
	}
	return false, nil
}

func (s *MemoryLeaseStore) Close(_ context.Context, leaseID string, costUSD float64, outcome string, when time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.m[leaseID]
	if !ok {
		return false, ErrNotFound
	}
	if l.Closed {
		// Already reported. Losing this race is how a redelivered report
		// learns not to charge the donor a second time.
		return false, nil
	}
	l.Closed = true
	l.CostUSD += costUSD
	l.Outcome = outcome
	t := when.UTC()
	l.ClosedAt = &t
	s.m[leaseID] = l
	return true, nil
}

func (s *MemoryLeaseStore) AddCost(_ context.Context, leaseID string, costUSD float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.m[leaseID]
	if !ok {
		return ErrNotFound
	}
	l.CostUSD += costUSD
	s.m[leaseID] = l
	return nil
}

func (s *MemoryLeaseStore) LiveCommitment(_ context.Context, pledgeID, excludeRunID string, now time.Time) (int, float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, committed := 0, 0.0
	for _, l := range s.m {
		if excludeRunID != "" && l.RunID == excludeRunID {
			continue
		}
		if l.PledgeID == pledgeID && !l.Closed && now.Before(l.ExpiresAt) {
			n++
			committed += l.GrantedCostUSD
		}
	}
	return n, committed, nil
}

func (s *MemoryLeaseStore) ListExpired(_ context.Context, now time.Time, limit int) ([]Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Lease
	for _, l := range s.m {
		if !l.Closed && !now.Before(l.ExpiresAt) {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt.Before(out[j].ExpiresAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryLeaseStore) ListByDonor(_ context.Context, donorID string, limit int) ([]Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Lease
	for _, l := range s.m {
		if l.DonorID == donorID {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AcquiredAt.After(out[j].AcquiredAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Ledger
// ---------------------------------------------------------------------------

type memBucket struct {
	runs                     int
	costMillis               int64
	inputTokens, outputToken int64
}

type MemoryLedger struct {
	mu sync.Mutex
	m  map[string]*memBucket
}

func NewMemoryLedger() *MemoryLedger { return &MemoryLedger{m: map[string]*memBucket{}} }

func (l *MemoryLedger) bucket(key string) *memBucket {
	b, ok := l.m[key]
	if !ok {
		b = &memBucket{}
		l.m[key] = b
	}
	return b
}

func (l *MemoryLedger) Reserve(_ context.Context, pledgeID string, when time.Time, lim Limits, live LiveCommitment) (float64, DenyReason, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lim.MaxConcurrentRuns > 0 && live.Runs >= lim.MaxConcurrentRuns {
		return 0, DenyConcurrency, nil
	}
	day := l.bucket(ledgerKey(pledgeID, periodDay, dayKey(when)))
	week := l.bucket(ledgerKey(pledgeID, periodWeek, weekKey(when)))

	// day.runs+1 — the judgement covers the admission at hand.
	remaining, deny := decide(lim, day.runs+1, millisToCost(day.costMillis), millisToCost(week.costMillis), live)
	if deny != DenyNone {
		return 0, deny, nil
	}
	day.runs++
	return remaining, DenyNone, nil
}

func (l *MemoryLedger) Renew(_ context.Context, pledgeID string, when time.Time, lim Limits, live LiveCommitment) (float64, DenyReason, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lim.MaxConcurrentRuns > 0 && live.Runs >= lim.MaxConcurrentRuns {
		return 0, DenyConcurrency, nil
	}
	day := l.bucket(ledgerKey(pledgeID, periodDay, dayKey(when)))
	week := l.bucket(ledgerKey(pledgeID, periodWeek, weekKey(when)))
	// runsAfterAdmit = 0 skips the run-count ceiling: this run was already
	// counted when it was first admitted.
	return decideRenew(lim, millisToCost(day.costMillis), millisToCost(week.costMillis), live)
}

// decideRenew is decide() with the run-count ceiling left out — and ONLY
// that one. MaxConcurrentRuns must survive: decide derives the per-slot
// share from it, so dropping it would hand a resumed run the donor's whole
// remaining allowance and refuse the very siblings they allowed.
func decideRenew(lim Limits, daySpent, weekSpent float64, live LiveCommitment) (float64, DenyReason, error) {
	remaining, deny := decide(Limits{
		MaxUSDPerDay:      lim.MaxUSDPerDay,
		MaxUSDPerWeek:     lim.MaxUSDPerWeek,
		MaxConcurrentRuns: lim.MaxConcurrentRuns,
	}, 0, daySpent, weekSpent, live)
	return remaining, deny, nil
}

func (l *MemoryLedger) ReleaseRun(_ context.Context, pledgeID string, when time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.m[ledgerKey(pledgeID, periodDay, dayKey(when))]; ok && b.runs > 0 {
		b.runs--
	}
	return nil
}

func (l *MemoryLedger) AddSpend(_ context.Context, pledgeID string, when time.Time, costUSD float64, in, out int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	millis := CostToMillis(costUSD)
	if millis == 0 && in <= 0 && out <= 0 {
		return nil
	}
	for _, key := range []string{
		ledgerKey(pledgeID, periodDay, dayKey(when)),
		ledgerKey(pledgeID, periodWeek, weekKey(when)),
	} {
		b := l.bucket(key)
		b.costMillis += millis
		if in > 0 {
			b.inputTokens += in
		}
		if out > 0 {
			b.outputToken += out
		}
	}
	return nil
}

func (l *MemoryLedger) Usage(_ context.Context, pledgeID string, when time.Time) (Usage, Usage, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.usageLocked(pledgeID, periodDay, dayKey(when)), l.usageLocked(pledgeID, periodWeek, weekKey(when)), nil
}

func (l *MemoryLedger) usageLocked(pledgeID, period, key string) Usage {
	u := Usage{Period: periodName(period), Key: key}
	if b, ok := l.m[ledgerKey(pledgeID, period, key)]; ok {
		u.Runs = b.runs
		u.CostUSD = millisToCost(b.costMillis)
		u.InputTokens = b.inputTokens
		u.OutputTokens = b.outputToken
	}
	return u
}

func (l *MemoryLedger) UsageMany(_ context.Context, pledgeIDs []string, when time.Time) (map[string]Usage, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := dayKey(when)
	out := make(map[string]Usage, len(pledgeIDs))
	for _, id := range pledgeIDs {
		out[id] = l.usageLocked(id, periodDay, key)
	}
	return out, nil
}

func periodName(period string) string {
	if period == periodWeek {
		return "week"
	}
	return "day"
}
