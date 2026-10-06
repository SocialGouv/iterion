package runner

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// metricsEmitter wraps a model.EventEmitter and taps llm_step_finished
// / delegate_finished / delegate_error events to keep the LLM token + cost
// counters up-to-date. The forward call to the underlying emitter happens
// regardless of metric outcome so write durability is unaffected.
//
// It also accumulates the run's own totals (cost + tokens) so the
// runner can charge the org's monthly usage bucket once at the end of
// the attempt — reg may be nil (no Prometheus) while the run totals
// still accumulate.
type metricsEmitter struct {
	inner model.EventEmitter
	reg   *metrics.Registry

	// modelByNode names the model each node's route is keyed on. Seeded by
	// delegate_started (the node's declared spec, provider included),
	// refined by llm_request (the id the call actually went to) and by
	// delegate_finished's effective_model — each re-qualified with the
	// declared provider when a backend reports the same model bare, see
	// routeModel. The step events carry no model of their own; this is
	// where they read it.
	//
	// declaredByNode keeps the declared spec beside it, because a bare
	// reported id needs the provider the declaration carried.
	//
	// stepsSeen marks a node whose CURRENT attempt produced
	// llm_step_finished events (each delegate_started opens a new attempt).
	// It decides whether a claw delegation total is a summary of steps
	// already counted or the only observation there is.
	//
	// priceByModel caches the resolved per-token rates so the
	// cost.EstimateUSD path (which hits claw's live registry — a disk
	// read + JSON parse each call) doesn't fire on every step event.
	// A workflow with 50 steps × 10 parallel branches would otherwise
	// serialise 500 disk hits through the metrics emitter mutex.
	mu             sync.Mutex
	modelByNode    map[string]string
	declaredByNode map[string]string
	stepsSeen      map[string]bool
	priceByModel   map[string]modelRate

	// declinedRoutes remembers the routes recordCredentialSpend could name
	// no credential for, so that decline is warned once per route.
	declinedRoutes map[routeKey]bool

	// Per-run accumulation for org metering. Covers both claw steps and
	// delegate calls; a node whose model the price table cannot price
	// contributes nothing, so this is a floor, not an exact invoice.
	runCostUSD         float64
	runInputTokens     int64
	runOutputTokens    int64
	runAggregateTokens int64

	// byRoute splits the same accumulation per (backend, model) — the unit
	// per-CREDENTIAL metering needs. One run can spend two credentials (a
	// claude_code forfait for the implementer, a platform codex key for the
	// plan review), and the run total cannot be attributed to either: it
	// would charge one for the other's calls. Keyed by the pair because the
	// model is what names the provider, and the backend alone does not (a
	// claw node can be pointed at any provider the registry knows).
	byRoute map[routeKey]routeTotals

	// sawAuthFailure records that the provider rejected this run's
	// credential at some point, even if the attempt did not END on that
	// error. Recovery converts an auth failure into a human pause, so by
	// the time the runner reports, execErr is ErrRunPaused and the
	// rejection is invisible — which left a dead lent credential first in
	// the pool's rotation, pausing run after run and burning a unit of its
	// donor's daily quota each time (measured on prod: 2 runs, credential
	// dead, pledge still "active").
	sawAuthFailure bool
}

// modelRate is the per-token cost (USD) for a given model, derived
// once via cost.EstimateUSD and cached. `known` distinguishes
// "table doesn't know this model" (skip the counter) from
// "rates are genuinely zero".
type modelRate struct {
	inputUSDPerToken  float64
	outputUSDPerToken float64
	known             bool
}

// routeKey names one (backend, model) pair a run spent on, and — for a
// delegate that stamps it — the credential route its session ran on
// (delegate_finished's `fingerprint`): two nodes of one backend and model
// can spend two credentials when their provider hints differ.
type routeKey struct{ backend, model, source string }

// routeTotals is one route's slice of the run's consumption.
type routeTotals struct {
	costUSD float64
	// inputTokens / outputTokens hold an OBSERVED split; aggregateTokens
	// holds a total the delegate could not split. A route fills one or the
	// other — see credusage.MonthlyUsage for why the distinction is public.
	inputTokens     int64
	outputTokens    int64
	aggregateTokens int64
	// unreportedCalls counts the route's calls whose usage the provider
	// did not report in full: their tokens above are a lower bound.
	unreportedCalls int64
}

// tokens is everything the route consumed. The three counters are disjoint —
// a delegate that cannot split its total fills only aggregateTokens — so a sum
// that omits it reports 0 for every claude_code route.
func (t routeTotals) tokens() int64 {
	return t.inputTokens + t.outputTokens + t.aggregateTokens
}

