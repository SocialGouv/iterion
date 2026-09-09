package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

var (
	// ErrArtifactContractIncompatible is a durable compatibility refusal. It
	// lets HTTP and resume callers classify an operator-actionable conflict.
	ErrArtifactContractIncompatible = errors.New("artifact contract incompatible")
	// ErrArtifactContractUnavailable means validation could not read the
	// durable evidence. It is an infrastructure error, not a source mismatch.
	ErrArtifactContractUnavailable = errors.New("artifact contract validation unavailable")
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
	for _, consumedRef := range e.consumedArtifactRefs(nodeID, rs) {
		if _, present := rs.artifacts[consumedRef]; !present {
			continue
		}
		revision, present := rs.artifactRevisions[consumedRef]
		if !present || revision.NodeID == "" {
			continue
		}
		logicalRef := consumedRef
		if revision.ContractLogicalRef != "" {
			logicalRef = revision.ContractLogicalRef
		}
		contract.Dependencies = append(contract.Dependencies, store.ArtifactDependency{
			LogicalRef: logicalRef,
			NodeID:     revision.NodeID,
			Version:    revision.Version,
			Required:   true,
		})
	}
	return contract
}

// consumedArtifactRefs mirrors buildNodeInputRS's selected-edge rules so a
// contract includes artifact references that reached the node through `with:`
// without binding it to an unselected sibling mapping.
func (e *Engine) consumedArtifactRefs(nodeID string, rs *runState) []string {
	if e.workflow == nil {
		return nil
	}
	selected, tracked := incomingFor(nodeID, resolveScope{rs: rs})
	if tracked && !incomingMatchesWorkflow(nodeID, selected, e.workflow.Edges) {
		tracked = false
	}
	overlayForward := tracked && incomingOnlyBounded(selected)
	return ir.NodeArtifactRefsForEdges(e.workflow, nodeID, func(edge *ir.Edge) bool {
		if _, ok := rs.outputs[edge.From]; !ok && edge.From != "" {
			return false
		}
		if !tracked || (overlayForward && !edge.IsBoundedIteration()) {
			return true
		}
		return edgeInIncoming(edge, selected)
	})
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

// ValidateCheckpointArtifactAvailability verifies that every exact physical
// revision named by the checkpoint can still be read. Unlike contract-policy
// validation, this is an execution prerequisite for every policy: the engine
// rebuilds {{artifacts.*}} from these immutable bodies before claiming a
// resume, and a preflight must make the same check before callers consume
// staged inputs.
func ValidateCheckpointArtifactAvailability(ctx context.Context, s store.RunStore, run *store.Run) error {
	if run == nil || run.Checkpoint == nil || s == nil {
		return nil
	}
	type revisionKey struct {
		nodeID  string
		version int
	}
	revisions := make(map[revisionKey]string)
	add := func(exact map[string]store.ArtifactRevisionRef) error {
		for logicalRef, revision := range exact {
			if revision.NodeID == "" {
				return fmt.Errorf("%w: artifact %q has no persisted producer identity", ErrArtifactContractUnavailable, logicalRef)
			}
			key := revisionKey{nodeID: revision.NodeID, version: revision.Version}
			if _, present := revisions[key]; !present {
				revisions[key] = logicalRef
			}
		}
		return nil
	}
	if err := add(run.Checkpoint.ArtifactRevisions); err != nil {
		return err
	}
	if run.Checkpoint.Parallel != nil {
		for _, branch := range run.Checkpoint.Parallel.Branches {
			if branch != nil {
				if err := add(branch.ArtifactRevisions); err != nil {
					return err
				}
			}
		}
	}
	keys := make([]revisionKey, 0, len(revisions))
	for key := range revisions {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].nodeID != keys[j].nodeID {
			return keys[i].nodeID < keys[j].nodeID
		}
		return keys[i].version < keys[j].version
	})
	for _, key := range keys {
		artifact, err := s.LoadArtifact(ctx, run.ID, key.nodeID, key.version)
		if err != nil {
			return fmt.Errorf("%w: load artifact %q from %s/%d: %v", ErrArtifactContractUnavailable, revisions[key], key.nodeID, key.version, err)
		}
		if artifact == nil || artifact.RunID != run.ID || artifact.NodeID != key.nodeID || artifact.Version != key.version {
			return fmt.Errorf("%w: artifact %q has mismatched persisted identity for %s/%d", ErrArtifactContractUnavailable, revisions[key], key.nodeID, key.version)
		}
	}
	return nil
}

