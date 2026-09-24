package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// A parent and a child whose bundles ship DIFFERENT engine-owned copies: a
// name only the parent ships, and a name both ship with different bytes. What
// each run reads is then observable name by name and byte by byte.
const (
	parentOnlySkill = "lang-parentonly.md"
	sharedNameSkill = "lang-python.md"
	parentOnlyBytes = "<!-- iterion:scanners\n[{\"id\":\"parent-only\",\"cmd\":\"true\"}]\n-->\n"
	parentPyBytes   = "<!-- iterion:scanners\n[{\"id\":\"parent-python\",\"cmd\":\"true\"}]\n-->\n"
	childPyBytes    = "<!-- iterion:scanners\n[{\"id\":\"child-python\",\"cmd\":\"true\"}]\n-->\n"
)

func parentOwnedCopy() map[string]string {
	return map[string]string{parentOnlySkill: parentOnlyBytes, sharedNameSkill: parentPyBytes}
}

func ownedCopyBundle(t *testing.T, files map[string]string) *bundle.Bundle {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("workflow resources:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skills := filepath.Join(dir, "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(skills, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &bundle.Bundle{Dir: dir, SourcePath: dir, SkillsDir: skills}
}

// readOwnedCopy reads the engine-owned copy the way a tool node does: at the
// directory ${BUNDLE_SKILLS_DIR} expands to, through the sandbox the node runs
// in (nil: on the host). It answers name → bytes.
func readOwnedCopy(run sandbox.Run, dir string) (map[string]string, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("${BUNDLE_SKILLS_DIR} expanded to %q, not an absolute path", dir)
	}
	out := map[string]string{}
	if run == nil {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			body, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}
			out[e.Name()] = string(body)
		}
		return out, nil
	}
	ctx := context.Background()
	res, err := run.Exec(ctx, []string{"sh", "-c", `cd "$1" && for f in *; do if test -f "$f"; then printf '%s\n' "$f"; fi; done`, "sh", dir}, sandbox.ExecOpts{})
	if err != nil || res.ExitCode != 0 {
		return nil, fmt.Errorf("list %s in the sandbox: %v (exit %d) %s", dir, err, res.ExitCode, res.Stderr)
	}
	for _, name := range strings.Fields(string(res.Stdout)) {
		body, err := run.Exec(ctx, []string{"cat", filepath.Join(dir, name)}, sandbox.ExecOpts{})
		if err != nil || body.ExitCode != 0 {
			return nil, fmt.Errorf("read %s/%s in the sandbox: %v (exit %d)", dir, name, err, body.ExitCode)
		}
		out[name] = string(body.Stdout)
	}
	return out, nil
}

func describeCopy(c map[string]string) string {
	names := make([]string, 0, len(c))
	for n := range c {
		names = append(names, n)
	}
	sort.Strings(names)
	return "[" + strings.Join(names, " ") + "]"
}

// copyPod is a copy-based parent sandbox: its commands act on its own copy of
// the workspace, and the write-through seam is how the host reaches that copy.
type copyPod interface {
	sandbox.Run
	sandbox.WorkspaceFileRefresher
}

// samePathCopyRun is the kubernetes shape: the driver copies the workspace
// into the pod at the SAME absolute path it has on the host and leaves
// WorkspaceFolder empty. Host and pod are two trees at one pathname, so the
// fake maps a path under the host workspace onto its own copy for every
// command it runs — without that, a reset in the "pod" would be a reset on the
// host and the two halves of the adoption could not be told apart.
type samePathCopyRun struct {
	*resourceCopyRun
	host string
}

func (r *samePathCopyRun) inPod(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if a == r.host || strings.HasPrefix(a, r.host+string(filepath.Separator)) {
			a = r.root + strings.TrimPrefix(a, r.host)
		}
		out[i] = a
	}
	return out
}

func (r *samePathCopyRun) Command(ctx context.Context, cmd []string, opts sandbox.ExecOpts) *exec.Cmd {
	return r.resourceCopyRun.Command(ctx, r.inPod(cmd), opts)
}

func (r *samePathCopyRun) Exec(ctx context.Context, cmd []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return r.resourceCopyRun.Exec(ctx, r.inPod(cmd), opts)
}

