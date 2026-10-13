package runtime

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"strconv"
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

// sandboxClaudeSymlinkExit is the status assertSharedResourceRoots exits with
// when the shared `.claude` root is a symbolic link. The script writes the
// link's target — and nothing else — to stdout, so the refusal the engine
// returns names it: the host cannot stat a path inside the sandbox. `readlink`
// is absent from some minimal images; an unnamed target refuses all the same.
const sandboxClaudeSymlinkExit = 4

// assertSharedResourceRoots is the first line of all three scripts (snapshot,
// restore, reset). `set -eu` alone would let a missing root pass: `cp` of
// nothing and `rm -rf` of nothing both exit 0, so the wrong-root case would
// read as a clean no-op.
//
// It catches a root the sandbox does not have at all. It does NOT certify that
// the root is the right one — an existing-but-wrong directory passes. That is
// why the derivation above mirrors the driver rather than guessing, and why
// this assert is the floor and not the guarantee.
//
// Then it holds the host sites' rule (refuseAClaudeSymlink) inside the
// sandbox: `rm -rf "$1/<name>"` and `cp -a` through a `.claude` symlink act on
// the link's TARGET, outside the workspace the run owns — and the agent
// working in that sandbox is exactly who can plant one. Only the final
// component is examined, as on the host: a workspace REACHED through a symlink
// is the same tree on both sides, and refusing it would refuse every run under
// a symlinked checkout.
var assertSharedResourceRoots = `test -d "$3" || { echo "shared workspace root $3 is absent from the sandbox — ` +
	`the driver copied the workspace somewhere else" >&2; exit 3; }
if test -L "$1"; then
 readlink "$1" 2>/dev/null || true
 echo "shared .claude root $1 is a symlink — the snapshot, restore and reset would remove and rewrite through it, outside the workspace; replace the link with a real directory" >&2
 exit ` + strconv.Itoa(sandboxClaudeSymlinkExit) + `
fi
`

// sharedResourceScriptError turns a shared-resource script's result into the
// engine's error, nil for a clean exit. The `.claude`-symlink refusal carries
// the typed error the host sites return, so one errors.As recognises the class
// whichever transport wrote.
//
// That typed message is the host's — a mirror writing through the link — and
// these sites instead `rm -rf` and `cp -a` through it. The script's stderr is
// the only account of what the script itself refused, and the only trace left
// when the status is NOT the guard's: exit 4 is the script's contract, not a
// status only the guard can produce, so a coreutil exiting exactly 4 lands in
// the same branch and the link the typed message names does not exist. The
// evidence travels with the verdict rather than being dropped.
//
// Both accounts always travel; the one holding the most specific fact LEADS.
// The live one-line view of a run error (pkg/cli's firstLine) keeps the text
// up to the first newline, and the production transport is `kubectl exec`,
// which appends its own exit-code line to the stderr it captures: a second
// line never reaches that view. With the link's target read, the typed
// refusal leads — the target is the one fact the host cannot read for itself.
// With nothing read, the typed message adds only `<unreadable>` and the
// script's account leads, being the whole account of an exit 4 that is not
// the guard's.
func sharedResourceScriptError(site, root string, res sandbox.ExecResult) error {
	if res.ExitCode == 0 {
		return nil
	}
	if res.ExitCode == sandboxClaudeSymlinkExit {
		target := strings.TrimSpace(string(res.Stdout))
		named := target != ""
		if !named {
			target = "<unreadable>"
		}
		linkErr := &claudeSymlinkError{link: root, target: target}
		said := strings.TrimSpace(string(res.Stderr))
		switch {
		case said == "":
			return fmt.Errorf("%s: %w", site, linkErr)
		case named:
			return fmt.Errorf("%s: %w; the script said: %s", site, linkErr, said)
		default:
			return fmt.Errorf("%s: %s: %w", site, said, linkErr)
		}
	}
	return fmt.Errorf("%s: exit %d: %s", site, res.ExitCode, res.Stderr)
}

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
		return sharedResourceScriptError("shared child resources (backup "+backup+")", root, res)
	}
	cctx, cancel := context.WithTimeout(ctx, childResourceIOTimeout)
	defer cancel()
	err = runScript(cctx, `set -eu
`+assertSharedResourceRoots+`mkdir -p "$2"
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
`+assertSharedResourceRoots+`for name in `+childResourceWords()+`; do
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
` + assertSharedResourceRoots + `for name in ` + childResourceWords() + `; do rm -rf "$1/$name"; done`,
		"sh", root, "", workspace}, sandbox.ExecOpts{})
	if err != nil {
		return err
	}
	return sharedResourceScriptError("reset borrowed sandbox resources", root, res)
}

// resourceDirForRun prevents a bare child.bot beside a bundle main.bot from
// inheriting that sibling's devbox.json through the legacy catalog fallback.
func (e *Engine) resourceDirForRun() string {
	if e.parentRunID != "" && e.bundle == nil {
		return ""
	}
	return bundleResourceDir(e.bundle, e.filePath)
}

// noopChildCleanup is the provisioning cleanup that owns nothing.
var noopChildCleanup = func() {}

// childDevboxStagingPath is where THIS provisioning stages a child's
// devbox install: the run id keeps it readable, the random suffix makes it
// this provisioning's alone. Keyed by the run id alone, two stagers of one
// run shared one directory and whichever finished first deleted the
// other's install while it was still in use (#1787 — measured on the
// runtime tests: every process staging for the run id "child" raced on
// /tmp/iterion-devbox/child-<sha256("child")>). The path sits in the
// sandbox's own /tmp: the install and the cleanup both run through the
// child's handle.
func childDevboxStagingPath(runID string) (string, error) {
	var nonce [8]byte
	if _, err := cryptorand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("runtime: stage child devbox: %w", err)
	}
	sum := sha256.Sum256([]byte(runID))
	return fmt.Sprintf("/tmp/iterion-devbox/child-%x-%s", sum[:6], hex.EncodeToString(nonce[:])), nil
}

// Child provisioning is staged at a unique path, then attached to the child's
// handle only. Neither the parent's botDevboxDir nor its environment changes.
func (e *Engine) provisionSharedChildDevbox(ctx context.Context, runID string, inherited sandbox.Run) (sandbox.Run, func()) {
	dir := e.resourceDirForRun()
	config := devboxConfigIn(dir, "child bundle", e.logger)
	if config == "" {
		return inherited, noopChildCleanup
	}
	inline, err := readInlineDevbox(dir)
	staged, stageErr := childDevboxStagingPath(runID)
	if stageErr != nil {
		err = errors.Join(err, stageErr)
	}
	var bin string
	if staged != "" {
		bin = path.Join(staged, devboxProfileBin)
	}
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
	cleanup := noopChildCleanup
	if staged != "" {
		cleanup = func() {
			cctx, cancel := context.WithTimeout(context.Background(), childResourceIOTimeout)
			defer cancel()
			if res, err := inherited.Exec(cctx, []string{"rm", "-rf", staged}, sandbox.ExecOpts{}); (err != nil || res.ExitCode != 0) && e.logger != nil {
				e.logger.Warn("runtime: child devbox cleanup %s: %v (exit %d)", staged, err, res.ExitCode)
			}
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
