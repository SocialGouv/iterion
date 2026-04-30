package model

import "context"

// TemplateData carries runtime state needed to resolve prompt template
// references in the `outputs.*`, `loop.*`, `artifacts.*`, and `run.*`
// namespaces. The runtime engine populates a snapshot of its current
// state before each node execution and attaches it via WithTemplateData
// so the executor can render prompt bodies that reference upstream
// outputs, loop counters, and previous-iteration snapshots.
//
// All fields are read-only views — callers must not mutate them.
type TemplateData struct {
	// Outputs is the per-node output map. Keys are node IDs.
	Outputs map[string]map[string]interface{}

	// LoopCounters is the current iteration count per loop name
	// (1-indexed once incremented by the engine).
	LoopCounters map[string]int

	// LoopMaxIterations is the declared `as <name>(N)` upper bound
	// per loop name, sourced from ir.Workflow.Loops.
	LoopMaxIterations map[string]int

	// LoopPreviousOutput is the snapshot of the source node's output
	// at the previous traversal of each loop's edge — i.e. one
	// iteration behind the current one. Nil on the first iteration.
	LoopPreviousOutput map[string]map[string]interface{}

	// Artifacts is the publish-name → output map for artifacts that
	// have been produced so far in this run.
	Artifacts map[string]map[string]interface{}

	// RunID is the current run identifier, exposed to prompts as
	// `{{run.id}}`.
	RunID string
}

// templateDataKey is the ctx key for the optional TemplateData
// snapshot. We use a private struct type so no other package can
// collide on the key.
type templateDataKey struct{}

// WithTemplateData returns a derived ctx carrying the template data
// snapshot. The runtime engine calls this once per node execution
// before invoking executor.Execute. Passing nil clears the snapshot.
func WithTemplateData(ctx context.Context, td *TemplateData) context.Context {
	return context.WithValue(ctx, templateDataKey{}, td)
}

// TemplateDataFromContext returns the TemplateData attached by
// WithTemplateData, or nil when none is set. Resolvers tolerate nil
// by leaving cross-namespace refs (`outputs.*`, `loop.*`, etc.)
// unresolved — callers that don't wire the runtime context still get
// `input.*` and `vars.*` resolution.
func TemplateDataFromContext(ctx context.Context) *TemplateData {
	td, _ := ctx.Value(templateDataKey{}).(*TemplateData)
	return td
}
