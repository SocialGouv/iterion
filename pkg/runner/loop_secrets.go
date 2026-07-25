package runner

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// materializeFileSecretsNoSandbox writes the workflow's `as: file` secrets to
// 0600 files at their mount paths in the runner pod when the run has no
// sandbox (a sandboxed run mounts them into the container instead). Returns
// the written files keyed by secret name (for the mid-run refresher) and a
// cleanup that removes them; both nil when nothing was written.
func (r *Runner) materializeFileSecretsNoSandbox(ctx context.Context, wf *ir.Workflow) (map[string]string, func(), error) {
	if wf == nil || len(wf.Secrets) == 0 ||
		runtime.WorkflowSandboxActive(wf, r.cfg.SandboxOverride, r.cfg.SandboxDefault) {
		// No secrets, or the run RESOLVES to an active sandbox (which mounts
		// file secrets into the container). The resolved decision — not
		// wf.Sandbox — is what matters: under ITERION_SANDBOX_OVERRIDE=none a
		// bot's sandbox block is neutralized and the run executes in this pod,
		// so its file secrets must be materialized here (run 019f4551's
		// push_auth_probe found no forge_token exactly because this gate used
		// to test the static declaration).
		return nil, nil, nil
	}
	creds, _ := secrets.CredentialsFromContext(ctx)
	written := map[string]string{}
	paths := func() []string {
		out := make([]string, 0, len(written))
		for _, p := range written {
			out = append(out, p)
		}
		return out
	}
	for name, s := range wf.Secrets {
		if !s.IsFile() {
			continue
		}
		val := creds.GenericSecret(name)
		if val == "" {
			continue // optional / unresolved → skip; the agent just won't find it
		}
		mp := secrets.ResolveFileMountPath(name, s.MountPath)
		// Confine writes to the secrets mount dir. The default mount path is
		// always under it; a DSL-supplied mount_path is tenant-controlled and
		// this runner pod is NOT sandboxed, so without this guard a crafted
		// mount_path (e.g. /root/.ssh/authorized_keys, /etc/cron.d/x) would
		// write the secret value to an arbitrary host path. The helper also
		// rejects path traversal and non-clean paths.
		if _, ok := secrets.RelativeToSecretFilesMountDir(mp); !ok {
			if r.cfg.Logger != nil {
				r.cfg.Logger.Warn("runner: refusing out-of-tree mount_path %q for file secret %q (must be under %s)", mp, name, secrets.SecretFilesMountDir)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(mp), 0o700); err != nil {
			return written, removeFilesFunc(paths()), err
		}
		if err := os.WriteFile(mp, []byte(val), 0o600); err != nil {
			return written, removeFilesFunc(paths()), err
		}
		written[name] = mp
	}
	if len(written) == 0 {
		return nil, nil, nil
	}
	return written, removeFilesFunc(paths()), nil
}

// fileSecretRefreshInterval paces the mid-run re-read of materialised file
// secrets from the generic-secret store. Well under the 10-minute cadence of
// the server-side refresh worker, so a rotated credential reaches the file
// within minutes of the store update.
const fileSecretRefreshInterval = 5 * time.Minute

// refreshFileSecretsLoop re-reads each materialised file secret's store
// record on a fixed cadence and rewrites the file when the value changed.
// Tools re-read the file per invocation (`cat /run/iterion/secrets/<name>`),
// so a rotation propagates to every subsequent forge push/comment without
// touching the process environment. Failures are logged and retried next
// tick — the file keeps its last good value; nothing is ever truncated.
func (r *Runner) refreshFileSecretsLoop(ctx context.Context, tenantID string, refs, files map[string]string) {
	tick := time.NewTicker(fileSecretRefreshInterval)
	defer tick.Stop()
	last := map[string]string{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		r.refreshFileSecretsOnce(ctx, tenantID, refs, files, last)
	}
}

// readFreshSecret re-reads and unseals a generic-secret record by id
// under the tenant scope. Bounded, tenant-scoped; never logs the value.
// Shared by the no-sandbox (refreshFileSecretsOnce) and sandboxed
// (refreshSandboxFileSecretsOnce) mid-run refresh paths.
func (r *Runner) readFreshSecret(ctx context.Context, tenantID, id string) ([]byte, error) {
	rctx, cancel := context.WithTimeout(store.WithTenant(ctx, tenantID), 15*time.Second)
	defer cancel()
	rec, err := r.cfg.GenericSecrets.Get(rctx, id)
	if err != nil {
		return nil, err
	}
	return secrets.OpenGenericSecret(r.cfg.Sealer, rec.ID, rec.SealedSecret)
}

// refreshFileSecretsOnce is one tick of refreshFileSecretsLoop: re-read every
// ref'd file secret and atomically rewrite the ones whose store value moved.
func (r *Runner) refreshFileSecretsOnce(ctx context.Context, tenantID string, refs, files, last map[string]string) {
	for name, path := range files {
		id := refs[name]
		if id == "" {
			continue // snapshot-only secret (no store ref) — nothing to refresh
		}
		val, err := r.readFreshSecret(ctx, tenantID, id)
		if err != nil {
			r.cfg.Logger.Warn("runner: refresh file secret %q (ref %s): %v", name, id, err)
			continue
		}
		if len(val) == 0 || string(val) == last[name] {
			continue
		}
		// Atomic replace so a concurrent `cat` never sees a torn write.
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, val, 0o600); err != nil {
			r.cfg.Logger.Warn("runner: refresh file secret %q: write: %v", name, err)
			continue
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			r.cfg.Logger.Warn("runner: refresh file secret %q: rename: %v", name, err)
			continue
		}
		last[name] = string(val)
		r.cfg.Logger.Info("runner: refreshed file secret %q from store (rotation picked up)", name)
	}
}

