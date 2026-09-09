package runview

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ExecutionContextPolicyFromEnv provides a common opt-in switch for launch
// surfaces that do not construct a runview.Service (notably the CLI runner).
// Invalid or unset values intentionally fall back to legacy compatibility.
func ExecutionContextPolicyFromEnv() store.ContextPolicy {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ITERION_EXECUTION_CONTEXT_POLICY"))) {
	case string(store.ContextPolicyReport):
		return store.ContextPolicyReport
	case string(store.ContextPolicyEnforce):
		return store.ContextPolicyEnforce
	default:
		return store.ContextPolicyLegacy
	}
}

// ResolveExecutionContext builds the effective context for a launch. Callers
// may provide a declaration, but authority-owned identities and revisions are
// always filled from the compiled workflow and the run store. This keeps the
// contract useful in legacy/report mode without changing existing semantics.
func ResolveExecutionContext(ctx context.Context, runStore store.RunStore, runID string, spec LaunchSpec, wf *ir.Workflow, workflowHash string, defaultPolicy store.ContextPolicy, defaultWorkDir string) *store.ExecutionContext {
	var out *store.ExecutionContext
	if spec.ExecutionContext != nil {
		out = spec.ExecutionContext.Clone()
	} else {
		out = &store.ExecutionContext{}
	}

	if out.RunStore.ID == "" {
		kind := fmt.Sprintf("%T", runStore)
		namespace := ""
		if runStore != nil {
			namespace = runStore.Root()
		}
		out.RunStore = store.ContextRef{
			ID:        store.StableContextID(kind, namespace),
			Kind:      kind,
			Namespace: namespace,
			Required:  true,
		}
	}
	if out.Policy == "" {
		out.Policy = defaultPolicy
		if out.Policy == "" {
			out.Policy = store.ContextPolicyLegacy
		}
	}
	if out.Workflow.WorkflowRevision == "" {
		out.Workflow.WorkflowRevision = workflowHash
	}
	if out.Workflow.WorkflowRoot == "" {
		out.Workflow.WorkflowRoot = spec.FilePath
		if out.Workflow.WorkflowRoot != "" {
			if abs, err := filepath.Abs(out.Workflow.WorkflowRoot); err == nil {
				out.Workflow.WorkflowRoot = abs
			}
		}
	}
	if out.Workflow.BundleRevision == "" && spec.BotBundle != nil {
		out.Workflow.BundleRevision = spec.BotBundle.SnapshotDigest
	}
	if out.Lineage.ParentRunID == "" {
		out.Lineage.ParentRunID = spec.ParentRunID
	}
	if out.Lineage.ParentNodeID == "" {
		out.Lineage.ParentNodeID = spec.ParentNodeID
	}
	if out.Lineage.RootRunID == "" {
		out.Lineage.RootRunID = rootRunID(ctx, runStore, spec.ParentRunID, runID)
	}
	if wf != nil {
		if out.Workspace.DeclaredMode == "" {
			out.Workspace.DeclaredMode = wf.Worktree
		}
		if out.Workspace.Mode == "" {
			if wf.Worktree == "auto" {
				out.Workspace.Mode = store.WorkspaceIsolated
			} else {
				out.Workspace.Mode = store.WorkspaceInherited
			}
		}
	}
	if out.Workspace.WorkspaceID == "" {
		if out.Workspace.Mode == store.WorkspaceIsolated {
			out.Workspace.WorkspaceID = runID
		} else if spec.ParentRunID != "" {
			out.Workspace.WorkspaceID = spec.ParentRunID
		} else {
			out.Workspace.WorkspaceID = runID
		}
	}
	if out.Workspace.DeclaredRoot == "" {
		out.Workspace.DeclaredRoot = spec.WorkDir
		if out.Workspace.DeclaredRoot == "" {
			out.Workspace.DeclaredRoot = defaultWorkDir
		}
	}
	if out.LaunchSurface == "" {
		out.LaunchSurface = "runview"
	}
	return out
}

func (s *Service) resolveExecutionContext(ctx context.Context, runID string, spec LaunchSpec, wf *ir.Workflow, workflowHash string) *store.ExecutionContext {
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
	for i := 0; i < 64 && current != ""; i++ {
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
