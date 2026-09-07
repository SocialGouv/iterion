// Mid-run preservation of a copy-based sandbox's work.
//
// On a driver whose workspace is a tar COPY inside a pod (kubernetes),
// NOTHING a run produces leaves that pod until teardown: the export runs
// once, at the end, and every push the runner performs reads the exported
// clone. So a pod that dies hard takes the whole run with it — measured on
// a campaign lot that spent eight hours and 655 tool calls in one agent
// node, died at its duration wall, and left `final_commit` empty with the
// export refused ("pod-side HEAD unknown"): the store records commits, the
// run had made none, and there was literally nothing left.
//
// Two halves of that loss, and this file closes both for the window it can
// reach: work that was never committed is preserved anyway (a checkpoint
// commit built from the working tree), and work that was committed leaves
// the pod while the pod is still healthy instead of at teardown, when it
// may no longer be.
//
// What it is NOT allowed to do, and does not: touch the run's own history.
// The checkpoint never moves HEAD, never writes the run's index, never
// touches the working tree, and lands on a ref of its own
// (`iterion/run-<id>-checkpoint`) authored by iterion — so it cannot be
// mistaken for the work of the party a gate judges, and a bot whose whole
// contract is "the lot commits its own work" is not silently committed for.
package runner

import (
	"context"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// workspaceCheckpointInterval is how often a copy-based sandbox's work is
// preserved outside the pod. The cost of a tick is one exec plus, when
// something moved, one push; the cost of NOT ticking is everything since
// the last one — so this is the maximum a hard pod death may destroy.
const workspaceCheckpointInterval = 10 * time.Minute

// workspaceCheckpointTimeout bounds one checkpoint round-trip (exec +
// push). Generous: the push crosses the network from inside the pod.
const workspaceCheckpointTimeout = 4 * time.Minute

// checkpointScript builds — and prints — the commit that preserves the
// sandbox workspace, WITHOUT touching anything the run owns.
//
// The tree is read into a TEMPORARY index (GIT_INDEX_FILE), so the run's
// own index is never written and an agent running `git add` alongside is
// not raced for the index lock. `commit-tree` writes an object and no ref:
// HEAD, the branch and the working tree come out of this byte-identical.
// When the tree matches HEAD's, there is nothing uncommitted to preserve
// and HEAD itself is printed — the commits still need to leave the pod.
//
// The identity is iterion's, explicitly, and not the run's: a checkpoint
// wearing the committer's name is evidence that lies about who did the
// work — the exact confusion the extension gates spend their code refusing.
const checkpointScript = `set -e
git rev-parse --git-dir >/dev/null 2>&1 || { echo "not-a-git-repo" >&2; exit 3; }
head=$(git rev-parse HEAD 2>/dev/null) || { echo "no-commit-yet" >&2; exit 3; }
idx="${TMPDIR:-/tmp}/iterion-checkpoint-index.$$"
rm -f "$idx"
GIT_INDEX_FILE="$idx" git read-tree "$head"
GIT_INDEX_FILE="$idx" git add -A
tree=$(GIT_INDEX_FILE="$idx" git write-tree)
rm -f "$idx"
if [ "$tree" = "$(git rev-parse "$head^{tree}")" ]; then
  echo "$head $tree $head"
  exit 0
fi
sha=$(GIT_AUTHOR_NAME=iterion GIT_AUTHOR_EMAIL=checkpoint@iterion.invalid \
GIT_COMMITTER_NAME=iterion GIT_COMMITTER_EMAIL=checkpoint@iterion.invalid \
  git commit-tree "$tree" -p "$head" \
    -m "iterion: workspace checkpoint — the run's uncommitted tree, preserved by the runner (not the run's own commit)")
echo "$head $tree $sha"
`

// checkpointWorkspaceLoop preserves the sandbox's work on a cadence until
// the run ends. Best-effort by construction: a tick that cannot read or
// push is logged and retried at the next one, and no failure here may fail
// a run — the run's own outcome is decided by its nodes, never by whether
// its safety net could be laid.
func (r *Runner) checkpointWorkspaceLoop(ctx context.Context, o sandboxObserverOpts, run sandbox.Run) {
	tick := time.NewTicker(workspaceCheckpointInterval)
	defer tick.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		last = r.checkpointWorkspaceOnce(ctx, o, run, last)
	}
}