// refreshGitCredentialsLoop keeps the clone's credential file live for the
// whole run. It is the counterpart of refreshFileSecretsLoop for the token git
// itself uses: a GitHub App installation token lives 1h, an app-building run
// takes several, and the push happens at the END — so without this the last
// and most valuable action of the run is the one that fails.
//
// When the run executes inside a copy-based sandbox (kubernetes), the
// rotated token is additionally written THROUGH into the pod workspace's
// own credential store — the host rewrite below lands in a clone the
// sandboxed git never reads (ADR-082 Phase 3 blocker 1).
//
// Best-effort by design: a transient store error just leaves the previous
// (still possibly valid) credential in place until the next tick.
func (r *Runner) refreshGitCredentialsLoop(ctx context.Context, tenantID, secretID, runID, dir, repoURL string) {
	tick := time.NewTicker(fileSecretRefreshInterval)
	defer tick.Stop()
	path := filepath.Join(dir, ".git", gitCredentialFile)
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		r.refreshGitCredentialsOnce(ctx, tenantID, secretID, runID, path, repoURL, &last)
	}
}

// refreshGitCredentialsOnce is one tick of refreshGitCredentialsLoop.
// `last` advances ONLY when the rotation reached EVERY consumer — the
// host clone file AND the sandbox workspace copy. Advancing it on a
// partial delivery (host ok, pod exec transiently failed) would park the
// pod on the previous token until the NEXT server-side rotation, ~1h
// away — exactly the stale-push window this loop exists to close; by
// keeping `last` unchanged the whole rotation is retried next tick (the
// host rewrite is idempotent).
func (r *Runner) refreshGitCredentialsOnce(ctx context.Context, tenantID, secretID, runID, path, repoURL string, last *string) {
	val, err := r.readFreshSecret(ctx, tenantID, secretID)
	if err != nil {
		r.cfg.Logger.Warn("runner: refresh git credential (ref %s): %v", secretID, err)
		return
	}
	if len(val) == 0 || string(val) == *last {
		return
	}
	if err := writeGitCredentials(path, repoURL, string(val)); err != nil {
		r.cfg.Logger.Warn("runner: refresh git credential: %v", err)
		return
	}
	if err := r.writeThroughSandboxGitCredential(runID, repoURL, string(val)); err != nil {
		r.cfg.Logger.Warn("runner: refresh git credential: %v — retrying the rotation next tick", err)
		return
	}
	*last = string(val)
	r.cfg.Logger.Info("runner: refreshed the clone's git credential from store (rotation picked up)")
}

