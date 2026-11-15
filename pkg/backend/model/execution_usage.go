package model

import (
	"cmp"
	"context"
	"errors"
	"math"
	"slices"
	"sync"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Version2 certifies disjoint, invocation-scoped raw components at the boundary.
const ExecutionAccountingProtocol = 2

type ExecutionUsage struct {
	CollectionID    string
	Tokens          int
	UnpricedTokens  int
	UnreportedCalls int
	OtherCostUSD    float64
	Sessions        []OutputCorrectionSessionCost
	Components      []store.UsageComponent
	// A checkpoint belongs to this exact collector, never a caller-supplied sum.
	owner  *ExecutionUsageCollector
	cursor int
}

type ExecutionUsageCollector struct {
	collectionID string
	mu           sync.Mutex
	total        ExecutionUsage
	components   []store.UsageComponent
	calls        map[string]bool
	err          error
}
type executionUsageKey struct{}
type executionUsageBackendKey struct{}

func WithExecutionUsageCollector(ctx context.Context, c *ExecutionUsageCollector) context.Context {
	return context.WithValue(ctx, executionUsageKey{}, c)
}
func executionUsageCollector(ctx context.Context) *ExecutionUsageCollector {
	c, _ := ctx.Value(executionUsageKey{}).(*ExecutionUsageCollector)
	return c
}
func withExecutionUsageBackend(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, executionUsageBackendKey{}, name)
}

func executionUsageFromComponents(collectionID string, components []store.UsageComponent) (ExecutionUsage, error) {
	obs, err := store.SummarizeUsageComponents(collectionID, components)
	if err != nil {
		return ExecutionUsage{}, err
	}
	out := ExecutionUsage{CollectionID: collectionID, Tokens: obs.Tokens, UnpricedTokens: obs.UnpricedTokens, UnreportedCalls: obs.UnreportedCalls, OtherCostUSD: obs.OtherCostUSD, Components: obs.Components}
	for _, s := range obs.Sessions {
		out.Sessions = append(out.Sessions, OutputCorrectionSessionCost{Backend: s.Backend, SessionID: s.SessionID, Fingerprint: s.Fingerprint, CostUSD: s.CostUSD})
	}
	slices.SortFunc(out.Sessions, func(a, b OutputCorrectionSessionCost) int {
		return cmp.Or(cmp.Compare(a.Backend, b.Backend), cmp.Compare(a.SessionID, b.SessionID), cmp.Compare(a.Fingerprint, b.Fingerprint))
	})
	return out, nil
}
func (c *ExecutionUsageCollector) snapshotFrom(cursor int) (ExecutionUsage, error) {
	out, err := executionUsageFromComponents(c.collectionID, c.components[cursor:])
	if err != nil {
		return out, err
	}
	out.owner = c
	out.cursor = len(c.components)
	return out, c.err
}

