package runview

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

func TestStudioSubbotWritesIntoTheLentWorkspace(t *testing.T) {
	storeDir := t.TempDir()
	podBase := t.TempDir()
	workspace := t.TempDir()
	botDir := t.TempDir()
	t.Chdir(podBase)
	writeFile(t, botDir, "child.bot", `tool create:
  command: write_file

workflow child:
  worktree: none
  sandbox: none
  entry: create
  create -> done
`)

	svc, err := NewService(storeDir, WithLogger(iterlog.Nop()), WithWorkDir(podBase), WithSandboxDefault("none"))
	if err != nil {
		t.Fatal(err)
	}
	defer stopService(t, svc)

	run := svc.subbotRunnerFor(filepath.Join(botDir, "parent.bot"), iterlog.Nop())
	_, err = run(context.Background(), runtime.SubbotRequest{
		Source: "child.bot", Vars: map[string]any{"path": "child.txt", "content": "studio child\n"},
		ParentRunID: "studio-parent", NodeID: "child", ReattachKey: "child", WorkDir: workspace,
	})
	if err != nil {
		t.Fatalf("studio subbot: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(workspace, "child.txt"))
	if err != nil {
		t.Errorf("read the lent workspace output: %v", err)
	} else if string(body) != "studio child\n" {
		t.Errorf("workspace output = %q, want the child content", body)
	}
	if _, err := os.Stat(filepath.Join(podBase, "child.txt")); !os.IsNotExist(err) {
		t.Errorf("subbot write_file created in the studio cwd: %v", err)
	}
}
