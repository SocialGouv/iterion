package runview

import (
	"context"
	"fmt"
	"sync"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// WireWorkflow is the JSON projection of an IR workflow used by the
// editor's "IR overlay" view. Heavier fields (schemas, prompts, vars,
// MCP config, full expression ASTs) are intentionally omitted — the
// overlay only needs the topology so it can layer execution counts.
type WireWorkflow struct {
	Name string `json:"name"`
	// Entry is the first node ID the runtime picks. Useful so the
	// frontend can mark it specially and start its layout from there.
	Entry string     `json:"entry"`
	Nodes []WireNode `json:"nodes"`
	Edges []WireEdge `json:"edges"`
	// StaleHash signals that the .iter source on disk no longer matches
	// the hash captured at run launch. The frontend can warn the user
	// that the IR they are viewing may diverge from what was executed.
	StaleHash bool `json:"stale_hash,omitempty"`
}

// WireNode is the minimal node projection used by the run-console canvas.
// It carries id + kind plus the LLM-call metadata (model / backend /
// reasoning_effort) for nodes that drive an LLM (Agent, Judge, Router-LLM)
// so the canvas can render those fields next to the node without the
// frontend having to parse the .iter source itself.
//
// OutputFields is populated for HumanNode so the run console can build a
// typed answer form when the run pauses on that node. Other node kinds
// don't need it: their outputs are produced by LLM/tool execution, not
// by user input, so the schema is invisible to the operator.
type WireNode struct {
	ID              string            `json:"id"`
	Kind            string            `json:"kind"`
	Model           string            `json:"model,omitempty"`
	Backend         string            `json:"backend,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
	OutputFields    []WireSchemaField `json:"output_schema,omitempty"`
}

// WireSchemaField is the JSON projection of an ir.SchemaField. The Type
// field carries the canonical string form ("string", "bool", "int",
// "float", "json", "string[]") so the frontend can switch on it without
// tracking the iota values from the ir package.
type WireSchemaField struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	EnumValues []string `json:"enum_values,omitempty"`
}

// WireEdge mirrors the runtime-relevant fields of ir.Edge. Expression
// is sent as the original source string (the AST itself isn't useful
// to the UI, and serializing it would leak compiler internals).
type WireEdge struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Condition  string `json:"condition,omitempty"`
	Negated    bool   `json:"negated,omitempty"`
	Expression string `json:"expression,omitempty"`
	Loop       string `json:"loop,omitempty"`
}

// wireWorkflowCache memoises projection results keyed by file path so
// repeated /api/runs/{id}/workflow requests (multiple browser tabs,
// snapshot replays) don't re-parse + re-compile + re-walk the .iter
// source. The cached entry stores the hash used to compile it; on
// each request we re-stat the file via CompileWorkflowWithHash's hash
// computation and only return the cache entry when the hash matches.
//
// The cache is unbounded but small in practice (one entry per .iter
// file in the workspace). Service holds it as a member rather than a
// package var so multiple Service instances in tests don't share state.
type wireWorkflowCache struct {
	mu    sync.Mutex
	byKey map[string]wireWorkflowCacheEntry
}

type wireWorkflowCacheEntry struct {
	wf   *WireWorkflow
	hash string
}

func (c *wireWorkflowCache) get(filePath, hash string) *WireWorkflow {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byKey == nil {
		return nil
	}
	if e, ok := c.byKey[filePath]; ok && e.hash == hash {
		return e.wf
	}
	return nil
}

func (c *wireWorkflowCache) put(filePath, hash string, wf *WireWorkflow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byKey == nil {
		c.byKey = make(map[string]wireWorkflowCacheEntry)
	}
	c.byKey[filePath] = wireWorkflowCacheEntry{wf: wf, hash: hash}
}

// LoadWireWorkflow recompiles the .iter source for runID and projects
// the resulting IR into WireWorkflow shape. Results are memoised by
// (filePath, content hash) so repeated calls for the same revision
// don't re-parse. The stale_hash flag is derived by comparing the
// freshly-computed hash against the one persisted in run.json at
// launch.
func (s *Service) LoadWireWorkflow(runID string) (*WireWorkflow, error) {
	r, err := s.store.LoadRun(context.Background(), runID)
	if err != nil {
		return nil, err
	}
	if r.FilePath == "" {
		return nil, fmt.Errorf("run %s has no persisted file_path", runID)
	}
	wf, hash, err := CompileWorkflowWithHash(r.FilePath)
	if err != nil {
		return nil, err
	}
	staleHash := r.WorkflowHash != "" && r.WorkflowHash != hash
	if cached := s.wireWFCache.get(r.FilePath, hash); cached != nil {
		// Stale flag depends on the run, not the file — clone the cached
		// projection with the per-request stale value.
		copied := *cached
		copied.StaleHash = staleHash
		return &copied, nil
	}
	out := &WireWorkflow{
		Name:      wf.Name,
		Entry:     wf.Entry,
		Nodes:     make([]WireNode, 0, len(wf.Nodes)),
		Edges:     make([]WireEdge, 0, len(wf.Edges)),
		StaleHash: staleHash,
	}
	for id, n := range wf.Nodes {
		out.Nodes = append(out.Nodes, projectNode(id, n, wf))
	}
	for _, e := range wf.Edges {
		out.Edges = append(out.Edges, WireEdge{
			From:       e.From,
			To:         e.To,
			Condition:  e.Condition,
			Negated:    e.Negated,
			Expression: e.ExpressionSrc,
			Loop:       e.LoopName,
		})
	}
	// Note: nodes map iteration is non-deterministic in Go, but the
	// frontend re-runs autoLayout (ELK) which is order-insensitive, so
	// we don't need to sort here. Callers wanting a stable diff can
	// post-process.
	s.wireWFCache.put(r.FilePath, hash, out)
	return out, nil
}

// IRWorkflowEndpointPath is exposed for symmetry with other runview
// helpers; the server wires the handler manually.
const IRWorkflowEndpointPath = "/api/runs/{id}/workflow"

// projectNode builds a WireNode from an ir.Node, attaching LLM metadata
// when the node drives an LLM call. Routers expose model/backend only in
// LLM mode — the other modes don't have a model.
//
// Env-substituted reasoning_effort literals (e.g. "${VIBE_EFFORT:-max}")
// are resolved against the iterion process env so the run console
// displays the actual level rather than the unexpanded source. Invalid
// expansions become "" — the run console treats that as "fall back to
// the registry default" via its capability prefetch.
//
// The wf parameter is needed to resolve schema names (HumanNode references
// schemas by name; the actual fields live on wf.Schemas[name]).
func projectNode(id string, n ir.Node, wf *ir.Workflow) WireNode {
	out := WireNode{ID: id, Kind: n.NodeKind().String()}
	switch v := n.(type) {
	case *ir.AgentNode:
		out.Model = v.Model
		out.Backend = v.Backend
		out.ReasoningEffort = ir.ResolveEffortLiteral(v.ReasoningEffort)
	case *ir.JudgeNode:
		out.Model = v.Model
		out.Backend = v.Backend
		out.ReasoningEffort = ir.ResolveEffortLiteral(v.ReasoningEffort)
	case *ir.RouterNode:
		if v.RouterMode == ir.RouterLLM {
			out.Model = v.Model
			out.Backend = v.Backend
			out.ReasoningEffort = ir.ResolveEffortLiteral(v.ReasoningEffort)
		}
	case *ir.HumanNode:
		out.OutputFields = projectSchemaFields(v.OutputSchema, wf)
	}
	return out
}

// projectSchemaFields resolves a schema name to its WireSchemaField slice.
// Returns nil when the name is empty or absent from wf.Schemas — the
// frontend treats nil as "no schema, fall back to free-text PauseForm".
func projectSchemaFields(schemaName string, wf *ir.Workflow) []WireSchemaField {
	if schemaName == "" || wf == nil {
		return nil
	}
	schema, ok := wf.Schemas[schemaName]
	if !ok || schema == nil || len(schema.Fields) == 0 {
		return nil
	}
	out := make([]WireSchemaField, 0, len(schema.Fields))
	for _, f := range schema.Fields {
		out = append(out, WireSchemaField{
			Name:       f.Name,
			Type:       f.Type.String(),
			EnumValues: f.EnumValues,
		})
	}
	return out
}
