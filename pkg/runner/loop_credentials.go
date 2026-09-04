package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// injectCredentials resolves the run's sealed bundle, decrypts it,
// stamps the plaintext into ctx via secrets.WithCredentials, and
// returns a cleanup func that performs LOCAL hygiene (wipes the
// in-memory plaintext keys + removes the OAuth temp dirs) at the call
// site. When no bundle is attached or the runner has no Sealer wired,
// returns the original ctx unchanged.
//
// The cleanup func runs on every executeRun return. Removal of the
// *persistent* sealed bundle from the store is intentionally NOT part
// of cleanup — see deleteRunSecrets, which executeRun invokes only on a
// terminal-clean outcome so a redelivered run can re-fetch its secrets.
//
// OAuth-forfait blobs are materialised in fresh temp directories
// (CLAUDE_CONFIG_DIR / CODEX_HOME-shaped) and wired through
// Credentials.OAuthCredentialFiles so the delegate backends point
// the spawned CLI at them. The cleanup func tears the dirs down on
// every exit path.
func (r *Runner) injectCredentials(ctx context.Context, msg *queue.RunMessage) (context.Context, func(), error) {
	if msg.SecretsRef == "" {
		return ctx, nil, nil
	}
	if r.cfg.RunSecrets == nil || r.cfg.Sealer == nil {
		return ctx, nil, fmt.Errorf("runner: SecretsRef set but RunSecrets/Sealer not wired")
	}
	rec, err := r.cfg.RunSecrets.Get(ctx, msg.SecretsRef)
	if err != nil {
		return ctx, nil, fmt.Errorf("fetch run_secrets %s: %w", msg.SecretsRef, err)
	}
	// Tenant binding: rec.TenantID and msg.TenantID must match exactly.
	// The old code allowed empty rec.TenantID to bypass the check, on
	// the assumption that legacy records predated multitenancy — but
	// once a tenant_id is on the wire (msg.TenantID), a SecretsRef
	// stamped without a tenant could be served to a different tenant
	// that happened to request the same ref. New writes always stamp
	// tenant_id; if you see this error for a legacy ref, backfill its
	// tenant via the migration script before resuming the run.
	if rec.TenantID != msg.TenantID {
		return ctx, nil, fmt.Errorf("run_secrets tenant mismatch (msg=%q sealed=%q)", msg.TenantID, rec.TenantID)
	}
	bundle, err := secrets.OpenRunBundle(r.cfg.Sealer, msg.RunID, rec.SealedBundle)
	if err != nil {
		return ctx, nil, fmt.Errorf("unseal run_secrets %s: %w", msg.SecretsRef, err)
	}

	// The audit identity of each credential: cleanup zeroes the secrets,
	// the fingerprints stay — they are what lets the usage-cap meter key
	// tell a rotated credential from the account it replaced. An API key
	// is static, so its own hash identifies it; an OAuth payload is NOT
	// (the refresh worker rewrites its tokens every few hours for the
	// same subscription), so the record's connect-time fingerprint
	// travels in the bundle instead. Absent for legacy records — those
	// keep the fingerprint-less meter they always had.
	fingerprints := map[string]string{}
	for prov, key := range bundle.APIKeys {
		if key != "" {
			fingerprints[string(prov)] = secrets.FingerprintSHA256(key)
		}
	}
	for kind, fp := range bundle.OAuthFingerprints {
		if fp != "" {
			fingerprints[kind] = fp
		}
	}

	creds := secrets.Credentials{
		APIKeys: bundle.APIKeys,
		Generic: bundle.GenericSecrets,
		// Per-secret egress narrowing from bot-secret bindings; the guard
		// intersects these with the workflow's declared hosts. Hostnames
		// are not secret, so cleanup below leaves them untouched.
		GenericHosts:         bundle.GenericSecretHosts,
		GenericRefs:          bundle.GenericSecretRefs,
		OAuthCredentialFiles: map[string]string{},
		ForgeAppBotLogin:     bundle.ForgeAppBotLogin,
		// Slot names (not values) — safe to keep past cleanup. The
		// usage-cap scope check reads them to meter platform-tier
		// credentials on the shared platform key.
		PlatformSourced: bundle.PlatformSourced,
		Fingerprints:    fingerprints,
	}
	tmpDirs := make([]string, 0, len(bundle.OAuthCredentials))
	// cancelRefresh stops the per-run OAuth-forfait token refreshers (set
	// below once the files are materialised). cleanup calls it so the
	// goroutines exit before their temp dirs are removed.
	cancelRefresh := func() {}
	// cleanup performs LOCAL process hygiene only — wiping the decrypted
	// API keys from memory and removing the materialised OAuth temp dirs.
	// It runs on EVERY executeRun return (including Nak-for-redelivery
	// paths) so plaintext never outlives the attempt. Deleting the
	// *persistent* sealed bundle is deliberately NOT done here: it must
	// happen only on a terminal-clean outcome (executeRun calls
	// deleteRunSecrets) so a redelivered run can re-fetch the same
	// SecretsRef instead of silently running credential-less.
	cleanup := func() {
		cancelRefresh()
		for k := range bundle.APIKeys {
			bundle.APIKeys[k] = ""
		}
		for k := range bundle.GenericSecrets {
			bundle.GenericSecrets[k] = ""
		}
		for _, dir := range tmpDirs {
			_ = os.RemoveAll(dir)
		}
	}
	// refreshFiles maps oauth kind → the materialised credential file path,
	// fed to the per-run forfait token refresher so a long run never hits an
	// expired token mid-workflow (see oauth_refresh.go).
	refreshFiles := make(map[string]string, len(bundle.OAuthCredentials))
	for kind, payload := range bundle.OAuthCredentials {
		dir, fname, err := materializeOAuthCredentials(kind, payload)
		if err != nil {
			r.cfg.Logger.Warn("runner: oauth materialise %s for run %s: %v", kind, msg.RunID, err)
			continue
		}
		tmpDirs = append(tmpDirs, dir)
		creds.OAuthCredentialFiles[kind] = dir
		refreshFiles[kind] = filepath.Join(dir, fname)
		r.cfg.Logger.Info("runner: oauth-forfait active run=%s tenant=%s kind=%s file=%s/%s", msg.RunID, msg.TenantID, kind, dir, fname)
	}
	if len(refreshFiles) > 0 {
		stopRefresh := make(chan struct{})
		var once sync.Once
		cancelRefresh = func() { once.Do(func() { close(stopRefresh) }) }
		r.startOAuthRefreshers(stopRefresh, msg.RunID, refreshFiles)
	}
	ctx = secrets.WithCredentials(ctx, creds)
	// The attempt holds these keys from here on: say so now, not only when
	// it ends — a multi-hour attempt otherwise reads as an idle key for
	// its whole duration (#659 pt 2).
	r.markCredFingerprintsUsed(ctx, msg, time.Now().UTC())
	return ctx, cleanup, nil
}

