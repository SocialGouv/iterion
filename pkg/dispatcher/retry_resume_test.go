package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

func TestIsResumeSourceChanged(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("delegate: claude-code failed: context canceled"), false},
		{
			"runtime source-changed verbatim",
			fmt.Errorf(`runtime: workflow source has changed since run "019e6dd0" was started (expected hash 80fcb275d074, got 31e3bb64518a); re-run from scratch or use --force`),
			true,
		},
		{
			"wrapped source-changed",
			fmt.Errorf("dispatch run failed: %w", errors.New("runtime: workflow source has changed since run X was started")),
			true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isResumeSourceChanged(c.err); got != c.want {
				t.Errorf("isResumeSourceChanged(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestFreshRetryKeepsLogicalWorkspaceGeneration(t *testing.T) {
	c, _ := newCleanupTestDispatcher(
		t,
		WorkspacePersistKeep,
		filepath.Join(t.TempDir(), "workspaces"),
	)
	prev := &runningEntry{
		IssueID:             "fake:retry-workspace",
		Identifier:          "fake#retry-workspace",
		RunID:               "run-first-attempt",
		WorkspaceGeneration: "workspace-lease-first-logical-run",
	}
	c.scheduleRetry(prev.IssueID, prev, errors.New("non-resumable failure"))
	retry := c.state.retries[prev.IssueID]
	if retry == nil {
		t.Fatal("retry was not scheduled")
	}
	defer retry.Timer.Stop()

	runID, resumeID, generation, attempt, ok := c.resolveRunID(
		context.Background(),
		tracker.Issue{ID: prev.IssueID, Identifier: prev.Identifier},
	)
	if !ok {
		t.Fatal("retry run ID resolution failed")
	}
	if resumeID != "" {
		t.Fatalf("resume ID=%q, want a fresh non-resume attempt", resumeID)
	}
	if runID == prev.RunID {
		t.Fatalf("fresh retry reused terminal run ID %q", runID)
	}
	if generation != prev.WorkspaceGeneration {
		t.Fatalf("workspace generation=%q, want stable lease %q", generation, prev.WorkspaceGeneration)
	}
	if attempt != 1 {
		t.Fatalf("retry attempt=%d, want 1", attempt)
	}
	if got, want := c.workspaces.PathForRun(prev.IssueID, generation),
		c.workspaces.PathForRun(prev.IssueID, prev.WorkspaceGeneration); got != want {
		t.Fatalf("fresh retry workspace=%q, want prior logical workspace %q", got, want)
	}
}
