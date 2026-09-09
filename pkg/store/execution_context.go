package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ExecutionContextVersion is the wire/storage version of the execution
// context contract. The field is deliberately independent from Run's
// format version: the context is consumed by launch, resume and watcher
// authorities that may be upgraded independently.
const ExecutionContextVersion = 1

// ContextPolicy controls how a launch authority treats the context. Legacy
// keeps the pre-contract behaviour, report records the resolved context for
// diagnostics, and enforce is reserved for the admission gate introduced by
// the next reliability tranche.
type ContextPolicy string

const (
	ContextPolicyLegacy  ContextPolicy = "legacy"
	ContextPolicyReport  ContextPolicy = "report"
	ContextPolicyEnforce ContextPolicy = "enforce"
)

// WorkspaceMode is the isolation posture declared by a workflow.
type WorkspaceMode string

const (
	WorkspaceInherited WorkspaceMode = "inherited"
	WorkspaceIsolated  WorkspaceMode = "isolated"
	WorkspaceShared    WorkspaceMode = "shared"
)

// ContextRef identifies a store or namespace without carrying credentials or
// arbitrary business data. Revision is the immutable version observed at
// launch (when the backing store exposes one).
type ContextRef struct {
	ID        string `json:"id" bson:"id"`
	Kind      string `json:"kind" bson:"kind"`
	Namespace string `json:"namespace,omitempty" bson:"namespace,omitempty"`
	Revision  string `json:"revision,omitempty" bson:"revision,omitempty"`
	Required  bool   `json:"required,omitempty" bson:"required,omitempty"`
}

// WorkspaceContext describes the workspace contract, not the contents of a
// workspace. Root is a declared logical root (or an absolute path for local
// runs); it is useful for diagnostics and is never used as an authority to
// access another run's files.
type WorkspaceContext struct {
	Mode         WorkspaceMode `json:"mode,omitempty" bson:"mode,omitempty"`
	DeclaredMode string        `json:"declared_mode,omitempty" bson:"declared_mode,omitempty"`
	WorkspaceID  string        `json:"workspace_id,omitempty" bson:"workspace_id,omitempty"`
	DeclaredRoot string        `json:"declared_root,omitempty" bson:"declared_root,omitempty"`
}

// WorkflowContext binds execution to the compiled workflow and its source
// root. WorkflowRevision is normally the source hash; BundleRevision binds a
// stored bundle snapshot when the workflow came from one.
type WorkflowContext struct {
	WorkflowRevision string `json:"workflow_revision,omitempty" bson:"workflow_revision,omitempty"`
	WorkflowRoot     string `json:"workflow_root,omitempty" bson:"workflow_root,omitempty"`
	BundleRevision   string `json:"bundle_revision,omitempty" bson:"bundle_revision,omitempty"`
}

// LineageContext makes root/parent/filiation explicit. ParentRunID and
// ParentNodeID mirror Run's lineage fields so every authority can compare the
// declared launch context with the persisted run record.
type LineageContext struct {
	RootRunID    string `json:"root_run_id,omitempty" bson:"root_run_id,omitempty"`
	ParentRunID  string `json:"parent_run_id,omitempty" bson:"parent_run_id,omitempty"`
	ParentNodeID string `json:"parent_node_id,omitempty" bson:"parent_node_id,omitempty"`
}

// ExecutionContext is the versioned contract shared by launch, resume,
// runtime and watcher surfaces. It intentionally contains identities and
// revisions only: secrets, model prompts and business payloads do not belong
// in this durable diagnostic record.
type ExecutionContext struct {
	Version        int              `json:"version" bson:"version"`
	Policy         ContextPolicy    `json:"policy,omitempty" bson:"policy,omitempty"`
	RunStore       ContextRef       `json:"run_store" bson:"run_store"`
	BusinessStores []ContextRef     `json:"business_stores,omitempty" bson:"business_stores,omitempty"`
	Workspace      WorkspaceContext `json:"workspace" bson:"workspace"`
	Workflow       WorkflowContext  `json:"workflow" bson:"workflow"`
	Lineage        LineageContext   `json:"lineage" bson:"lineage"`
	LaunchSurface  string           `json:"launch_surface,omitempty" bson:"launch_surface,omitempty"`
}

