// Package runtime — sandbox mount helpers extracted from sandbox.go
// to keep [resolveAndStartSandbox] focused on the lifecycle skeleton.
package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// addOptionalBindMount stats hostDir; if it exists and is readable it
// appends a bind-mount entry to spec.Mounts and returns the resolved
// container path. Missing hostDir is a silent skip — every aux mount
// (attachments, run-files, bundle) is optional. Returns "" when no
// mount was added so callers know to skip downstream wiring (env vars,
// etc.). label is used only in the warn log when stat fails for a
// non-ENOENT reason.
func addOptionalBindMount(
	spec *sandbox.Spec,
	hostDir, containerPath, defaultContainerPath, label string,
	readOnly bool,
	logger *iterlog.Logger,
) string {
	if hostDir == "" {
		return ""
	}
	_, statErr := os.Stat(hostDir)
	switch {
	case statErr == nil:
		if containerPath == "" {
			containerPath = defaultContainerPath
		}
		entry := fmt.Sprintf("source=%s,target=%s,type=bind", hostDir, containerPath)
		if readOnly {
			entry += ",readonly"
		}
		spec.Mounts = append(spec.Mounts, entry)
		return containerPath
	case errors.Is(statErr, fs.ErrNotExist):
		return ""
	default:
		if logger != nil {
			logger.Warn("runtime: sandbox %s host dir %s: %v — skipping mount", label, hostDir, statErr)
		}
		return ""
	}
}

// applyHostStateMounts auto-binds ~/.iterion and ~/.claude into the
// container at the same absolute path, so persistent memory survives
// across runs and Claude Code's cwd-derived project key resolves
// identically inside and outside the sandbox. Emits
// [store.EventSandboxHostStateMounted] with the precedence source so
// operators can audit why the mount fired (or didn't).
//
// Pre-conditions: spec is the resolved active spec; the caller has
// already decided that mounts/env mutations are safe.
func applyHostStateMounts(
	spec *sandbox.Spec,
	wf *ir.Workflow,
	p SandboxParams,
	emitEvent func(store.EventType, map[string]interface{}) error,
	logger *iterlog.Logger,
) {
	var wfHostState string
	if wf != nil && wf.Sandbox != nil {
		wfHostState = wf.Sandbox.HostState
	}
	resolvedHostState, hsSource := pickHostState(wfHostState, p.HostStateOverride, p.HostStateDefault)
	spec.HostState = sandbox.HostState(resolvedHostState)
	if !spec.HostState.Active() {
		_ = emitEvent(store.EventSandboxHostStateMounted, map[string]interface{}{
			"enabled": false,
			"source":  hsSource,
		})
		return
	}

	absWorkspace, absErr := filepath.Abs(p.WorkspacePath)
	if absErr != nil {
		absWorkspace = p.WorkspacePath
	}
	// Workspace at host's absolute path keeps cwd-derived state
	// (Claude Code project key, ${PROJECT_DIR}, absolute-path tool
	// args) resolvable identically in/out container. Preserve any
	// explicit DSL workspace_folder override.
	if spec.WorkspaceFolder == "" {
		spec.WorkspaceFolder = absWorkspace
	}

	homeDir := resolveHostHomeDir()
	iterionHomeDir := store.GlobalIterionDataDir()
	var claudeDir string
	if homeDir != "" {
		claudeDir = filepath.Join(homeDir, ".claude")
	}

	mounts := collectHostStateMounts(absWorkspace, iterionHomeDir, claudeDir)
	mountPairs := make([]string, 0, len(mounts))
	for _, m := range mounts {
		entry := fmt.Sprintf("source=%s,target=%s,type=bind", m.HostPath, m.ContainerPath)
		if m.ReadOnly {
			entry += ",readonly"
		}
		spec.Mounts = append(spec.Mounts, entry)
		mountPairs = append(mountPairs, m.HostPath+":"+m.ContainerPath)
	}

	// Force HOME inside the container to the host home path so
	// processes that resolve `~` (Claude Code, git, anything reading
	// $HOME) land in the mounted tree rather than a stock image's /root
	// or empty $HOME.
	if homeDir != "" {
		if spec.Env == nil {
			spec.Env = map[string]string{}
		}
		if _, alreadySet := spec.Env["HOME"]; !alreadySet {
			spec.Env["HOME"] = homeDir
		}
	}

	applyHostUIDRemap(spec, emitEvent, logger)

	_ = emitEvent(store.EventSandboxHostStateMounted, map[string]interface{}{
		"enabled":          true,
		"source":           hsSource,
		"workspace_folder": spec.WorkspaceFolder,
		"mounts":           mountPairs,
	})
}

