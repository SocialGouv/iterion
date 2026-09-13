package runview

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/portsactivation"
	"github.com/SocialGouv/iterion/pkg/store"
)

// rewindPortRun invalidates the selected data-graph suffix in one run-document
// CAS. Physical immutable output captures remain available for audit, but no
// invalidated publication can satisfy a later consumer or workflow export.
func (s *Service) rewindPortRun(ctx context.Context, run *store.Run, spec RewindSpec) (*RewindResult, error) {
	if err := portsactivation.RequireExistingAdmission(s.store, run); err != nil {
		return nil, err
	}
	if !run.Status.CanNativeResume() {
		return nil, fmt.Errorf("%w: %s", ErrRewindNotRewindable, run.Status)
	}
	if run.PortExecution == nil {
		return nil, fmt.Errorf("runview: rewind: native run %s has no execution checkpoint", run.ID)
	}
	if err := store.ValidatePortExecution(run.PortExecution); err != nil {
		return nil, err
	}
	if spec.Auto && spec.NodeID == "" {
		return nil, fmt.Errorf("runview: rewind: automatic pivot selection is not verified for native data graphs; select --node explicitly")
	}
	if !spec.KeepFiles && spec.RestoreScope != "" && spec.RestoreScope != RestoreScopeNone {
		return nil, fmt.Errorf("%w: native data graphs do not have a workspace boundary snapshot for this restore scope; use --restore-scope none", ErrRewindScopeUnavailable)
	}
	if run.Worktree && !spec.KeepFiles && spec.RestoreScope != RestoreScopeNone {
		return nil, fmt.Errorf("%w: native worktree rewind requires explicit --restore-scope none until per-node workspace snapshots are available", ErrRewindScopeUnavailable)
	}
	for _, invocation := range run.PortExecution.Invocations {
		if invocation.Status == store.PortAdmitted || invocation.Status == store.PortRunning || invocation.Status == store.PortUncertain {
			return nil, fmt.Errorf("runview: rewind: native invocation %s still owns work or an unresolved effect", invocation.ID)
		}
	}
	if len(run.PortExecution.Budget.Reservations) != 0 {
		return nil, fmt.Errorf("runview: rewind: native budget reservations remain unresolved")
	}
	sourcePath := spec.SourcePath
	if sourcePath == "" {
		sourcePath = resolveWorkflowPath(run)
	}
	if sourcePath == "" {
		return nil, fmt.Errorf("runview: rewind: native run %s has no workflow source path", run.ID)
	}
	wf, _, _, err := CompileWorkflowPath(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("runview: rewind: compile native source: %w", err)
	}
	if wf.RuntimeSemantics != ir.RuntimeSemanticsPortsV1 || wf.Ports == nil {
		return nil, fmt.Errorf("runview: rewind: source cannot change native runtime semantics: %w", store.ErrRunSemantics)
	}
	if wf.Ports.Nodes[spec.NodeID] == nil {
		return nil, fmt.Errorf("runview: rewind: node %q is not in the native graph", spec.NodeID)
	}
	reached := run.PortExecution.Collections[spec.NodeID] != nil
	for _, invocation := range run.PortExecution.Invocations {
		if invocation.Node == spec.NodeID && invocation.Status != store.PortPending {
			reached = true
			break
		}
	}
	if !reached {
		return nil, fmt.Errorf("%w: %q", ErrRewindNodeNotReached, spec.NodeID)
	}

	affected := nativeRewindDescendants(run.PortExecution, wf.Ports, spec.NodeID)
	next, err := run.PortExecution.Clone()
	if err != nil {
		return nil, err
	}
	next.Revision++
	next.Generation++
	for id, collection := range next.Collections {
		if !affected[collection.Node] {
			continue
		}
		for _, item := range collection.Items {
			delete(next.Invocations, item)
		}
		delete(next.Collections, id)
	}
	for _, invocation := range next.Invocations {
		if !affected[invocation.Node] {
			continue
		}
		if invocation.Status != store.PortPending {
			invocation.Attempt++
		}
		invocation.Status = store.PortPending
		invocation.Identity.Inputs = ""
		invocation.Inputs = map[string]string{}
		invocation.Outputs = nil
		invocation.Failure = ""
		invocation.EffectDispatched = false
		invocation.RecoveryDecision = ""
		invocation.RecoveryAttempt = 0
		invocation.ChildRunID = ""
		invocation.Resources = nil
	}
	for revision, publication := range next.Publications {
		if publication.Producer == "input" {
			continue
		}
		producer := publication.Producer
		if invocation := run.PortExecution.Invocations[producer]; invocation != nil && affected[invocation.Node] {
			delete(next.Publications, revision)
		} else if collection := run.PortExecution.Collections[producer]; collection != nil && affected[collection.Node] {
			delete(next.Publications, revision)
		}
	}
	for name, revision := range next.Exports {
		if next.Publications[revision] == nil {
			delete(next.Exports, name)
		}
	}
	if err := store.ValidatePortExecution(next); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(affected))
	for id := range affected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	correctionOwners := append([]string(nil), ids...)
	for id, invocation := range run.PortExecution.Invocations {
		if affected[invocation.Node] && id != invocation.Node {
			correctionOwners = append(correctionOwners, id)
		}
	}
	retireOutputCorrections(run, correctionOwners, time.Now().UTC())
	run.PortExecution = next
	run.Status = store.RunStatusCancelled
	run.Error, run.FailureCode = "", ""
	now := time.Now().UTC()
	run.UpdatedAt, run.FinishedAt = now, &now
	if err := s.store.SaveRun(ctx, run); err != nil {
		return nil, fmt.Errorf("runview: rewind: save native checkpoint: %w", err)
	}
	_, _ = s.store.AppendEvent(context.WithoutCancel(ctx), run.ID, store.Event{Type: store.EventRunRewound, RunID: run.ID, NodeID: spec.NodeID,
		Data: map[string]any{"runtime_semantics": ir.RuntimeSemanticsPortsV1, "dropped_nodes": ids, "generation": next.Generation, "files_reverted": false}})
	return &RewindResult{RunID: run.ID, NodeID: spec.NodeID, DroppedNodes: ids, Status: string(run.Status),
		Files: &FileRevertResult{Scope: string(RestoreScopeNone), SkipReason: "native rewind kept the workspace; immutable output captures were invalidated by reference"}}, nil
}

func nativeRewindDescendants(state *store.PortExecution, graph *ir.PortGraph, pivot string) map[string]bool {
	affected := map[string]bool{pivot: true}
	for changed := true; changed; {
		changed = false
		for id, instance := range graph.Nodes {
			for _, dependency := range instance.Dependencies {
				if affected[dependency] && !affected[id] {
					affected[id], changed = true, true
				}
			}
		}
		// Current source may have changed since the run. Captured supplier
		// revisions preserve the old dependency edges actually consumed.
		for _, invocation := range state.Invocations {
			for _, revision := range invocation.Inputs {
				publication := state.Publications[revision]
				if publication == nil {
					continue
				}
				producer := publication.Producer
				upstream := ""
				if source := state.Invocations[producer]; source != nil {
					upstream = source.Node
				} else if source := state.Collections[producer]; source != nil {
					upstream = source.Node
				}
				if affected[upstream] && !affected[invocation.Node] {
					affected[invocation.Node], changed = true, true
				}
			}
		}
	}
	return affected
}
