package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestCompileDelegatedWorkerRequiresHostContextAndWorktree(t *testing.T) {
	featureDev := filepath.Join("..", "..", "bots", "feature-dev", "main.bot")
	wf, err := compileDelegatedWorker(featureDev, "", "")
	if err != nil {
		t.Fatalf("feature-dev delegation contract: %v", err)
	}
	if wf.Worktree != "auto" {
		t.Fatalf("worktree = %q", wf.Worktree)
	}

	unsafe := `
vars:
  failure_context: string = ""
  delegation_instructions: string = ""
workflow unsafe:
  entry: done
  worktree: none
done done:
`
	if _, err := compileDelegatedWorker("unsafe.bot", unsafe, ""); err == nil {
		t.Fatal("worktree:none worker was accepted")
	}
}

func TestAssistantContextExposesRepairabilityWithoutHostPath(t *testing.T) {
	ctx := context.Background()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := rs.CreateRun(ctx, "failed", "broken_bot", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Status = store.RunStatusFailed
	r.BotOrigin = &store.BotOrigin{
		Kind: "git", ProjectID: "project-1", RepoRoot: "/host/secret/repo",
		Commit: "abc123", Package: "vertical-pipeline", WorkflowPath: "vertical-pipeline/bots/x/main.bot",
	}
	if err := rs.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	resolved, err := loadAssistantRun(ctx, r.ID, rs)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Repair == nil || !resolved.Repair.Repairable || resolved.Repair.SourceProject != "project-1" {
		t.Fatalf("repair context = %#v", resolved.Repair)
	}
}
