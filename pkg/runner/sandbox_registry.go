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
func (r *Runner) sandboxRunObserver(ctx context.Context, runID, tenantID string, refs map[string]string) func(sandbox.Run) {
	return func(run sandbox.Run) {
		r.registerSandboxRun(runID, run)
		if len(refs) == 0 {
			return
		}
		refresher, ok := run.(sandbox.SecretFileRefresher)
		if !ok {
			r.cfg.Logger.Warn("runner: sandbox driver %q does not support mid-run secret refresh; a long run may push with a stale token", run.Driver())
			return
		}
		goTracked("runner.refreshSandboxFileSecrets", func() {
			r.refreshSandboxFileSecretsLoop(ctx, tenantID, refs, refresher)
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