func newMetricsEmitter(inner model.EventEmitter, reg *metrics.Registry) *metricsEmitter {
	return &metricsEmitter{
		inner:          inner,
		reg:            reg,
		modelByNode:    make(map[string]string),
		declaredByNode: make(map[string]string),
		stepsSeen:      make(map[string]bool),
		priceByModel:   make(map[string]modelRate),
		byRoute:        make(map[routeKey]routeTotals),
		declinedRoutes: make(map[routeKey]bool),
	}
}

// routeModel names the model a node's route is keyed on, from the spec the
// node declared and the id a backend reported. A report that is itself a
// routing spec — its prefix is a provider the registry routes on, or a
// credential provider — names its own route (a fallback element's model).
// Any other report is an id as the backend called it: CLI backends report
// the bare model, and a model id may hold slashes of its own
// ("meta-llama/Llama-3.3-70B", "scaleway/gpt-oss-120b") — keyed on it, the
// route would fall to the backend's default wire and its tokens be charged
// to the Anthropic credential. So when the report is the declared model —
// exactly for a gateway id, up to its snapshot alias for a vendor id — the
// declared spec names the route; a different id is kept as reported.
func routeModel(declared, reported string) string {
	switch {
	case reported == "":
		return declared
	case declared != "" && reported == modelroute.Parse(declared).Wire:
		// The declared model as its wire id — even when that id's own first
		// segment names a provider ("anthropic/claude-sonnet-4.5" served by
		// OpenRouter): a reporter that knows the route names it in full.
		return declared
	case declared == "" || isRoutingSpec(reported):
		return reported
	case sameDeclaredModel(declared, reported):
		return declared
	}
	return reported
}

// servedRoute keys a delegation on the route its executor names — the chain
// element that served — refined by the model the backend reports when that is
// another model. The route's provider is a fact the executor knows; the id a
// backend reports names no provider of its own (OpenRouter's
// "anthropic/claude-sonnet-4.5"), only the model it ran.
func servedRoute(route, effective string) string {
	if effective == "" || effective == route || sameDeclaredModel(route, effective) {
		return route
	}
	if p := modelroute.Parse(route).Provider; p != "" {
		return p + "/" + effective
	}
	return effective
}

// isRoutingSpec reports whether a model string carries a provider prefix
// iterion routes or funds on, rather than a slash of the model id itself.
func isRoutingSpec(s string) bool {
	p := strings.ToLower(modelroute.Parse(s).Provider)
	return p != "" && (modelroute.IsRoutingProvider(p) || secrets.Provider(p).Valid())
}

// sameDeclaredModel reports whether a reported id is the declared model. A
// gateway id is opaque — two gateway models may share a last segment — so
// it matches its wire id exactly; a vendor id matches as a snapshot alias.
func sameDeclaredModel(declared, reported string) bool {
	if d := modelroute.Parse(declared); d.Gateway() {
		return reported == d.Wire
	}
	return delegate.SameModelID(declared, reported)
}

// noteDeclinedRoute records that no credential could be named for a route
// and reports whether this is the first time — the warning it gates is
// written once per route per attempt.
func (m *metricsEmitter) noteDeclinedRoute(k routeKey) (first bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.declinedRoutes[k] {
		return false
	}
	if m.declinedRoutes == nil {
		m.declinedRoutes = make(map[routeKey]bool)
	}
	m.declinedRoutes[k] = true
	return true
}

// addRouteLocked folds one observation into its (backend, model) bucket.
// Called with m.mu held, alongside the run-total accumulation it mirrors —
// the two must never diverge, so they are updated in the same critical
// section.
func (m *metricsEmitter) addRouteLocked(backend, modelName, source string, cost float64, in, out, aggregate int64) {
	if m.byRoute == nil {
		m.byRoute = make(map[routeKey]routeTotals)
	}
	k := routeKey{backend: backend, model: modelName, source: source}
	t := m.byRoute[k]
	t.costUSD += cost
	t.inputTokens += in
	t.outputTokens += out
	t.aggregateTokens += aggregate
	m.byRoute[k] = t
}

// RouteTotals snapshots what the run spent per (backend, model, source). The unit
// per-credential metering charges, because one run can draw on two
// credentials and the run total belongs to neither.
func (m *metricsEmitter) RouteTotals() map[routeKey]routeTotals {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[routeKey]routeTotals, len(m.byRoute))
	for k, v := range m.byRoute {
		out[k] = v
	}
	return out
}

