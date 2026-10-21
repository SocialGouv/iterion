package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestCLISubbotWritesIntoTheLentWorkspace(t *testing.T) {
	storeDir := t.TempDir()
	podBase := t.TempDir()
	workspace := t.TempDir()
	botDir := t.TempDir()
	t.Chdir(podBase)
	childPath := filepath.Join(botDir, "child.bot")
	writeFile(t, childPath, `tool create:
  command: write_file

workflow child:
  worktree: none
  sandbox: none
  entry: create
  create -> done
`)
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}

	run := subbotRunnerForCLI(filepath.Join(botDir, "parent.bot"), storeDir, st, iterlog.Nop(), RunOptions{Sandbox: "none"})
	_, err = run(context.Background(), runtime.SubbotRequest{
		Source: "child.bot", Vars: map[string]any{"path": "child.txt", "content": "CLI child\n"},
		ParentRunID: "cli-parent", NodeID: "child", ReattachKey: "child", WorkDir: workspace,
	})
	if err != nil {
		t.Fatalf("CLI subbot: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(workspace, "child.txt"))
	if err != nil {
		t.Errorf("read the lent workspace output: %v", err)
	} else if string(body) != "CLI child\n" {
		t.Errorf("workspace output = %q, want the child content", body)
	}
	if _, err := os.Stat(filepath.Join(podBase, "child.txt")); !os.IsNotExist(err) {
		t.Errorf("subbot write_file created in the CLI cwd: %v", err)
	}
}
