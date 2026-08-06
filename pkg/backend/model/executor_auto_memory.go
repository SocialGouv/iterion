package model

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/backend/automemory"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/memory"
	"github.com/SocialGouv/iterion/pkg/store"
)

// autoMemoryMode is the node's effective auto-memory setting. Both the tool
// allowlist (assembled while the task is built) and the mirror (prepared just
// before dispatch) read it, and they must never disagree: a node granted the
// file tools but handed no directory is merely wasteful, whereas the reverse
// hands claw a MEMORY.md it cannot open.
func (e *ClawExecutor) autoMemoryMode(f backendFields) automemory.Mode {
	return automemory.Resolve(e.autoMemoryOverride, f.autoMemory, e.wfAutoMemory, e.autoMemoryEnvDefault)
}

// applyAutoMemory resolves the node's auto-memory decision, materialises the
// run's memory space on disk when it is on, and stamps the directory onto the
// task. The returned closure folds the agent's edits back into the store and
// must be called once the node is done — deferred, so a node that fails after
// writing its notes still keeps them.
//
// It returns a no-op closure (never nil) whenever auto-memory is off, so the
// caller defers unconditionally rather than nil-checking.
//
// A failure to prepare the mirror is NOT fatal: the node runs with
// auto-memory off and a warning. Memory is a side channel — losing it costs a
// run its notes, whereas failing the node costs the run itself.
func (e *ClawExecutor) applyAutoMemory(ctx context.Context, task *delegate.Task, f backendFields, backendName string) func() {
	noop := func() {}

	if !e.autoMemoryMode(f).Enabled() || !automemory.SupportsBackend(backendName) {
		return noop
	}

	warn := func(format string, args ...any) {
		if e.logger != nil {
			e.logger.Warn("[%s/auto_memory] "+format, append([]any{f.id}, args...)...)
		}
	}

	// A sandbox whose workspace is a COPY of the host's (kubernetes) cannot
	// carry this round trip. The mirror is a directory the agent reads and
	// rewrites for the whole node, and the copy-based drivers offer a push
	// seam but no per-file pull: the hydrated files would never reach the pod,
	// and the agent's notes would sit in it until teardown — long after the
	// sync that was supposed to persist them.
	//
	// Half a cycle is worse than none, because its only symptom is a memory
	// that is always empty: no error, no failed node, nothing to search for.
	// Refuse the feature out loud and run the node without it.
	if delegate.SandboxCopiesWorkspace(*task) {
		warn("running without MEMORY.md: the %s sandbox works on a copy of the workspace, "+
			"so the agent's notes could not be read back — auto-memory needs a bind-mounted "+
			"or host workspace", task.Sandbox.Driver())
		return noop
	}

	ref, spaceRoot, err := e.autoMemoryLocation(ctx, *task)
	if err != nil {
		warn("running without MEMORY.md: %v", err)
		return noop
	}

	// A Mirror OWNS its directory: Hydrate makes the directory match the space,
	// which means deleting whatever else is in it. So every node gets its own,
	// created fresh here and removed once its edits are folded back.
	//
	// Sharing one directory across the run is what this replaces, and it lost
	// data: parallel branches all hydrate into it, and the second branch's
	// Hydrate deletes the note the first branch's agent has written but not yet
	// synced — reproduced end to end, the note never reaches the store and
	// nothing warns. Isolating the directories makes the STORE the only place
	// branches meet, which is where iterion already merges concurrent work.
	dir, releaseDir, err := automemory.NewNodeDir(spaceRoot)
	if err != nil {
		warn("running without MEMORY.md: create mirror dir: %v", err)
		return noop
	}
	memStore := e.autoMemStore
	if memStore == nil {
		memStore = memory.DefaultFSStore()
	}
	mirror := automemory.NewMirror(memStore, ref, dir, autoMemoryAttribution(e.botID))
	if err := mirror.Hydrate(ctx); err != nil {
		warn("running without MEMORY.md: %v", err)
		releaseDir()
		return noop
	}

	task.AutoMemoryDir = mirror.Dir()
	if automemory.NeedsPromptSection(backendName) {
		task.AutoMemoryPrompt = automemory.PromptSection(mirror.Dir())
	}
	return func() {
		// Detached from the node's context ON PURPOSE. The node is over by the
		// time this runs, and the ordinary way a node ends early is exactly
		// when its notes matter most: an operator Cancel, a runner drain, a
		// timeout. The cloud store honours cancellation, so syncing on the
		// node's context discarded everything the agent had written — invisible
		// locally, because the filesystem store ignores the context. Same
		// reasoning as the quota compensation in pkg/store/mongo.
		// No deadline here: SyncBack sets its own once it knows how much it has
		// to persist. A fixed budget chosen from this side is either too short
		// for a large memory (notes lost, one warning) or too long for the
		// operator waiting on Cancel.
		if err := mirror.SyncBack(context.WithoutCancel(ctx)); err != nil {
			warn("some memory was not persisted: %v", err)
		}
		// The directory is a re-derivable cache of the space; keeping it would
		// only accumulate one per node per run under a root shared by every run
		// on the host. Releasing the lock is what tells a later sweep this one
		// is finished rather than crashed.
		releaseDir()
	}
}

