package runner

import (
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/store"
	"math"
)

// observeStructuredExtraction consumes the helper's own route and usage. It
// never reads or changes per-node provenance and never adds the parent bill.
// Deduplication is scoped to this attempt's event collector, not a durable
// receipt for retrying the runner's eventual org/credential write.
func (m *metricsEmitter) observeStructuredExtraction(evt store.Event) {
	id, _ := evt.Data["call_id"].(string)
	backend, _ := evt.Data["backend"].(string)
	modelName, _ := evt.Data["model"].(string)
	if id == "" || modelName == "" || backend != delegate.BackendClaw {
		return
	}
	in, out, unreported := toFloat(evt.Data["input_tokens"]), toFloat(evt.Data["output_tokens"]), toFloat(evt.Data["unreported_calls"])
	known, _ := evt.Data["cost_known"].(bool)
	price := 0.0
	if known {
		price = toFloat(evt.Data["cost_usd"])
	}
	for _, n := range []float64{in, out, unreported, price} {
		if n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
			return
		}
	}
	// Event counts are integral. Reject malformed transport instead of
	// allowing float-to-int overflow to corrupt the totals.
	for _, n := range []float64{in, out, unreported} {
		if n >= math.Exp2(63) || n != math.Trunc(n) {
			return
		}
	}
	m.mu.Lock()
	if m.extractionSeen[id] {
		m.mu.Unlock()
		return
	}
	if m.extractionSeen == nil {
		m.extractionSeen = map[string]bool{}
	}
	m.extractionSeen[id] = true
	m.runCostUSD += price
	m.runInputTokens += int64(in)
	m.runOutputTokens += int64(out)
	k := routeKey{backend: backend, model: modelName, credential: observedCredential(evt.Data)}
	m.addRouteLocked(k, price, int64(in), int64(out), 0)
	t := m.byRoute[k]
	t.unreportedCalls += int64(unreported)
	m.byRoute[k] = t
	m.mu.Unlock()
	m.addTokens(backend, modelName, "input", in)
	m.addTokens(backend, modelName, "output", out)
	m.addTokens(backend, modelName, "cache_read", evt.Data["cache_read_tokens"])
	m.addTokens(backend, modelName, "cache_write", evt.Data["cache_write_tokens"])
	if known && m.reg != nil {
		m.reg.LLMCostUSDTotal.WithLabelValues(backend, normalizeModelLabel(modelName)).Add(price)
	}
}
