package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/shellquote"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

const childResourceIOTimeout = 30 * time.Second

// sharedClaudeRoot is the in-sandbox path of the shared workspace's `.claude`
// directory, for the two places that read or reset a child's borrowed
// resources.
//
// It applies the rule containerWorkspaceFolder already states: an explicit
// WorkspaceFolder wins, an empty one means "the same absolute path as on the
// host". A driver with no host filesystem — the kubernetes one — cannot
// bind-mount anything, so it COPIES the workspace to that same absolute path
// inside the pod and leaves WorkspaceFolder empty. Empty is therefore a normal
// state on the driver production runs on, not a missing value.
//
// The refusal is kept for the state that really is broken: no absolute path on
// either side. Joining an empty workspace yields the RELATIVE ".claude", which
// resolves against whatever cwd the exec lands in — a silent wrong target for
// a recursive delete.
//
// The host-side derivation holds only while every copy-based driver honours
// same-absolute-path, and the Run handle exposes no accessor to ask it — the
// refresher addresses files RELATIVE to a root it never surfaces. Worse, the
// interface doc points the other way: RunInfo.WorkspacePath says the driver
// copies "at [Spec.WorkspaceFolder] (default `/workspace`)". A second
// copy-based driver written against that sentence would copy to a fixed root,
// and this fallback would then name a path that is simply ABSENT from the
// sandbox. So the scripts below assert the root before touching it: the
// mismatch becomes a named refusal instead of a reset of the wrong tree.
func (e *Engine) sharedClaudeRoot() (workspace, claudeRoot string, err error) {
	workspace = e.sharedWorkspaceFolder()
	if workspace == "" {
		workspace = e.workDir
	}
	if !path.IsAbs(workspace) {
		return "", "", fmt.Errorf("child resources: no absolute shared workspace path "+
			"(sandbox workspace folder %q, engine work dir %q) — a relative root "+
			"would resolve against the exec's cwd",
			e.sharedWorkspaceFolder(), e.workDir)
	}
	return workspace, path.Join(workspace, ".claude"), nil
}

// assertWorkspaceRoot is the first line of both scripts. `set -eu` alone would
// let a missing root pass: `cp` of nothing and `rm -rf` of nothing both exit 0,
// so the wrong-root case would read as a clean no-op.
const assertWorkspaceRoot = `test -d "$3" || { echo "shared workspace root $3 is absent from the sandbox — ` +
	`the driver copied the workspace somewhere else" >&2; exit 3; }
`

func (e *Engine) sharedWorkspaceFolder() string {
	if e.sharedSandbox == nil {
		return ""
	}
	return e.sharedSandbox.WorkspaceFolder
}

func (e *Engine) snapshotSharedChildResources(ctx context.Context, backupName string) (func() error, error) {
	shared := e.sharedSandbox
	if shared == nil || shared.Run == nil || !sharedSandboxIsCopyBased(shared.Run) {
		return func() error { return nil }, nil
	}
	workspace, root, err := e.sharedClaudeRoot()
	if err != nil {
		return nil, err
	}
	backup := path.Join("/tmp", backupName)
	runScript := func(ctx context.Context, script string) error {
		res, err := shared.Run.Exec(ctx, []string{"sh", "-c", script, "sh", root, backup, workspace}, sandbox.ExecOpts{})
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("shared child resources (backup %s): exit %d: %s", backup, res.ExitCode, res.Stderr)
		}
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, childResourceIOTimeout)
	defer cancel()
	err = runScript(cctx, `set -eu
`+assertWorkspaceRoot+`mkdir -p "$2"
for name in skills commands agents settings.json; do
 if test -e "$1/$name" || test -L "$1/$name"; then cp -a "$1/$name" "$2/$name"; fi
done`)
	if err != nil {
		return nil, err
	}
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), childResourceIOTimeout)
		defer cancel()
		return runScript(ctx, `set -eu
`+assertWorkspaceRoot+`for name in skills commands agents settings.json; do
 rm -rf "$1/$name"
 if test -e "$2/$name" || test -L "$2/$name"; then mkdir -p "$1"; cp -a "$2/$name" "$1/$name"; fi
done
rm -rf "$2"`)
	}, nil
}

// The copy must receive the complete effective child tree. Merely overwriting
// files leaves parent-only files behind when a child replaces a directory skill.
// beginRunResources already saved the original copy for restoration on all exits.
func (e *Engine) clearBorrowedSandboxResources(ctx context.Context) error {
	if e.resourceScope == nil || !e.resourceScope.borrowed || !sharedSandboxIsCopyBased(e.sharedSandbox.Run) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, childResourceIOTimeout)
	defer cancel()
	workspace, root, err := e.sharedClaudeRoot()
	if err != nil {
		return err
	}
	res, err := e.sharedSandbox.Run.Exec(ctx, []string{"sh", "-c", `set -eu
` + assertWorkspaceRoot + `for name in skills commands agents settings.json; do rm -rf "$1/$name"; done`,
		"sh", root, "", workspace}, sandbox.ExecOpts{})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("reset borrowed sandbox resources: exit %d: %s", res.ExitCode, res.Stderr)
	}
	return nil
}

