package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/modelcatalog"
)

// handleModels serves the model registry: every model iterion knows about,
// crossed with the credentials a *run from this request* would receive, the
// capabilities each model has, and what it costs.
//
// Local studio evaluates the host process (the same cached detect.Report
// that backs /api/backends/detect). Cloud evaluates the authenticated
// tenant's launch tiers — BYOK, user/org OAuth-forfait, platform — never
// the control-plane env. A control-plane key the run will not get must
// not make a model look reachable; a tenant key absent from that env
// must not make it look unreachable.
//
// Query params:
//
//	spec=provider/model  repeatable; ADDS to the known set (so a caller can ask
//	                     about a model the curated list omits — e.g. a node's
//	                     own DSL default — without narrowing the picker to the
//	                     models already in use, which is the one list from
//	                     which no new choice can be made)
//	refresh=1            re-probe host credentials AND re-fetch the model-spec
//	                     aggregator before answering (local only: cloud
//	                     presence is read from the stores each request)
//
// Read-only, and reveals only capability + credential SOURCE names
// (e.g. "ANTHROPIC_API_KEY") — never a credential value.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1"
	opts := modelcatalog.Options{
		ExtraSpecs: dedupeSpecs(r.URL.Query()["spec"]),
	}
	var refreshErr error
	if refresh {
		refreshErr = s.refreshModels(r.Context())
	}

	if s.cfg.Mode == "cloud" {
		opts.Reachability = modelcatalog.ReachabilityCloud
		opts.UnprovenIsUnknown = true
		var presence modelcatalog.CloudPresence
		if id, ok := auth.FromContext(r.Context()); ok {
			presence = s.probeCloudRunPresence(r.Context(), id)
		}
		report := modelcatalog.ReportFromCloudPresence(presence)
		opts.Report = &report
	} else {
		s.detectorOnce.Do(func() {
			s.detector = detect.NewCachedDetector(backendDetectTTL)
		})
		report := s.detector.Get(r.Context())
		opts.Reachability = modelcatalog.ReachabilityLocal
		opts.Report = &report
	}

	cat, err := modelcatalog.Build(r.Context(), opts)
	if err != nil {
		// Defensive: the handler only ever passes ExtraSpecs, which Build
		// degrades into cat.InvalidSpecs rather than raising. A malformed
		// ?spec= must not 400 the whole registry — LaunchView asks about
		// every node's DSL default at once, so one bot pinning a bad
		// `model:` would otherwise render an empty picker for every model
		// on the host.
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	if refresh {
		cat.Refreshed = true
		if refreshErr != nil {
			cat.RefreshError = refreshErr.Error()
		}
	}
	// A non-cloud server can still publish runs to a remote queue. In that
	// hybrid shape, credentials are sealed per run and consumed in a runner
	// pod, so advertising THIS process's detect.Report would be misleading.
	// Cloud mode is different: the catalog above already comes from the
	// authenticated tenant's launch tiers and must retain that reachability.
	if s.queue != nil && s.cfg.Mode != "cloud" {
		const reason = "cloud runs resolve credentials at launch, not from this API pod"
		cat.RecommendedSpec = ""
		cat.ResolvedDefaultBackend = ""
		cat.Backends = nil
		for i := range cat.Models {
			cat.Models[i].Usable = false
			cat.Models[i].UnusableReason = reason
			cat.Models[i].Backends = nil
			cat.Models[i].CredentialSource = ""
			cat.Models[i].Recommended = false
		}
	}
	s.writeJSONFor(w, r, cat)
}

// refreshModels runs the refresh work that is independent of a request's
// extra model specs once per overlapping burst. Waiter cancellation affects
// only that request; the shared refresh continues for the other waiters.
type modelsRefreshFlight struct {
	done    chan struct{}
	err     error
	waiters int
}

func (s *Server) refreshModels(ctx context.Context) error {
	s.modelsRefreshMu.Lock()
	flight := s.modelsRefresh
	if flight == nil {
		flight = &modelsRefreshFlight{done: make(chan struct{})}
		s.modelsRefresh = flight
		go s.runModelsRefresh(flight)
	}
	flight.waiters++
	s.modelsRefreshMu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-flight.done:
		return flight.err
	}
}

func (s *Server) runModelsRefresh(flight *modelsRefreshFlight) {
	if s.cfg.Mode != "cloud" {
		s.detectorOnce.Do(func() {
			s.detector = detect.NewCachedDetector(backendDetectTTL)
		})
		// The desktop hook may mutate env, so it must precede invalidation and
		// the one forced probe shared by every waiter.
		if s.OnForceRefresh != nil {
			s.OnForceRefresh()
		}
		s.detector.Invalidate()
		s.detector.Get(context.Background())
	}
	refreshFn := s.modelSpecsRefresh
	if refreshFn == nil {
		refreshFn = model.RefreshModelSpecs
	}
	flight.err = refreshFn(context.Background())

	s.modelsRefreshMu.Lock()
	// Close this generation while holding the lock, before allowing a later
	// request to install a new channel.
	close(flight.done)
	if s.modelsRefresh == flight {
		s.modelsRefresh = nil
	}
	s.modelsRefreshMu.Unlock()
}

