package credusage

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryCounter is the in-process Counter for tests and local mode. Keep its
// semantics in lock-step with MongoCounter.
type MemoryCounter struct {
	mu   sync.Mutex
	rows map[string]*memRow
}

type memRow struct {
	key             Key
	month           string
	nature          Nature
	costUSDMillis   int64
	inputTokens     int64
	outputTokens    int64
	aggregateTokens int64
	runs            int
	backends        map[string]bool
}

func NewMemoryCounter() *MemoryCounter {
	return &MemoryCounter{rows: make(map[string]*memRow)}
}

func (c *MemoryCounter) AddSpend(_ context.Context, when time.Time, s Spend) error {
	if !s.recordable() {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	id := docID(s.Key, when)
	r, ok := c.rows[id]
	if !ok {
		r = &memRow{key: s.Key, month: monthKey(when), nature: s.Nature, backends: map[string]bool{}}
		c.rows[id] = r
	}
	r.costUSDMillis += CostToMillis(s.CostUSD)
	if s.InputTokens > 0 {
		r.inputTokens += s.InputTokens
	}
	if s.OutputTokens > 0 {
		r.outputTokens += s.OutputTokens
	}
	if s.AggregateTokens > 0 {
		r.aggregateTokens += s.AggregateTokens
	}
	r.runs++
	if s.Backend != "" {
		r.backends[s.Backend] = true
	}
	return nil
}

func (c *MemoryCounter) Usage(_ context.Context, when time.Time, k Key) (MonthlyUsage, error) {
	out := MonthlyUsage{
		Month: monthKey(when), Fingerprint: k.Fingerprint,
		Provider: k.Provider, Tier: k.Tier, TenantID: k.TenantID, RepoID: k.RepoID,
	}
	if !k.Valid() {
		return out, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.rows[docID(k, when)]; ok {
		out = r.view()
	}
	return out, nil
}

func (c *MemoryCounter) List(_ context.Context, when time.Time, tenantID string) ([]MonthlyUsage, error) {
	return c.summed(when, func(r *memRow) bool { return r.key.TenantID == tenantID }), nil
}

func (c *MemoryCounter) ListByFingerprint(_ context.Context, when time.Time, fingerprint string) ([]MonthlyUsage, error) {
	if fingerprint == "" {
		return nil, nil
	}
	return c.summed(when, func(r *memRow) bool { return r.key.Fingerprint == fingerprint }), nil
}

func (c *MemoryCounter) ListByTier(_ context.Context, when time.Time, tier Tier) ([]MonthlyUsage, error) {
	if tier == "" {
		return nil, nil
	}
	return c.summed(when, func(r *memRow) bool { return r.key.Tier == tier }), nil
}

// ListByRepo keeps the per-repository rows apart — it is the one listing
// scoped to a single repo, so summing them would collapse the very dimension
// asked for.
func (c *MemoryCounter) ListByRepo(_ context.Context, when time.Time, repoID string) ([]MonthlyUsage, error) {
	if repoID == "" {
		return nil, nil
	}
	rows := c.list(when, func(r *memRow) bool { return r.key.RepoID == repoID })
	sortUsage(rows)
	return rows, nil
}

// summed is the shape every listing older than the repo dimension takes: the
// rows a credential accumulated across repositories, added back together.
func (c *MemoryCounter) summed(when time.Time, keep func(*memRow) bool) []MonthlyUsage {
	rows := aggregateRepos(c.list(when, keep))
	sortUsage(rows)
	return rows
}

func (c *MemoryCounter) list(when time.Time, keep func(*memRow) bool) []MonthlyUsage {
	month := monthKey(when)
	c.mu.Lock()
	defer c.mu.Unlock()
	// Deterministic before anything downstream groups or sorts: Go's map
	// order is randomised, and aggregateRepos keeps first-seen order — so an
	// unordered scan would make the aggregated sequence differ run to run
	// wherever two rows tie, and the conformance suite compares sequences.
	ids := make([]string, 0, len(c.rows))
	for id, r := range c.rows {
		if r.month != month || !keep(r) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]MonthlyUsage, 0, len(ids))
	for _, id := range ids {
		out = append(out, c.rows[id].view())
	}
	return out
}

func (r *memRow) view() MonthlyUsage {
	backends := make([]string, 0, len(r.backends))
	for b := range r.backends {
		backends = append(backends, b)
	}
	sort.Strings(backends)
	return MonthlyUsage{
		Month:           r.month,
		Fingerprint:     r.key.Fingerprint,
		Provider:        r.key.Provider,
		Tier:            r.key.Tier,
		TenantID:        r.key.TenantID,
		RepoID:          r.key.RepoID,
		Nature:          r.nature,
		CostUSD:         millisToCost(r.costUSDMillis),
		InputTokens:     r.inputTokens,
		OutputTokens:    r.outputTokens,
		AggregateTokens: r.aggregateTokens,
		Runs:            r.runs,
		Backends:        backends,
	}
}

// sortUsage orders a listing biggest-spend first, then by fingerprint so the
// order is stable when amounts tie (both twins, so a client can diff two
// responses).
func sortUsage(rows []MonthlyUsage) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].CostUSD != rows[j].CostUSD {
			return rows[i].CostUSD > rows[j].CostUSD
		}
		if rows[i].Fingerprint != rows[j].Fingerprint {
			return rows[i].Fingerprint < rows[j].Fingerprint
		}
		return rows[i].Tier < rows[j].Tier
	})
}
