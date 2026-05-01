package runview

import (
	"fmt"
	"sync"
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

// WireNode is the minimal node projection: id + kind. Inspector-style
// detail comes from the editor's existing forms (the user can click
// "Open in editor" if they need to drill in).
type WireNode struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
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
	r, err := s.store.LoadRun(runID)
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
		out.Nodes = append(out.Nodes, WireNode{ID: id, Kind: n.NodeKind().String()})
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
