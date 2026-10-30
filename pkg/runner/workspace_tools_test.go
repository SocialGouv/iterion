package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

const workspaceWriteChild = `tool create:
  command: write_file

workflow child:
  worktree: none
  sandbox: none
  entry: create
  create -> done
`

func TestRunnerClawToolsUseTheRunClone(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	podBase := gittest.SourceRepo(t)
	clone := gittest.SourceRepo(t)
	for dir, body := range map[string]string{podBase: "pod\n", clone: "clone\n"} {
		if err := os.WriteFile(filepath.Join(dir, "needle.txt"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(podBase)

	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop(), WorkDir: podBase, SandboxOverride: "none"}}
	msg := &queue.RunMessage{RunID: "run-workspace-tools", WorkflowName: "wf"}
	exec, _, err := r.buildExecutor(context.Background(), msg, &ir.Workflow{Name: "wf"}, clone, iterlog.Nop(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeExecutor(exec)

	t.Run("read_file relative reads the clone", func(t *testing.T) {
		out, err := exec.Execute(context.Background(), &ir.ToolNode{
			BaseNode: ir.BaseNode{ID: "read"}, Command: "read_file",
		}, map[string]any{"path": "needle.txt"})
		if err != nil {
			t.Fatal(err)
		}
		if got := out["result"]; got != "clone\n" {
			t.Errorf("read_file = %#v, want the clone content, not the pod cwd", got)
		}
	})

	t.Run("write_file relative creates in the clone", func(t *testing.T) {
		_, err := exec.Execute(context.Background(), &ir.ToolNode{
			BaseNode: ir.BaseNode{ID: "write"}, Command: "write_file",
		}, map[string]any{"path": "created.txt", "content": "from the clone\n"})
		if err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(filepath.Join(clone, "created.txt"))
		if err != nil {
			t.Errorf("read the clone output: %v", err)
		} else if string(body) != "from the clone\n" {
			t.Errorf("clone output = %q, want the tool content", body)
		}
		if _, err := os.Stat(filepath.Join(podBase, "created.txt")); !os.IsNotExist(err) {
			t.Errorf("write_file created in the pod cwd: %v", err)
		}
	})
}

func TestRunnerSubbotWritesIntoTheLentWorkspace(t *testing.T) {
	r, _ := subbotTestRunner(t)
	podBase := t.TempDir()
	workspace := t.TempDir()
	botDir := t.TempDir()
	t.Chdir(podBase)
	writeSubbotFixture(t, botDir, "child.bot", workspaceWriteChild)
	msg := &queue.RunMessage{RunID: "run-parent-workspace", TenantID: "t1", OwnerID: "u1", BotID: "parent"}

	run := r.subbotRunnerFor(msg, botDir, workspace, iterlog.Nop())
	_, err := run(context.Background(), runtime.SubbotRequest{
		Source: "child.bot", Vars: map[string]any{"path": "child.txt", "content": "runner child\n"},
		ParentRunID: msg.RunID, NodeID: "child", ReattachKey: "child", WorkDir: workspace,
	})
	if err != nil {
		t.Fatalf("runner subbot: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(workspace, "child.txt"))
	if err != nil {
		t.Errorf("read the lent workspace output: %v", err)
	} else if string(body) != "runner child\n" {
		t.Errorf("workspace output = %q, want the child content", body)
	}
	if _, err := os.Stat(filepath.Join(podBase, "child.txt")); !os.IsNotExist(err) {
		t.Errorf("subbot write_file created in the pod cwd: %v", err)
	}
}

func TestRunnerBuildExecutorCreatesAMissingWorkspace(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	podBase := gittest.SourceRepo(t)
	t.Chdir(podBase)

	// A run with no repository to clone keeps the runner's base WorkDir,
	// which nothing materializes before the executor is built — the
	// cloud-e2e pod died on exactly that stat.
	missing := filepath.Join(t.TempDir(), "not-yet-created")
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop(), WorkDir: podBase, SandboxOverride: "none"}}
	msg := &queue.RunMessage{RunID: "run-missing-workspace", WorkflowName: "wf"}
	exec, _, err := r.buildExecutor(context.Background(), msg, &ir.Workflow{Name: "wf"}, missing, iterlog.Nop(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeExecutor(exec)

	if _, err := os.Stat(missing); err != nil {
		t.Fatalf("the run workspace was not materialized: %v", err)
	}
	_, err = exec.Execute(context.Background(), &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "write"}, Command: "write_file",
	}, map[string]any{"path": "created.txt", "content": "in the fresh workspace\n"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(missing, "created.txt")); err != nil {
		t.Errorf("write_file did not land in the materialized workspace: %v", err)
	}
}
