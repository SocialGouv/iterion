package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

var (
	// ErrArtifactContractIncompatible is a durable compatibility refusal. It
	// lets HTTP and resume callers classify an operator-actionable conflict.
	ErrArtifactContractIncompatible = errors.New("artifact contract incompatible")
	// ErrArtifactContractUnavailable means validation could not read the
	// durable evidence. It is an infrastructure error, not a source mismatch.
	ErrArtifactContractUnavailable     = errors.New("artifact contract validation unavailable")
	artifactContractReportWriteTimeout = 5 * time.Second
)

func artifactContractPolicy(run *store.Run) store.ContextPolicy {
	if run != nil && run.ExecutionContext != nil && run.ExecutionContext.Policy != "" {
		return run.ExecutionContext.Policy
	}
	return store.ContextPolicyLegacy
}

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
		if !present || revision.NodeID == "" || revision.Unverified {
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

// nodePublishingRef resolves a logical artifact reference back to the node
// that publishes it, or "" when the workflow publishes no such ref. Iterated
// in sorted order so a workflow that (illegally) publishes one ref twice
// still resolves deterministically.
func nodePublishingRef(wf *ir.Workflow, logicalRef string) string {
	if wf == nil || logicalRef == "" {
		return ""
	}
	ids := make([]string, 0, len(wf.Nodes))
	for id := range wf.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if nodePublish(wf.Nodes[id]) == logicalRef {
			return id
		}
	}
	return ""
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
	floor := settledFloorFor(nodeID, resolveScope{rs: rs})
	return ir.NodeArtifactRefsForEdges(e.workflow, nodeID, func(edge *ir.Edge) bool {
		if edge.From != "" {
			if _, local := rs.outputs[edge.From]; !local {
				if _, inherited := rs.inheritedOutputs[edge.From]; !inherited {
					// An edge the settled floor feeds into the node reaches
					// it exactly like any other, so its artifact references
					// belong in the node's contract. Same eligibility rule
					// as the resolver — ONE function, so the two cannot
					// drift and leave a join consuming an artifact its
					// contract never named.
					return settledFloorEligible(edge, floor, false)
				}
			}
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
	return validateArtifactContracts(ctx, s, run, wf, currentRevision, forceSourceChange, nil, true, nil, false)
}

// ValidateArtifactContractsPreflight applies the synchronous compatibility
// gate without emitting report-mode telemetry. Use it before a detached or
// queued handoff: Engine.Resume repeats the authoritative check and emits the
// single event for that execution attempt.
func ValidateArtifactContractsPreflight(ctx context.Context, s store.RunStore, run *store.Run, wf *ir.Workflow, currentRevision string, forceSourceChange bool) error {
	return validateArtifactContracts(ctx, s, run, wf, currentRevision, forceSourceChange, nil, false, nil, false)
}

// ArtifactResumePreflight is an opaque, same-process snapshot of the exact
// immutable artifact bodies checked before a resume. It is deliberately tied
// to the artifact-relevant run state and workflow instance so Engine.Resume
// cannot reuse a stale preflight after an intervening checkpoint mutation.
type ArtifactResumePreflight struct {
	runID             string
	runSignature      string
	workflow          *ir.Workflow
	currentRevision   string
	forceSourceChange bool
	artifacts         map[artifactRevisionKey]*store.Artifact
}

func (p *ArtifactResumePreflight) matches(run *store.Run, wf *ir.Workflow, currentRevision string, forceSourceChange bool) bool {
	return p != nil && run != nil &&
		p.runID == run.ID && p.runSignature == artifactResumeValidationSignature(run) &&
		p.workflow == wf && p.currentRevision == currentRevision &&
		p.forceSourceChange == forceSourceChange
}

// consume returns the checked bodies only when the snapshot still matches and
// always releases its payload. The snapshot is a one-shot handoff: aliases in
// service launch state must not pin validation-only artifact histories after
// this resume boundary, including when an intervening mutation invalidates it.
func (p *ArtifactResumePreflight) consume(run *store.Run, wf *ir.Workflow, currentRevision string, forceSourceChange bool) (map[artifactRevisionKey]*store.Artifact, bool) {
	if p == nil {
		return nil, false
	}
	matches := p.matches(run, wf, currentRevision, forceSourceChange)
	artifacts := p.artifacts
	p.artifacts = nil
	if !matches {
		return nil, false
	}
	return artifacts, true
}

// artifactResumeValidationSignature excludes admission/status bookkeeping: an
// Engine persists its admission decision between the service preflight and
// Resume's artifact gate. It includes every run field that selects revisions
// or changes contract policy, so a real checkpoint/index mutation invalidates
// the snapshot even if it happens during that handoff.
func artifactResumeValidationSignature(run *store.Run) string {
	if run == nil {
		return ""
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00", run.ID, artifactContractPolicy(run), run.ArtifactCompatibilityRevision)
	for _, revision := range artifactRevisionsForValidation(run) {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%d\x00", revision.LogicalRef, revision.NodeID, revision.Version)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ValidateResumeArtifacts combines the exact checkpoint-availability guard
// and the emitting artifact-contract guard into one read pass. The returned
// snapshot may be handed to an Engine created with the same workflow through
// WithArtifactResumePreflight.
func ValidateResumeArtifacts(ctx context.Context, s store.RunStore, run *store.Run, wf *ir.Workflow, currentRevision string, forceSourceChange bool) (*ArtifactResumePreflight, error) {
	return validateResumeArtifacts(ctx, s, run, wf, currentRevision, forceSourceChange, true)
}

// ValidateResumeArtifactsPreflight is the non-emitting, verify-only variant
// for a detached or queued handoff. It validates each artifact in one pass
// without retaining its body: the remote Engine must load the immutable
// bodies itself, and a shared control-plane process must not accumulate an
// entire run's artifact history merely to admit the queued resume.
func ValidateResumeArtifactsPreflight(ctx context.Context, s store.RunStore, run *store.Run, wf *ir.Workflow, currentRevision string, forceSourceChange bool) (*ArtifactResumePreflight, error) {
	if err := validateArtifactContracts(ctx, s, run, wf, currentRevision, forceSourceChange, nil, false, nil, true); err != nil {
		return nil, err
	}
	return nil, nil
}

func validateResumeArtifacts(ctx context.Context, s store.RunStore, run *store.Run, wf *ir.Workflow, currentRevision string, forceSourceChange, emitReport bool) (*ArtifactResumePreflight, error) {
	loaded, err := loadCheckpointArtifactAvailability(ctx, s, run, nil)
	if err != nil {
		return nil, err
	}
	if loaded == nil {
		loaded = make(map[artifactRevisionKey]*store.Artifact)
	}
	if err := validateArtifactContracts(ctx, s, run, wf, currentRevision, forceSourceChange, nil, emitReport, loaded, false); err != nil {
		return nil, err
	}
	p := &ArtifactResumePreflight{
		workflow:          wf,
		currentRevision:   currentRevision,
		forceSourceChange: forceSourceChange,
		artifacts:         loaded,
	}
	if run != nil {
		p.runID = run.ID
		p.runSignature = artifactResumeValidationSignature(run)
	}
	return p, nil
}

// ValidateArtifactContractsExcept applies the resume/rewind contract guard
// while ignoring artifacts owned by nodes the caller is about to invalidate.
// It is used by rewind after it has computed the exact downstream set: an
// obsolete artifact must not prevent the operation that removes it.
func ValidateArtifactContractsExcept(ctx context.Context, s store.RunStore, run *store.Run, wf *ir.Workflow, currentRevision string, forceSourceChange bool, ignoredNodes map[string]bool) error {
	return validateArtifactContracts(ctx, s, run, wf, currentRevision, forceSourceChange, ignoredNodes, true, nil, false)
}

// ValidateCheckpointArtifactAvailability verifies that every exact physical
// revision named by an enforce-policy checkpoint can still be read. Legacy
// keeps checkpoint Outputs authoritative without touching the artifact store;
// report may observe an unavailable body but must not turn that observation
// into a resume refusal.
func ValidateCheckpointArtifactAvailability(ctx context.Context, s store.RunStore, run *store.Run) error {
	_, err := loadCheckpointArtifactAvailability(ctx, s, run, nil)
	return err
}

// ValidateCheckpointArtifactAvailabilityExcept applies the enforce-policy
// physical-availability guard while excluding producers a rewind is about to
// invalidate. Only revisions that survive the mutation need to remain
// executable.
func ValidateCheckpointArtifactAvailabilityExcept(ctx context.Context, s store.RunStore, run *store.Run, ignoredNodes map[string]bool) error {
	_, err := loadCheckpointArtifactAvailability(ctx, s, run, ignoredNodes)
	return err
}

// loadCheckpointArtifactAvailability returns the bodies it verified so a
// direct Engine.Resume can reuse the trunk revisions during reconstruction
// instead of issuing a second pre-claim S3 GET for the same immutable data.
func loadCheckpointArtifactAvailability(ctx context.Context, s store.RunStore, run *store.Run, ignoredNodes map[string]bool) (map[artifactRevisionKey]*store.Artifact, error) {
	if run == nil || run.Checkpoint == nil || s == nil {
		return nil, nil
	}
	if artifactContractPolicy(run) != store.ContextPolicyEnforce {
		return nil, nil
	}
	revisions := make(map[artifactRevisionKey]string)
	add := func(exact map[string]store.ArtifactRevisionRef) error {
		for logicalRef, revision := range exact {
			if ignoredNodes[revision.NodeID] {
				continue
			}
			if revision.NodeID == "" {
				return fmt.Errorf("%w: artifact %q has no persisted producer identity", ErrArtifactContractUnavailable, logicalRef)
			}
			key := artifactRevisionKey{nodeID: revision.NodeID, version: revision.Version}
			if _, present := revisions[key]; !present {
				revisions[key] = logicalRef
			}
		}
		return nil
	}
	if err := add(run.Checkpoint.ArtifactRevisions); err != nil {
		return nil, err
	}
	if run.Checkpoint.Parallel != nil {
		for _, branch := range run.Checkpoint.Parallel.Branches {
			if branch != nil {
				if err := add(branch.ArtifactRevisions); err != nil {
					return nil, err
				}
			}
		}
	}
	keys := make([]artifactRevisionKey, 0, len(revisions))
	for key := range revisions {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].nodeID != keys[j].nodeID {
			return keys[i].nodeID < keys[j].nodeID
		}
		return keys[i].version < keys[j].version
	})
	loaded := make(map[artifactRevisionKey]*store.Artifact, len(keys))
	for _, key := range keys {
		artifact, err := s.LoadArtifact(ctx, run.ID, key.nodeID, key.version)
		if err != nil {
			return nil, fmt.Errorf("%w: load artifact %q from %s/%d: %v", ErrArtifactContractUnavailable, revisions[key], key.nodeID, key.version, err)
		}
		if artifact == nil || artifact.RunID != run.ID || artifact.NodeID != key.nodeID || artifact.Version != key.version {
			return nil, fmt.Errorf("%w: artifact %q has mismatched persisted identity for %s/%d", ErrArtifactContractUnavailable, revisions[key], key.nodeID, key.version)
		}
		loaded[key] = artifact
	}
	return loaded, nil
}

func validateArtifactContracts(ctx context.Context, s store.RunStore, run *store.Run, wf *ir.Workflow, currentRevision string, forceSourceChange bool, ignoredNodes map[string]bool, emitReport bool, preloaded map[artifactRevisionKey]*store.Artifact, requireExactAvailability bool) error {
	if run == nil || s == nil || wf == nil {
		return nil
	}
	policy := artifactContractPolicy(run)
	// Legacy cannot produce a refusal or report event. Avoid N full artifact
	// reads (S3 GETs for the cloud store) on the default compatibility path.
	if policy == store.ContextPolicyLegacy {
		return nil
	}
	// A forced migration acknowledges all source-derived differences for one
	// exact target revision. The acknowledgement is persisted on the run when
	// the engine restamps its source, so later ordinary and automatic resumes
	// do not require an operator to repeat --force. Artifact identity and exact
	// dependency availability remain validated below on every resume.
	sourceChangeAccepted := forceSourceChange ||
		(currentRevision != "" && run.ArtifactCompatibilityRevision == currentRevision)
	var violations []string
	type dependencyKey struct {
		logicalRef string
		version    int
	}
	// Contracts written by early versions may omit dependency.NodeID. The
	// checkpoint is the authoritative record of which producer supplied a
	// logical value, so prefer it over an arbitrary current workflow publisher.
	persistedProducers := make(map[dependencyKey]string)
	for _, revision := range artifactRevisionsForValidation(run) {
		if revision.LogicalRef == "" || revision.NodeID == "" {
			continue
		}
		key := dependencyKey{logicalRef: revision.LogicalRef, version: revision.Version}
		if _, exists := persistedProducers[key]; !exists {
			persistedProducers[key] = revision.NodeID
		}
	}
	// Only checkpoint revisions can be consumed by resume reconstruction.
	// Dependency-only bodies still need to be inspected for contract integrity,
	// but retaining them in an in-process preflight needlessly pins potentially
	// large payloads until the engine handoff completes.
	reconstructable := make(map[artifactRevisionKey]bool)
	if run.Checkpoint != nil {
		addReconstructable := func(revisions map[string]store.ArtifactRevisionRef) {
			for _, revision := range revisions {
				if revision.NodeID != "" && !ignoredNodes[revision.NodeID] {
					reconstructable[artifactRevisionKey{nodeID: revision.NodeID, version: revision.Version}] = true
				}
			}
		}
		addReconstructable(run.Checkpoint.ArtifactRevisions)
		if run.Checkpoint.Parallel != nil {
			for _, branch := range run.Checkpoint.Parallel.Branches {
				if branch != nil {
					addReconstructable(branch.ArtifactRevisions)
				}
			}
		}
	}
	type validationKey struct {
		logicalRef string
		nodeID     string
		version    int
	}
	type validationLoad struct {
		artifact *store.Artifact
		err      error
	}
	visited := make(map[validationKey]bool)
	// Keep only identity and contract metadata for validation-only revisions.
	// This deduplicates physical reads when the same revision is reached under
	// multiple logical references without retaining its payload in the resume
	// preflight or across recursive validation frames.
	validationLoads := make(map[artifactRevisionKey]validationLoad)
	var validateRevision func(artifactValidationRevision) error
	validateRevision = func(revision artifactValidationRevision) error {
		nodeID, version := revision.NodeID, revision.Version
		if nodeID == "" {
			if requireExactAvailability && policy == store.ContextPolicyEnforce {
				return fmt.Errorf("%w: artifact %q has no persisted producer identity", ErrArtifactContractUnavailable, revision.LogicalRef)
			}
			violations = append(violations, fmt.Sprintf("artifact %q has no persisted producer identity", revision.LogicalRef))
			return nil
		}
		key := validationKey{logicalRef: revision.LogicalRef, nodeID: nodeID, version: version}
		if visited[key] {
			return nil
		}
		visited[key] = true
		physicalKey := artifactRevisionKey{nodeID: nodeID, version: version}
		artifact, loaded := preloaded[physicalKey]
		var err error
		if !loaded {
			if cached, ok := validationLoads[physicalKey]; ok {
				artifact, err = cached.artifact, cached.err
			} else {
				artifact, err = s.LoadArtifact(ctx, run.ID, nodeID, version)
				if err == nil && artifact != nil {
					if preloaded != nil && reconstructable[physicalKey] &&
						artifact.RunID == run.ID && artifact.NodeID == nodeID && artifact.Version == version {
						preloaded[physicalKey] = artifact
					}
					artifact = &store.Artifact{
						RunID: artifact.RunID, NodeID: artifact.NodeID, Version: artifact.Version,
						Contract: artifact.Contract,
					}
				}
				validationLoads[physicalKey] = validationLoad{artifact: artifact, err: err}
			}
		}
		if err != nil {
			msg := fmt.Sprintf("artifact %s/%d could not be loaded: %v", nodeID, version, err)
			if policy == store.ContextPolicyEnforce {
				return fmt.Errorf("%w: %s", ErrArtifactContractUnavailable, msg)
			}
			violations = append(violations, msg)
			return nil
		}
		if artifact == nil {
			if requireExactAvailability && policy == store.ContextPolicyEnforce {
				return fmt.Errorf("%w: artifact %s/%d returned no persisted body", ErrArtifactContractUnavailable, nodeID, version)
			}
			violations = append(violations, fmt.Sprintf("artifact %s/%d returned no persisted body", nodeID, version))
			return nil
		}
		if artifact.RunID != run.ID || artifact.NodeID != nodeID || artifact.Version != version {
			if requireExactAvailability && policy == store.ContextPolicyEnforce {
				return fmt.Errorf("%w: artifact %s/%d has mismatched persisted identity", ErrArtifactContractUnavailable, nodeID, version)
			}
			violations = append(violations, fmt.Sprintf("artifact %s/%d loaded as %s/%s/%d", nodeID, version, artifact.RunID, artifact.NodeID, artifact.Version))
			return nil
		}
		if artifact.Contract == nil {
			return nil
		}
		contract := artifact.Contract
		if contract.ProducerNode != nodeID {
			violations = append(violations, fmt.Sprintf("artifact %s/%d names producer %q", nodeID, version, contract.ProducerNode))
			return nil
		}
		if contract.LogicalRef == "" || contract.Version != version {
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
		if !sourceChangeAccepted {
			node, ok := wf.Nodes[nodeID]
			if !ok {
				violations = append(violations, fmt.Sprintf("artifact %q was produced by missing node %q", contract.LogicalRef, nodeID))
			} else {
				if got := nodePublish(node); got != contract.LogicalRef {
					violations = append(violations, fmt.Sprintf("artifact %q is now published as %q", contract.LogicalRef, got))
				}
				// Compare the schema shape whenever both sides carry a
				// fingerprint. Names remain the compatibility fallback for
				// artifacts written before SchemaHash existed.
				schema := ir.NodeOutputSchema(node)
				if hash := schemaFingerprint(wf, schema); contract.SchemaHash != "" && hash != "" {
					if hash != contract.SchemaHash {
						violations = append(violations, fmt.Sprintf("artifact %q was produced against a different definition of schema %q", contract.LogicalRef, contract.Schema))
					}
				} else if schema != contract.Schema {
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
			// A dependency may name its producer NODE, or only the logical ref
			// the workflow publishes it under. Falling back to the ref as if it
			// were a node id looks a publish name up in a store keyed by node
			// id: the load then fails for an artifact that is present, and
			// since an unreadable artifact fails closed under enforce, that
			// misresolution refuses a perfectly good resume as an
			// infrastructure error. Resolve the ref through the workflow that
			// publishes it instead, and when nothing does, say so as the
			// compatibility violation it is.
			depNode := dep.NodeID
			if depNode == "" {
				depNode = persistedProducers[dependencyKey{logicalRef: dep.LogicalRef, version: dep.Version}]
				if depNode == "" {
					depNode = nodePublishingRef(wf, dep.LogicalRef)
				}
			}
			if depNode == "" {
				violations = append(violations, fmt.Sprintf("artifact %q requires %s, which no node of this workflow publishes", contract.LogicalRef, dep.LogicalRef))
				continue
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
		if emitReport {
			eventCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), artifactContractReportWriteTimeout)
			defer cancel()
			_, _ = s.AppendEvent(eventCtx, run.ID, store.Event{
				Type:  store.EventArtifactContractViolation,
				RunID: run.ID,
				Data: map[string]any{
					"policy":     string(policy),
					"violations": append([]string(nil), violations...),
				},
			})
		}
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
