package server

import (
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/model"
)

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