func validateArtifactContracts(ctx context.Context, s store.RunStore, run *store.Run, wf *ir.Workflow, currentRevision string, forceSourceChange bool, ignoredNodes map[string]bool) error {
	if run == nil || s == nil || wf == nil {
		return nil
	}
	policy := store.ContextPolicyLegacy
	if run.ExecutionContext != nil && run.ExecutionContext.Policy != "" {
		policy = run.ExecutionContext.Policy
	}
	// Legacy cannot produce a refusal or report event. Avoid N full artifact
	// reads (S3 GETs for the cloud store) on the default compatibility path.
	if policy == store.ContextPolicyLegacy {
		return nil
	}
	var violations []string
	type validationKey struct {
		logicalRef string
		nodeID     string
		version    int
	}
	visited := make(map[validationKey]bool)
	var validateRevision func(artifactValidationRevision) error
	validateRevision = func(revision artifactValidationRevision) error {
		nodeID, version := revision.NodeID, revision.Version
		if nodeID == "" {
			violations = append(violations, fmt.Sprintf("artifact %q has no persisted producer identity", revision.LogicalRef))
			return nil
		}
		key := validationKey{logicalRef: revision.LogicalRef, nodeID: nodeID, version: version}
		if visited[key] {
			return nil
		}
		visited[key] = true
		artifact, err := s.LoadArtifact(ctx, run.ID, nodeID, version)
		if err != nil {
			msg := fmt.Sprintf("artifact %s/%d could not be loaded: %v", nodeID, version, err)
			if policy == store.ContextPolicyEnforce {
				return fmt.Errorf("%w: %s", ErrArtifactContractUnavailable, msg)
			}
			violations = append(violations, msg)
			return nil
		}
		if artifact == nil {
			violations = append(violations, fmt.Sprintf("artifact %s/%d returned no persisted body", nodeID, version))
			return nil
		}
		if artifact.Contract == nil {
			return nil
		}
		contract := artifact.Contract
		if artifact.RunID != run.ID || artifact.NodeID != nodeID || artifact.Version != version ||
			contract.LogicalRef == "" || contract.ProducerNode != nodeID || contract.Version != version {
			violations = append(violations, fmt.Sprintf("artifact %s/%d has an incomplete contract", nodeID, version))
			return nil
		}
		if revision.LogicalRef != "" && revision.LogicalRef != contract.LogicalRef {
			violations = append(violations, fmt.Sprintf("artifact %s/%d is recorded as %q but its contract publishes %q", nodeID, version, revision.LogicalRef, contract.LogicalRef))
			return nil
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
			} else {
				if got := nodePublish(node); got != contract.LogicalRef {
					violations = append(violations, fmt.Sprintf("artifact %q is now published as %q", contract.LogicalRef, got))
				}
				if schema := ir.NodeOutputSchema(node); schema != contract.Schema {
					violations = append(violations, fmt.Sprintf("artifact %q schema changed from %q to %q", contract.LogicalRef, contract.Schema, schema))
				}
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
			if err := validateRevision(artifactValidationRevision{LogicalRef: dep.LogicalRef, NodeID: depNode, Version: dep.Version}); err != nil {
				return fmt.Errorf("artifact %q requires %s v%d: %w", contract.LogicalRef, dep.LogicalRef, dep.Version, err)
			}
		}
		return nil
	}
	for _, revision := range artifactRevisionsForValidation(run) {
		if ignoredNodes[revision.NodeID] {
			continue
		}
		if err := validateRevision(revision); err != nil {
			return err
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
	return fmt.Errorf("%w: %s", ErrArtifactContractIncompatible, strings.Join(violations, "; "))
}

type artifactValidationRevision struct {
	LogicalRef string
	NodeID     string
	Version    int
}

// artifactRevisionsForValidation follows the checkpoint because it is the
// authoritative resume snapshot. ArtifactIndex is only a best-effort lookup
// cache in Mongo and may lag a completed write; it remains the fallback for
// legacy/no-checkpoint runs that predate exact logical provenance.
func artifactRevisionsForValidation(run *store.Run) []artifactValidationRevision {
	if run == nil {
		return nil
	}
	byKey := make(map[string]artifactValidationRevision)
	authoritative := run.Checkpoint != nil && run.Checkpoint.ArtifactRevisionsKnown
	add := func(revisions map[string]store.ArtifactRevisionRef) {
		if len(revisions) == 0 {
			return
		}
		authoritative = true
		for logicalRef, revision := range revisions {
			contractLogicalRef := logicalRef
			if revision.ContractLogicalRef != "" {
				contractLogicalRef = revision.ContractLogicalRef
			}
			key := fmt.Sprintf("%s\x00%d\x00%s", revision.NodeID, revision.Version, contractLogicalRef)
			byKey[key] = artifactValidationRevision{LogicalRef: contractLogicalRef, NodeID: revision.NodeID, Version: revision.Version}
		}
	}
	if run.Checkpoint != nil {
		add(run.Checkpoint.ArtifactRevisions)
		if run.Checkpoint.Parallel != nil {
			for _, branch := range run.Checkpoint.Parallel.Branches {
				if branch != nil {
					add(branch.ArtifactRevisions)
				}
			}
		}
	}
	if !authoritative {
		for nodeID, version := range run.ArtifactIndex {
			key := fmt.Sprintf("%s\x00%d", nodeID, version)
			byKey[key] = artifactValidationRevision{NodeID: nodeID, Version: version}
		}
	}
	out := make([]artifactValidationRevision, 0, len(byKey))
	for _, revision := range byKey {
		out = append(out, revision)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NodeID != out[j].NodeID {
			return out[i].NodeID < out[j].NodeID
		}
		if out[i].Version != out[j].Version {
			return out[i].Version < out[j].Version
		}
		return out[i].LogicalRef < out[j].LogicalRef
	})
	return out
}