// Snapshot makes independent copies of routes, splits and raw observations.
func (c *ExecutionUsageCollector) Snapshot() (ExecutionUsage, error) {
	if c == nil {
		return ExecutionUsage{}, errors.New("model: missing execution usage collector")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureCollectionID()
	return c.snapshotFrom(0)
}

// Since selects new components, then reduces that lot. It never subtracts
// rounded cumulative costs. Session readings belong only to this lot; the
// durable store applies its maxima from earlier receipts and other invocations.
func (c *ExecutionUsageCollector) Since(before ExecutionUsage) (ExecutionUsage, error) {
	if c == nil {
		return ExecutionUsage{}, errors.New("model: missing execution usage collector")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if before.owner != c || before.CollectionID != c.collectionID || before.cursor < 0 || before.cursor > len(c.components) {
		return ExecutionUsage{}, errors.New("model: execution usage checkpoint belongs to another collector")
	}
	return c.snapshotFrom(before.cursor)
}
func observeExecutionUsage(ctx context.Context, task delegate.Task, r delegate.Result) {
	observeExecutionUsagePriced(ctx, task, r, false)
}
func observeExecutionUsagePriced(ctx context.Context, task delegate.Task, r delegate.Result, known bool) {
	c := executionUsageCollector(ctx)
	if c == nil {
		return
	}
	backend, _ := ctx.Value(executionUsageBackendKey{}).(string)
	c.observePriced(firstNonEmpty(r.BackendName, backend), task, r, known)
}
func (c *ExecutionUsageCollector) observe(backend string, task delegate.Task, r delegate.Result) {
	c.observePriced(backend, task, r, false)
}
func (c *ExecutionUsageCollector) observePriced(backend string, task delegate.Task, r delegate.Result, known bool) {
	route := &store.InvocationUsageRoute{Scope: "invocation", Backend: backend, RequestedModel: task.Model, ReportedModel: r.EffectiveModel, SessionID: firstNonEmpty(r.SessionID, task.SessionID), SessionFingerprint: firstNonEmpty(r.SessionFingerprint, task.SessionFingerprint)}
	component := store.UsageComponent{Source: store.UsageDelegate, Route: route, Tokens: r.Tokens, UnreportedCalls: r.UnreportedCalls, CostKnown: known || r.CostUSD > 0, CostKind: store.UsageAdditiveCost, CostUSD: r.CostUSD}
	if !component.CostKnown {
		component.UnpricedTokens = r.Tokens
	}
	// A cumulative declaration without its session identity cannot safely become
	// additive spend. Preserve the declaration so validation names the missing key.
	if r.CostIsSessionTotal {
		component.CostKind = store.UsageSessionTotal
	}
	c.observeComponent(component)
}
func observeStructuredExtractionUsage(ctx context.Context, info StructuredExtractionInfo) {
	c := executionUsageCollector(ctx)
	if c == nil {
		return
	}
	component := store.UsageComponent{Source: store.UsageStructuredExtraction, Route: &store.InvocationUsageRoute{Scope: "invocation", Backend: info.Backend, RequestedModel: info.Model, Provider: info.Provider}, CallID: info.CallID, Outcome: info.Outcome, Tokens: info.InputTokens + info.OutputTokens, TokenSplit: &store.UsageTokenSplit{Input: info.InputTokens, Output: info.OutputTokens, CacheRead: info.CacheReadTokens, CacheWrite: info.CacheWriteTokens}, UnreportedCalls: info.UnreportedCalls, CostKnown: info.CostKnown, CostKind: store.UsageAdditiveCost, CostUSD: info.CostUSD}
	if !component.CostKnown {
		component.UnpricedTokens = component.Tokens
	}
	c.observeComponent(component)
}
func (c *ExecutionUsageCollector) observeComponent(component store.UsageComponent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.ensureCollectionID()
	component.Ordinal = uint64(len(c.components)) + 1
	if err := store.ValidateUsageComponent(component); err != nil {
		c.err = err
		return
	}
	if component.CallID != "" && c.calls[component.CallID] {
		c.err = errors.New("model: duplicate extraction call ID")
		return
	}
	next := c.total
	if component.Tokens > math.MaxInt-next.Tokens || component.UnpricedTokens > math.MaxInt-next.UnpricedTokens || component.UnreportedCalls > math.MaxInt-next.UnreportedCalls {
		c.err = errors.New("model: execution usage overflow")
		return
	}
	next.Tokens += component.Tokens
	next.UnpricedTokens += component.UnpricedTokens
	next.UnreportedCalls += component.UnreportedCalls
	if component.CostKind == store.UsageAdditiveCost {
		next.OtherCostUSD += component.CostUSD
		if math.IsInf(next.OtherCostUSD, 0) {
			c.err = errors.New("model: execution usage cost overflow")
			return
		}
	}
	c.components = append(c.components, store.CloneUsageComponents([]store.UsageComponent{component})[0])
	c.total = next
	if component.CallID != "" {
		if c.calls == nil {
			c.calls = map[string]bool{}
		}
		c.calls[component.CallID] = true
	}
}

// NewExecutionUsageCollector creates provenance before the invocation begins.
// The zero value remains usable and initializes its ID once on first use.
func NewExecutionUsageCollector() *ExecutionUsageCollector {
	return &ExecutionUsageCollector{collectionID: store.NewUsageCollectionID()}
}
func (c *ExecutionUsageCollector) ensureCollectionID() {
	if c.collectionID == "" {
		c.collectionID = store.NewUsageCollectionID()
	}
}
