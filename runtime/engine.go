// Package runtime implements the sequential workflow execution engine.
// It walks the compiled IR graph node by node, persists outputs and
// artifacts via the store, evaluates edge conditions and loop counters,
// and emits lifecycle events.
package runtime

import (
	"context"
	"fmt"

	"github.com/iterion-ai/iterion/ir"
	"github.com/iterion-ai/iterion/store"
)

// NodeExecutor is the abstraction called by the engine to actually run a
// node (LLM call, tool invocation, etc.). The runtime itself is agnostic
// to the concrete implementation — tests supply stubs, production code
// plugs in real providers.
type NodeExecutor interface {
	// Execute runs the given node with the provided input and returns its
	// output. For terminal nodes (done/fail) this is never called.
	Execute(ctx context.Context, node *ir.Node, input map[string]interface{}) (map[string]interface{}, error)
}

// Engine is the sequential runtime. It executes one node at a time,
// following edges until a terminal node is reached.
type Engine struct {
	workflow *ir.Workflow
	store    *store.RunStore
	executor NodeExecutor
}

// New creates a new sequential Engine.
func New(wf *ir.Workflow, s *store.RunStore, exec NodeExecutor) *Engine {
	return &Engine{workflow: wf, store: s, executor: exec}
}

// Run executes the workflow sequentially. It creates a run, walks the
// graph from the entry node, and returns when a terminal node is reached
// or an error occurs.
func (e *Engine) Run(ctx context.Context, runID string, inputs map[string]interface{}) error {
	// Create run in store.
	if _, err := e.store.CreateRun(runID, e.workflow.Name, inputs); err != nil {
		return fmt.Errorf("runtime: create run: %w", err)
	}

	// Emit run_started.
	if err := e.emit(runID, store.EventRunStarted, "", nil); err != nil {
		return err
	}

	// Resolve initial vars (defaults merged with inputs).
	vars := e.resolveVars(inputs)

	// Runtime state: outputs per node, loop counters, artifact versions.
	outputs := make(map[string]map[string]interface{})
	loopCounters := make(map[string]int)
	artifactVersions := make(map[string]int) // node ID → next version

	currentNodeID := e.workflow.Entry

	for {
		select {
		case <-ctx.Done():
			_ = e.store.UpdateRunStatus(runID, store.RunStatusFailed, ctx.Err().Error())
			_ = e.emit(runID, store.EventRunFailed, currentNodeID, map[string]interface{}{
				"error": ctx.Err().Error(),
			})
			return ctx.Err()
		default:
		}

		node, ok := e.workflow.Nodes[currentNodeID]
		if !ok {
			return e.failRun(runID, currentNodeID, fmt.Sprintf("node %q not found", currentNodeID))
		}

		// --- Terminal nodes ---
		if node.Kind == ir.NodeDone {
			if err := e.emit(runID, store.EventNodeStarted, currentNodeID, nil); err != nil {
				return err
			}
			if err := e.emit(runID, store.EventNodeFinished, currentNodeID, nil); err != nil {
				return err
			}
			if err := e.store.UpdateRunStatus(runID, store.RunStatusFinished, ""); err != nil {
				return err
			}
			return e.emit(runID, store.EventRunFinished, "", nil)
		}
		if node.Kind == ir.NodeFail {
			if err := e.emit(runID, store.EventNodeStarted, currentNodeID, nil); err != nil {
				return err
			}
			if err := e.emit(runID, store.EventNodeFinished, currentNodeID, nil); err != nil {
				return err
			}
			return e.failRun(runID, currentNodeID, "workflow reached fail node")
		}

		// --- Emit node_started ---
		if err := e.emit(runID, store.EventNodeStarted, currentNodeID, map[string]interface{}{
			"kind": node.Kind.String(),
		}); err != nil {
			return err
		}

		// --- Build node input from edge mappings ---
		nodeInput := e.buildNodeInput(currentNodeID, vars, outputs, inputs)

		// --- Execute node ---
		output, err := e.executor.Execute(ctx, node, nodeInput)
		if err != nil {
			return e.failRun(runID, currentNodeID, fmt.Sprintf("node %q execution failed: %v", currentNodeID, err))
		}

		// Store output.
		outputs[currentNodeID] = output

		// Persist artifact if node has publish.
		if node.Publish != "" {
			version := artifactVersions[currentNodeID]
			artifact := &store.Artifact{
				RunID:   runID,
				NodeID:  currentNodeID,
				Version: version,
				Data:    output,
			}
			if err := e.store.WriteArtifact(artifact); err != nil {
				return fmt.Errorf("runtime: write artifact: %w", err)
			}
			artifactVersions[currentNodeID] = version + 1

			_ = e.emit(runID, store.EventArtifactWritten, currentNodeID, map[string]interface{}{
				"publish": node.Publish,
				"version": version,
			})
		}

		// --- Emit node_finished ---
		if err := e.emit(runID, store.EventNodeFinished, currentNodeID, nil); err != nil {
			return err
		}

		// --- Select outgoing edge ---
		nextNodeID, err := e.selectEdge(runID, currentNodeID, output, loopCounters)
		if err != nil {
			return e.failRun(runID, currentNodeID, err.Error())
		}

		currentNodeID = nextNodeID
	}
}

