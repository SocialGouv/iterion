package runtime

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/store"
)

// correctionEntry is scoped to one public Run/Resume call. The entry identity
// never changes; the only grant it can hold comes from its own successful CAS.
// It is not an executor/Engine setter and cannot leak into a later invocation.
type correctionEntry struct {
	mu          sync.Mutex
	original    store.RunExecutionExpectation
	initialized bool
	observed    *store.Run
	grant       *correctionExecution
}
type correctionEntryKey struct{}

func (e *Engine) beginCorrectionEntry(ctx context.Context, r *store.Run) context.Context {
	ctx = context.WithValue(ctx, correctionExecutionKey{}, (*correctionExecution)(nil))
	ctx = model.WithoutOutputCorrectionOwnership(ctx)
	ctx = model.WithExecutionUsageCollector(ctx, nil)
	var entry *correctionEntry
	if e.correctionProtocolCapable() {
		entry = &correctionEntry{}
		if r != nil {
			entry.observe(r)
		}
	}
	return context.WithValue(ctx, correctionEntryKey{}, entry)
}
func (c *correctionEntry) observe(r *store.Run) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.initialized {
		return
	}
	c.original = store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status}
	if r.QueuedAt != nil {
		copy := *r.QueuedAt
		c.original.QueuedAt = &copy
	}
	c.initialized = true
	c.observed = r
}
func (c *correctionEntry) expected(r *store.Run, published time.Time) (store.RunExecutionExpectation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.initialized || c.grant != nil {
		return store.RunExecutionExpectation{}, errCorrectionOwnershipLost
	}
	expected := c.original
	expected.Version = r.CASVersion
	expected.PublishedAt = published
	if !expected.Matches(r) {
		return store.RunExecutionExpectation{}, errCorrectionOwnershipLost
	}
	return expected, nil
}
func (c *correctionEntry) acquired(x *correctionExecution) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.grant != nil {
		return errCorrectionOwnershipLost
	}
	c.grant = x
	return nil
}
func correctionExecutionFromContext(ctx context.Context) *correctionExecution {
	if x, _ := ctx.Value(correctionExecutionKey{}).(*correctionExecution); x != nil {
		return x
	}
	if c, _ := ctx.Value(correctionEntryKey{}).(*correctionEntry); c != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.grant
	}
	return nil
}
func (e *Engine) acquireEntryExecution(ctx context.Context, r *store.Run, intent store.RunExecutionIntent) (*store.Run, error) {
	c, _ := ctx.Value(correctionEntryKey{}).(*correctionEntry)
	if c == nil {
		return nil, errors.New("output correction: missing invocation entry")
	}
	c.observe(r)
	expected, err := c.expected(r, e.queuedAttempt)
	if err != nil {
		return nil, err
	}
	ledger := store.AsObservedExecutionStore(e.store)
	if ledger == nil {
		return nil, errors.New("output correction: incomplete execution store")
	}
	a, ok, err := ledger.AcquireRunExecutionIf(ctx, r.ID, expected, intent)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errCorrectionOwnershipLost
	}
	if a.RunID != r.ID || a.Epoch == "" {
		return nil, errors.New("output correction: invalid acquisition response")
	}
	x := &correctionExecution{runs: e.store, ledger: ledger, authority: a}
	if err = c.acquired(x); err != nil {
		return nil, err
	}
	// Re-read metadata only after retaining the exact claim response. A newer
	// epoch cannot replace that grant, even if status has returned to running.
	_, fresh, err := store.LoadRunWithValueFormat(ctx, e.store, r.ID)
	if err != nil {
		return nil, err
	}
	if !a.Matches(fresh) {
		return nil, errCorrectionOwnershipLost
	}
	return fresh, nil
}
func (e *Engine) acquireResumeEntry(ctx context.Context, id string, allowed []store.RunStatus) (bool, error) {
	c, _ := ctx.Value(correctionEntryKey{}).(*correctionEntry)
	if c == nil {
		return false, errors.New("output correction: missing resume entry")
	}
	_, fresh, err := store.LoadRunWithValueFormat(ctx, e.store, id)
	if err != nil {
		return false, err
	}
	if !slices.Contains(allowed, fresh.Status) {
		return false, nil
	}
	if _, err = c.expected(fresh, e.queuedAttempt); err != nil {
		return false, err
	}
	claimed, err := e.acquireEntryExecution(ctx, fresh, store.ExecutionResume)
	if err != nil {
		return false, err
	}
	if err = e.prepareCorrectionAccounting(ctx, claimed); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if claimed.Checkpoint != nil {
		if c.observed.Checkpoint == nil {
			c.observed.Checkpoint = claimed.Checkpoint
		} else {
			copyCorrectionAccountingFields(c.observed.Checkpoint, claimed.Checkpoint)
		}
	}
	return true, nil
}
