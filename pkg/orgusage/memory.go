package orgusage

import (
	"context"
	"sync"
	"time"
)

// MemoryCounter is the in-process Counter for tests and local mode.
// Keep its semantics in lock-step with MongoCounter.
type MemoryCounter struct {
	mu    sync.Mutex
	usage map[string]*memUsage // usageKey -> counters
}

type memUsage struct {
	runs            int
	costUSDMillis   int64
	inputTokens     int64
	outputTokens    int64
	aggregateTokens int64
}

func NewMemoryCounter() *MemoryCounter {
	return &MemoryCounter{usage: make(map[string]*memUsage)}
}

func (c *MemoryCounter) get(subject Subject, when time.Time) *memUsage {
	key := usageKey(subject, when)
	u, ok := c.usage[key]
	if !ok {
		u = &memUsage{}
		c.usage[key] = u
	}
	return u
}

func (c *MemoryCounter) AllowRun(_ context.Context, subject Subject, when time.Time, maxRuns int, maxCostMillis int64) (DenyReason, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.get(subject, when)
	if maxRuns > 0 && u.runs+1 > maxRuns {
		return DenyRuns, nil
	}
	if maxCostMillis > 0 && u.costUSDMillis >= maxCostMillis {
		return DenyCost, nil
	}
	u.runs++
	return DenyNone, nil
}

func (c *MemoryCounter) ReleaseRun(_ context.Context, subject Subject, when time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if u, ok := c.usage[usageKey(subject, when)]; ok && u.runs > 0 {
		u.runs--
	}
	return nil
}

func (c *MemoryCounter) AddSpend(_ context.Context, subject Subject, when time.Time, costUSD float64, inputTokens, outputTokens, aggregateTokens int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.get(subject, when)
	u.costUSDMillis += CostToMillis(costUSD)
	if inputTokens > 0 {
		u.inputTokens += inputTokens
	}
	if outputTokens > 0 {
		u.outputTokens += outputTokens
	}
	if aggregateTokens > 0 {
		u.aggregateTokens += aggregateTokens
	}
	return nil
}

func (c *MemoryCounter) Usage(_ context.Context, subject Subject, when time.Time) (MonthlyUsage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := MonthlyUsage{Month: monthKey(when)}
	if u, ok := c.usage[usageKey(subject, when)]; ok {
		out.Runs = u.runs
		out.CostUSD = millisToCost(u.costUSDMillis)
		out.InputTokens = u.inputTokens
		out.OutputTokens = u.outputTokens
		out.AggregateTokens = u.aggregateTokens
	}
	return out, nil
}