// selectEdge picks the next node by evaluating outgoing edges from the
// current node. Conditional edges are checked first; the first matching
// unconditional edge serves as fallback. Loop counters are enforced.
func (e *Engine) selectEdge(runID, fromNodeID string, output map[string]interface{}, loopCounters map[string]int) (string, error) {
	var unconditional *ir.Edge
	var selected *ir.Edge

	for _, edge := range e.workflow.Edges {
		if edge.From != fromNodeID {
			continue
		}

		if edge.Condition == "" {
			// Unconditional edge — keep first as fallback.
			if unconditional == nil {
				unconditional = edge
			}
			continue
		}

		// Evaluate condition against output.
		val, ok := output[edge.Condition]
		if !ok {
			continue
		}
		boolVal, isBool := val.(bool)
		if !isBool {
			continue
		}
		if edge.Negated {
			boolVal = !boolVal
		}
		if boolVal {
			selected = edge
			break
		}
	}

	if selected == nil {
		selected = unconditional
	}
	if selected == nil {
		return "", fmt.Errorf("no outgoing edge from node %q", fromNodeID)
	}

	// Enforce loop counter.
	if selected.LoopName != "" {
		loop, ok := e.workflow.Loops[selected.LoopName]
		if !ok {
			return "", fmt.Errorf("edge references unknown loop %q", selected.LoopName)
		}
		count := loopCounters[selected.LoopName]
		if count >= loop.MaxIterations {
			return "", fmt.Errorf("loop %q exceeded max iterations (%d)", selected.LoopName, loop.MaxIterations)
		}
		loopCounters[selected.LoopName] = count + 1
	}

	// Emit edge_selected.
	data := map[string]interface{}{
		"from": selected.From,
		"to":   selected.To,
	}
	if selected.Condition != "" {
		data["condition"] = selected.Condition
		data["negated"] = selected.Negated
	}
	if selected.LoopName != "" {
		data["loop"] = selected.LoopName
		data["iteration"] = loopCounters[selected.LoopName]
	}
	_ = e.emit(runID, store.EventEdgeSelected, "", data)

	return selected.To, nil
}

// buildNodeInput constructs the input map for a node by looking at the
// edge `with` mappings that target this node. If no mappings exist, the
// run-level inputs are used as a starting point for the entry node.
func (e *Engine) buildNodeInput(nodeID string, vars map[string]interface{}, outputs map[string]map[string]interface{}, runInputs map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})

	// Find the edge that targets this node and has `with` mappings.
	// In a sequential run there is at most one incoming edge per step.
	for _, edge := range e.workflow.Edges {
		if edge.To != nodeID || len(edge.With) == 0 {
			continue
		}
		// Only use mappings from an edge whose source has already produced output.
		if _, ok := outputs[edge.From]; !ok && edge.From != "" {
			continue
		}
		for _, dm := range edge.With {
			val := e.resolveMapping(dm, vars, outputs, runInputs)
			if val != nil {
				result[dm.Key] = val
			}
		}
		if len(result) > 0 {
			return result
		}
	}

	// Fallback: for the entry node use run-level inputs.
	if nodeID == e.workflow.Entry {
		for k, v := range runInputs {
			result[k] = v
		}
	}

	return result
}

// resolveMapping resolves a DataMapping's references to concrete values.
// For simplicity in the minimal runtime, if there is exactly one ref we
// return the resolved value directly; otherwise we return the raw template.
func (e *Engine) resolveMapping(dm *ir.DataMapping, vars map[string]interface{}, outputs map[string]map[string]interface{}, runInputs map[string]interface{}) interface{} {
	if len(dm.Refs) == 1 {
		return e.resolveRef(dm.Refs[0], vars, outputs, runInputs)
	}
	// Multiple refs or no refs: return raw template as-is.
	return dm.Raw
}

// resolveRef resolves a single Ref to a concrete value.
func (e *Engine) resolveRef(ref *ir.Ref, vars map[string]interface{}, outputs map[string]map[string]interface{}, runInputs map[string]interface{}) interface{} {
	switch ref.Kind {
	case ir.RefVars:
		if len(ref.Path) > 0 {
			return vars[ref.Path[0]]
		}
	case ir.RefInput:
		if len(ref.Path) > 0 {
			return runInputs[ref.Path[0]]
		}
	case ir.RefOutputs:
		if len(ref.Path) == 0 {
			return nil
		}
		nodeOut := outputs[ref.Path[0]]
		if nodeOut == nil {
			return nil
		}
		if len(ref.Path) == 1 {
			return nodeOut
		}
		return nodeOut[ref.Path[1]]
	case ir.RefArtifacts:
		// Artifacts are resolved via the same outputs map for now.
		if len(ref.Path) > 0 {
			return outputs[ref.Path[0]]
		}
	}
	return nil
}

// resolveVars builds the vars map from workflow variable defaults.
func (e *Engine) resolveVars(inputs map[string]interface{}) map[string]interface{} {
	vars := make(map[string]interface{})
	for name, v := range e.workflow.Vars {
		if v.HasDefault {
			vars[name] = v.Default
		}
	}
	// Inputs can override vars.
	for k, v := range inputs {
		if _, isVar := e.workflow.Vars[k]; isVar {
			vars[k] = v
		}
	}
	return vars
}

// emit is a convenience wrapper for appending an event.
func (e *Engine) emit(runID string, typ store.EventType, nodeID string, data map[string]interface{}) error {
	_, err := e.store.AppendEvent(runID, store.Event{
		Type:   typ,
		NodeID: nodeID,
		Data:   data,
	})
	if err != nil {
		return fmt.Errorf("runtime: emit %s: %w", typ, err)
	}
	return nil
}

// failRun marks a run as failed and emits the run_failed event.
func (e *Engine) failRun(runID, nodeID, reason string) error {
	_ = e.store.UpdateRunStatus(runID, store.RunStatusFailed, reason)
	_ = e.emit(runID, store.EventRunFailed, nodeID, map[string]interface{}{
		"error": reason,
	})
	return fmt.Errorf("runtime: %s", reason)
}