// childShape is one way a child in place meets its parent's workspace.
type childShape struct {
	name string
	// pod builds the parent's live copy-based sandbox from the parent's
	// workspace and returns it with the WorkspaceFolder the parent reports.
	// nil: neither run has a sandbox, and the host tree is the one they share.
	pod func(t *testing.T, work string) (copyPod, string)
}

func copyOfWorkspace(t *testing.T, work string) string {
	t.Helper()
	root := t.TempDir()
	if err := copyResourceEntry(filepath.Join(work, ".claude"), filepath.Join(root, ".claude")); err != nil {
		t.Fatal(err)
	}
	return root
}

var childShapes = []childShape{
	{name: "no sandbox"},
	{name: "copy-based sandbox, workspace at a path of its own", pod: func(t *testing.T, work string) (copyPod, string) {
		root := copyOfWorkspace(t, work)
		return &resourceCopyRun{sharedFakeRun: &sharedFakeRun{}, root: root}, root
	}},
	{name: "copy-based sandbox, empty WorkspaceFolder (the kubernetes shape)", pod: func(t *testing.T, work string) (copyPod, string) {
		return &samePathCopyRun{resourceCopyRun: &resourceCopyRun{sharedFakeRun: &sharedFakeRun{}, root: copyOfWorkspace(t, work)}, host: work}, ""
	}},
}

// podOwnedDir is where a run executing in the pod finds the owned copy: the
// expansion of ${BUNDLE_SKILLS_DIR} for a container workspace, or the host
// path when WorkspaceFolder is empty.
func podOwnedDir(folder, work string) string {
	if folder != "" {
		return ownedSkillsContainerDir(folder)
	}
	return OwnedSkillsDir(work)
}

// runParentWithOwnedCopy runs the parent to completion, leaving its own
// engine-owned copy in the workspace, and returns it.
func runParentWithOwnedCopy(t *testing.T, work string) *Engine {
	t.Helper()
	parent := New(resourceWorkflow(""), tmpStore(t), newStubExecutor(), WithWorkDir(work),
		WithBundle(ownedCopyBundle(t, parentOwnedCopy())), WithSandboxOverride("none"))
	if err := parent.Run(context.Background(), "parent", nil); err != nil {
		t.Fatalf("parent: %v", err)
	}
	return parent
}

// assertParentCopy: the parent reads its own bundle's names and bytes, and
// nothing else.
func assertParentCopy(t *testing.T, run sandbox.Run, dir, where string) {
	t.Helper()
	got, err := readOwnedCopy(run, dir)
	if err != nil {
		t.Fatalf("%s: the parent's engine-owned copy is unreadable after the child: %v", where, err)
	}
	want := parentOwnedCopy()
	if len(got) != len(want) {
		t.Fatalf("%s: after the child returned, the parent reads %s, want its own %s", where, describeCopy(got), describeCopy(want))
	}
	for name, body := range want {
		if got[name] != body {
			t.Fatalf("%s: after the child returned, the parent reads %s with %q, want its own bytes %q", where, name, got[name], body)
		}
	}
}

func childOptions(t *testing.T, work string, shape childShape, wrap func(copyPod) copyPod) ([]EngineOption, copyPod, string) {
	t.Helper()
	opts := []EngineOption{WithWorkDir(work), WithBundle(ownedCopyBundle(t, map[string]string{sharedNameSkill: childPyBytes})), WithParentRunID("parent")}
	if shape.pod == nil {
		return append(opts, WithSandboxOverride("none")), nil, ""
	}
	pod, folder := shape.pod(t, work)
	if wrap != nil {
		pod = wrap(pod)
	}
	return append(opts, WithSharedSandbox(&SharedSandbox{Run: pod, WorkspaceFolder: folder})), pod, folder
}

