package runview

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/reliability"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ExecutionContextPolicyFromEnv provides a common opt-in switch for launch
// surfaces that do not construct a runview.Service (notably the CLI runner).
// Invalid or unset values intentionally fall back to legacy compatibility.
//
// The resolution itself lives in pkg/reliability, which owns the staged
// rollout knobs: ITERION_RELIABILITY_MODE is authoritative and
// ITERION_EXECUTION_CONTEXT_POLICY is the compatibility alias consulted when
// the new mode is unset or invalid. Delegating instead of re-deriving keeps a
// rollout report from naming a policy the launch surfaces do not actually apply.
func ExecutionContextPolicyFromEnv() store.ContextPolicy {
	return reliability.ContextPolicyFromEnv()
}

// ResolveExecutionContext builds the effective context for a launch. Callers
// may provide a declaration, but authority-owned identities and revisions are
// always filled from the compiled workflow and the run store. This keeps the
// contract useful in legacy/report mode without changing existing semantics.
func ResolveExecutionContext(ctx context.Context, runStore store.RunStore, runID string, spec LaunchSpec, wf *ir.Workflow, workflowHash string, defaultPolicy store.ContextPolicy, defaultWorkDir string) (*store.ExecutionContext, error) {
	var out *store.ExecutionContext
	if spec.ExecutionContext != nil {
		out = spec.ExecutionContext.Clone()
	} else {
		out = &store.ExecutionContext{}
	}

	// These identities come from the launch authority, never from the caller's
	// declaration. A stale or forged declaration must not become the value a
	// later enforce gate compares as authoritative.
	identity, err := store.ResolveStoreContextRef(ctx, runStore)
	if err != nil {
		return nil, fmt.Errorf("runview: resolve run-store identity: %w", err)
	}
	// Preserve an explicit requirement only for this exact resolved identity.
	// Legacy references can opt out too; equality does not certify their store.
	if store.SameContextRefIdentity(out.RunStore, identity) {
		identity.Required = out.RunStore.Required
	}
	out.RunStore = identity
	if out.Policy == "" {
		out.Policy = defaultPolicy
		if out.Policy == "" {
			out.Policy = store.ContextPolicyLegacy
		}
	}
	if workflowHash != "" {
		out.Workflow.WorkflowRevision = workflowHash
	}
	if spec.FilePath != "" {
		out.Workflow.WorkflowRoot = spec.FilePath
		if abs, err := filepath.Abs(out.Workflow.WorkflowRoot); err == nil {
			out.Workflow.WorkflowRoot = abs
		}
	}
	if spec.BotBundle != nil {
		out.Workflow.BundleRevision = spec.BotBundle.SnapshotDigest
	}
	out.Lineage.ParentRunID = spec.ParentRunID
	out.Lineage.ParentNodeID = spec.ParentNodeID
	out.Lineage.RootRunID = rootRunID(ctx, runStore, spec.ParentRunID, runID)
	if wf != nil {
		out.Workspace.DeclaredMode = wf.Worktree
		declared := make([]store.ContextRef, 0, len(wf.BusinessStores))
		for _, ref := range wf.BusinessStores {
			declared = append(declared, store.ContextRef{ID: ref.ID, Kind: ref.Kind, Namespace: ref.Namespace, Required: ref.Required})
		}
		out.BusinessStores, err = store.MergeBusinessStores(out.BusinessStores, declared)
		if err != nil {
			return nil, fmt.Errorf("runview: invalid business-store declaration: %w", err)
		}
	}
	// Effective mode and identity are runtime-owned and stay unresolved until
	// worktree setup/adoption has made the real isolation decision.
	out.Workspace.Mode = ""
	out.Workspace.WorkspaceID = ""
	if spec.WorkDir != "" || defaultWorkDir != "" {
		out.Workspace.DeclaredRoot = spec.WorkDir
		if out.Workspace.DeclaredRoot == "" {
			out.Workspace.DeclaredRoot = defaultWorkDir
		}
	}
	if out.LaunchSurface == "" {
		out.LaunchSurface = "runview"
	}
	if err := out.Normalize(); err != nil {
		return nil, fmt.Errorf("runview: invalid execution context: %w", err)
	}
	return out, nil
}

func (s *Service) resolveExecutionContext(ctx context.Context, runID string, spec LaunchSpec, wf *ir.Workflow, workflowHash string) (*store.ExecutionContext, error) {
	return ResolveExecutionContext(ctx, s.store, runID, spec, wf, workflowHash, s.executionContextPolicy, s.workDir)
}

// rootRunID follows persisted parent links when available. A missing parent
// is intentionally non-fatal: legacy launch paths may create the parent and
// child in different stores, and preserving the best known lineage is safer
// than rejecting an otherwise valid launch.
func rootRunID(ctx context.Context, runStore store.RunStore, parentID, fallback string) string {
	if parentID == "" || runStore == nil {
		return fallback
	}
	current := parentID
	seen := map[string]struct{}{}
	for i := 0; i < maxLineageWalk && current != ""; i++ {
		if _, ok := seen[current]; ok {
			break
		}
		seen[current] = struct{}{}
		r, err := runStore.LoadRun(ctx, current)
		if err != nil || r == nil || r.ParentRunID == "" {
			return current
		}
		current = r.ParentRunID
	}
	if current != "" {
		return current
	}
	return fallback
}
