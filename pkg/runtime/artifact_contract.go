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
		LogicalRef:        logicalRef,
		ProducerNode:      nodeID,
		ProducerRevision:  e.workflowHash,
		Version:           version,
		Schema:            schema,
		SchemaFingerprint: schemaFingerprint(e.workflow, schema),
		Effects:           []string{"persist"},
	}
}

// schemaFingerprint canonicalises a RESOLVED output schema so a change to
// its body is visible even when the node still references the same name —
// which is what ir.NodeOutputSchema returns, and the most common shape of
// an incompatible edit. Empty when there is no schema to resolve; that
// reads as "unknown" and skips the comparison rather than inventing one.
func schemaFingerprint(wf *ir.Workflow, name string) string {
	if name == "" || wf == nil {
		return ""
	}
	schema, ok := wf.Schemas[name]
	if !ok || schema == nil {
		return ""
	}
	fields := make([]string, 0, len(schema.Fields))
	for _, f := range schema.Fields {
		if f == nil {
			continue
		}
		// An enum is a membership set, so its declaration order is not
		// part of the shape either.
		enum := append([]string(nil), f.EnumValues...)
		sort.Strings(enum)
		// The type goes in through String(), NEVER its iota value:
		// inserting a member into the FieldType enum would otherwise
		// invalidate every artifact ever written, fleet-wide, on an
		// engine upgrade that changed nothing about the workflow.
		fields = append(fields, f.Name+":"+f.Type.String()+"("+strings.Join(enum, ",")+")")
	}
	// Node outputs are maps, so a pure reordering of the declaration is
	// not an incompatibility and must not fire.
	sort.Strings(fields)
	sum := sha256.Sum256([]byte(name + "\n" + strings.Join(fields, "\n")))
	return hex.EncodeToString(sum[:])
}

// ArtifactContractRemedy is the operator action that resolves a contract
// refusal on a RESUME, shared by every surface that reports one — the engine
// hint the CLI prints and the wrap the HTTP layer answers with — so the
// studio does not show a dead end where the CLI shows a way out. Deliberately
// not baked into the validator's own error: the same function serves the
// rewind, where "rewind to the producing node" is not advice, it is what the
// caller is already doing.
const ArtifactContractRemedy = "rewind to the producing node so it re-executes (iterion rewind --node <id>), or restore its publish/output declaration"

// ArtifactContractCheck carries the inputs of ValidateArtifactContracts. A
// struct rather than positional arguments because the caller set is
// heterogeneous — a resume threads `--force`, a rewind threads the artifacts
// it is about to invalidate — and the next argument would be the seventh.
type ArtifactContractCheck struct {
	// Store reads the artifact versions named by Run.ArtifactIndex.
	Store store.RunStore
	// Run is the run about to be resumed or rewound.
	Run *store.Run
	// Workflow is the freshly compiled graph the caller will execute.
	Workflow *ir.Workflow
	// Revision is the workflow hash of Workflow. Empty skips the
	// provenance comparison (a rewind asserts no revision).
	Revision string
	// Force mirrors `resume --force`. It silences the provenance advisory
	// only: every substantive incompatibility — incomplete identity, a
	// changed publish reference, a changed schema, a missing dependency —
	// still refuses, because force is the operator asserting the stored
	// outputs are compatible, not a request to stop checking whether they
	// are (docs/resume.md).
	Force bool
	// Skip names nodes whose artifacts the caller is about to invalidate,
	// so a rewind is never refused by the very output it is discarding.
	Skip map[string]bool
	// Logger surfaces the advisories. Optional.
	Logger *iterlog.Logger
}