// A child in place reads ITS bundle's owned copy — never a name only the
// parent ships, never the parent's bytes under a shared name — and once it
// returns, the parent reads its OWN copy again, on the host and in the pod.
//
// Driven through Run on both sides, which is the only path that opens the
// child's resource scope, resets and refills the copy in the pod, and restores
// it at the end. The kubernetes shape (empty WorkspaceFolder) is the one
// production runs on.
func TestAChildReadsItsOwnSkillsCopyAndTheParentGetsItsBack(t *testing.T) {
	for _, shape := range childShapes {
		t.Run(shape.name, func(t *testing.T) {
			work := t.TempDir()
			parent := runParentWithOwnedCopy(t, work)
			opts, pod, folder := childOptions(t, work, shape, nil)

			ex := &sandboxCapturingExecutor{stubExecutor: newStubExecutor()}
			var child *Engine
			reads := 0
			for _, node := range []string{"before", "after"} {
				ex.on(node, func(map[string]any) (map[string]any, error) {
					got, err := readOwnedCopy(ex.sandbox, child.varExpandFn()("BUNDLE_SKILLS_DIR"))
					if err != nil {
						return nil, err
					}
					if _, leaked := got[parentOnlySkill]; leaked {
						return nil, fmt.Errorf("the child reads %s, a name only the parent's bundle ships: %s", parentOnlySkill, describeCopy(got))
					}
					if got[sharedNameSkill] != childPyBytes {
						return nil, fmt.Errorf("the child reads %s as %q, not its own bundle's bytes", sharedNameSkill, got[sharedNameSkill])
					}
					reads++
					return map[string]any{}, nil
				})
			}
			child = New(resourceWorkflow(""), parent.store, ex, opts...)
			if err := child.Run(context.Background(), "child", nil); err != nil {
				t.Fatalf("child: %v", err)
			}
			if reads != 2 {
				t.Fatalf("the child's nodes read the owned copy %d times, want 2", reads)
			}

			assertParentCopy(t, nil, parent.varExpandFn()("BUNDLE_SKILLS_DIR"), "on the host")
			if pod != nil {
				assertParentCopy(t, pod, podOwnedDir(folder, work), "in the sandbox")
			}
		})
	}
}

// refusingWriteThrough fails the write-through of every file whose
// workspace-relative path starts with refuse.
type refusingWriteThrough struct {
	copyPod
	refuse string
}

func (r refusingWriteThrough) RefreshWorkspaceFile(ctx context.Context, rel string, value []byte) error {
	if strings.HasPrefix(rel, r.refuse) {
		return fmt.Errorf("simulated write-through failure for %s", rel)
	}
	return r.copyPod.RefreshWorkspaceFile(ctx, rel, value)
}

// An adoption that aborts after the pod's copy was reset — the child's own
// copy did not land — refuses the child, names the consequence, and leaves the
// parent reading its OWN copy: the restore runs on every exit, this one
// included.
func TestAnAbortedAdoptionLeavesTheParentItsSkillsCopy(t *testing.T) {
	for _, shape := range childShapes {
		if shape.pod == nil {
			continue
		}
		t.Run(shape.name, func(t *testing.T) {
			work := t.TempDir()
			parent := runParentWithOwnedCopy(t, work)
			opts, pod, folder := childOptions(t, work, shape, func(p copyPod) copyPod {
				return refusingWriteThrough{copyPod: p, refuse: ".claude/" + ownedSkillsDirName + "/"}
			})
			child := New(resourceWorkflow(""), parent.store, &sandboxCapturingExecutor{stubExecutor: newStubExecutor()}, opts...)
			err := child.Run(context.Background(), "child", nil)
			if err == nil {
				t.Fatal("the adoption proceeded with an owned copy that never landed")
			}
			if !strings.Contains(err.Error(), "not covered") {
				t.Fatalf("the refusal does not name its consequence: %v", err)
			}
			assertParentCopy(t, nil, parent.varExpandFn()("BUNDLE_SKILLS_DIR"), "on the host")
			assertParentCopy(t, pod, podOwnedDir(folder, work), "in the sandbox")
		})
	}
}

