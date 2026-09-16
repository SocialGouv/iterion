package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// A real filesystem stands in for the pod's COPY; refreshes write there and
// commands execute there. Host and sandbox skill trees remain distinct.
type resourceCopyRun struct {
	*sharedFakeRun
	root            string
	secretRefreshes int
}

func (r *resourceCopyRun) RefreshSecretFile(context.Context, string, []byte) error {
	r.secretRefreshes++
	return nil
}

func (r *resourceCopyRun) Command(ctx context.Context, cmd []string, opts sandbox.ExecOpts) *exec.Cmd {
	if opts.WorkDir == "" {
		opts.WorkDir = r.root
	}
	return r.sharedFakeRun.Command(ctx, cmd, opts)
}
func (r *resourceCopyRun) Exec(ctx context.Context, cmd []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	out, err := r.Command(ctx, cmd, opts).CombinedOutput()
	if err == nil {
		return sandbox.ExecResult{Stdout: out}, nil
	}
	if e, ok := err.(*exec.ExitError); ok {
		return sandbox.ExecResult{ExitCode: e.ExitCode(), Stderr: out}, nil
	}
	return sandbox.ExecResult{}, err
}
func (r *resourceCopyRun) RefreshWorkspaceFile(ctx context.Context, rel string, value []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p := filepath.Join(r.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, value, 0o644)
}

func TestChildResourcesRestoreSharedSandboxAndOwnDevboxPath(t *testing.T) {
	work := t.TempDir()
	st := tmpStore(t)
	parentBundle := resourceBundle(t, "parent")
	if err := os.WriteFile(filepath.Join(parentBundle.SkillsDir, "directory", "parent-only.md"), []byte("parent extra"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent := New(resourceWorkflow(""), st, newStubExecutor(), WithWorkDir(work), WithBundle(parentBundle), WithSandboxOverride("none"))
	if err := parent.Run(context.Background(), "parent", nil); err != nil {
		t.Fatal(err)
	}
	pod := &resourceCopyRun{sharedFakeRun: &sharedFakeRun{}, root: t.TempDir()}
	if err := copyResourceEntry(filepath.Join(work, ".claude"), filepath.Join(pod.root, ".claude")); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	// Install creates the child's declared executable in the project named by
	// -c. No network/Nix installation; PATH lookup and process execution are real.
	installer := `#!/bin/sh
set -eu
project="$3"
mkdir -p "$project/.devbox/nix/profile/default/bin"
printf '#!/bin/sh\nprintf child' > "$project/.devbox/nix/profile/default/bin/child-tool"
chmod +x "$project/.devbox/nix/profile/default/bin/child-tool"
`
	for name, body := range map[string]string{"devbox": installer, "parent-tool": "#!/bin/sh\nprintf parent\n"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	childBundle := resourceBundle(t, "child")
	if err := os.WriteFile(filepath.Join(childBundle.Dir, "devbox.json"), []byte(`{"packages":["child-tool"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := &sandboxCapturingExecutor{stubExecutor: newStubExecutor()}
	for _, name := range []string{"before", "after"} {
		ex.on(name, func(map[string]any) (map[string]any, error) {
			assertResourceBody(t, work, "child")
			assertResourceBody(t, pod.root, "child")
			if _, err := os.Stat(filepath.Join(pod.root, ".claude", "skills", "directory", "parent-only.md")); !os.IsNotExist(err) {
				return nil, fmt.Errorf("child sees leftover parent directory file: %v", err)
			}
			refresher, ok := ex.sandbox.(sandbox.SecretFileRefresher)
			if !ok {
				return nil, fmt.Errorf("child lost file-secret refresh capability")
			}
			if err := refresher.RefreshSecretFile(context.Background(), "test", []byte("test-value")); err != nil {
				return nil, err
			}
			res, err := ex.sandbox.Exec(context.Background(), []string{"sh", "-c", "test \"$(child-tool)\" = child && test \"$(parent-tool)\" = parent"}, sandbox.ExecOpts{})
			if err != nil || res.ExitCode != 0 {
				return nil, fmt.Errorf("child tools: %v, %s", err, res.Stderr)
			}
			return map[string]any{}, nil
		})
	}
	child := New(resourceWorkflow(""), st, ex, WithWorkDir(work), WithBundle(childBundle), WithParentRunID("parent"), WithSharedSandbox(&SharedSandbox{Run: pod, WorkspaceFolder: pod.root}))
	if err := child.Run(context.Background(), "child", nil); err != nil {
		t.Fatal(err)
	}
	assertResourceBody(t, work, "parent")
	assertResourceBody(t, pod.root, "parent")
	if raw, err := os.ReadFile(filepath.Join(pod.root, ".claude", "skills", "directory", "parent-only.md")); err != nil || string(raw) != "parent extra" {
		t.Fatalf("parent extra not restored: %s, %v", raw, err)
	}
	res, err := pod.Exec(context.Background(), []string{"sh", "-c", "command -v child-tool"}, sandbox.ExecOpts{})
	if err != nil || res.ExitCode == 0 {
		t.Fatalf("parent inherited child PATH: %v %s", err, res.Stdout)
	}
	ev := devboxEvent(t, st, "child")
	if ev["target"] != "shared_sandbox" {
		t.Fatalf("devbox event=%v", ev)
	}
	for _, raw := range ev["bin_dirs"].([]any) {
		if _, err := os.Stat(fmt.Sprint(raw)); !os.IsNotExist(err) {
			t.Fatalf("child install leaked: %s (%v)", raw, err)
		}
	}
	if pod.secretRefreshes != 2 {
		t.Fatalf("secret refreshes = %d", pod.secretRefreshes)
	}
	if pod.cleanups != 0 {
		t.Fatal("child cleaned up parent's sandbox")
	}
	if !strings.Contains(fmt.Sprint(ev["sources"]), "bot") {
		t.Fatalf("event=%v", ev)
	}
}