// checkpointWorkspaceOnce is one tick: resolve what would be lost right
// now, and push it if it moved since the last checkpoint. Returns the
// commit now preserved (or `last` unchanged when nothing was).
//
// The push is forced on purpose: successive checkpoints are SIBLINGS, not
// descendants (each is parented on the run's current HEAD), so a
// fast-forward push would refuse every checkpoint after the first. The ref
// is derived from the run's own id and contested by nobody.
func (r *Runner) checkpointWorkspaceOnce(ctx context.Context, o sandboxObserverOpts, run sandbox.Run, last string) string {
	ref := "iterion/run-" + o.runID + "-checkpoint"
	cctx, cancel := context.WithTimeout(ctx, workspaceCheckpointTimeout)
	defer cancel()

	res, err := run.Exec(cctx, []string{"sh", "-c", checkpointScript}, sandbox.ExecOpts{})
	if err != nil || res.ExitCode != 0 {
		// Exit 3 is the script saying there is nothing a checkpoint could
		// hold (no repository, or no commit yet to parent one on): a
		// state, not a failure, and it must not read as one every ten
		// minutes for the length of a run.
		if err == nil && res.ExitCode == 3 {
			return last
		}
		r.cfg.Logger.Warn("runner: run %s: workspace checkpoint: cannot read the sandbox tree (%v; exit %d): %s",
			o.runID, err, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
		return last
	}
	// WHAT MOVED is (HEAD, tree) — never the checkpoint commit, which
	// embeds a timestamp: on a dirty tree that has stopped changing, every
	// tick produces a different sha for identical content (measured: three
	// ticks a second apart, three shas, one tree). Comparing the commit
	// meant a force-push every tick for nothing, and worse: each push
	// emitted an event, every event re-arms stall detection, so a run that
	// was stuck with a dirty tree would have read as alive forever — the
	// safety net blinding the alarm it was laid beside.
	state, sha := checkpointState(string(res.Stdout))
	if sha == "" || state == last {
		return last
	}

	push := "git push --force origin " + sha + ":refs/heads/" + ref
	pres, perr := run.Exec(cctx, []string{"sh", "-c", push}, sandbox.ExecOpts{})
	if perr != nil || pres.ExitCode != 0 {
		r.cfg.Logger.Warn("runner: run %s: workspace checkpoint: push of %s to %s FAILED (%v; exit %d): %s — the work is still only inside the pod",
			o.runID, sha[:min(12, len(sha))], ref, perr, pres.ExitCode, strings.TrimSpace(string(pres.Stderr)))
		r.recordCheckpoint(o, map[string]any{
			"ref":   ref,
			"error": strutilFirstLine(string(pres.Stderr), perr),
		})
		return last
	}
	r.cfg.Logger.Info("runner: run %s: workspace checkpoint pushed: %s -> %s", o.runID, sha[:min(12, len(sha))], ref)
	r.recordCheckpoint(o, map[string]any{"ref": ref, "commit": sha})
	return state
}

// checkpointState splits the script's answer into what identifies the WORK
// ("<head> <tree>") and the commit that carries it. The first is content:
// two ticks over an unchanged workspace produce the same pair whether or not
// anything is committed. The second is not: a checkpoint commit is made
// fresh each time and its timestamp moves.
//
// Both halves of the pair matter. The tree alone would go quiet when the run
// commits exactly what was already checkpointed — same content, new history,
// and that history is what a resume reads.
func checkpointState(out string) (string, string) {
	f := strings.Fields(lastLine(out))
	if len(f) != 3 {
		return "", ""
	}
	return f[0] + " " + f[1], f[2]
}

// recordCheckpoint puts the checkpoint on the run's timeline. A safety net
// nobody can see is a safety net nobody trusts — and the operator who has
// to recover a lost run needs the ref NAME, which lives nowhere else (the
// run doc is deliberately untouched: a checkpoint is not a bank, and must
// never make a half-done run look merge-eligible).
func (r *Runner) recordCheckpoint(o sandboxObserverOpts, data map[string]any) {
	wctx, cancel := context.WithTimeout(context.Background(), parkStoreOpTimeout)
	defer cancel()
	idCtx := store.WithIdentity(wctx, o.tenantID, o.ownerID)
	if _, err := r.cfg.Store.AppendEvent(idCtx, o.runID, store.Event{
		Type: store.EventRunWorkspaceCheckpoint,
		Data: data,
	}); err != nil {
		r.cfg.Logger.Warn("runner: run %s: could not emit run_workspace_checkpoint: %v", o.runID, err)
	}
}

// lastLine is the last non-empty line of s, trimmed — git prints the sha
// last, after whatever the credential helper or a hook wrote before it.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if v := strings.TrimSpace(lines[i]); v != "" {
			return v
		}
	}
	return ""
}

// strutilFirstLine names a failure in one line: git's message when it said
// something, the Go error otherwise.
func strutilFirstLine(stderr string, err error) string {
	if v := strings.TrimSpace(stderr); v != "" {
		return lastLine(v)
	}
	if err != nil {
		return err.Error()
	}
	return "push failed without a message"
}