// A file OUTSIDE the owned copy keeps the documented behaviour: a skill the
// agent cannot read is a degraded run, not a dead one. The two classes must
// not collapse into one rule.
func TestAdoptionToleratesAFailedAgentSkillWriteThrough(t *testing.T) {
	shape := childShapes[2]
	work := t.TempDir()
	parent := runParentWithOwnedCopy(t, work)
	opts, _, _ := childOptions(t, work, shape, func(p copyPod) copyPod {
		return refusingWriteThrough{copyPod: p, refuse: ".claude/skills/"}
	})
	child := New(resourceWorkflow(""), parent.store, &sandboxCapturingExecutor{stubExecutor: newStubExecutor()}, opts...)
	if err := child.Run(context.Background(), "child", nil); err != nil {
		t.Fatalf("an agent-facing skill that did not land must degrade the child, not kill it: %v", err)
	}
}

// A shared sandbox that cannot name its workspace absolutely is refused
// before anything in it is touched.
func TestAdoptionRefusesAnUnlocatableSharedWorkspace(t *testing.T) {
	work := t.TempDir()
	parent := runParentWithOwnedCopy(t, work)
	pod := &resourceCopyRun{sharedFakeRun: &sharedFakeRun{}, root: copyOfWorkspace(t, work)}
	child := New(resourceWorkflow(""), parent.store, &sandboxCapturingExecutor{stubExecutor: newStubExecutor()},
		WithWorkDir(work), WithBundle(ownedCopyBundle(t, map[string]string{sharedNameSkill: childPyBytes})),
		WithParentRunID("parent"), WithSharedSandbox(&SharedSandbox{Run: pod, WorkspaceFolder: "workspace"}))
	err := child.Run(context.Background(), "child", nil)
	if err == nil {
		t.Fatal("adoption accepted a shared workspace it cannot name")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("the refusal does not name what is wrong: %v", err)
	}
	assertParentCopy(t, nil, parent.varExpandFn()("BUNDLE_SKILLS_DIR"), "on the host")
	assertParentCopy(t, pod, ownedSkillsContainerDir(pod.root), "in the sandbox")
}

// Adopting a copy-based sandbox without the borrowed scope that saves the
// parent's entries is refused, and the pod is left as it was: resetting there
// would destroy the parent's copy, and not resetting would let its names
// answer for the child.
func TestCopyBasedAdoptionWithoutABorrowedScopeIsRefused(t *testing.T) {
	work := t.TempDir()
	parent := runParentWithOwnedCopy(t, work)
	pod := &resourceCopyRun{sharedFakeRun: &sharedFakeRun{}, root: copyOfWorkspace(t, work)}
	child := New(resourceWorkflow(""), parent.store, &sandboxCapturingExecutor{stubExecutor: newStubExecutor()},
		WithWorkDir(work), WithParentRunID("parent"), WithSharedSandbox(&SharedSandbox{Run: pod, WorkspaceFolder: pod.root}))
	if _, err := parent.store.CreateRun(context.Background(), "child", "resource-scope", nil); err != nil {
		t.Fatal(err)
	}
	_, err := child.startSandbox(context.Background(), "child", work, "", nil)
	if err == nil {
		t.Fatal("a copy-based adoption went ahead with nothing saved to restore the parent's entries from")
	}
	if !strings.Contains(err.Error(), "borrowed resource scope") {
		t.Fatalf("the refusal does not name what is missing: %v", err)
	}
	assertParentCopy(t, pod, ownedSkillsContainerDir(pod.root), "in the sandbox")
}

// A part of the owned copy the walk cannot read is a file that will not land,
// and the write-through says so. An owned copy that does not exist at all is a
// bundle shipping no skills, and is not an error.
func TestWriteThroughReportsAnUnreadablePartOfTheOwnedCopy(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not stop root from reading a directory")
	}
	pod := &resourceCopyRun{sharedFakeRun: &sharedFakeRun{}, root: t.TempDir()}

	work := t.TempDir()
	nested := filepath.Join(OwnedSkillsDir(work), "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "lang-go.md"), []byte(parentPyBytes), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nested, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(nested, 0o755) })
	if _, err := writeThroughMirroredSkills(context.Background(), work, pod, nil); err == nil || !strings.Contains(err.Error(), "walk") {
		t.Fatalf("an unreadable part of the owned copy went unreported: %v", err)
	}

	if _, err := writeThroughMirroredSkills(context.Background(), t.TempDir(), pod, nil); err != nil {
		t.Fatalf("a run whose bundle ships no skills was refused: %v", err)
	}
}