// resourceDirForRun prevents a bare child.bot beside a bundle main.bot from
// inheriting that sibling's devbox.json through the legacy catalog fallback.
func (e *Engine) resourceDirForRun() string {
	if e.parentRunID != "" && e.bundle == nil {
		return ""
	}
	return bundleResourceDir(e.bundle, e.filePath)
}

// Child provisioning is staged at a unique path, then attached to the child's
// handle only. Neither the parent's botDevboxDir nor its environment changes.
func (e *Engine) provisionSharedChildDevbox(ctx context.Context, runID string, inherited sandbox.Run) (sandbox.Run, func()) {
	dir := e.resourceDirForRun()
	config := devboxConfigIn(dir, "child bundle", e.logger)
	noop := func() {}
	if config == "" {
		return inherited, noop
	}
	inline, err := readInlineDevbox(dir)
	staged := fmt.Sprintf("/tmp/iterion-devbox/child-%x", sha256.Sum256([]byte(runID)))
	bin := path.Join(staged, devboxProfileBin)
	if err == nil {
		script := devboxInstallSnippet([]devboxProject{{label: "child bot", hostConfig: config, dir: staged, inline: inline}}) + "\ntest -d " + shellquote.Quote(bin)
		installCtx, cancel := context.WithTimeout(ctx, hostDevboxInstallTimeout)
		res, runErr := inherited.Exec(installCtx, []string{"sh", "-c", script}, sandbox.ExecOpts{})
		cancel()
		err = runErr
		if err == nil && res.ExitCode != 0 {
			err = fmt.Errorf("child devbox install exit %d: %s", res.ExitCode, res.Stderr)
		}
	}
	cleanup := func() {
		cctx, cancel := context.WithTimeout(context.Background(), childResourceIOTimeout)
		defer cancel()
		if res, err := inherited.Exec(cctx, []string{"rm", "-rf", staged}, sandbox.ExecOpts{}); (err != nil || res.ExitCode != 0) && e.logger != nil {
			e.logger.Warn("runtime: child devbox cleanup %s: %v (exit %d)", staged, err, res.ExitCode)
		}
	}
	data := map[string]any{"target": "shared_sandbox", "sources": []string{"bot"}, "configs": []string{config}}
	result := inherited
	if err != nil {
		data["errors"] = []string{err.Error()}
		if e.logger != nil {
			e.logger.Warn("runtime: child devbox: %v — child packages unavailable", err)
		}
	} else {
		data["bin_dirs"] = []string{bin}
		result = withChildSandboxPath(inherited, bin)
	}
	if emitErr := e.emit(ctx, runID, store.EventSandboxDevboxProvisioned, "", data); emitErr != nil && e.logger != nil {
		e.logger.Warn("runtime: child devbox event: %v", emitErr)
	}
	return result, cleanup
}

type childPathRun struct {
	sandbox.Run
	bin string
}

func (r *childPathRun) command(argv []string) []string {
	return append([]string{"sh", "-c", "PATH=" + shellquote.Quote(r.bin) + ":\"$PATH\"; export PATH; exec \"$@\"", "sh"}, argv...)
}
func (r *childPathRun) Command(ctx context.Context, argv []string, opts sandbox.ExecOpts) *exec.Cmd {
	return r.Run.Command(ctx, r.command(argv), opts)
}
func (r *childPathRun) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return r.Run.Exec(ctx, r.command(argv), opts)
}
func (r *childPathRun) Cleanup(context.Context) error { return nil } // borrowed handle
// Preserve the capability tested by script staging and delegate state mirroring.
type childCopyPathRun struct {
	*childPathRun
	sandbox.WorkspaceFileRefresher
}

type childSecretPathRun struct {
	*childPathRun
	sandbox.SecretFileRefresher
}
type childCopySecretPathRun struct {
	*childCopyPathRun
	sandbox.SecretFileRefresher
}

func withChildSandboxPath(run sandbox.Run, bin string) sandbox.Run {
	wrapped := &childPathRun{Run: run, bin: bin}
	secret, secrets := run.(sandbox.SecretFileRefresher)
	if refresher, ok := run.(sandbox.WorkspaceFileRefresher); ok {
		copyRun := &childCopyPathRun{childPathRun: wrapped, WorkspaceFileRefresher: refresher}
		if secrets {
			return &childCopySecretPathRun{childCopyPathRun: copyRun, SecretFileRefresher: secret}
		}
		return copyRun
	}
	if secrets {
		return &childSecretPathRun{childPathRun: wrapped, SecretFileRefresher: secret}
	}
	return wrapped
}

func combineResourceRestore(first, second func() error) func() error {
	return func() error { return errors.Join(first(), second()) }
}
