package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
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
	schema := ir.NodeOutputSchema(node)
	return &store.ArtifactContract{
		LogicalRef:       logicalRef,
		ProducerNode:     nodeID,
		ProducerRevision: e.workflowHash,
		Version:          version,
		Schema:           schema,
		SchemaHash:       schemaFingerprint(e.workflow, schema),
		Effects:          []string{"persist"},
	}
}

// schemaFingerprint digests a schema DEFINITION so the contract binds an
// artifact to its data SHAPE and not merely to the label the shape is
// declared under. Returns "" when the name is empty or the workflow does not
// resolve it — the caller then falls back to comparing names, which is all a
// legacy contract ever recorded.
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
	// Resolve the policy BEFORE reading anything. `legacy` is the default for
	// every run that predates the contract and it can neither refuse nor
	// report, so loading one artifact body per published node — an S3 GET
	// each on the cloud store, and agent outputs are not small — would buy
	// literally nothing. Only `report` and `enforce` pay for the reads.
	policy := store.ContextPolicyLegacy
	if c.Run.ExecutionContext != nil && c.Run.ExecutionContext.Policy != "" {
		policy = c.Run.ExecutionContext.Policy
	}
	if policy != store.ContextPolicyReport && policy != store.ContextPolicyEnforce {
		return nil
	}
	violations := collectArtifactContractViolations(ctx, c)
	if len(violations) == 0 {
		return nil
	}
	if policy != store.ContextPolicyEnforce {
		// `report` is the migration probe an operator runs BEFORE flipping a
		// deployment to `enforce`. Returning nil silently would make it a
		// policy that pays for every artifact read and answers nothing.
		if c.Logger != nil {
			c.Logger.Warn("run %s: artifact contract mismatch under policy %q — `enforce` would refuse this resume: %s",
				c.Run.ID, policy, strings.Join(violations, "; "))
		}
		return nil
	}
	return fmt.Errorf("artifact contract incompatible: %s", strings.Join(violations, "; "))
}

// collectArtifactContractViolations reads every persisted contract the caller
// did not exclude and returns what no longer matches the current workflow.
// Node ids are walked in sorted order so the message an operator reads is
// stable across runs.
func collectArtifactContractViolations(ctx context.Context, c ArtifactContractCheck) []string {
	run, s, wf := c.Run, c.Store, c.Workflow
	nodeIDs := make([]string, 0, len(run.ArtifactIndex))
	for nodeID := range run.ArtifactIndex {
		if !c.Skip[nodeID] {
			nodeIDs = append(nodeIDs, nodeID)
		}
	}
	sort.Strings(nodeIDs)
	var violations []string
	for _, nodeID := range nodeIDs {
		version := run.ArtifactIndex[nodeID]
		artifact, err := s.LoadArtifact(ctx, run.ID, nodeID, version)
		if err != nil {
			// An integrity policy must not read "I could not fetch the
			// contract" as "the contract is compatible". On the cloud store
			// this call IS the authoritative S3 GET, so an outage, an authz
			// failure, a timeout or a corrupt object all land here — and a
			// proven absence is not distinguishable from them today
			// (pkg/store/mongo/artifacts.go flattens blob.ErrArtifactNotFound
			// into a plain error). The index is only ever advanced by a
			// WriteArtifact that already persisted the body, so an entry that
			// will not load is itself the anomaly: report it, do not skip it.
			violations = append(violations, fmt.Sprintf("artifact %s/%d could not be read: %v", nodeID, version, err))
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
		// The SHAPE decides whenever both sides carry a fingerprint: renaming
		// a schema whose body is unchanged breaks nothing, while editing the
		// fields under a stable name is exactly the change that invalidates
		// the persisted artifact a downstream node reads as
		// `outputs.<node>.<field>`. Names remain the fallback for a contract
		// written before SchemaHash existed, and for a node whose schema the
		// current workflow no longer resolves.
		schema := ir.NodeOutputSchema(node)
		if hash := schemaFingerprint(wf, schema); contract.SchemaHash != "" && hash != "" {
			if hash != contract.SchemaHash {
				violations = append(violations, fmt.Sprintf("artifact %q was produced against a different definition of schema %q", contract.LogicalRef, contract.Schema))
			}
		} else if schema != contract.Schema {
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
	return violations
}
