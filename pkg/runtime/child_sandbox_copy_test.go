package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// copySandboxRun stands for the copy-based transport (the k8s path): the child
// reads its OWN copy of the workspace, so a `sh` script the engine runs inside
// it acts on that copy and not on the host tree.
//
// Here the copy is a real directory and the script really runs, which is what
// makes the difference between the two transports observable at all. Every
// fixture covering child resources runs with `sandbox: none`, so nothing in
// the suite exercised this path — and a divergence nothing exercises is one
// nobody measures.
type copySandboxRun struct{ root string }

func (copySandboxRun) Driver() string { return "copy-test" }

func (copySandboxRun) Command(ctx context.Context, cmd []string, _ sandbox.ExecOpts) *exec.Cmd {
	return exec.CommandContext(ctx, cmd[0], cmd[1:]...)
}

func (r copySandboxRun) Exec(ctx context.Context, cmd []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	c := r.Command(ctx, cmd, opts)
	var out, errOut bytes.Buffer
	c.Stdout, c.Stderr = &out, &errOut
	err := c.Run()
	res := sandbox.ExecResult{Stdout: out.Bytes(), Stderr: errOut.Bytes()}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		res.ExitCode = exit.ExitCode()
		return res, nil
	}
	return res, err
}

func (copySandboxRun) Cleanup(context.Context) error { return nil }

// The refresher is what makes the engine take the copy path at all
// (sharedSandboxIsCopyBased is a type assertion on it).
func (r copySandboxRun) RefreshWorkspaceFile(_ context.Context, rel string, value []byte) error {
	dst := filepath.Join(r.root, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, value, 0o600)
}

func writeClaudeFile(t *testing.T, root, rel, body string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTheCopyAChildReadsMatchesTheHostAfterAdoption.
//
// adoptSharedSandbox runs TWO steps: clearBorrowedSandboxResources wipes the
// four borrowed entries in the copy, and writeThroughMirroredSkills then walks
// the host's whole .claude/{skills,commands,agents} plus settings.json and
// pushes every file back. Judging either half alone says nothing about what
// the child reads.
//
// This drives the pair, which is the only thing that answers the question the
// two transports are compared on: does the child see the workspace's own
// resources?
func TestTheCopyAChildReadsMatchesTheHostAfterAdoption(t *testing.T) {
	host, copyRoot := t.TempDir(), t.TempDir()
	workspace := []string{
		".claude/settings.json",
		".claude/commands/review.md",
		".claude/agents/scout.md",
		".claude/skills/hand-written/SKILL.md",
	}
	for _, rel := range workspace {
		writeClaudeFile(t, host, rel, "workspace")
		writeClaudeFile(t, copyRoot, rel, "workspace")
	}
	// A parent file the host side already hid (hideParentSkillCollisions runs
	// before adoption): present in the copy, absent from the host.
	writeClaudeFile(t, copyRoot, ".claude/skills/from-parent/SKILL.md", "parent")

	run := copySandboxRun{root: copyRoot}
	e := &Engine{
		workDir:       host,
		sharedSandbox: &SharedSandbox{Run: run, WorkspaceFolder: copyRoot},
		resourceScope: &runResourceScope{path: host, borrowed: true, owned: map[string]string{}},
	}
	if err := e.clearBorrowedSandboxResources(context.Background()); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if n := writeThroughMirroredSkills(context.Background(), host, run, nil); n != len(workspace) {
		t.Fatalf("wrote %d files back, want %d", n, len(workspace))
	}

	for _, rel := range workspace {
		body, err := os.ReadFile(filepath.Join(copyRoot, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("the child's copy lost %s, which the workspace owns: %v", rel, err)
			continue
		}
		if string(body) != "workspace" {
			t.Errorf("%s = %q, want the workspace's bytes", rel, body)
		}
	}
	// And a parent file the host no longer has does not survive in the copy:
	// the child would otherwise read a resource its parent contributed.
	if _, err := os.Stat(filepath.Join(copyRoot, ".claude", "skills", "from-parent", "SKILL.md")); err == nil {
		t.Error("the child's copy kept a parent file the host had already hidden")
	}
}