// The executor writes through this emitter, but the ENGINE writes straight
// to its own store — and node_recovery, the only place a recovery-absorbed
// auth failure is still named, is an ENGINE event. The runner therefore
// also registers observe via runtime.WithEventObserver (see loop.go).

// rateForLocked returns the cached per-token rates for the given model,
// resolving once via cost.EstimateUSD. Called under m.mu.
func (m *metricsEmitter) rateForLocked(modelName string) modelRate {
	if r, ok := m.priceByModel[modelName]; ok {
		return r
	}
	const probe = 1_000_000
	inUSD := cost.EstimateUSD(modelName, probe, 0)
	outUSD := cost.EstimateUSD(modelName, 0, probe)
	r := modelRate{
		inputUSDPerToken:  inUSD / float64(probe),
		outputUSDPerToken: outUSD / float64(probe),
		known:             inUSD > 0 || outUSD > 0,
	}
	m.priceByModel[modelName] = r
	return r
}

func (m *metricsEmitter) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	m.observe(evt)
	return m.inner.AppendEvent(ctx, runID, evt)
}

// AppendPlanSnapshot forwards plan-snapshot persistence to the wrapped
// emitter when it implements model.PlanWriter (the Mongo cloud store now
// does). Without this explicit forward the capture hook's plain
// `emitter.(PlanWriter)` assertion runs against THIS wrapper — which,
// lacking the method, would yield nil and silently disable plan capture
// for every cloud run even once the store supports it. When the inner
// store is not a PlanWriter (a store without the seam) this is a benign
// no-op (wrote=false, no error) — identical to today's nil-planSink
// behaviour, and NOT the loud store-write failure path.
func (m *metricsEmitter) AppendPlanSnapshot(ctx context.Context, runID string, snap store.PlanSnapshot) (store.PlanSnapshot, bool, error) {
	pw, ok := m.inner.(model.PlanWriter)
	if !ok {
		return snap, false, nil
	}
	return pw.AppendPlanSnapshot(ctx, runID, snap)
}

// WriteTurn forwards per-LLM-turn checkpoint persistence to the wrapped
// emitter when it implements model.TurnWriter (the Mongo cloud store now
// does). Same rationale as AppendPlanSnapshot: without this explicit
// forward the capture hook's `emitter.(TurnWriter)` assertion runs against
// THIS wrapper — which, lacking the method, would yield nil and silently
// disable per-turn capture for every cloud run (breaking the studio
// timeline + fork-from-turn) even once the store supports it. When the
// inner store is not a TurnWriter this is a benign no-op (nil error),
// matching today's nil-turnSink skip behaviour.
func (m *metricsEmitter) WriteTurn(ctx context.Context, t *store.TurnCheckpoint) error {
	tw, ok := m.inner.(model.TurnWriter)
	if !ok {
		return nil
	}
	return tw.WriteTurn(ctx, t)
}

// WriteToolBlob forwards per-tool-call I/O sidecar persistence to the
// wrapped emitter when it implements model.ToolBlobWriter (the Mongo
// cloud store now does). Same rationale as WriteTurn: the capture hook's
// `emitter.(ToolBlobWriter)` assertion runs against THIS wrapper, so
// without the forward large tool outputs would silently fall back to the
// capped inline preview for every cloud run. When the inner store is not
// a ToolBlobWriter this signals "no sidecar" the same way a nil
// blobSink does — persistToolPayload then keeps the capped inline body.
func (m *metricsEmitter) WriteToolBlob(ctx context.Context, runID, toolUseID, kind string, body []byte) (int64, error) {
	bw, ok := m.inner.(model.ToolBlobWriter)
	if !ok {
		return 0, fmt.Errorf("runner: inner store does not persist tool blobs")
	}
	return bw.WriteToolBlob(ctx, runID, toolUseID, kind, body)
}

// RecordNodeServed forwards the last (backend, model) that served a
// node onto the wrapped store. Same rationale as WriteTurn: the capture
// hook's `emitter.(NodeServedRecorder)` assertion runs against THIS
// wrapper, so without the forward every cloud run would silently skip
// writing NodesServed onto run.json.
func (m *metricsEmitter) RecordNodeServed(ctx context.Context, runID, nodeID string, served store.NodeServed) error {
	r, ok := m.inner.(model.NodeServedRecorder)
	if !ok {
		return nil
	}
	return r.RecordNodeServed(ctx, runID, nodeID, served)
}

