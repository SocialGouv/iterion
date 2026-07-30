package model

import (
	"context"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The engine reports which skills IT wrote into the workspace, and a backend
// passing skills explicitly may hand over only those — the workspace is a
// checkout of an untrusted repository, so nothing read back from it can
// establish provenance.
//
// This crosses the executor seam of that channel. Every other test in the chain
// hand-builds a delegate.Task, so a broken join here (the field never copied,
// or copied for one backend only) would leave them all green while pi received
// nothing — the exact shape of the MCP-forwarding regression next door.
func TestBuildTaskCarriesMirroredSkills(t *testing.T) {
	owned := []string{"/w/.claude/skills/doc-enrichment", "/w/.claude/skills/changelog-writer.md"}

	e := &ClawExecutor{logger: iterlog.Nop()}
	e.SetMirroredSkills(owned)

	node := &ir.AgentNode{}
	node.ID = "n"
	f := backendFields{id: "n"}

	// Not pi-specific: the channel states a fact about the workspace, and any
	// backend may come to consume it. claw is left out because reaching
	// buildTask on that path needs far more executor wiring (same reason as
	// TestBuildTaskForwardsDeclaredMCPServersPerBackend) — and the assignment
	// is unconditional, above every backend-specific branch, so these two
	// cover the seam.
	for _, backend := range []string{delegate.BackendPi, delegate.BackendClaudeCode} {
		t.Run(backend, func(t *testing.T) {
			task, err := e.buildTask(context.Background(), node, f, map[string]any{}, backend)
			if err != nil {
				t.Fatalf("buildTask: %v", err)
			}
			if !slices.Equal(task.MirroredSkills, owned) {
				t.Errorf("Task.MirroredSkills = %v, want %v", task.MirroredSkills, owned)
			}
		})
	}
}

// An executor that was never told carries nothing — the backend must not fall
// back to guessing paths from the workspace.
func TestBuildTaskMirroredSkillsDefaultsEmpty(t *testing.T) {
	e := &ClawExecutor{logger: iterlog.Nop()}
	node := &ir.AgentNode{}
	node.ID = "n"

	task, err := e.buildTask(context.Background(), node, backendFields{id: "n"}, map[string]any{}, delegate.BackendPi)
	if err != nil {
		t.Fatalf("buildTask: %v", err)
	}
	if len(task.MirroredSkills) != 0 {
		t.Errorf("Task.MirroredSkills = %v, want none", task.MirroredSkills)
	}
}