// dedupeSpecs trims, drops empties and removes duplicates while preserving the
// caller's order. LaunchView asks about every LLM node's default at once, and
// a bot with twenty nodes on one model should not produce twenty rows.
func dedupeSpecs(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		// Repeated ?spec= params and a single comma-separated one are both
		// natural to write; accept either rather than 400 on the wrong guess.
		for _, s := range strings.Split(raw, ",") {
			s = strings.TrimSpace(s)
			if s == "" || seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// modelCapabilitiesResponse is what a model picker needs to caption its
// selection: the limits and the published price, plus where they came from.
//
// The capability flags are POINTERS because the bare-id path genuinely cannot
// answer them — they resolve through a curated per-provider branch, and a bare
// id names no provider. Omitting beats defaulting: a `false` a client renders
// as "no tool calling" would be a claim nothing supports.
type modelCapabilitiesResponse struct {
	Spec     string `json:"spec"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model"`
	// Source is "aggregator" when the online model-spec table contributed,
	// "curated" when only the static heuristics did. A client caches an
	// aggregator answer indefinitely and a curated one briefly: the curated
	// one is what a cold lookup returns while the background refresh is still
	// in flight, so it can still improve.
	Source          string  `json:"source"`
	ContextWindow   int     `json:"context_window"`
	MaxOutputTokens int     `json:"max_output_tokens"`
	InputCostPerM   float64 `json:"input_cost_per_m"`
	OutputCostPerM  float64 `json:"output_cost_per_m"`
	Reasoning       *bool   `json:"reasoning,omitempty"`
	ToolCall        *bool   `json:"tool_call,omitempty"`
	Temperature     *bool   `json:"temperature,omitempty"`
}

// handleModelCapabilities answers
//
//	GET /api/model-capabilities?spec=<qualified-or-bare>[&provider=<name>]
//
// with the limits, published prices and resolution source for a model.
//
// A BARE id is served, not rejected. `.bot` files pin bare ids and a studio
// picker holds one while the operator types, so a 400 there would blank the
// capability caption in the common case. It resolves against the aggregator's
// bare (consensus-filtered) index and returns only the fields that index can
// honestly support.
//
// `provider` is honoured when the caller genuinely has one. It is NOT inferred
// from a node's backend: backend is not provider — claw and claude_code both
// serve anthropic, pi serves some three dozen — so inferring would attach one
// vendor's numbers to another's model.
func (s *Server) handleModelCapabilities(w http.ResponseWriter, r *http.Request) {
	spec := strings.TrimSpace(r.URL.Query().Get("spec"))
	if spec == "" {
		httpError(w, http.StatusBadRequest, "missing required query param: spec")
		return
	}
	if len(spec) > maxResolveLiteralBytes {
		httpError(w, http.StatusBadRequest, "spec too long")
		return
	}

	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	if provider != "" && !strings.Contains(spec, "/") {
		spec = provider + "/" + spec
	}

	if strings.Contains(spec, "/") {
		rc, err := model.ResolveSpec(spec)
		if err != nil {
			httpError(w, http.StatusBadRequest, "%v", err)
			return
		}
		writeJSON(w, modelCapabilitiesResponse{
			Spec:            rc.Spec,
			Provider:        rc.Provider,
			Model:           rc.Model,
			Source:          string(rc.Source),
			ContextWindow:   rc.ContextWindow,
			MaxOutputTokens: rc.MaxOutputTokens,
			InputCostPerM:   rc.InputCostPerM,
			OutputCostPerM:  rc.OutputCostPerM,
			Reasoning:       &rc.Reasoning,
			ToolCall:        &rc.ToolCall,
			Temperature:     &rc.Temperature,
		})
		return
	}

	bare := model.ResolveBareModel(spec)
	resp := modelCapabilitiesResponse{
		Spec:            spec,
		Model:           spec,
		Source:          string(model.SourceCurated),
		ContextWindow:   bare.ContextWindow,
		MaxOutputTokens: bare.MaxOutputTokens,
		InputCostPerM:   bare.InputCostPerM,
		OutputCostPerM:  bare.OutputCostPerM,
	}
	if bare.Found {
		resp.Source = string(model.SourceAggregator)
	}
	writeJSON(w, resp)
}