func (m *metricsEmitter) observe(evt store.Event) {
	switch evt.Type {
	case store.EventDelegateStarted:
		if evt.NodeID == "" {
			return
		}
		declared, _ := evt.Data["declared_model"].(string)
		m.mu.Lock()
		// A new attempt of the node: whether its steps are observed is
		// decided afresh.
		delete(m.stepsSeen, evt.NodeID)
		if declared != "" {
			m.declaredByNode[evt.NodeID] = declared
			m.modelByNode[evt.NodeID] = declared
		}
		m.mu.Unlock()
	case store.EventLLMRequest:
		if model, _ := evt.Data["model"].(string); model != "" && evt.NodeID != "" {
			// A request that names its wire id beside its model says the model
			// is the routing spec it was resolved from — the route itself, even
			// when its wire id happens to equal the declared one.
			wire, _ := evt.Data["wire_model"].(string)
			m.mu.Lock()
			if wire != "" {
				m.modelByNode[evt.NodeID] = model
			} else {
				m.modelByNode[evt.NodeID] = routeModel(m.declaredByNode[evt.NodeID], model)
			}
			m.mu.Unlock()
		}
	case store.EventLLMStepFinished:
		const backend = "claw"
		inputT := toFloat(evt.Data["input_tokens"])
		outputT := toFloat(evt.Data["output_tokens"])

		// Single critical section: resolve the per-node model name,
		// accumulate run-level token + cost totals, and compute the
		// per-model cost delta against the cached rate (rateForLocked
		// requires the lock held by its caller). Prometheus writes and
		// the addTokens helper run AFTER the unlock — counter Add is
		// atomic on the vec, addTokens reads only its locals.
		m.mu.Lock()
		if evt.NodeID != "" {
			m.stepsSeen[evt.NodeID] = true
		}
		modelName := m.modelByNode[evt.NodeID]
		if modelName == "" {
			modelName = "unknown"
		}
		m.runInputTokens += int64(inputT)
		m.runOutputTokens += int64(outputT)
		var costDelta float64
		if modelName != "unknown" {
			rate := m.rateForLocked(modelName)
			if rate.known {
				if c := inputT*rate.inputUSDPerToken + outputT*rate.outputUSDPerToken; c > 0 {
					m.runCostUSD += c
					costDelta = c
				}
			}
		}
		m.addRouteLocked(backend, modelName, "", costDelta, int64(inputT), int64(outputT), 0)
		if unreported, _ := evt.Data["usage_unreported"].(bool); unreported {
			k := routeKey{backend: backend, model: modelName}
			t := m.byRoute[k]
			t.unreportedCalls++
			m.byRoute[k] = t
		}
		m.mu.Unlock()

		m.addTokens(backend, modelName, "input", evt.Data["input_tokens"])
		m.addTokens(backend, modelName, "output", evt.Data["output_tokens"])
		m.addTokens(backend, modelName, "cache_read", evt.Data["cache_read_tokens"])
		m.addTokens(backend, modelName, "cache_write", evt.Data["cache_write_tokens"])
		// LLMCostUSDTotal: unknown models leave the counter untouched so
		// observers can tell "no data" from "$0" via the absence of
		// samples.
		if costDelta > 0 && m.reg != nil {
			m.reg.LLMCostUSDTotal.WithLabelValues(backend, normalizeModelLabel(modelName)).Add(costDelta)
		}
	case store.EventNodeRecovery:
		// The engine already classified the failure here (typed, from
		// recovery.Classify) — this is the only place the reason survives
		// once recovery turns an auth rejection into a pause.
		if code, _ := evt.Data["code"].(string); code == string(runtime.ErrCodeAuthFailed) {
			m.mu.Lock()
			m.sawAuthFailure = true
			m.mu.Unlock()
		}
	case store.EventDelegateStall:
		// The F8.1 class: a served model blocking on an orchestration wait
		// it never armed. Counted per outcome so "recovered in place" and
		// "aborted, node restarted" stay distinguishable.
		if m.reg == nil {
			return
		}
		backend, _ := evt.Data["backend"].(string)
		if backend == "" {
			backend = "delegate"
		}
		modelName, _ := evt.Data["model"].(string)
		if modelName == "" {
			modelName = m.lookupModel(evt.NodeID)
		}
		outcome, _ := evt.Data["outcome"].(string)
		if outcome == "" {
			outcome = "aborted"
		}
		m.reg.DelegateIdleDeadlockTotal.WithLabelValues(backend, normalizeModelLabel(modelName), outcome).Inc()
	case store.EventDelegateBackground:
		if m.reg == nil {
			return
		}
		backend, _ := evt.Data["backend"].(string)
		if backend == "" {
			backend = "delegate"
		}
		phase, _ := evt.Data["phase"].(string)
		if phase == "" {
			phase = "unknown"
		}
		m.reg.DelegateBackgroundTotal.WithLabelValues(backend, phase).Inc()
	case store.EventDelegateError:
		// A delegation that dies (timeout, rate limit, transport error)
		// burned its tokens and cost exactly like a finished one — booking
		// only the finished branch left every failed attempt unbilled in
		// RunTotals, and the org's monthly bucket under-read what the
		// credential actually spent. Same accounting as the finished
		// branch below: one aggregate token count to the aggregate
		// direction, cost from the delegate's own figure, the claw
		// summarised guard so a loop whose llm_step_finished steps were
		// already priced is not charged twice when its aggregate then
		// errors. The two events are mutually exclusive per attempt
		// (the executor fires one or the other), so no sum double-counts.
		backend, _ := evt.Data["backend"].(string)
		if backend == "" {
			backend = "delegate"
		}
		tokensF := toFloat(evt.Data["tokens"])
		effective, _ := evt.Data["effective_model"].(string)
		served, _ := evt.Data["route_model"].(string)
		var source string
		if backend == delegate.BackendClaudeCode {
			source, _ = evt.Data["fingerprint"].(string)
		}

		m.mu.Lock()
		switch {
		case served != "" && evt.NodeID != "":
			m.modelByNode[evt.NodeID] = servedRoute(served, effective)
		case effective != "" && evt.NodeID != "":
			m.modelByNode[evt.NodeID] = routeModel(m.declaredByNode[evt.NodeID], effective)
		}
		modelName := m.modelByNode[evt.NodeID]
		summarised := backend == "claw" && m.stepsSeen[evt.NodeID]
		var costDelta float64
		if !summarised {
			costDelta = toFloat(evt.Data["cost_usd"])
			if costDelta == 0 && backend == "claw" && tokensF > 0 && modelName != "" {
				if rate := m.rateForLocked(modelName); rate.known {
					costDelta = tokensF * rate.inputUSDPerToken
				}
			}
			m.runAggregateTokens += int64(tokensF)
			m.runCostUSD += costDelta
			m.addRouteLocked(backend, modelName, source, costDelta, 0, 0, int64(tokensF))
		}
		m.mu.Unlock()
		if summarised {
			return
		}
		m.addTokens(backend, modelName, "aggregate", evt.Data["tokens"])
		if costDelta > 0 && m.reg != nil {
			m.reg.LLMCostUSDTotal.WithLabelValues(backend, normalizeModelLabel(modelName)).Add(costDelta)
		}
	case store.EventDelegateFinished:
		backend, _ := evt.Data["backend"].(string)
		if backend == "" {
			backend = "delegate"
		}
		tokensF := toFloat(evt.Data["tokens"])
		effective, _ := evt.Data["effective_model"].(string)
		served, _ := evt.Data["route_model"].(string)
		// Only claude_code's labels name a credential slot the ledger reads
		// (routeSlot); keying another backend's route on its label would only
		// split one route in two.
		var source string
		if backend == delegate.BackendClaudeCode {
			source, _ = evt.Data["fingerprint"].(string)
		}

		// Single critical section: resolve the per-node model name and
		// accumulate the aggregated token count. Prometheus write
		// happens after the unlock via addTokens (counter Add is atomic).
		m.mu.Lock()
		switch {
		case served != "" && evt.NodeID != "":
			m.modelByNode[evt.NodeID] = servedRoute(served, effective)
		case effective != "" && evt.NodeID != "":
			m.modelByNode[evt.NodeID] = routeModel(m.declaredByNode[evt.NodeID], effective)
		}
		modelName := m.modelByNode[evt.NodeID]
		// claw dispatches through the same observability hook as the CLI
		// delegates, so when its LLM loop has been observed step by step
		// (llm_step_finished, counted and priced above) this delegation
		// total is the SUM of those steps: counting it again would charge
		// every claw run twice — an org's monthly cap tripping at half its
		// budget, a lending donor drained at twice the rate they agreed to.
		// The steps are the evidence, not the backend name: a claw loop in a
		// sandbox container relays them across the IPC, and an in-container
		// runner too old to relay them leaves this total as the only
		// observation there is — which is then booked like any delegate's.
		summarised := backend == "claw" && m.stepsSeen[evt.NodeID]
		var costDelta float64
		if !summarised {
			// The delegate already priced this call (its own CLI figure, the
			// token estimate a subscription session falls back to, or claw's
			// in-container annotation) and carries the result on the event;
			// accumulating it is what keeps a claude_code run from reporting
			// $0 for the whole attempt — the number every consumer of
			// RunTotals (org monthly cost cap, credential-pool quota) meters
			// on. An absent key means the delegate's price sources did not
			// know the model. claw's sources were consulted in the container,
			// where no cache may exist yet, so the host prices the aggregate
			// off its own — at the INPUT rate, a floor: one aggregate count
			// cannot be split into input and output, and output is dearer. A
			// model no source prices stays at zero: unknown, never free.
			costDelta = toFloat(evt.Data["cost_usd"])
			if costDelta == 0 && backend == "claw" && tokensF > 0 && modelName != "" {
				if rate := m.rateForLocked(modelName); rate.known {
					costDelta = tokensF * rate.inputUSDPerToken
				}
			}
			m.runAggregateTokens += int64(tokensF)
			m.runCostUSD += costDelta
			// The delegate reports ONE token count and no split. It goes to
			// the aggregate counter rather than to input: a sum stays exact
			// either way, and only this way does a reader of the public
			// per-credential endpoint see "not split" instead of a
			// confident, wrong input figure (#992).
			m.addRouteLocked(backend, modelName, source, costDelta, 0, 0, int64(tokensF))
		}
		m.mu.Unlock()
		if summarised {
			return
		}

		// Delegate events report a single aggregated token count. It gets
		// its own direction label: summing every direction stays exact,
		// and a dashboard reading `direction="input"` is no longer served
		// output tokens under that name.
		m.addTokens(backend, modelName, "aggregate", evt.Data["tokens"])
		if costDelta > 0 && m.reg != nil {
			m.reg.LLMCostUSDTotal.WithLabelValues(backend, normalizeModelLabel(modelName)).Add(costDelta)
		}
	}
}

