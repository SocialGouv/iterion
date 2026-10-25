package runview

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
	"github.com/SocialGouv/iterion/pkg/store"
)

// WireWorkflow is the JSON projection of an IR workflow used by the
// studio's "IR overlay" view. Heavier fields (schemas, prompts, vars,
// MCP config, full expression ASTs) are intentionally omitted — the
// overlay only needs the topology so it can layer execution counts.
type WireWorkflow struct {
	Attempt      *store.RunAttempt `json:"attempt,omitempty"`
	Provenance   string            `json:"provenance"`
	Name         string            `json:"name"`
	WorkflowHash string            `json:"workflow_hash,omitempty"`
	// Entry is the first node ID the runtime picks. Useful so the
	// frontend can mark it specially and start its layout from there.
	Entry string     `json:"entry"`
	Nodes []WireNode `json:"nodes"`
	Edges []WireEdge `json:"edges"`
	// StaleHash flags a best-effort legacy reconstruction whose current
	// source differs from the run hash. Immutable attempts never need it.
	StaleHash bool `json:"stale_hash,omitempty"`
}

// WireNode is the minimal node projection used by the run-console canvas.
// Model/backend/reasoning_effort are populated for LLM-driving nodes
// (Agent, Judge, Router-LLM); OutputFields is populated for HumanNode so
// the run console can render a typed answer form on pause; Source/Isolated
// are populated for subbot nodes — the child .bot path relative to the
// parent file, and whether the child is workspace-isolated (parallel-safe).
type WireNode struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	Description     string `json:"description,omitempty"`
	Model           string `json:"model,omitempty"`
	Backend         string `json:"backend,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// InputFields is the HumanNode's declared `input_schema`, the TYPE
	// of the payload the gate receives (Interaction.Questions carries
	// the resolved values). The studio renders that payload above the
	// answer form so the operator sees what they are validating —
	// `json` fields as structured data, `file` fields as previews —
	// instead of the author having to stringify it into `instructions:`.
	// Absent when the node declares no input schema; the studio then
	// falls back to inferring a renderer from each value's shape.
	InputFields []WireSchemaField `json:"input_schema,omitempty"`
	// InstructionInputs names the inbound keys the HumanNode's
	// `instructions:` prompt already interpolates (`{{input.<key>}}`,
	// substituted by Engine.renderHumanInstructions against the very same
	// resolved map). The studio skips them when rendering the inbound
	// payload, so a gate whose instructions embed its input — the
	// historical workaround, and still 13 gates across 8 shipped catalog
	// bots — shows that value once, not twice.
	//
	// Wire-side rather than a text-containment check in the browser
	// because the run console never receives the resolved instructions
	// string, only the board does.
	InstructionInputs []string          `json:"instruction_inputs,omitempty"`
	OutputFields      []WireSchemaField `json:"output_schema,omitempty"`
	Source            string            `json:"source,omitempty"`
	Isolated          bool              `json:"isolated,omitempty"`
}

// WireSchemaField projects an ir.SchemaField as JSON. Type uses the
// canonical string form ("string", "bool", "int", "float", "json",
// "string[]") so the frontend doesn't track ir's iota values.
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

// wireWorkflowCache holds immutable projections by tenant and workflow_hash.
// Mutable legacy sources are resolved before a cache lookup, so deletion is
// still observable. The cached graph never contains run-specific metadata.
type wireWorkflowCache struct {
	mu    sync.Mutex
	byKey map[string]*WireWorkflow
}

func (c *wireWorkflowCache) get(key string) *WireWorkflow {
	if c == nil || key == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byKey[key]
}
func (c *wireWorkflowCache) put(key string, wf *WireWorkflow) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byKey == nil {
		c.byKey = map[string]*WireWorkflow{}
	}
	c.byKey[key] = wf
}

// LegacyWorkflowResolver supplies stored-bot resources only when no immutable
// attempt exists. The server supplies its tenant-aware botsource resolver.
type LegacyWorkflowResolver func(context.Context, *store.Run) (*workflowfile.Snapshot, error)

// WorkflowUnavailable is the typed 404 for missing provenance. Source is a
// logical bot name or attempt ID, never a server filesystem path.
type WorkflowUnavailable struct {
	Code    string `json:"error_code"`
	Source  string `json:"source"`
	Message string `json:"error"`
}

func (e *WorkflowUnavailable) Error() string { return e.Message }
func unavailable(code, source string) error {
	return &WorkflowUnavailable{Code: code, Source: source, Message: fmt.Sprintf("workflow source %q is unavailable (%s)", source, code)}
}

func (s *Service) LoadWireWorkflow(runID string) (*WireWorkflow, error) {
	return s.LoadWireWorkflowCtx(context.Background(), runID)
}
func (s *Service) LoadWireWorkflowCtx(ctx context.Context, runID string) (*WireWorkflow, error) {
	return s.LoadWireWorkflowAttemptCtx(ctx, runID, "", nil)
}
func (s *Service) LoadWireWorkflowAttemptCtx(ctx context.Context, runID, attemptID string, resolve LegacyWorkflowResolver) (*WireWorkflow, error) {
	return s.LoadWireWorkflowFromStoreCtx(ctx, s.store, runID, attemptID, resolve)
}

// LoadWireWorkflowFromStoreCtx shares the revision cache with cross-store
// views while the owning store still authorizes every run/snapshot read.
func (s *Service) LoadWireWorkflowFromStoreCtx(ctx context.Context, st store.RunStore, runID, attemptID string, resolve LegacyWorkflowResolver) (*WireWorkflow, error) {
	return buildWireWorkflow(ctx, st, runID, attemptID, resolve, &s.wireWFCache)
}
func BuildWireWorkflowFromStore(ctx context.Context, s store.RunStore, runID string) (*WireWorkflow, error) {
	return buildWireWorkflow(ctx, s, runID, "", nil, nil)
}
func BuildWireWorkflowAttemptFromStore(ctx context.Context, s store.RunStore, runID, attemptID string, resolve LegacyWorkflowResolver) (*WireWorkflow, error) {
	return buildWireWorkflow(ctx, s, runID, attemptID, resolve, nil)
}
func buildWireWorkflow(ctx context.Context, st store.RunStore, runID, attemptID string, resolve LegacyWorkflowResolver, cache *wireWorkflowCache) (*WireWorkflow, error) {
	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if len(r.Attempts) == 0 {
		if attemptID != "" {
			return nil, unavailable("attempt_not_found", attemptID)
		}
		return buildLegacyWireWorkflow(ctx, r, resolve, cache)
	}
	a := r.Attempts[len(r.Attempts)-1]
	if attemptID != "" {
		found := false
		for _, candidate := range r.Attempts {
			if candidate.ID == attemptID {
				a = candidate
				found = true
				break
			}
		}
		if !found {
			return nil, unavailable("attempt_not_found", attemptID)
		}
	}
	snapshots := store.AsAttemptStore(st)
	if snapshots == nil {
		return nil, unavailable("attempt_snapshot_missing", a.ID)
	}
	captured, err := snapshots.LoadAttempt(ctx, r.ID, a.ID)
	if errors.Is(err, store.ErrAttemptNotFound) {
		return nil, unavailable("attempt_snapshot_missing", a.ID)
	}
	if err != nil {
		return nil, err
	}
	if captured.Source == nil {
		return nil, unavailable("attempt_snapshot_missing", a.ID)
	}
	graph, err := cachedSourceWorkflow(captured.Source, r.TenantID, captured.WorkflowHash, cache)
	if err != nil {
		return nil, fmt.Errorf("compile captured attempt %s: %w", a.ID, err)
	}
	out := *graph
	out.Attempt = &a
	out.WorkflowHash = captured.WorkflowHash
	out.Provenance = "attempt_snapshot"
	return &out, nil
}

// Retained for local callers and tests that hold a legacy run document.
func buildWireWorkflowFromRun(r *store.Run, cache *wireWorkflowCache) (*WireWorkflow, error) {
	return buildLegacyWireWorkflow(context.Background(), r, nil, cache)
}
func recordedSource(r *store.Run) *workflowfile.Snapshot {
	if r.WorkflowSource == "" && len(r.WorkflowSources) == 0 {
		return nil
	}
	source := &workflowfile.Snapshot{Main: "main.bot", Files: map[string]string{"main.bot": r.WorkflowSource}}
	if len(r.WorkflowSources) > 0 {
		source.Main = r.WorkflowSources[0].Path
		source.Files = map[string]string{}
		for _, f := range r.WorkflowSources {
			source.Files[f.Path] = f.Text
		}
	}
	return source
}
func buildLegacyWireWorkflow(ctx context.Context, r *store.Run, resolve LegacyWorkflowResolver, cache *wireWorkflowCache) (*WireWorkflow, error) {
	recorded := recordedSource(r)
	if recorded != nil {
		wf, _, err := compileSourceSnapshot(recorded)
		if err == nil {
			out := projectWorkflow(wf)
			out.Provenance = "recorded_source"
			return out, nil
		}
		// A legacy source may lack its bundle prompts. Only legacy runs can
		// acquire those resources from a current source below.
		if !r.ServedByStoredBot() {
			return nil, err
		}
	}
	if r.ServedByStoredBot() {
		name := r.BotID
		if name == "" {
			name = r.BundleName
		}
		if resolve == nil {
			return nil, unavailable("bot_source_missing", name)
		}
		source, err := resolve(ctx, r)
		if err != nil {
			return nil, err
		}
		if source == nil {
			return nil, unavailable("bot_source_missing", name)
		}
		// Keep the captured program when only resources were missing.
		if recorded != nil {
			for path, text := range recorded.Files {
				if path == recorded.Main {
					source.Files[source.Main] = text
				} else {
					source.Files[path] = text
				}
			}
		}
		graph, err := cachedSourceWorkflow(source, r.TenantID, r.WorkflowHash, cache)
		if err != nil {
			return nil, err
		}
		out := *graph
		out.StaleHash = r.WorkflowHash != "" && graph.WorkflowHash != r.WorkflowHash
		out.Provenance = "bot_source_legacy"
		return &out, nil
	}
	var wf *ir.Workflow
	var hash string
	var err error
	switch {
	case r.FilePath != "":
		wf, hash, _, err = CompileWorkflowPath(resolveWorkflowPath(r))
	case r.BundlePath != "":
		var b *bundle.Bundle
		b, err = bundle.OpenDir(r.BundlePath)
		if err == nil {
			wf, hash, err = CompileBundleWorkflow(b.IterPath, b)
		}
	default:
		return nil, unavailable("workflow_source_missing", r.WorkflowName)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, unavailable("workflow_source_missing", r.WorkflowName)
		}
		return nil, err
	}
	out := projectWorkflow(wf)
	out.StaleHash = r.WorkflowHash != "" && hash != r.WorkflowHash
	out.Provenance = "filesystem_legacy"
	return out, nil
}
func cachedSourceWorkflow(source *workflowfile.Snapshot, tenant, hash string, cache *wireWorkflowCache) (*WireWorkflow, error) {
	key := ""
	if hash != "" {
		body, err := json.Marshal(source)
		if err != nil {
			return nil, err
		}
		key = fmt.Sprintf("%s/%s/%x", tenant, hash, sha256.Sum256(body))
	}
	if cached := cache.get(key); cached != nil {
		return cached, nil
	}
	wf, actualHash, err := compileSourceSnapshot(source)
	if err != nil {
		return nil, err
	}
	out := projectWorkflow(wf)
	out.WorkflowHash = actualHash
	cache.put(key, out)
	return out, nil
}
func projectWorkflow(wf *ir.Workflow) *WireWorkflow {
	out := &WireWorkflow{Name: wf.Name, Entry: wf.Entry, Nodes: make([]WireNode, 0, len(wf.Nodes)), Edges: make([]WireEdge, 0, len(wf.Edges))}
	for id, n := range wf.Nodes {
		out.Nodes = append(out.Nodes, projectNode(id, n, wf))
	}
	for _, e := range wf.Edges {
		out.Edges = append(out.Edges, WireEdge{From: e.From, To: e.To, Condition: e.Condition, Negated: e.Negated, Expression: e.ExpressionSrc, Loop: e.LoopName})
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].ID < out.Nodes[j].ID })
	return out
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
// wf is needed to resolve HumanNode input_schema/output_schema names
// against wf.Schemas; pass nil when only LLM nodes are projected.
func projectNode(id string, n ir.Node, wf *ir.Workflow) WireNode {
	out := WireNode{ID: id, Kind: n.NodeKind().String()}
	// Every ir node embeds BaseNode, which promotes NodeDescription; the
	// assertion keeps the ir.Node interface itself unchanged.
	if d, ok := n.(interface{ NodeDescription() string }); ok {
		out.Description = d.NodeDescription()
	}
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
		out.InputFields = projectSchemaFields(v.InputSchema, wf)
		out.InstructionInputs = projectInstructionInputs(v.Instructions, wf)
		out.OutputFields = projectSchemaFields(v.OutputSchema, wf)
	case *ir.SubbotNode:
		out.Source = v.Source
		out.Isolated = v.Isolated
	}
	return out
}

// projectInstructionInputs returns the sorted, de-duplicated top-level
// `input.*` keys a human node's instructions prompt interpolates.
//
// Only the FIRST path segment matters: `{{input.plan.title}}` consumes
// the `plan` item as far as the operator is concerned — the payload
// renderer works per top-level key, not per leaf.
//
// Returns nil for an unset or unknown prompt name, which the studio
// reads as "nothing already shown".
func projectInstructionInputs(promptName string, wf *ir.Workflow) []string {
	if promptName == "" || wf == nil {
		return nil
	}
	p, ok := wf.Prompts[promptName]
	if !ok || p == nil {
		return nil
	}
	seen := make(map[string]bool)
	out := make([]string, 0, len(p.TemplateRefs))
	for _, ref := range p.TemplateRefs {
		if ref == nil || ref.Kind != ir.RefInput || len(ref.Path) == 0 {
			continue
		}
		key := ref.Path[0]
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// projectSchemaFields resolves a schema name. Returns nil for empty
// or unknown names; frontend treats nil as "free-text fallback".
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
