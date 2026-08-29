package model

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

// Public, operator-facing view over the capability resolver.
//
// capabilitiesForModel() and the specRegistry are unexported and live on the
// runtime hot path. The `iterion models` CLI needs to (a) resolve the same
// ModelCapabilities the runtime would use, (b) report WHERE each value came
// from (the online aggregator vs the curated static fallback), and (c) force a
// cache refresh. This file exposes exactly that surface without duplicating the
// resolver — ResolveCapabilities calls capabilitiesForModel() and asks the same
// registry whether the aggregator contributed.

// CapabilitySource records where a resolved capability value came from.
type CapabilitySource string

const (
	// SourceAggregator means the online model-spec aggregator (models.dev,
	// cached under ~/.iterion) had an entry for the model and was merged over
	// the curated fallback.
	SourceAggregator CapabilitySource = "aggregator"
	// SourceCurated means the curated static table in capabilities.go was the
	// sole source — the aggregator lacked the model, was disabled, or offline.
	SourceCurated CapabilitySource = "curated"
)

// ResolvedCapabilities is the CLI-facing resolution result: the same
// ModelCapabilities the runtime would compute, plus the resolution source.
type ResolvedCapabilities struct {
	Provider      string           `json:"provider"`
	Model         string           `json:"model"`
	Spec          string           `json:"spec"`
	Source        CapabilitySource `json:"source"`
	Reasoning     bool             `json:"reasoning"`
	ToolCall      bool             `json:"tool_call"`
	Temperature   bool             `json:"temperature"`
	ContextWindow int              `json:"context_window"`
	// MaxOutputTokens is the aggregator's published completion cap. Zero
	// means the aggregator had no figure — never "uncapped".
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	// Per-million-token prices as published by the aggregator. Zero means
	// the aggregator had no price — never "free". Surfaced so the pricing
	// audit can hold this figure next to the committed table.
	InputCostPerM  float64 `json:"input_cost_per_m,omitempty"`
	OutputCostPerM float64 `json:"output_cost_per_m,omitempty"`
}

// ResolveCapabilities resolves capabilities for an explicit provider + model ID,
// exactly as the runtime does via capabilitiesForModel(), and additionally
// reports whether the dynamic aggregator contributed (Source). It performs no
// blocking network fetch — call RefreshModelSpecs first to force one.
func ResolveCapabilities(provider, modelID string) ResolvedCapabilities {
	caps := capabilitiesForModel(provider, modelID)
	src := SourceCurated
	if _, ok := modelspecs.Default().Lookup(provider, modelID); ok {
		src = SourceAggregator
	}
	return ResolvedCapabilities{
		Provider:        provider,
		Model:           modelID,
		Spec:            provider + "/" + modelID,
		Source:          src,
		Reasoning:       caps.Reasoning,
		ToolCall:        caps.ToolCall,
		Temperature:     caps.Temperature,
		ContextWindow:   caps.ContextWindow,
		MaxOutputTokens: caps.MaxOutputTokens,
		InputCostPerM:   caps.InputCostPerM,
		OutputCostPerM:  caps.OutputCostPerM,
	}
}

// BareModelSpec is everything that can be resolved for a model id carrying NO
// provider — the shape a `.bot` may pin (`model: "claude-opus-5"`) and the one
// a studio picker holds mid-typing.
//
// It deliberately carries no capability flags. Those resolve through a curated
// per-provider branch (curatedCapabilities), and without a provider there is no
// branch to take; inventing one would report another vendor's answer as this
// model's. The aggregator's bare index is consensus-filtered across publishers,
// so the numbers below are the fields it can honestly supply.
// It is a plain Go value, not a wire type — every caller projects it onto its
// own response shape — so it carries no JSON tags to imply otherwise.
type BareModelSpec struct {
	Model string
	// Found is false when the aggregator has no entry. The zero fields below
	// then mean "nothing known", which is the same thing every zero in this
	// area means: unknown, never none.
	Found           bool
	ContextWindow   int
	MaxOutputTokens int
	InputCostPerM   float64
	OutputCostPerM  float64
}

// ResolveBareModel resolves an unqualified model id against the aggregator's
// bare index. Found is false when the aggregator has no entry — the caller
// then has nothing to show, which is the honest answer rather than a curated
// guess made under an invented provider.
func ResolveBareModel(modelID string) BareModelSpec {
	out := BareModelSpec{Model: modelID}
	spec, ok := modelspecs.Default().LookupBare(modelID)
	if !ok {
		return out
	}
	out.Found = true
	out.ContextWindow = spec.ContextWindow
	out.MaxOutputTokens = spec.MaxOutputTokens
	out.InputCostPerM = spec.InputCostPerM
	out.OutputCostPerM = spec.OutputCostPerM
	return out
}

// ResolveSpec resolves a "provider/model-id" spec string. It returns an error
// (suitable for surfacing as a user-input error) when the spec is malformed.
func ResolveSpec(spec string) (ResolvedCapabilities, error) {
	provider, modelID, err := ParseModelSpec(spec)
	if err != nil {
		return ResolvedCapabilities{}, err
	}
	return ResolveCapabilities(provider, modelID), nil
}

// RefreshModelSpecs force-refetches the model-spec cache synchronously via the
// existing resolver, blocking until the fetch completes or fails. On success the
// on-disk cache (~/.iterion) and the in-process table are both updated; on
// failure the prior cache is left untouched and the error is returned. This is
// the `iterion models --refresh` path.
func RefreshModelSpecs(ctx context.Context) error {
	return modelspecs.Default().Refresh(ctx)
}

// KnownModelSpecs returns a representative set of model specs to list when the
// `iterion models` command is invoked without an explicit model. It spans the
// providers the resolver knows curated heuristics for (Anthropic Claude, the
// GLM family served via the Anthropic-compatible endpoint, and OpenAI) so the
// listing exercises every curated branch. It is a display convenience, not an
// exhaustive catalogue — any "provider/model-id" can be passed explicitly.
func KnownModelSpecs() []string {
	return []string{
		"anthropic/claude-opus-5",
		"anthropic/claude-sonnet-5",
		"anthropic/claude-opus-4-8",
		"anthropic/claude-sonnet-4-6",
		"anthropic/claude-haiku-4-5",
		"anthropic/glm-5.2",
		"anthropic/glm-5.1",
		"anthropic/glm-4.6",
		"openai/gpt-5.5",
		"openai/gpt-5.4-mini",
		"openai/o3",
	}
}
