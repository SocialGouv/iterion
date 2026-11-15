package intake

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemoryStore is the in-process Store: the test twin, and the state the file
// store loads, mutates and writes back under its lock.
type MemoryStore struct {
	mu      sync.Mutex
	intakes map[string]*Intake
	pairs   map[string]*Pair
	// now is the clock expiry is judged against; nil reads the wall clock.
	now func() time.Time
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{intakes: map[string]*Intake{}, pairs: map[string]*Pair{}}
}

// memoryState is the store's serialized form (the file store's journal).
type memoryState struct {
	Intakes map[string]*Intake `json:"intakes"`
	Pairs   map[string]*Pair   `json:"pairs"`
}

func (m *MemoryStore) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// prune drops the rows whose expiry passed — what Mongo's TTL index does on
// its own schedule.
func (m *MemoryStore) prune() {
	now := m.clock()
	for id, in := range m.intakes {
		if in.ExpiresAt != nil && !in.ExpiresAt.After(now) {
			delete(m.intakes, id)
		}
	}
	for id, p := range m.pairs {
		if p.ExpiresAt != nil && !p.ExpiresAt.After(now) {
			delete(m.pairs, id)
		}
	}
}

func (m *MemoryStore) Accept(_ context.Context, in Intake) (Intake, bool, error) {
	in, err := prepareIntake(in)
	if err != nil {
		return Intake{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	if cur, ok := m.intakes[in.ID]; ok {
		return copyIntake(*cur), false, nil
	}
	stored := copyIntake(in)
	m.intakes[in.ID] = &stored
	return copyIntake(stored), true, nil
}

func (m *MemoryStore) GetIntake(_ context.Context, tenantID, id string) (Intake, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	in, ok := m.intakes[id]
	if !ok || in.TenantID != tenantID {
		return Intake{}, fmt.Errorf("%w: intake %s", ErrNotFound, id)
	}
	return copyIntake(*in), nil
}

func (m *MemoryStore) CountBacklog(_ context.Context, tenantID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	n := 0
	for _, in := range m.intakes {
		if in.TenantID == tenantID && (in.State == IntakePending || in.State == IntakeClaimed) {
			n++
		}
	}
	return n, nil
}

func (m *MemoryStore) ListIntakes(_ context.Context, tenantID, state string, limit int) ([]Intake, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	var out []Intake
	for _, in := range m.intakes {
		if in.TenantID == tenantID && (state == "" || in.State == state) {
			out = append(out, copyIntake(*in))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ReceivedAt.Equal(out[j].ReceivedAt) {
			return out[i].ReceivedAt.After(out[j].ReceivedAt)
		}
		return out[i].ID < out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStore) ClaimIntakes(_ context.Context, owner string, now time.Time, ttl time.Duration, limit int) ([]Intake, error) {
	if err := checkClaim(owner, ttl, limit); err != nil {
		return nil, err
	}
	now = stamp(now)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	var due []*Intake
	for _, in := range m.intakes {
		if (in.State == IntakePending || in.State == IntakeClaimed) && !in.NotBefore.After(now) {
			due = append(due, in)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].NotBefore.Equal(due[j].NotBefore) {
			return due[i].NotBefore.Before(due[j].NotBefore)
		}
		return due[i].ID < due[j].ID
	})
	if len(due) > limit {
		due = due[:limit]
	}
	out := make([]Intake, 0, len(due))
	for _, in := range due {
		if in.State == IntakeClaimed {
			in.Attempts++
		}
		in.State = IntakeClaimed
		in.Owner = owner
		in.Generation++
		in.NotBefore = stamp(now.Add(ttl))
		in.UpdatedAt = now
		out = append(out, copyIntake(*in))
	}
	return out, nil
}

// claimedIntake returns the row a fenced write may change, or why it may not.
func (m *MemoryStore) claimedIntake(id string, generation int64) (*Intake, error) {
	in, ok := m.intakes[id]
	if !ok {
		return nil, fmt.Errorf("%w: intake %s", ErrNotFound, id)
	}
	if in.State != IntakeClaimed || in.Generation != generation {
		return nil, fmt.Errorf("%w: intake %s is %s at generation %d, the write holds generation %d", ErrClaimLost, id, in.State, in.Generation, generation)
	}
	return in, nil
}

func (m *MemoryStore) MarkIntakeMatched(_ context.Context, id string, generation int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	in, err := m.claimedIntake(id, generation)
	if err != nil {
		return err
	}
	at = stamp(at)
	in.State = IntakeMatched
	in.Owner = ""
	in.MatchedAt = &at
	in.LastError = ""
	in.UpdatedAt = at
	return nil
}

func (m *MemoryStore) RetryIntake(_ context.Context, id string, generation int64, attempts int, notBefore time.Time, lastErr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	in, err := m.claimedIntake(id, generation)
	if err != nil {
		return err
	}
	in.State = IntakePending
	in.Owner = ""
	in.Attempts = attempts
	in.NotBefore = stamp(notBefore)
	in.LastError = lastErr
	in.UpdatedAt = stamp(m.clock())
	return nil
}

func (m *MemoryStore) BuryIntake(_ context.Context, id string, generation int64, attempts int, lastErr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	in, err := m.claimedIntake(id, generation)
	if err != nil {
		return err
	}
	in.State = IntakeDead
	in.Owner = ""
	in.Attempts = attempts
	in.LastError = lastErr
	in.UpdatedAt = stamp(m.clock())
	return nil
}

func (m *MemoryStore) ReplayIntake(_ context.Context, tenantID, id string, now time.Time) (Intake, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	in, ok := m.intakes[id]
	if !ok || in.TenantID != tenantID {
		return Intake{}, fmt.Errorf("%w: intake %s", ErrNotFound, id)
	}
	if in.State != IntakeDead {
		return Intake{}, fmt.Errorf("%w: intake %s is %s", ErrNotReplayable, id, in.State)
	}
	in.State = IntakePending
	in.Attempts = 0
	in.Generation++
	in.NotBefore = stamp(now)
	in.LastError = ""
	in.UpdatedAt = stamp(now)
	return copyIntake(*in), nil
}

func (m *MemoryStore) WritePairs(_ context.Context, pairs []Pair) error {
	prepared := make([]Pair, 0, len(pairs))
	for _, p := range pairs {
		p, err := preparePair(p)
		if err != nil {
			return err
		}
		prepared = append(prepared, p)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	for _, p := range prepared {
		if _, ok := m.pairs[p.ID]; ok {
			continue
		}
		stored := copyPair(p)
		m.pairs[p.ID] = &stored
	}
	return nil
}

func (m *MemoryStore) GetPair(_ context.Context, tenantID, id string) (Pair, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	p, ok := m.pairs[id]
	if !ok || p.TenantID != tenantID {
		return Pair{}, fmt.Errorf("%w: pair %s", ErrNotFound, id)
	}
	return copyPair(*p), nil
}

func (m *MemoryStore) ListPairs(_ context.Context, tenantID string, q PairQuery, limit int) ([]Pair, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	var out []Pair
	for _, p := range m.pairs {
		if p.TenantID == tenantID && (q.IntakeID == "" || p.IntakeID == q.IntakeID) && (q.State == "" || p.State == q.State) {
			out = append(out, copyPair(*p))
		}
	}
	sort.Slice(out, func(i, j int) bool { return pairBefore(out[i], out[j]) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// pairBefore is ListPairs' order: newest intake first, then intake id, then
// subscription id.
func pairBefore(a, b Pair) bool {
	if !a.ReceivedAt.Equal(b.ReceivedAt) {
		return a.ReceivedAt.After(b.ReceivedAt)
	}
	if a.IntakeID != b.IntakeID {
		return a.IntakeID < b.IntakeID
	}
	return a.SubscriptionID < b.SubscriptionID
}

func (m *MemoryStore) ClaimPairs(_ context.Context, owner string, now time.Time, ttl time.Duration, limit int) ([]Pair, error) {
	if err := checkClaim(owner, ttl, limit); err != nil {
		return nil, err
	}
	now = stamp(now)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	var due []*Pair
	for _, p := range m.pairs {
		if (p.State == PairPending || p.State == PairClaimed) && !p.NotBefore.After(now) {
			due = append(due, p)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].NotBefore.Equal(due[j].NotBefore) {
			return due[i].NotBefore.Before(due[j].NotBefore)
		}
		return due[i].ID < due[j].ID
	})
	if len(due) > limit {
		due = due[:limit]
	}
	out := make([]Pair, 0, len(due))
	for _, p := range due {
		if p.State == PairClaimed {
			p.Attempts++
		}
		p.State = PairClaimed
		p.Owner = owner
		p.Generation++
		p.NotBefore = stamp(now.Add(ttl))
		p.UpdatedAt = now
		out = append(out, copyPair(*p))
	}
	return out, nil
}

// claimedPair returns the row a fenced write may change, or why it may not.
func (m *MemoryStore) claimedPair(id string, generation int64) (*Pair, error) {
	p, ok := m.pairs[id]
	if !ok {
		return nil, fmt.Errorf("%w: pair %s", ErrNotFound, id)
	}
	if p.State != PairClaimed || p.Generation != generation {
		return nil, fmt.Errorf("%w: pair %s is %s at generation %d, the write holds generation %d", ErrClaimLost, id, p.State, p.Generation, generation)
	}
	return p, nil
}

func (m *MemoryStore) MarkPairLaunched(_ context.Context, id string, generation int64, runID string, at time.Time) error {
	if runID == "" {
		return fmt.Errorf("%w: pair %s launched without a run id", ErrInvalid, id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	p, err := m.claimedPair(id, generation)
	if err != nil {
		return err
	}
	at = stamp(at)
	p.State = PairLaunched
	p.Owner = ""
	p.RunID = runID
	p.LaunchedAt = &at
	p.LastError = ""
	p.UpdatedAt = at
	return nil
}

func (m *MemoryStore) RetryPair(_ context.Context, id string, generation int64, r PairRetry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	p, err := m.claimedPair(id, generation)
	if err != nil {
		return err
	}
	if r.Epoch < p.Epoch {
		return fmt.Errorf("%w: pair %s is at epoch %d, a retry cannot move it back to %d", ErrInvalid, id, p.Epoch, r.Epoch)
	}
	p.State = PairPending
	p.Owner = ""
	p.Attempts = r.Attempts
	p.NotBefore = stamp(r.NotBefore)
	p.LastError = r.LastError
	p.Epoch = r.Epoch
	p.UpdatedAt = stamp(m.clock())
	return nil
}

func (m *MemoryStore) RefusePair(_ context.Context, id string, generation int64, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	p, err := m.claimedPair(id, generation)
	if err != nil {
		return err
	}
	p.State = PairRefused
	p.Owner = ""
	p.Reason = reason
	p.UpdatedAt = stamp(m.clock())
	return nil
}

func (m *MemoryStore) BuryPair(_ context.Context, id string, generation int64, attempts int, lastErr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	p, err := m.claimedPair(id, generation)
	if err != nil {
		return err
	}
	p.State = PairDead
	p.Owner = ""
	p.Attempts = attempts
	p.LastError = lastErr
	p.UpdatedAt = stamp(m.clock())
	return nil
}

func (m *MemoryStore) ReplayPair(_ context.Context, tenantID, id string, now time.Time) (Pair, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	p, ok := m.pairs[id]
	if !ok || p.TenantID != tenantID {
		return Pair{}, fmt.Errorf("%w: pair %s", ErrNotFound, id)
	}
	if err := replayable(p); err != nil {
		return Pair{}, err
	}
	if p.State == PairRefused {
		p.Epoch++
	}
	p.State = PairPending
	p.Reason = ""
	p.Attempts = 0
	p.Generation++
	p.NotBefore = stamp(now)
	p.LastError = ""
	p.UpdatedAt = stamp(now)
	return copyPair(*p), nil
}

// replayable says why a pair cannot be replayed, if it cannot.
func replayable(p *Pair) error {
	switch {
	case p.State == PairRefused && p.MatchRefusal:
		return fmt.Errorf("%w: pair %s was refused by matching (%s): no filter ever matched it, so it is not replayed into a launch", ErrNotReplayable, p.ID, p.Reason)
	case p.State != PairDead && p.State != PairRefused:
		return fmt.Errorf("%w: pair %s is %s", ErrNotReplayable, p.ID, p.State)
	}
	return nil
}

// MinClaimTTL is the shortest claim a store honours: below its millisecond
// precision, a claim would expire the instant it is written and the same
// call could take the row again.
const MinClaimTTL = time.Millisecond

// checkClaim refuses a claim no worker could honour.
func checkClaim(owner string, ttl time.Duration, limit int) error {
	switch {
	case owner == "":
		return fmt.Errorf("%w: a claim names its owner", ErrInvalid)
	case ttl < MinClaimTTL:
		return fmt.Errorf("%w: claim ttl %s is under %s", ErrInvalid, ttl, MinClaimTTL)
	case limit <= 0:
		return fmt.Errorf("%w: claim limit %d", ErrInvalid, limit)
	}
	return nil
}

// copyIntake detaches a row from the store's own: the event bytes and the
// instants behind pointers are copied.
func copyIntake(in Intake) Intake {
	if in.Event != nil {
		in.Event = append([]byte(nil), in.Event...)
	}
	in.ExpiresAt = copyTime(in.ExpiresAt)
	in.MatchedAt = copyTime(in.MatchedAt)
	return in
}

func copyPair(p Pair) Pair {
	p.ExpiresAt = copyTime(p.ExpiresAt)
	p.LaunchedAt = copyTime(p.LaunchedAt)
	return p
}

func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

var _ Store = (*MemoryStore)(nil)