// RunTotals snapshots the run's accumulated LLM consumption — what
// the runner charges to the org's monthly usage bucket.
//
// The three token counts are disjoint: a run mixing an in-process claw loop
// with a CLI delegate fills the directional pair AND the aggregate, and its
// true total is the sum of all three.
func (m *metricsEmitter) RunTotals() (costUSD float64, inputTokens, outputTokens, aggregateTokens int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runCostUSD, m.runInputTokens, m.runOutputTokens, m.runAggregateTokens
}

// SawAuthFailure reports whether the provider rejected this run's
// credential at any point during the attempt.
func (m *metricsEmitter) SawAuthFailure() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sawAuthFailure
}

func (m *metricsEmitter) lookupModel(nodeID string) string {
	if nodeID == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.modelByNode[nodeID]
}

func (m *metricsEmitter) addTokens(backend, modelName, direction string, raw any) {
	n := toFloat(raw)
	if n <= 0 || backend == "" || m.reg == nil {
		return
	}
	m.reg.LLMTokensTotal.WithLabelValues(backend, normalizeModelLabel(modelName), direction).Add(n)
}

// normalizeModelLabel bounds the prometheus `model` label cardinality
// by stripping trailing date-style version suffixes (e.g. "-20260427",
// "-2026-04-27") and truncating overlong identifiers. Without this,
// label values churn every time a provider ships a new dated snapshot,
// growing the time-series set without bound.
func normalizeModelLabel(s string) string {
	if s == "" {
		return "unknown"
	}
	// Strip trailing -<digits[-digits...]> patterns.
	for {
		i := strings.LastIndexByte(s, '-')
		if i < 0 || i == len(s)-1 {
			break
		}
		tail := s[i+1:]
		alldigit := true
		for _, r := range tail {
			if r < '0' || r > '9' {
				alldigit = false
				break
			}
		}
		if !alldigit {
			break
		}
		s = s[:i]
	}
	const maxLen = 64
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return s
}

// toFloat coerces the JSON-decoded scalar (always float64 in Go's
// encoding/json) to a non-negative float64, returning 0 when the
// value is missing, nil, or not a number.
func toFloat(raw any) float64 {
	switch v := raw.(type) {
	case float64:
		if v < 0 {
			return 0
		}
		return v
	case int:
		if v < 0 {
			return 0
		}
		return float64(v)
	case int64:
		if v < 0 {
			return 0
		}
		return float64(v)
	}
	return 0
}