// autoMemoryLocation resolves the space and the ROOT its per-node mirror
// directories are created under, and runs the state-root guards. All of it is
// fixed for the whole run — the space depends on repo root, bot id and tenant,
// and the guards inspect a path that does not change — so it is computed on
// the first node and reused, rather than re-walking the filesystem and
// re-deriving the space once per node. (Verified: SetRepoRoot / SetWorkDir /
// SetSharedStateDir all run at run start, before the first node, so the first
// task's values are every task's values.)
//
// The first attempt's error is cached too: a run whose state root is refused
// must not re-refuse (and re-log) on every subsequent node.
func (e *ClawExecutor) autoMemoryLocation(ctx context.Context, task delegate.Task) (knowledge.SpaceRef, string, error) {
	e.autoMemoryOnce.Do(func() {
		root, loc := task.StateDir(automemory.StateDirName)
		e.autoMemoryRef = e.autoMemorySpaceRef(ctx, task)
		// One sub-root PER SPACE, under which each node gets its own directory.
		// The state root is shared by every run on the host, so without the
		// space segment two different bots would mirror into the same tree —
		// each syncing the other's notes into its own space. A digest of the
		// space id is the right name: stable, collision-free, filesystem-safe.
		dir := filepath.Join(root, knowledge.ChecksumHex([]byte(e.autoMemoryRef.ID()))[:16])
		// Guard the directory we actually write under, NOT its parent.
		//
		// Guarding `root` alone left the space sub-directory unprotected, and
		// its name is derivable from public facts — the repository path and the
		// bot id. A target repo committing `.iterion/auto-memory/<digest>` as a
		// symlink had every mirror the run creates land wherever it pointed:
		// reproduced, with iterion writing the agent's notes into an
		// attacker-chosen directory under the operator's own uid. The leaf here
		// is a name iterion derives and creates, so a symlink at it is never
		// the operator's doing.
		// loc was resolved for `root`, and dir sits under it — a path inside the
		// checkout stays inside it, a path on the shared mount stays on it — so
		// the classification carries. Re-deriving it is what StateDir's own
		// contract warns against.
		if err := delegate.PrepareStateRoot(task, dir, loc, "auto-memory",
			"the agent's MEMORY.md notes", e.logger); err != nil {
			e.autoMemoryErr = err
			return
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			e.autoMemoryErr = fmt.Errorf("create %s: %w", dir, err)
			return
		}
		e.autoMemoryDir = dir
		// A node directory only survives a crash — the defer that removes it
		// does not run on SIGKILL or an OOM. Nothing else reaps them, so
		// without this they accumulate for the life of the store.
		automemory.SweepStaleNodeDirs(dir, e.logger)
	})
	return e.autoMemoryRef, e.autoMemoryDir, e.autoMemoryErr
}

// autoMemorySpaceRef addresses the space holding this run's MEMORY.md: the
// bot's own, scoped to the project it is running on.
//
// `bot` visibility is what makes the memory useful and safe at once — every
// run of this bot on this project shares it, and no other bot sees it. The
// project key comes from RepoRoot when there is one, so a `worktree: auto`
// run and a plain run of the same bot on the same repository land in the same
// space instead of one per worktree — the very failure that makes each
// backend's own workDir-derived directory useless here.
func (e *ClawExecutor) autoMemorySpaceRef(ctx context.Context, task delegate.Task) knowledge.SpaceRef {
	base := task.WorkDir
	if task.RepoRoot != "" {
		base = task.RepoRoot
	}
	tenant, _ := store.TenantFromContext(ctx)
	owner, _ := store.OwnerFromContext(ctx)
	return memory.ResolveSpaceRef(knowledge.VisibilityBot, automemory.SpaceName, e.botID, "", memory.SpaceRefInputs{
		TenantID:  tenant,
		UserID:    owner,
		ProjectID: memory.ProjectKey(base),
		BotID:     e.botID,
	})
}

// autoMemoryAttribution stamps agent-written documents so the studio memory
// panel and `iterion memory` can tell an agent's note from an operator's edit.
func autoMemoryAttribution(botID string) string {
	if botID == "" {
		return ""
	}
	return fmt.Sprintf("bot:%s", botID)
}
