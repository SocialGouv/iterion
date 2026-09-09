package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	schema := ir.NodeOutputSchema(node)
	contract := &store.ArtifactContract{
		LogicalRef:       logicalRef,
		ProducerNode:     nodeID,
		ProducerRevision: e.workflowHash,
		Version:          version,
		Schema:           schema,
		SchemaHash:       schemaFingerprint(e.workflow, schema),
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
		contract.Dependencies = append(contract.Dependencies, store.ArtifactDependency{
			LogicalRef: consumedRef,
			NodeID:     revision.NodeID,
			Version:    revision.Version,
			Required:   true,
		})
	}
	return contract
}

// schemaFingerprint digests a schema DEFINITION so the contract binds an
// artifact to its data SHAPE and not merely to the label the shape is
// declared under. `Schema` alone is a reference NAME: editing a schema's
// fields — removing or renaming a field a downstream node reads as
// `outputs.x.field`, i.e. the change that genuinely invalidates a persisted
// artifact — keeps that name, while renaming an unchanged schema changes no
// shape at all, so a name comparison misses the first and refuses the second.
//
// Returns "" when the name is empty or the workflow does not resolve it; the
// caller then falls back to comparing names, which is all a contract written
// before this field ever recorded.
//
// Fields are sorted before hashing: an artifact is a JSON object keyed by
// field name, so reordering a schema's declarations changes no shape. Enum
// values are part of the digest — narrowing an enum can invalidate a
// persisted value.
func schemaFingerprint(wf *ir.Workflow, name string) string {
	if wf == nil || name == "" {
		return ""
	}
	schema := wf.Schemas[name]
	if schema == nil {
		return ""
	}
	fields := make([]string, 0, len(schema.Fields))
	for _, f := range schema.Fields {
		if f == nil {
			continue
		}
		enum := append([]string(nil), f.EnumValues...)
		sort.Strings(enum)
		fields = append(fields, fmt.Sprintf("%s\x00%s\x00%s", f.Name, f.Type, strings.Join(enum, "\x01")))
	}
	sort.Strings(fields)
	sum := sha256.Sum256([]byte(strings.Join(fields, "\n")))
	return hex.EncodeToString(sum[:])
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

func validateArtifactContracts(ctx context.Context, s store.RunStore, run *store.Run, wf *ir.Workflow, currentRevision string, forceSourceChange bool, ignoredNodes map[string]bool) error {
	if run == nil || s == nil || wf == nil || len(run.ArtifactIndex) == 0 {
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
	// Walk the index in sorted order. Ranging a map hands the operator a
	// differently-ordered violation list on every attempt — the same broken
	// run reads as a different refusal each time it is retried, the report
	// event's `violations` array cannot be diffed between two passes, and a
	// test asserting on more than one violation is flaky by construction.
	nodeIDs := make([]string, 0, len(run.ArtifactIndex))
	for nodeID := range run.ArtifactIndex {
		if !ignoredNodes[nodeID] {
			nodeIDs = append(nodeIDs, nodeID)
		}
	}
	sort.Strings(nodeIDs)
	var violations []string
	for _, nodeID := range nodeIDs {
		version := run.ArtifactIndex[nodeID]
		artifact, err := s.LoadArtifact(ctx, run.ID, nodeID, version)
		if err != nil {
			msg := fmt.Sprintf("artifact %s/%d could not be loaded: %v", nodeID, version, err)
			if policy == store.ContextPolicyEnforce {
				return fmt.Errorf("%w: %s", ErrArtifactContractUnavailable, msg)
			}
			violations = append(violations, msg)
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
			// The SHAPE decides whenever both sides carry a fingerprint.
			// Comparing the schema NAME alone admits the change that actually
			// invalidates the artifact (editing the fields under a stable
			// name) and refuses the one that does not (renaming a schema whose
			// body is unchanged). Names stay the fallback for a contract
			// written before SchemaHash existed, and for a node whose schema
			// this workflow no longer resolves.
			schema := ir.NodeOutputSchema(node)
			if hash := schemaFingerprint(wf, schema); contract.SchemaHash != "" && hash != "" {
				if hash != contract.SchemaHash {
					violations = append(violations, fmt.Sprintf("artifact %q was produced against a different definition of schema %q", contract.LogicalRef, contract.Schema))
				}
			} else if schema != contract.Schema {
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
			persisted, err := s.LoadArtifact(ctx, run.ID, depNode, dep.Version)
			if err != nil {
				msg := fmt.Sprintf("artifact %q requires %s v%d, which could not be loaded: %v", contract.LogicalRef, dep.LogicalRef, dep.Version, err)
				if policy == store.ContextPolicyEnforce {
					return fmt.Errorf("%w: %s", ErrArtifactContractUnavailable, msg)
				}
				violations = append(violations, msg)
			} else if persisted == nil || persisted.NodeID != depNode || persisted.Version != dep.Version {
				violations = append(violations, fmt.Sprintf("artifact %q requires %s v%d, but the stored revision identity does not match", contract.LogicalRef, dep.LogicalRef, dep.Version))
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
	return fmt.Errorf("%w: %s", ErrArtifactContractIncompatible, strings.Join(violations, "; "))
}
