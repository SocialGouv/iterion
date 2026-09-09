package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// artifactContractFor derives the immutable portion of an artifact contract
// from the compiled workflow. It deliberately does not infer external side
// effects from output data: only the declared publish reference is persisted.
func (e *Engine) artifactContractFor(nodeID string, node ir.Node, version int) *store.ArtifactContract {
	logicalRef := nodePublish(node)
	if logicalRef == "" {
		return nil
	}
	return &store.ArtifactContract{
		LogicalRef:       logicalRef,
		ProducerNode:     nodeID,
		ProducerRevision: e.workflowHash,
		Version:          version,
		Schema:           ir.NodeOutputSchema(node),
		Effects:          []string{"persist"},
	}
}

// ValidateArtifactContracts checks persisted artifact metadata before a
// resume or rewind can mutate the run. Legacy artifacts without a contract
// are accepted; report/legacy context policies record the mismatch through
// the caller while enforce refuses it nondestructively.
func ValidateArtifactContracts(ctx context.Context, s store.RunStore, run *store.Run, wf *ir.Workflow, currentRevision string) error {
	if run == nil || s == nil || wf == nil || len(run.ArtifactIndex) == 0 {
		return nil
	}
	var violations []string
	for nodeID, version := range run.ArtifactIndex {
		artifact, err := s.LoadArtifact(ctx, run.ID, nodeID, version)
		if err != nil {
			// A stale index is a legacy/cache condition. Do not turn it into a
			// destructive rewind; the store's artifact reader remains the source
			// of truth and the next write repairs the index.
			continue
		}
		if artifact == nil || artifact.Contract == nil {
			continue
		}
		contract := artifact.Contract
		if contract.LogicalRef == "" || contract.ProducerNode == "" || contract.Version != artifact.Version {
			violations = append(violations, fmt.Sprintf("artifact %s/%d has an incomplete contract", nodeID, version))
			continue
		}
		node, ok := wf.Nodes[nodeID]
		if !ok {
			violations = append(violations, fmt.Sprintf("artifact %q was produced by missing node %q", contract.LogicalRef, nodeID))
			continue
		}
		if got := nodePublish(node); got != contract.LogicalRef {
			violations = append(violations, fmt.Sprintf("artifact %q is now published as %q", contract.LogicalRef, got))
		}
		if schema := ir.NodeOutputSchema(node); schema != contract.Schema {
			violations = append(violations, fmt.Sprintf("artifact %q schema changed from %q to %q", contract.LogicalRef, contract.Schema, schema))
		}
		if currentRevision != "" && contract.ProducerRevision != "" && contract.ProducerRevision != currentRevision {
			violations = append(violations, fmt.Sprintf("artifact %q was produced by workflow revision %q, current revision is %q", contract.LogicalRef, contract.ProducerRevision, currentRevision))
		}
		for _, dep := range contract.Dependencies {
			if dep.LogicalRef == "" || !dep.Required {
				continue
			}
			depNode := dep.NodeID
			if depNode == "" {
				depNode = dep.LogicalRef
			}
			depVersion := run.ArtifactIndex[depNode]
			if depVersion < dep.Version {
				violations = append(violations, fmt.Sprintf("artifact %q requires %s v%d, persisted v%d", contract.LogicalRef, dep.LogicalRef, dep.Version, depVersion))
			}
		}
	}
	if len(violations) == 0 {
		return nil
	}
	policy := store.ContextPolicyLegacy
	if run.ExecutionContext != nil && run.ExecutionContext.Policy != "" {
		policy = run.ExecutionContext.Policy
	}
	if policy != store.ContextPolicyEnforce {
		return nil
	}
	return fmt.Errorf("artifact contract incompatible: %s", strings.Join(violations, "; "))
}
