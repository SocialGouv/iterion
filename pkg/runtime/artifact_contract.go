package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
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

// ArtifactContractCheck is one artifact-contract validation request. It is a
// struct rather than a parameter list because the optional inputs — the skip
// set above all — are exactly what distinguishes the call sites.
type ArtifactContractCheck struct {
	Store store.RunStore
	Run   *store.Run
	// Workflow is the CURRENT compiled source the caller is about to run.
	Workflow *ir.Workflow
	// CurrentRevision is that source's hash. Empty disables the
	// producer-revision comparison, which is what a rewind wants: rewinding
	// after editing the .bot is its primary use case.
	CurrentRevision string
	// Force carries the operator's `--force`. It waives ONLY the
	// producer-revision comparison. docs/resume.md defines --force as an
	// assertion that stored outputs, node ids and SCHEMAS are still
	// compatible, so waiving the checks that verify that assertion would
	// make it self-certifying.
	Force bool
	// Skip lists node ids whose artifacts the caller is about to invalidate.
	// A rewind must not be refused by the very outputs it exists to
	// supersede — those are the state the operator invoked it to discard,
	// and they are replaced by contract-less tombstones moments later.
	Skip map[string]bool
	// Logger receives the report-mode diagnostic. Optional.
	Logger *iterlog.Logger
}

// ValidateArtifactContracts checks persisted artifact metadata before a
// resume or rewind can mutate the run. Legacy artifacts without a contract
// are accepted; enforce refuses an incompatible one nondestructively.
func ValidateArtifactContracts(ctx context.Context, c ArtifactContractCheck) error {
	if c.Run == nil || c.Store == nil || c.Workflow == nil || len(c.Run.ArtifactIndex) == 0 {
		return nil
	}
	run, s, wf := c.Run, c.Store, c.Workflow
	var violations []string
	for nodeID, version := range run.ArtifactIndex {
		if c.Skip[nodeID] {
			continue
		}
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
		// --force is the established escape hatch for deliberately resuming
		// against edited workflow source. It waives only the producer-revision
		// comparison: logical reference, schema and dependency compatibility
		// are still enforced below.
		if !c.Force && c.CurrentRevision != "" && contract.ProducerRevision != "" && contract.ProducerRevision != c.CurrentRevision {
			violations = append(violations, fmt.Sprintf("artifact %q was produced by workflow revision %q, current revision is %q", contract.LogicalRef, contract.ProducerRevision, c.CurrentRevision))
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
	policy := store.ContextPolicyLegacy
	if run.ExecutionContext != nil && run.ExecutionContext.Policy != "" {
		policy = run.ExecutionContext.Policy
	}
	if policy != store.ContextPolicyEnforce {
		return nil
	}
	return fmt.Errorf("artifact contract incompatible: %s", strings.Join(violations, "; "))
}
