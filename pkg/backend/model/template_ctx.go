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

	// Attachments maps the workflow's attachment names to their
	// resolved per-run metadata. Populated from Run.Attachments at
	// the start of every node execution. Resolvers expose:
	//
	//	{{attachments.<name>}}        → host filesystem path
	//	{{attachments.<name>.path}}   → host filesystem path (explicit)
	//	{{attachments.<name>.url}}    → presigned URL (lazy: filled on demand)
	//	{{attachments.<name>.mime}}   → sniffed MIME
	//	{{attachments.<name>.size}}   → byte length as decimal string
	//	{{attachments.<name>.sha256}} → hex SHA-256
	Attachments map[string]AttachmentInfo
}

// AttachmentInfo is the resolved per-attachment view consumed by
// template references. Built once per node execution from the
// store's AttachmentRecord plus a lazy presign hook so prompts that
// never touch `.url` don't pay the URL signing cost.
type AttachmentInfo struct {
	Name             string
	Path             string // absolute host path; empty in cloud unless prefetched
	OriginalFilename string
	MIME             string
	Size             int64
	SHA256           string
	// PresignURL, when set, is invoked lazily the first time a
	// `.url` reference is resolved. Returns "" when no presigner
	// is wired (e.g. unit-test stubs).
	PresignURL func() (string, error)
}

// URL evaluates the presign hook. Each call re-runs the hook because
// AttachmentInfo is stored by value in TemplateData.Attachments and
// cache mutation wouldn't be visible across map lookups.
func (a AttachmentInfo) URL() (string, error) {
	if a.PresignURL == nil {
		return "", nil
	}
	return a.PresignURL()
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