// gitCredentialSecretRef returns the store id backing the run's forge token,
// or "" when the run has no refreshable one (snapshot-only, or no store).
// Mirrors the name precedence cloneRepo uses to pick the token.
func (r *Runner) gitCredentialSecretRef(ctx context.Context) string {
	if r.cfg.GenericSecrets == nil || r.cfg.Sealer == nil {
		return ""
	}
	creds, ok := secrets.CredentialsFromContext(ctx)
	if !ok {
		return ""
	}
	for _, name := range []string{"forge_token", "gitlab_token", "github_token"} {
		if id := creds.GenericRefs[name]; id != "" {
			return id
		}
	}
	return ""
}

// sandboxFileSecretRefs returns the file secrets of wf that carry a store
// ref in the run's credentials — the ones a mid-run refresh can rewrite.
// Nil when the store/creds/refs are absent or no file secret is
// refreshable. Shared shape with the no-sandbox refresher's refs map
// (name → generic-secret id).
func (r *Runner) sandboxFileSecretRefs(ctx context.Context, wf *ir.Workflow) map[string]string {
	if wf == nil || len(wf.Secrets) == 0 || r.cfg.GenericSecrets == nil {
		return nil
	}
	creds, ok := secrets.CredentialsFromContext(ctx)
	if !ok || len(creds.GenericRefs) == 0 {
		return nil
	}
	refs := map[string]string{}
	for name, s := range wf.Secrets {
		if !s.IsFile() {
			continue
		}
		if id := creds.GenericRefs[name]; id != "" {
			refs[name] = id
		}
	}
	if len(refs) == 0 {
		return nil
	}
	return refs
}

// refreshSandboxFileSecretsLoop re-reads each refreshable file secret's
// store record on a fixed cadence and, when the value rotated, hands it
// to the sandbox driver's SecretFileRefresher to propagate into the
// running container. Mirrors refreshFileSecretsLoop; failures are logged
// (never the value) and retried next tick.
func (r *Runner) refreshSandboxFileSecretsLoop(ctx context.Context, tenantID string, refs map[string]string, refresher sandbox.SecretFileRefresher) {
	tick := time.NewTicker(fileSecretRefreshInterval)
	defer tick.Stop()
	last := map[string]string{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		r.refreshSandboxFileSecretsOnce(ctx, tenantID, refs, refresher, last)
	}
}

// refreshSandboxFileSecretsOnce is one tick of
// refreshSandboxFileSecretsLoop: re-read every ref'd file secret and push
// the ones whose store value moved into the sandbox.
func (r *Runner) refreshSandboxFileSecretsOnce(ctx context.Context, tenantID string, refs map[string]string, refresher sandbox.SecretFileRefresher, last map[string]string) {
	for name, id := range refs {
		val, err := r.readFreshSecret(ctx, tenantID, id)
		if err != nil {
			r.cfg.Logger.Warn("runner: refresh sandboxed file secret %q (ref %s): %v", name, id, err)
			continue
		}
		if len(val) == 0 || string(val) == last[name] {
			continue
		}
		if err := refresher.RefreshSecretFile(ctx, name, val); err != nil {
			r.cfg.Logger.Warn("runner: refresh sandboxed file secret %q: %v", name, err)
			continue
		}
		last[name] = string(val)
		r.cfg.Logger.Info("runner: refreshed sandboxed file secret %q into the container (rotation picked up)", name)
	}
}

func removeFilesFunc(paths []string) func() {
	return func() {
		for _, p := range paths {
			_ = os.Remove(p)
		}
	}
}
