package cli

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A card whose run was started outside the server (a CLI relaunch carrying a
// corrected input) keeps pointing at the previous attempt, so the board shows a
// stale run and no pending review. Repairing the link must go through the same
// locked SetLastRun the dispatcher uses, and must leave the rest of the issue
// untouched.
func TestRunIssueLinkRun_RepairsTheCardLink(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	project := t.TempDir()
	storeDir := filepath.Join(project, store.StoreDirName)
	root := filepath.Join(storeDir, "dispatcher")
	s, err := native.NewStore(root)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	created, err := s.Create(native.Issue{Title: "POC · epic", Body: "body"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if err := s.SetLastRun(created.ID, "run-old", ""); err != nil {
		t.Fatalf("seed previous attempt: %v", err)
	}

	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	if err := os.Chdir(project); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	p := &Printer{W: io.Discard}
	if err := RunIssueLinkRun(p, IssueLinkRunOptions{
		IDOrPrefix: created.ID,
		RunID:      "run-new",
	}); err != nil {
		t.Fatalf("link-run: %v", err)
	}

	reopened, err := native.NewStore(root)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	got, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatalf("get issue: %v", err)
	}
	if got.LastRunID != "run-new" {
		t.Fatalf("last run not repaired: got %q, want %q", got.LastRunID, "run-new")
	}
	// The previous attempt stays in the history: the card moves on, the record
	// of what already ran does not disappear.
	if len(got.Runs) != 2 || got.Runs[0].RunID != "run-old" || got.Runs[1].RunID != "run-new" {
		t.Fatalf("run history should append, got %+v", got.Runs)
	}
	if got.Title != "POC · epic" || got.Body != "body" {
		t.Fatalf("link-run must not touch the rest of the issue: %+v", got)
	}
}

func TestRunIssueLinkRun_RequiresRunID(t *testing.T) {
	if err := RunIssueLinkRun(&Printer{W: io.Discard}, IssueLinkRunOptions{IDOrPrefix: "x"}); err == nil {
		t.Fatal("an empty --run-id must be rejected, not silently clear the link")
	}
}