// applyHostUIDRemap enforces the "container UID == host UID" invariant
// on Linux hosts so writes to the bind-mounted ~/.iterion and ~/.claude
// trees stay host-owned. macOS / Windows Docker Desktop perform their
// own userns-remap implicitly, so this is a no-op there. Host UID 0
// (CI runners) is also a no-op — same UID either way.
func applyHostUIDRemap(
	spec *sandbox.Spec,
	emitEvent func(store.EventType, map[string]interface{}) error,
	logger *iterlog.Logger,
) {
	if goruntime.GOOS != "linux" {
		return
	}
	hostUID := os.Getuid()
	hostGID := os.Getgid()
	if hostUID == 0 {
		return
	}
	if spec.User == "" {
		spec.User = strconv.Itoa(hostUID) + ":" + strconv.Itoa(hostGID)
		_ = emitEvent(store.EventSandboxUserRemap, map[string]interface{}{
			"uid":    hostUID,
			"gid":    hostGID,
			"reason": "host_state=auto: align container UID with host so writes to ~/.iterion + ~/.claude remain host-owned",
		})
		return
	}
	specUID, ok := parseUserUID(spec.User)
	if ok && specUID != hostUID {
		_ = emitEvent(store.EventSandboxUIDMismatchWarning, map[string]interface{}{
			"spec_user": spec.User,
			"host_uid":  hostUID,
		})
		if logger != nil {
			logger.Warn("runtime: sandbox host_state active but container user %q (UID %d) != host UID %d — writes to ~/.iterion + ~/.claude will be owned by UID %d and may be unreadable to subsequent host invocations",
				spec.User, specUID, hostUID, specUID)
		}
	} else if !ok && logger != nil {
		// Non-numeric user (e.g. "node") — we can't verify UID alignment
		// without inspecting the image. Surface so the operator at least
		// knows host_state can corrupt home-dir permissions if the
		// image's user doesn't resolve to the host UID at runtime.
		logger.Warn("runtime: sandbox host_state active but container user %q has no parseable UID — cannot verify host UID alignment; ~/.iterion + ~/.claude writes may end up with unexpected ownership",
			spec.User)
	}
}

// addClawBinaryMount bind-mounts a host iterion binary into the
// container at /usr/local/bin/iterion when the workflow uses claw
// nodes — the runner subprocess (`iterion __claw-runner`) must be on
// the container PATH. Production sandbox images bake iterion in, so
// the silent skip when no host binary can be located is intentional;
// the absence will then surface as a clear "exec: iterion: not found"
// at runner invocation time.
func addClawBinaryMount(spec *sandbox.Spec, wf *ir.Workflow) {
	if wf == nil || !containsClawNode(wf) {
		return
	}
	hostBin := locateHostIterionBinary()
	if hostBin == "" {
		return
	}
	spec.Mounts = append(spec.Mounts,
		fmt.Sprintf("source=%s,target=/usr/local/bin/iterion,type=bind,readonly", hostBin),
	)
}

// addWorktreeGitMount bind-mounts the per-run worktree's git-private
// directory (`<repoRoot>/.git/worktrees/<run-id>`) into the container
// at the SAME host path. Without this mount the worktree's `.git`
// pointer file — a one-line `gitdir: <this-path>` — cannot be resolved
// by in-sandbox git commands and every tool node touching git fails
// with `fatal: not a git repository`.
//
// We deliberately mount only the per-run gitdir (not the whole
// `<repoRoot>/.git`) so concurrent sandboxed runs cannot read each
// other's worktree state. The bind is read-write because git writes
// HEAD, refs, packed-refs, index, ORIG_HEAD, …, into this directory
// during normal `git commit` / `git checkout` flows.
//
// Silently skips when:
//   - gitDir is empty (non-worktree run, or cloud runner that never
//     populated the worktreeContext);
//   - the path doesn't exist on disk (e.g. an inconsistent on-resume
//     state where the worktree was manually removed but the run
//     record still claims worktree=true).
//
// The kubernetes driver hard-errors on `type=bind` at manifest render
// time (see pkg/sandbox/kubernetes/mounts.go) — that surfaces as a
// clear "type=bind not supported in cloud" diagnostic at run start,
// which is the right behaviour for cloud runners that lack a host
// filesystem to bind. Cloud workflows that need worktree-aware git
// access will need a different mechanism (init container + PVC) —
// out of scope here.
func addWorktreeGitMount(spec *sandbox.Spec, gitDir string, logger *iterlog.Logger) {
	if gitDir == "" {
		return
	}
	info, statErr := os.Stat(gitDir)
	if statErr != nil {
		if !errors.Is(statErr, fs.ErrNotExist) && logger != nil {
			logger.Warn("runtime: sandbox worktree gitdir %s: %v — skipping mount", gitDir, statErr)
		}
		return
	}
	if !info.IsDir() {
		return
	}
	spec.Mounts = append(spec.Mounts,
		fmt.Sprintf("source=%s,target=%s,type=bind", gitDir, gitDir),
	)
}