// ValidateArtifactContracts checks persisted artifact metadata before a
// resume or rewind can mutate the run, by the run's context policy:
//
//   - legacy — the regime does not apply; returns before reading anything.
//   - report — logs what enforce would have refused, so a deployment can
//     measure the flip before making it, and refuses nothing.
//   - enforce — refuses, nondestructively: the caller has not yet claimed
//     the checkpoint or touched the workspace.
//
// An artifact carrying no contract at all predates the feature and is
// accepted under every policy.
func ValidateArtifactContracts(ctx context.Context, check ArtifactContractCheck) error {
	s, run, wf := check.Store, check.Run, check.Workflow
	if run == nil || s == nil || wf == nil || len(run.ArtifactIndex) == 0 {
		return nil
	}
	// Resolve the policy BEFORE reading anything. `legacy` — the default
	// until ITERION_EXECUTION_CONTEXT_POLICY says otherwise, and what a run
	// predating execution contexts resolves to — means this regime does not
	// apply to the run, so the loop below would read every latest artifact
	// body (an S3 GET per published node on cloud, on every resume and every
	// usage-window retry) to reach a verdict nobody acts on. A deployment
	// that wants to see the verdict without acting on it has a tier for
	// that: `report`.
	policy := store.ContextPolicyLegacy
	if run.ExecutionContext != nil && run.ExecutionContext.Policy != "" {
		policy = run.ExecutionContext.Policy
	}
	if policy == store.ContextPolicyLegacy {
		return nil
	}
	var violations, advisories []string
	for nodeID, version := range run.ArtifactIndex {
		if check.Skip[nodeID] {
			continue
		}
		artifact, err := s.LoadArtifact(ctx, run.ID, nodeID, version)
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// The caller went away; that is not a verdict on the contract.
			// Folding it into the violations would report a cancellation as
			// an incompatible artifact — Engine.Resume stamps those
			// RESUME_INVALID with a "restore the declaration" hint — and
			// send the operator after a problem that does not exist.
			return fmt.Errorf("runtime: validate artifact contracts for run %s: %w", run.ID, err)
		case errors.Is(err, store.ErrArtifactNotFound):
			// An index entry with no artifact behind it is a legacy/cache
			// condition, and there is no contract to read: tolerated, and
			// never turned into a destructive rewind.
			continue
		case err != nil:
			// Anything else — a decode failure, a permission error, an
			// object-store outage — means the contract could NOT be read,
			// which is not the same as it being compatible. Admitting it
			// would make an enforce run pass everything for the duration
			// of a blip, the exact opposite of failing closed.
			violations = append(violations, fmt.Sprintf("artifact %s/%d could not be read: %v", nodeID, version, err))
			continue
		}
		if artifact == nil || artifact.Contract == nil {
			continue
		}
		// The identity fields are redundant on purpose, so they are checked
		// against the identity the artifact was ADDRESSED by — the index
		// key and version handed to the loader — not against the copy the
		// same blob reports about itself, which would partly compare a
		// value to itself and let a misbound contract through claiming
		// another producer or version.
		contract := artifact.Contract
		switch {
		case contract.LogicalRef == "" || contract.ProducerNode == "":
			violations = append(violations, fmt.Sprintf("artifact %s/%d has an incomplete contract", nodeID, version))
			continue
		case contract.ProducerNode != nodeID || artifact.NodeID != nodeID:
			violations = append(violations, fmt.Sprintf("artifact %s/%d carries the contract of node %q", nodeID, version, contract.ProducerNode))
			continue
		case contract.Version != version || artifact.Version != version:
			violations = append(violations, fmt.Sprintf("artifact %s/%d carries a contract for version %d", nodeID, version, contract.Version))
			continue
		}
		node, ok := wf.Nodes[nodeID]
		if !ok {
			// A node the graph no longer declares is ADVISORY, not a
			// refusal. Its artifact is unreachable — rebuildArtifacts only
			// maps nodes present in the workflow, and `{{outputs.<gone>}}`
			// is a compile error — so it can feed nothing. Refusing on it
			// left no way out at all: the resume refused, and so did the
			// rewind, because a node absent from the current graph is
			// absent from the rewind's invalidated set and never reaches
			// its skip list. Deleting a node is a normal edit; inert
			// history it leaves behind must not brick the run.
			advisories = append(advisories, fmt.Sprintf("artifact %q was produced by node %q, which the workflow no longer declares", contract.LogicalRef, nodeID))
			continue
		}
		if got := nodePublish(node); got != contract.LogicalRef {
			violations = append(violations, fmt.Sprintf("artifact %q is now published as %q", contract.LogicalRef, got))
		}
		schema := ir.NodeOutputSchema(node)
		switch {
		case schema != contract.Schema:
			violations = append(violations, fmt.Sprintf("artifact %q schema changed from %q to %q", contract.LogicalRef, contract.Schema, schema))
		case contract.SchemaFingerprint == "":
			// Legacy artifact, written before the body was fingerprinted.
		default:
			if got := schemaFingerprint(wf, schema); got != "" && got != contract.SchemaFingerprint {
				violations = append(violations, fmt.Sprintf("artifact %q was written against a different definition of schema %q", contract.LogicalRef, schema))
			}
		}
		// Provenance drift is ADVISORY, never a refusal. It is the same
		// coarse signal as the run-level source hash, which every resume
		// path already gates through ValidateResumeWorkflowHash — where
		// `--force` is the documented override. Refusing on it here too
		// would double-gate one signal with no override, and permanently:
		// a forced resume restamps Run.WorkflowHash to the new revision
		// (restampWorkflowSource) while the artifacts written before it
		// keep the old one, so every later resume of that run would need
		// a `--force` nothing indicates. What decides compatibility is
		// the substantive set above, which force does not waive.
		if !check.Force && check.Revision != "" && contract.ProducerRevision != "" && contract.ProducerRevision != check.Revision {
			advisories = append(advisories, fmt.Sprintf("artifact %q was produced by workflow revision %q, current revision is %q", contract.LogicalRef, contract.ProducerRevision, check.Revision))
		}
		for _, dep := range contract.Dependencies {
			if dep.LogicalRef == "" || !dep.Required {
				continue
			}
			depNode := dep.NodeID
			if depNode == "" {
				depNode = dep.LogicalRef
			}
			// Comma-ok, not the single-value form: artifact versions are
			// 0-based, so a producing node entirely absent from the index
			// would read as v0 and satisfy a `Version: 0` requirement —
			// silently admitting exactly the missing revision this contract
			// exists to catch.
			depVersion, present := run.ArtifactIndex[depNode]
			switch {
			case !present:
				violations = append(violations, fmt.Sprintf("artifact %q requires %s v%d, which is absent from the run", contract.LogicalRef, dep.LogicalRef, dep.Version))
			case depVersion < dep.Version:
				violations = append(violations, fmt.Sprintf("artifact %q requires %s v%d, persisted v%d", contract.LogicalRef, dep.LogicalRef, dep.Version, depVersion))
			}
		}
	}
	// Both lists are built by ranging run.ArtifactIndex, a map, so they are
	// sorted before joining: the same incompatibility must produce the same
	// message twice in a row, or an operator diffing two refusals reads a
	// reordering as a change.
	sort.Strings(advisories)
	sort.Strings(violations)
	if len(advisories) > 0 && check.Logger != nil {
		check.Logger.Warn("runtime: run %s carries artifacts the current workflow no longer accounts for: %s", run.ID, strings.Join(advisories, "; "))
	}
	if len(violations) == 0 {
		return nil
	}
	if policy != store.ContextPolicyEnforce {
		// The point of the report tier is to show what enforce WOULD refuse
		// before the policy is flipped. Returning nil in silence made it
		// inert and made this function's own doc comment false: nothing
		// downstream ever saw the violations to record them.
		if check.Logger != nil {
			check.Logger.Warn("runtime: run %s would be refused under the enforce policy (current policy %q): artifact contract incompatible: %s",
				run.ID, policy, strings.Join(violations, "; "))
		}
		return nil
	}
	return fmt.Errorf("artifact contract incompatible: %s", strings.Join(violations, "; "))
}