// AdmissionDecision is the durable result of the pre-execution admission
// check. It is intentionally a small, append-only-shaped projection: a
// watcher can explain why a run was allowed or refused without replaying
// provider calls or guessing from free-form errors.
type AdmissionDecision struct {
	Decision         string        `json:"decision" bson:"decision"` // allowed | denied
	Phase            string        `json:"phase,omitempty" bson:"phase,omitempty"`
	Code             string        `json:"code,omitempty" bson:"code,omitempty"`
	Reason           string        `json:"reason,omitempty" bson:"reason,omitempty"`
	Policy           ContextPolicy `json:"policy,omitempty" bson:"policy,omitempty"`
	ContextVersion   int           `json:"context_version,omitempty" bson:"context_version,omitempty"`
	WorkflowRevision string        `json:"workflow_revision,omitempty" bson:"workflow_revision,omitempty"`
	CheckedAt        time.Time     `json:"checked_at" bson:"checked_at"`
}

// StableContextID returns a non-secret, deterministic identifier for a
// backing store/namespace pair. It lets local paths and remote namespaces be
// compared without persisting credentials or depending on a process pointer.
func StableContextID(kind, namespace string) string {
	kind = strings.TrimSpace(kind)
	namespace = strings.TrimSpace(namespace)
	sum := sha256.Sum256([]byte(kind + "\x00" + namespace))
	return hex.EncodeToString(sum[:8])
}

// Clone returns a detached context suitable for attaching to a Run. Launch
// callers often reuse their request object; sharing its slices would allow a
// later mutation to change the persisted contract before SaveRun.
func (c *ExecutionContext) Clone() *ExecutionContext {
	if c == nil {
		return nil
	}
	out := *c
	out.BusinessStores = append([]ContextRef(nil), c.BusinessStores...)
	return &out
}

// Normalize validates and canonicalizes a context. Sorting business stores
// makes equality and diagnostic fingerprints stable across launch surfaces.
func (c *ExecutionContext) Normalize() error {
	if c == nil {
		return nil
	}
	if c.Version == 0 {
		c.Version = ExecutionContextVersion
	}
	if c.Version != ExecutionContextVersion {
		return fmt.Errorf("store: unsupported execution context version %d", c.Version)
	}
	if c.Policy == "" {
		c.Policy = ContextPolicyLegacy
	}
	switch c.Policy {
	case ContextPolicyLegacy, ContextPolicyReport, ContextPolicyEnforce:
	default:
		return fmt.Errorf("store: unsupported execution context policy %q", c.Policy)
	}
	if c.RunStore.ID == "" {
		return fmt.Errorf("store: execution context run_store.id is required")
	}
	if c.RunStore.Kind == "" {
		return fmt.Errorf("store: execution context run_store.kind is required")
	}
	if err := validateWorkspaceMode(c.Workspace.Mode); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(c.BusinessStores))
	for i := range c.BusinessStores {
		ref := &c.BusinessStores[i]
		if strings.TrimSpace(ref.ID) == "" {
			return fmt.Errorf("store: execution context business_stores[%d].id is required", i)
		}
		if strings.TrimSpace(ref.Kind) == "" {
			return fmt.Errorf("store: execution context business_stores[%d].kind is required", i)
		}
		key := ref.Kind + "\x00" + ref.Namespace + "\x00" + ref.ID
		if _, ok := seen[key]; ok {
			return fmt.Errorf("store: duplicate execution context business store %q", ref.ID)
		}
		seen[key] = struct{}{}
	}
	sort.SliceStable(c.BusinessStores, func(i, j int) bool {
		return contextRefKey(c.BusinessStores[i]) < contextRefKey(c.BusinessStores[j])
	})
	return nil
}

func validateWorkspaceMode(mode WorkspaceMode) error {
	if mode == "" {
		return nil
	}
	switch mode {
	case WorkspaceInherited, WorkspaceIsolated, WorkspaceShared:
		return nil
	default:
		return fmt.Errorf("store: unsupported execution context workspace mode %q", mode)
	}
}

func contextRefKey(ref ContextRef) string {
	return ref.Kind + "\x00" + ref.Namespace + "\x00" + ref.ID + "\x00" + ref.Revision
}
