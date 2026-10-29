// Package llmtypes defines iterion-owned types for the LLM generation layer.
// These types decouple iterion's tool registry and model registry from any
// specific LLM SDK (claw-code-go, etc.), breaking what would otherwise
// be a circular dependency between model/ and tool/.
package llmtypes

import (
	"context"
	"encoding/json"
)

// LLMTool is an iterion-owned tool definition passed to the LLM generation
// layer. It decouples iterion from any SDK's tool shape — both claw-code-go's
// sdk.Tool and the existing tool.ToolDef bridge through this type.
type LLMTool struct {
	// Name is the tool's identifier (sanitized for LLM APIs).
	Name string

	// Description explains what the tool does.
	Description string

	// InputSchema is the JSON Schema for the tool's input parameters.
	InputSchema json.RawMessage

	// Execute runs the tool with the given JSON input and returns the result text.
	Execute func(ctx context.Context, input json.RawMessage) (string, error)
}

// FatalToolError is the interface for tool errors that should immediately
// stop the generation loop (e.g. rate limits, credit exhaustion).
// Implementations return true from IsFatal() to signal that the error
// should not be retried or absorbed by the LLM tool loop.
type FatalToolError interface {
	error
	IsFatal() bool
}

// ModelCapabilities describes what features a model supports.
// This is iterion-owned — decoupled from any SDK's capability type.
type ModelCapabilities struct {
	// Reasoning, ToolCall and Temperature say whether the model supports
	// extended thinking, tool calling and a temperature parameter — or are
	// nil when no table answered them: a gateway id (openai_compatible/<id>)
	// whose catalog record carries no flags, a bare id with no provider to
	// resolve it against. A consumer reads nil as UNKNOWN and never as
	// false: a false that was really "nobody said" clamps an effort, hides
	// tools or refuses a launch the model may well serve (#2103). A curated
	// vendor entry answers all three (Known).
	Reasoning   *bool
	ToolCall    *bool
	Temperature *bool

	// Source is the table that answered this record, flags included
	// (CapabilitySource): what tells a reader a published fact from a
	// curated heuristic, and an unknown from a false.
	Source CapabilitySource

	// ContextWindow is the model's context window in tokens. Zero = unknown.
	ContextWindow int

	// MaxOutputTokens is the largest completion the model will emit, in
	// tokens, as published by the spec aggregator. Zero = the aggregator had
	// no figure, which callers must read as "unknown" and never as "no cap":
	// a caller that treated zero as unbounded would size a request against a
	// limit the provider will still enforce.
	MaxOutputTokens int

	// InputCostPerM and OutputCostPerM are the per-million-token prices in
	// USD as published by the spec aggregator. Zero = the aggregator had no
	// price, which callers must treat as "unknown" and never as free.
	//
	// The aggregator's prices were fetched and cached long before anything
	// read them: the cost estimator consults a different live source and
	// then a hand-maintained table, so a model the aggregator knew the price
	// of could still report no cost at all. Carrying the price here is what
	// lets the two be compared instead of silently disagreeing.
	InputCostPerM  float64
	OutputCostPerM float64
}

// Known is a flag a table answered.
func Known(v bool) *bool { return &v }

// CapabilitySource names the table a capability record came from.
type CapabilitySource string

const (
	// SourceCurated means the curated static table alone answered — the
	// online aggregator lacked the model, was disabled or offline.
	SourceCurated CapabilitySource = "curated"
	// SourceAggregator means the online model-spec aggregator had an entry
	// for the model, merged over the curated fallback.
	SourceAggregator CapabilitySource = "aggregator"
	// The gateway catalog's tables, spelled as pkg/backend/compatgw spells
	// them (a test keeps the two from drifting): the operator's own table,
	// the live models.dev registry, the embedded snapshot — and
	// SourceUnknown for an id no table carries, whose flags are nil.
	SourceOperator        CapabilitySource = "operator"
	SourceCatalogLive     CapabilitySource = "catalog-live"
	SourceCatalogSnapshot CapabilitySource = "catalog-snapshot"
	SourceUnknown         CapabilitySource = "unknown"
)