// deleteRunSecrets best-effort removes the persistent sealed bundle for
// this run from the RunSecrets store. executeRun calls it ONLY on a
// terminal-clean outcome (success or paused-for-resume) — never on a
// Nak-for-redelivery path, where the SAME SecretsRef must survive so the
// redelivered attempt can re-fetch its credentials. Detached from the
// (possibly already-cancelled) run context with its own short timeout. A
// failed delete is logged but non-fatal: the store's 24h TTL reaps the
// bundle regardless.
func (r *Runner) deleteRunSecrets(msg *queue.RunMessage) {
	if msg.SecretsRef == "" || r.cfg.RunSecrets == nil {
		return
	}
	ctxDel, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if delErr := r.cfg.RunSecrets.Delete(ctxDel, msg.SecretsRef); delErr != nil {
		r.cfg.Logger.Warn("runner: run_secrets delete for %s (ref=%s): %v", msg.RunID, msg.SecretsRef, delErr)
	}
}

// materializeOAuthCredentials writes the sealed payload to a fresh
// temp dir under the file name the corresponding CLI expects.
//
//   - claude_code → <dir>/.credentials.json (CLAUDE_CONFIG_DIR=<dir>)
//   - codex       → <dir>/auth.json         (CODEX_HOME=<dir>)
//
// The directory is mode 0o700, the file 0o600 so other local users
// (including a sandbox host's UID-shifted writer) cannot read.
func materializeOAuthCredentials(kind string, payload []byte) (dir string, fname string, err error) {
	switch secrets.OAuthKind(kind) {
	case secrets.OAuthKindClaudeCode:
		fname = ".credentials.json"
	case secrets.OAuthKindCodex:
		fname = "auth.json"
	default:
		return "", "", fmt.Errorf("unknown oauth kind %q", kind)
	}
	dir, err = os.MkdirTemp("", "iter-oauth-")
	if err != nil {
		return "", "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	full := filepath.Join(dir, fname)
	if err := os.WriteFile(full, payload, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	return dir, fname, nil
}
