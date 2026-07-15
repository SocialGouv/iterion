package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestSubbotRunnerForCLI_NestedSubbot is the regression for nested subbots on
// the CLI path: subbotRunnerForCLI built the child engine WITHOUT its own
// SubbotRunner, so a child .bot that itself declared a subbot node died with
// "no SubbotRunner is wired" — even though maxSubbotDepth exists precisely to
// bound that recursion. The runner must be wired recursively, with the
// grandchild's source resolving relative to the CHILD's directory.
func TestSubbotRunnerForCLI_NestedSubbot(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "grandchild-ran.txt")

	grandchild := `vars:
  id: string = "none"
schema out:
  ok: bool
tool deep:
  command: ` + "`printf x > " + marker + `; printf '{"ok":true}'` + "`" + `
  output: out
workflow grandchild:
  worktree: none
  entry: deep
  deep -> done
`
	child := `vars:
  id: string = "none"
schema out:
  ok: bool
subbot go_deeper:
  source: "grandchild.bot"
  with { id: "{{vars.id}}" }
  output: out
workflow child:
  worktree: none
  entry: go_deeper
  go_deeper -> done
`
	for name, src := range map[string]string{"grandchild.bot": grandchild, "child.bot": child} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	storeDir := filepath.Join(dir, "store")
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	logger := iterlog.New(iterlog.LevelError, os.Stderr)

	runner := subbotRunnerForCLI(filepath.Join(dir, "parent.bot"), storeDir, s, logger, RunOptions{NoInteractive: true})
	out, err := runner(context.Background(), runtime.SubbotRequest{
		Source:      "child.bot",
		Vars:        map[string]any{"id": "n1"},
		ParentRunID: "parent-run",
		NodeID:      "run_child",
	})
	if err != nil {
		t.Fatalf("nested subbot run failed: %v", err)
	}
	// The child's terminal output IS its subbot node's output, i.e. the
	// grandchild's terminal output.
	if ok, _ := out["ok"].(bool); !ok {
		t.Errorf("child terminal output = %v, want grandchild's {ok:true}", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("grandchild tool never ran (marker missing): %v", err)
	}
}

// TestSubbotRunnerForCLI_RecursionDepthGuard proves the recursive wiring keeps
// the maxSubbotDepth backstop intact: a self-recursive child errors out with
// the depth-guard message instead of recursing forever.
func TestSubbotRunnerForCLI_RecursionDepthGuard(t *testing.T) {
	dir := t.TempDir()
	selfRef := `schema out:
  ok: bool
subbot again:
  source: "self.bot"
  output: out
workflow selfref:
  worktree: none
  entry: again
  again -> done
`
	if err := os.WriteFile(filepath.Join(dir, "self.bot"), []byte(selfRef), 0o644); err != nil {
		t.Fatalf("write self.bot: %v", err)
	}

	storeDir := filepath.Join(dir, "store")
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	logger := iterlog.New(iterlog.LevelError, os.Stderr)

	runner := subbotRunnerForCLI(filepath.Join(dir, "parent.bot"), storeDir, s, logger, RunOptions{NoInteractive: true})
	_, err = runner(context.Background(), runtime.SubbotRequest{
		Source:      "self.bot",
		ParentRunID: "parent-run",
		NodeID:      "again",
	})
	if err == nil {
		t.Fatal("self-recursive subbot must fail on the depth guard, got nil error")
	}
	if !strings.Contains(err.Error(), "recursion too deep") {
		t.Fatalf("expected depth-guard error, got: %v", err)
	}
}
