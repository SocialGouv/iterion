// Runner-side registry of live sandbox Run handles + the mid-run
// credential write-through paths that depend on it (ADR-082 Phase 3).
//
// The kubernetes sandbox workspace is a tar COPY of the runner's clone,
// and the forfait CLAUDE_CONFIG_DIR is a seeded in-pod copy — so every
// host-side credential rewrite the refreshers perform must ALSO be
// pushed through the sandbox exec seam, or the run's last (and most
// valuable) actions — `git push`, the forge review post, a late LLM
// call — authenticate with the launch-time token.
package runner

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// sandboxWriteThroughTimeout bounds one write-through exec into the
// sandbox (kubectl/docker exec round-trip).
const sandboxWriteThroughTimeout = 30 * time.Second

func (r *Runner) registerSandboxRun(runID string, run sandbox.Run) {
	r.sandboxRunsMu.Lock()
	defer r.sandboxRunsMu.Unlock()
	if r.sandboxRuns == nil {
		r.sandboxRuns = map[string]sandbox.Run{}
	}
	r.sandboxRuns[runID] = run
}

func (r *Runner) unregisterSandboxRun(runID string) {
	r.sandboxRunsMu.Lock()
	defer r.sandboxRunsMu.Unlock()
	delete(r.sandboxRuns, runID)
}

// sandboxRunFor returns the live sandbox Run for runID, or nil when the
// run has no active sandbox (unsandboxed, or the sandbox hasn't started
// yet).
func (r *Runner) sandboxRunFor(runID string) sandbox.Run {
	r.sandboxRunsMu.Lock()
	defer r.sandboxRunsMu.Unlock()
	return r.sandboxRuns[runID]
}

// sandboxRunObserver returns the engine's sandbox-run observer for this
// run: it registers the live Run in the write-through registry and, when
// the run carries refreshable file secrets, starts the sandboxed
// file-secret refresh loop (the previous sandboxSecretRefreshObserver
// behaviour). The caller must defer unregisterSandboxRun(runID) and
// cancel ctx when the run ends.
// sandboxObserverOpts names what the observer needs about the run whose
// sandbox it is watching. A struct rather than a parameter list because
// the two callers differ on one field that MATTERS: a subbot child under
// a shared sandbox is handed its PARENT's handle, and a second checkpoint
// loop on the same pod would push the same tree twice, under two names.
type sandboxObserverOpts struct {
	runID    string
	tenantID string
	ownerID  string
	// secretRefs are the run's refreshable file secrets (empty = none).
	secretRefs map[string]string
	// checkpoint preserves the sandbox workspace mid-run. True only for
	// the run that OWNS the pod AND whose workflow did not decline the net
	// (`workspace_checkpoint: off` — a run that writes no commit for the
	// repository it reads, resolved at the launch site).
	checkpoint bool
}

// checkpointsWorkspace answers the one question that decides whether this
// run gets a safety net: is its work unreachable until teardown, and is
// this the run that owns the pod?
//
// Copy-based drivers (kubernetes) are the ones whose workspace lives
// inside the pod — the same type assertion the export path gates on, so
// the two cannot disagree about what "copy-based" means. Bind-mount
// drivers share the host inode: their work is already outside the
// container and a checkpoint would preserve what cannot be lost.
//
// The second half is not an optimisation: a subbot child under a shared
// sandbox is handed its PARENT's handle, so without it two loops would
// push the same tree to two refs, doubling the cost and inventing a
// second story about one workspace.
func checkpointsWorkspace(run sandbox.Run, o sandboxObserverOpts) bool {
	if !o.checkpoint {
		return false
	}
	_, copyBased := run.(sandbox.WorkspaceExporter)
	return copyBased
}

