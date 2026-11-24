package model

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/store"
)

// StructuredExtractionInfo is one helper invocation's settlement, including
// its internal requests. It contains no answer, prompt, error text or credential material.
// CallID identifies this event, not a budget/daily initial or round receipt:
// those receipts already include this usage and must not be charged again by
// the route meter. Credential records the resolved non-secret spend identity.
type StructuredExtractionInfo struct {
	Credential                                                   credroute.Spend
	CallID, Backend, Model, Provider                             string
	InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens int
	UnreportedCalls                                              int
	CostUSD                                                      float64
	CostKnown                                                    bool
	Outcome                                                      string
}

func newStructuredExtraction(ctx context.Context, model string, u Usage, success bool) StructuredExtractionInfo {
	price, known := cost.EstimateUSDWithCacheKnownInEnv(ctx, model, u.InputTokens, u.OutputTokens, u.CacheReadTokens)
	outcome := "error"
	if success {
		outcome = "success"
	}
	return StructuredExtractionInfo{CallID: rand.Text(), Backend: delegate.BackendClaw, Model: model,
		Provider: string(clawPinnedProvider(model)), InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens,
		UnreportedCalls: u.UnreportedCalls, CostUSD: price, CostKnown: known, Outcome: outcome}
}

// extractionClient fences a delayed cold-watchdog goroutine after settlement.
// Closing before the client seam is entered produces no event; entering the
// seam records an attempted call even when StreamResponse returns an error.
// Tokens come only from TotalUsage; response observation preserves unknown
// usage on cancellation after entry, including before a stream opens.
type extractionClient struct {
	inner                api.APIClient
	mu                   sync.Mutex
	entered, responded   int
	unreportedAdjustment int
	closed               bool
}

func (c *extractionClient) StreamResponse(ctx context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	c.entered++
	c.mu.Unlock()
	return c.inner.StreamResponse(ctx, req)
}
func (c *extractionClient) finish() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return c.entered > 0
}

// observeResponse is called once per internal turn, before the next turn.
// Direct generation regards errors before a stream opens as unbilled. A
// cancelled/deadline request which entered the client has an unknown bill,
// while a cancellation before that seam must not manufacture one.
func (c *extractionClient) observeResponse(info ResponseInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entered := c.entered > c.responded
	if errors.Is(info.Error, context.Canceled) || errors.Is(info.Error, context.DeadlineExceeded) {
		c.closed = true
		if entered && info.Usage.UnreportedCalls == 0 {
			c.unreportedAdjustment++
		}
	}
	// A cold watchdog can expire before its scheduled client goroutine starts.
	// Generation marks one abandoned call in that case; fence the delayed
	// goroutine and remove that phantom observation from this helper's usage.
	var idle *StreamIdleError
	if errors.As(info.Error, &idle) && !entered {
		c.closed = true
		c.unreportedAdjustment--
	}
	c.responded = c.entered
}

func (c *extractionClient) settleUsage(u Usage) Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	u.UnreportedCalls += c.unreportedAdjustment
	return u
}

func (h *storeHooks) onStructuredExtraction(nodeID string, i StructuredExtractionInfo) {
	data := map[string]any{"call_id": i.CallID, "backend": i.Backend, "model": i.Model, "provider": i.Provider,
		"input_tokens": i.InputTokens, "output_tokens": i.OutputTokens,
		"cache_read_tokens": i.CacheReadTokens, "cache_write_tokens": i.CacheWriteTokens,
		"unreported_calls": i.UnreportedCalls, "cost_known": i.CostKnown, "outcome": i.Outcome}
	credroute.PutEventKeys(data, i.Credential)
	if i.CostKnown {
		data["cost_usd"] = i.CostUSD
	} else if i.InputTokens+i.OutputTokens > 0 {
		data["cost_unpriced"] = true
	}
	// A cancelled call still incurred usage. Like the runner's final accounting
	// writes, this best-effort event gets a detached, bounded persistence attempt.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), 5*time.Second)
	defer cancel()
	if _, err := h.emitter.AppendEvent(ctx, h.runID, store.Event{Type: store.EventStructuredExtraction, RunID: h.runID, NodeID: nodeID, Data: data}); err != nil {
		h.logger.Warn("[%s] structured extraction settlement could not be persisted", nodeID)
	}
}
