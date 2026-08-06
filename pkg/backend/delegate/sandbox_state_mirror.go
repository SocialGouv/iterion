package delegate

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// mirrorStateFileIntoSandbox re-writes a host-written state file INSIDE the
// sandbox when the driver's workspace is a COPY of the host's rather than the
// same inode.
//
// A copy-based sandbox (kubernetes) populates its workspace by streaming a tar
// at pod start, so a host-side write afterwards never lands inside it. The CLI
// backends hand their agent several files BY PATH — the iterion pi extension,
// the composed system prompt, the openai-codex credential — and the agent
// resolves those paths in the pod. Without this mirror the run dies on
// `Extension path does not exist` or, worse because nothing crashes, starts
// with no system prompt and no credential.
//
// The in-pod workspace sits at the SAME absolute path as the host one (the
// kubernetes driver aligns them deliberately so a tool node's `git -C
// <worktree>` resolves), so only the WRITE mechanism differs — every path
// iterion puts in an argv is already correct on both sides. That is why this
// mirrors rather than relocates: the host copy stays the one every guard
// (write-root refusal, symlink refusal, the .gitignore guard) already inspects,
// and it stays the one `cleanup` removes.
//
// Drivers that share the host filesystem (docker bind mount, noop) see the host
// write directly and deliberately do not implement the interface, so this is a
// no-op for them — the type assertion is the driver-fact oracle, not a name.
// SandboxCopiesWorkspace reports whether the run's sandbox works on a COPY of
// the host workspace rather than the same inode.
//
// The type assertion is the driver-fact oracle, not a name: a driver that
// bind-mounts (docker) or runs on the host (noop) deliberately does not
// implement the refresh interface, because it does not need to.
//
// A caller that hands the agent a host DIRECTORY it will keep reading and
// writing throughout the node — rather than a file written once before the
// spawn — cannot use the mirror above: there is a push seam but no per-file
// pull seam, so the agent's edits stay in the pod until teardown, long after
// the node needed them. Such a caller must refuse the feature visibly instead
// of running a half-cycle whose only symptom is memory that is always empty.
func SandboxCopiesWorkspace(task Task) bool {
	_, copyBased := task.Sandbox.(sandbox.WorkspaceFileRefresher)
	return copyBased
}

func mirrorStateFileIntoSandbox(ctx context.Context, task Task, absPath string, value []byte) error {
	refresher, copyBased := task.Sandbox.(sandbox.WorkspaceFileRefresher)
	if !copyBased {
		return nil
	}
	// RefreshWorkspaceFile addresses the pod workspace root, so a path outside
	// the workspace has no in-pod address at all. Refuse loudly: returning nil
	// would report success for a file the agent will never see, which is the
	// exact failure mode this function exists to end.
	_, rel := relBelow(task.WorkDir, absPath)
	if rel == "" {
		return fmt.Errorf("delegate: %q is outside the sandbox workspace %q, so a copy-based "+
			"driver (%s) cannot make it visible to the agent", absPath, task.WorkDir, task.Sandbox.Driver())
	}
	if err := refresher.RefreshWorkspaceFile(ctx, rel, value); err != nil {
		return fmt.Errorf("delegate: mirror %s into the sandbox: %w", rel, err)
	}
	return nil
}
