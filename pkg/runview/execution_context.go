package runview

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// resolveExecutionContext builds the effective context for a local launch.
// Callers may provide a declaration, but authority-owned identities and
// revisions are always filled from the compiled workflow and the run store.
// This keeps the first contract useful in legacy/report mode without changing
// existing launch semantics.
func (s *Service) resolveExecutionContext(ctx context.Context, runID string, spec LaunchSpec, wf *ir.Workflow, workflowHash string) *store.ExecutionContext {
	var out *store.ExecutionContext
	if spec.ExecutionContext != nil {
		out = spec.ExecutionContext.Clone()
	} else {
		out = &store.ExecutionContext{}
	}

	if out.RunStore.ID == "" {
		kind := fmt.Sprintf("%T", s.store)
		namespace := ""
		if s.store != nil {
			namespace = s.store.Root()
		}
		out.RunStore = store.ContextRef{
			ID:        store.StableContextID(kind, namespace),
			Kind:      kind,
			Namespace: namespace,
			Required:  true,
		}
	}
	if out.Policy == "" {
		out.Policy = store.ContextPolicyLegacy
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
	if out.Lineage.RootRunID == "" {
		out.Lineage.RootRunID = s.rootRunID(ctx, spec.ParentRunID, runID)
	}
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
			out.Workspace.DeclaredRoot = s.workDir
		}
	}
	if out.LaunchSurface == "" {
		out.LaunchSurface = "runview"
	}
	return out
}

// rootRunID follows persisted parent links when available. A missing parent
// is intentionally non-fatal: legacy launch paths may create the parent and
// child in different stores, and preserving the best known lineage is safer
// than rejecting an otherwise valid launch.
func (s *Service) rootRunID(ctx context.Context, parentID, fallback string) string {
	if parentID == "" || s.store == nil {
		return fallback
	}
	current := parentID
	seen := map[string]struct{}{}
	for i := 0; i < 64 && current != ""; i++ {
		if _, ok := seen[current]; ok {
			break
		}
		seen[current] = struct{}{}
		r, err := s.store.LoadRun(ctx, current)
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
