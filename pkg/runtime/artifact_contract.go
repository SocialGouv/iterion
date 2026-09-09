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
func (e *Engine) artifactContractFor(nodeID string, node ir.Node, version int, rs *runState) *store.ArtifactContract {
	logicalRef := nodePublish(node)
	if logicalRef == "" {
		return nil
	}
	contract := &store.ArtifactContract{
		LogicalRef:       logicalRef,
		ProducerNode:     nodeID,
		ProducerRevision: e.workflowHash,
		Version:          version,
		Schema:           ir.NodeOutputSchema(node),
		Effects:          []string{"persist"},
	}
	if rs == nil {
		return contract
	}
	for _, consumedRef := range ir.NodeArtifactRefs(e.workflow, nodeID) {
		if _, present := rs.artifacts[consumedRef]; !present {
			continue
		}
		producerID := e.artifactProducer(consumedRef)
		nextVersion, present := rs.artifactVersions[producerID]
		if producerID == "" || !present || nextVersion <= 0 {
			continue
		}
		contract.Dependencies = append(contract.Dependencies, store.ArtifactDependency{
			LogicalRef: consumedRef,
			NodeID:     producerID,
			Version:    nextVersion - 1,
			Required:   true,
		})
	}
	return contract
}

func (e *Engine) artifactProducer(logicalRef string) string {
	if e.workflow == nil {
		return ""
	}
	for nodeID, node := range e.workflow.Nodes {
		if nodePublish(node) == logicalRef {
			return nodeID
		}
	}
	return ""
}

// ValidateArtifactContracts checks persisted artifact metadata before a
// resume or rewind can mutate the run. Legacy artifacts without a contract
// are accepted; report/legacy context policies record the mismatch through
// the caller while enforce refuses it nondestructively.
func ValidateArtifactContracts(ctx context.Context, s store.RunStore, run *store.Run, wf *ir.Workflow, currentRevision string, forceSourceChange bool) error {
	return validateArtifactContracts(ctx, s, run, wf, currentRevision, forceSourceChange, nil)
}

// ValidateArtifactContractsExcept applies the resume/rewind contract guard
// while ignoring artifacts owned by nodes the caller is about to invalidate.
// It is used by rewind after it has computed the exact downstream set: an
// obsolete artifact must not prevent the operation that removes it.
func ValidateArtifactContractsExcept(ctx context.Context, s store.RunStore, run *store.Run, wf *ir.Workflow, currentRevision string, forceSourceChange bool, ignoredNodes map[string]bool) error {
	return validateArtifactContracts(ctx, s, run, wf, currentRevision, forceSourceChange, ignoredNodes)
}

func validateArtifactContracts(ctx context.Context, s store.RunStore, run *store.Run, wf *ir.Workflow, currentRevision string, forceSourceChange bool, ignoredNodes map[string]bool) error {
	if run == nil || s == nil || wf == nil || len(run.ArtifactIndex) == 0 {
		return nil
	}
	policy := store.ContextPolicyLegacy
	if run.ExecutionContext != nil && run.ExecutionContext.Policy != "" {
		policy = run.ExecutionContext.Policy
	}
	var violations []string
	for nodeID, version := range run.ArtifactIndex {
		if ignoredNodes[nodeID] {
			continue
		}
		artifact, err := s.LoadArtifact(ctx, run.ID, nodeID, version)
		if err != nil {
			// Legacy documents historically treated the index as a cache. Once a
			// run opts into report/enforce, however, an unreadable indexed artifact
			// means validation could not be completed and must be visible.
			if policy != store.ContextPolicyLegacy {
				violations = append(violations, fmt.Sprintf("artifact %s/%d could not be loaded: %v", nodeID, version, err))
			}
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
		// --force is the established acknowledgement that the operator wants
		// to recover against deliberately edited workflow source. Publishing
		// names, schemas, node presence and the producer revision all derive
		// from that source, so they must be waived together. Persisted contract
		// integrity and dependency versions remain enforced below.
		if !forceSourceChange {
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
		}
		for _, dep := range contract.Dependencies {
			if dep.LogicalRef == "" || !dep.Required {
				continue
			}
			depNode := dep.NodeID
			if depNode == "" {
				depNode = dep.LogicalRef
			}
			depVersion, present := run.ArtifactIndex[depNode]
			if !present {
				violations = append(violations, fmt.Sprintf("artifact %q requires %s v%d, which is absent from the run", contract.LogicalRef, dep.LogicalRef, dep.Version))
			} else if depVersion < dep.Version {
				violations = append(violations, fmt.Sprintf("artifact %q requires %s v%d, persisted v%d", contract.LogicalRef, dep.LogicalRef, dep.Version, depVersion))
			}
		}
	}
	if len(violations) == 0 {
		return nil
	}
	if policy == store.ContextPolicyReport {
		_, _ = s.AppendEvent(context.WithoutCancel(ctx), run.ID, store.Event{
			Type:  store.EventArtifactContractViolation,
			RunID: run.ID,
			Data: map[string]any{
				"policy":     string(policy),
				"violations": append([]string(nil), violations...),
			},
		})
		return nil
	}
	if policy != store.ContextPolicyEnforce {
		return nil
	}
	return fmt.Errorf("artifact contract incompatible: %s", strings.Join(violations, "; "))
}
