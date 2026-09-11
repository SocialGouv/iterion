package runview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestBuildExecutor_UsesExplicitWorkDirForRelativeClawTools(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "needle.txt"), []byte("workspace needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	otherDir := t.TempDir()

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{
		Name: "workspace-tools",
		Nodes: map[string]ir.Node{
			"probe": &ir.AgentNode{
				BaseNode: ir.BaseNode{ID: "probe"},
				Tools:    []string{"workspace_grep"},
			},
		},
	}
	exec, err := BuildExecutor(ExecutorSpec{
		Workflow: wf,
		Store:    st,
		RunID:    "workspace-tools",
		StoreDir: t.TempDir(),
		WorkDir:  workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Close() })

	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(otherDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previousDir) })

	read, err := exec.Execute(context.Background(), &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "read"},
		Command:  "read_file",
	}, map[string]any{"path": "needle.txt"})
	if err != nil {
		t.Fatalf("relative read_file: %v", err)
	}
	if got := read["result"]; got != "workspace needle\n" {
		t.Fatalf("read_file result = %#v, want workspace content", got)
	}

	grep, err := exec.Execute(context.Background(), &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "grep"},
		Command:  "workspace_grep",
	}, map[string]any{"path": ".", "pattern": "workspace needle", "glob": "*.txt"})
	if err != nil {
		t.Fatalf("relative workspace_grep: %v", err)
	}
	result, _ := grep["result"].(string)
	if !strings.Contains(result, "needle.txt:1:workspace needle") {
		t.Fatalf("workspace_grep result = %q, want workspace match", result)
	}
}

func TestResolveExecutorWorkspace_ExplicitRootBeatsProcessCWD(t *testing.T) {
	workspace := t.TempDir()
	otherDir := t.TempDir()
	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(otherDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previousDir) })

	got, err := resolveExecutorWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if got != workspace {
		t.Fatalf("explicit workspace = %q, want %q", got, workspace)
	}

	fallback, err := resolveExecutorWorkspace("")
	if err != nil {
		t.Fatal(err)
	}
	if fallback != otherDir {
		t.Fatalf("fallback workspace = %q, want cwd %q", fallback, otherDir)
	}

	if _, err := resolveExecutorWorkspace(filepath.Join(workspace, "missing")); err == nil {
		t.Fatal("missing explicit workspace did not fail fast")
	}
}

func TestServiceEffectiveWorkDir_UsesOverrideThenServiceRoot(t *testing.T) {
	svc := &Service{workDir: "/service-root"}
	if got := svc.effectiveWorkDir("/run-root"); got != "/run-root" {
		t.Fatalf("override workdir = %q, want /run-root", got)
	}
	if got := svc.effectiveWorkDir(""); got != "/service-root" {
		t.Fatalf("fallback workdir = %q, want /service-root", got)
	}
}

func TestResumeExecutorSpec_UsesPersistedWorkDirThenServiceRoot(t *testing.T) {
	svc := &Service{workDir: "/service-root"}
	wf := &ir.Workflow{Name: "resume-workspace"}
	if got := svc.resumeExecutorSpec(wf, &store.Run{WorkDir: "/persisted-run-root"}, nil, "").WorkDir; got != "/persisted-run-root" {
		t.Fatalf("persisted resume workspace = %q, want /persisted-run-root", got)
	}
	if got := svc.resumeExecutorSpec(wf, &store.Run{}, nil, "").WorkDir; got != "/service-root" {
		t.Fatalf("legacy resume workspace = %q, want /service-root", got)
	}
}