func (r *Runner) sandboxRunObserver(ctx context.Context, o sandboxObserverOpts) func(sandbox.Run) {
	return func(run sandbox.Run) {
		r.registerSandboxRun(o.runID, run)
		// A workspace that is a COPY inside a pod keeps every commit —
		// and everything not yet committed — out of reach until the
		// export at teardown, which is exactly the moment a dying pod
		// stops answering. Bind-mount drivers share the host inode and
		// need no net: their work is already outside the container.
		if checkpointsWorkspace(run, o) {
			errtrack.Go("runner.checkpointWorkspace", func() {
				r.checkpointWorkspaceLoop(ctx, o, run)
			})
		}
		if len(o.secretRefs) == 0 {
			return
		}
		refresher, ok := run.(sandbox.SecretFileRefresher)
		if !ok {
			r.cfg.Logger.Warn("runner: sandbox driver %q does not support mid-run secret refresh; a long run may push with a stale token", run.Driver())
			return
		}
		errtrack.Go("runner.refreshSandboxFileSecrets", func() {
			r.refreshSandboxFileSecretsLoop(ctx, o.tenantID, o.secretRefs, refresher)
		})
	}
}

// writeThroughSandboxGitCredential pushes a rotated forge token into the
// sandbox workspace's COPY of the clone credential store
// (`.git/iterion-credentials`). Only drivers whose workspace is a copy
// implement [sandbox.WorkspaceFileRefresher] (kubernetes); bind-mount
// drivers (docker) share the host inode with the file
// refreshGitCredentialsLoop just rewrote, and the noop passthrough IS
// the host — both are correctly a nil no-op here.
//
// A non-nil error means the pod is still holding the PREVIOUS token: the
// caller must NOT record the rotation as applied (it would otherwise
// only retry on the next server-side rotation, ~1h away) — retrying the
// same value next tick is the recovery.
func (r *Runner) writeThroughSandboxGitCredential(runID, repoURL, token string) error {
	run := r.sandboxRunFor(runID)
	if run == nil {
		return nil
	}
	refresher, ok := run.(sandbox.WorkspaceFileRefresher)
	if !ok {
		return nil
	}
	line, err := renderGitCredentialLine(repoURL, token)
	if err != nil {
		return fmt.Errorf("sandbox git-credential write-through: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sandboxWriteThroughTimeout)
	defer cancel()
	if err := refresher.RefreshWorkspaceFile(ctx, ".git/"+gitCredentialFile, []byte(line)); err != nil {
		return fmt.Errorf("sandbox git-credential write-through: %w", err)
	}
	r.cfg.Logger.Info("runner: rotated git credential written through into the sandbox workspace")
	return nil
}

// propagateForfaitToSandbox pushes the just-refreshed Claude forfait
// credentials file (path, on the runner host) into the run's live
// sandbox: the ADR-070 file-secret mount (k8s Secret / docker temp-file
// bind) AND the writable in-sandbox CLAUDE_CONFIG_DIR copy the CLI
// actually reads. No-op when the run has no real sandbox — the noop
// passthrough reads the host file directly.
func (r *Runner) propagateForfaitToSandbox(runID, path string) {
	run := r.sandboxRunFor(runID)
	if run == nil {
		return
	}
	refresher, ok := run.(sandbox.SecretFileRefresher)
	if !ok {
		return
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		r.cfg.Logger.Warn("runner: forfait sandbox write-through run=%s: read refreshed file: %v", runID, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sandboxWriteThroughTimeout)
	defer cancel()
	// Keep the ADR-070 mount truthful (k8s propagates the Secret update to
	// the read-only projection; docker rewrites the bind source) …
	if err := refresher.RefreshSecretFile(ctx, secrets.ClaudeCodeOAuthSecretName, payload); err != nil {
		r.cfg.Logger.Warn("runner: forfait sandbox write-through run=%s: refresh secret mount: %v", runID, err)
	}
	// … and rewrite the WRITABLE seeded copy the claude CLI reads.
	if err := sandbox.WriteFileExec(ctx, run, secrets.ClaudeCodeSandboxCredentialsPath, payload); err != nil {
		r.cfg.Logger.Warn("runner: forfait sandbox write-through run=%s: rewrite config copy: %v", runID, err)
		return
	}
	r.cfg.Logger.Info("runner: refreshed forfait credentials written through into the sandbox run=%s", runID)
}
