package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"strings"
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
// An explicit WorkspaceFolder wins, an empty one means "the same absolute path
// as on the host" — the rule containerWorkspaceFolder states. A driver with no
// host filesystem cannot bind-mount anything, so it COPIES the workspace to
// that same absolute path and leaves WorkspaceFolder empty. Empty is a normal
// state on the driver production runs on, not a missing value, and that is the
// case this function exists to stop refusing.
//
// The refusal is kept for the state that really is broken: no absolute path on
// either side. Joining an empty workspace yields the RELATIVE ".claude", which
// resolves against whatever cwd the exec lands in — a silent wrong target for
// a recursive delete.
//
// KNOWN GAP, review finding R9e4d97, deliberately not closed here. When a bot
// DECLARES `sandbox.workspace_folder:`, the kubernetes driver's two roots
// diverge: populate copies to info.WorkspacePath (driver.go:611) while the pod
// manifest mounts the volume at Spec.WorkspaceFolder (manifest.go:266). This
// derivation follows the manifest, so it would address the mount point while
// the files sit in the copy — and since the mount exists, the assert below
// passes and both callers become quiet no-ops.
//
// Inverting the preference here was tried and is NOT the answer: it makes
// TestTheCopyAChildReadsMatchesTheHostAfterAdoption fail, which pins the
// opposite. The two cannot both be satisfied while the driver disagrees with
// itself, so the question belongs to the driver — should the manifest follow
// info.WorkspacePath too? — and not to a guess made here.
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
//
// It catches a root the sandbox does not have at all. It does NOT certify that
// the root is the right one — an existing-but-wrong directory passes. That is
// why the derivation above mirrors the driver rather than guessing, and why
// this assert is the floor and not the guarantee.
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
for name in `+childResourceWords()+`; do
 if test -e "$1/$name" || test -L "$1/$name"; then cp -a "$1/$name" "$2/$name"; fi
done`)
	if err != nil {
		return nil, err
	}
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), childResourceIOTimeout)
		defer cancel()
		return runScript(ctx, `set -eu
`+assertWorkspaceRoot+`for name in `+childResourceWords()+`; do
 rm -rf "$1/$name"
 if test -e "$2/$name" || test -L "$2/$name"; then mkdir -p "$1"; cp -a "$2/$name" "$1/$name"; fi
done
rm -rf "$2"`)
	}, nil
}

// childResourceWords renders childResourcePaths as shell words, so the scripts
// run inside a sandbox act on exactly the entries the host-side snapshot saves.
func childResourceWords() string {
	words := make([]string, len(childResourcePaths))
	for i, name := range childResourcePaths {
		words[i] = shellquote.Quote(name)
	}
	return strings.Join(words, " ")
}

// The copy must receive the complete effective child tree. Merely overwriting
// files leaves parent-only files behind when a child replaces a directory skill
// — and for the engine-owned skills copy, a name only the parent's bundle ships
// would answer for the child. beginRunResources already saved the original copy
// for restoration on all exits, an aborted adoption included.
//
// A copy-based adoption with no borrowed scope is refused rather than skipped:
// nothing would have saved the parent's entries, so resetting them destroys
// them and leaving them lets the parent's names answer for the child. Every
// child that adopts runs in place and opens that scope (Run and Resume both
// call beginRunResources before startSandbox); this states the precondition
// instead of trusting it.
func (e *Engine) clearBorrowedSandboxResources(ctx context.Context) error {
	if e.sharedSandbox == nil || !sharedSandboxIsCopyBased(e.sharedSandbox.Run) {
		return nil
	}
	if e.resourceScope == nil || !e.resourceScope.borrowed {
		return fmt.Errorf("runtime: adopting the parent's copy-based sandbox (%s) needs a borrowed resource scope, and this run holds none: "+
			"the parent's .claude entries (%s) would be replaced in the sandbox with no saved copy to restore",
			e.sharedSandbox.Run.Driver(), strings.Join(childResourcePaths, ", "))
	}
	ctx, cancel := context.WithTimeout(ctx, childResourceIOTimeout)
	defer cancel()
	workspace, root, err := e.sharedClaudeRoot()
	if err != nil {
		return err
	}
	res, err := e.sharedSandbox.Run.Exec(ctx, []string{"sh", "-c", `set -eu
` + assertWorkspaceRoot + `for name in ` + childResourceWords() + `; do rm -rf "$1/$name"; done`,
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
