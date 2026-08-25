package forge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TokenRefresher renews one connection's admin credential. Per-provider
// implementations (OAuth refresh-token grant, GitHub-App installation-token
// mint) satisfy it; PAT connections have no refresher and are skipped.
//
// refreshToken is the connection's current refresh token (already unsealed
// by the worker). Refresh returns the new token material. A nil error with
// an empty AccessToken means "nothing to do" (the implementation decided no
// refresh was needed). Returning ErrUnauthorized marks the connection
// revoked.
type TokenRefresher interface {
	Refresh(ctx context.Context, conn Connection, refreshToken string) (RefreshedToken, error)
}

// RefreshedToken is the output of a successful refresh.
type RefreshedToken struct {
	AccessToken  string
	RefreshToken string // may be rotated by the provider; empty = keep current
	ExpiresAt    time.Time
	Scopes       []string
}

// RefreshWorker keeps OAuth-app and GitHub-App connection tokens fresh and
// rewrites the connection's managed generic secret so bot runs always read
// a live token. Mirrors pkg/secrets/oauth_refresh.go's role for LLM
// forfaits. PAT connections are never touched.
type RefreshWorker struct {
	Connections ConnectionStore
	Secrets     secrets.GenericSecretStore
	Sealer      secrets.Sealer
	// Refresher resolves a per-provider/kind refresher for a connection.
	// Returns (nil, nil) when the connection kind cannot/should-not refresh
	// (e.g. PAT) — the worker skips it.
	RefresherFor func(conn Connection) TokenRefresher
	// SecurityMinter, when set, mints the org-wide vulnerability_alerts:read
	// installation token for a github_app connection that opted into
	// SecurityReadEnabled. The worker re-mints it alongside each connection
	// refresh (both tokens live ~1h, so they rotate together) and merges it
	// into the tenant's dependabot_tokens secret. Nil = feature unwired.
	SecurityMinter func(ctx context.Context, conn Connection) (string, error)
	// Lead is how far before expiry a token is refreshed (default 5m).
	Lead time.Duration
	Now  func() time.Time
}

func (w *RefreshWorker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now().UTC()
}

func (w *RefreshWorker) lead() time.Duration {
	if w.Lead > 0 {
		return w.Lead
	}
	return 5 * time.Minute
}

// RunOnce refreshes every connection expiring within the lead window.
// Returns the number refreshed; per-connection errors are collected but do
// not abort the sweep (one bad connection must not wedge the others).
func (w *RefreshWorker) RunOnce(ctx context.Context) (int, error) {
	cutoff := w.now().Add(w.lead())
	due, err := w.Connections.ExpiringBefore(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("forge: list expiring connections: %w", err)
	}
	refreshed := 0
	var firstErr error
	for _, conn := range due {
		if err := w.refreshOne(ctx, conn); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		refreshed++
	}
	return refreshed, firstErr
}

// refreshOne renews one connection. Ordering is load-bearing: the
// connection blob is written FIRST (the canonical record), the managed
// generic secret SECOND, so a crash between them leaves a working-but-old
// secret rather than a zero value.
func (w *RefreshWorker) refreshOne(ctx context.Context, conn Connection) error {
	if w.RefresherFor == nil {
		return nil
	}
	r := w.RefresherFor(conn)
	if r == nil {
		return nil // not refreshable (PAT) — skip
	}
	// A degraded connection failed on a PERMANENT config mismatch (see
	// markDegraded). Re-minting it every tick can never self-heal — it just
	// re-hits the forge and re-spams the warning — so skip it until an
	// operator reconnect flips it back to StatusActive. Its security-read
	// entry is withdrawn rather than left rotting: the map carries no
	// expiry, so a stale token would only surface as an unexplained 401 in
	// the consuming bot an hour later — pulling the entry makes the bot's
	// missing-org gate name the problem instead.
	if conn.Status == StatusDegraded {
		return w.dropSecurityReadEntry(ctx, conn)
	}
	// RunOnce iterates connections across every tenant, so its ctx carries no
	// tenant. The managed-secret store is tenant-scoped (Get/Update require the
	// tenant on the ctx); without this the token mint succeeds and the
	// connection updates (its store keys on _id only), but rewriteManagedSecret
	// silently fails to persist the fresh token — leaving bot runs reading a
	// stale, expired installation token (HTTP 401). Scope the ctx to THIS
	// connection's tenant for all its store writes.
	ctx = store.WithTenant(ctx, conn.TenantID)
	cur, err := openConnectionSecret(w.Sealer, conn.ID, conn.SealedPayload)
	if err != nil {
		return err
	}
	out, err := r.Refresh(ctx, conn, cur.RefreshToken)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			return w.markRevoked(ctx, conn)
		}
		// A permission mismatch is a PERMANENT config error (the install was
		// approved with fewer permissions than iterion now requests). Mark the
		// connection degraded — recording the actionable reason ONCE — so the
		// worker stops re-minting it, instead of returning the error (which the
		// server would Warn-log every 10-minute tick).
		if errors.Is(err, ErrPermissionsNotGranted) {
			return w.markDegraded(ctx, conn, err)
		}
		return err
	}
	if out.AccessToken == "" {
		return nil // refresher decided nothing to do
	}

	// 1) re-seal the connection blob (canonical).
	cur.AccessToken = out.AccessToken
	if out.RefreshToken != "" {
		cur.RefreshToken = out.RefreshToken
	}
	if !out.ExpiresAt.IsZero() {
		cur.ExpiresAt = out.ExpiresAt
	}
	sealed, err := sealConnectionSecret(w.Sealer, conn.ID, cur)
	if err != nil {
		return err
	}
	now := w.now()
	conn.SealedPayload = sealed
	conn.Status = StatusActive
	conn.StatusReason = "" // a successful mint clears any prior degrade/reauth reason
	conn.LastRefreshedAt = &now
	if !out.ExpiresAt.IsZero() {
		exp := out.ExpiresAt
		conn.AccessTokenExpiresAt = &exp
	}
	if len(out.Scopes) > 0 {
		conn.Scopes = out.Scopes
	}
	conn.UpdatedAt = now
	if err := w.Connections.Update(ctx, conn); err != nil {
		return err
	}

	// 2) rewrite the managed generic secret plaintext so bot runs read the
	// fresh token. A failure here leaves the secret stale-but-valid until
	// the next tick — acceptable (the connection is already updated).
	if conn.ManagedSecretID != "" {
		if err := w.rewriteManagedSecret(ctx, conn.ManagedSecretID, out.AccessToken); err != nil {
			return err
		}
	}

	// 3) security-read token (opt-in): re-mint the org-wide
	// vulnerability_alerts:read token in the same cycle — it shares the
	// ~1h installation-token lifetime, so refreshing it here keeps the
	// dependabot_tokens map exactly as fresh as the forge token. A failure
	// is returned (→ one visible warn per cycle, and the connection health
	// view names the missing grant), never silently swallowed: an hourly
	// vuln-watch reading a dead token would otherwise fail with no trail
	// on the server side.
	if conn.SecurityReadEnabled && conn.Kind == KindGitHubApp && w.SecurityMinter != nil {
		secTok, err := w.SecurityMinter(ctx, conn)
		if errors.Is(err, ErrPermissionsNotGranted) {
			// PERMANENT (the org revoked, or never approved, the alerts
			// grant): retrying can never self-heal, and the entry already in
			// the map dies within the hour. Withdraw it so the consuming bot
			// names the missing org instead of hitting an unexplained 401,
			// and record the reason ONCE rather than erroring every tick —
			// the same treatment the refresher's own permission failure gets.
			if derr := w.dropSecurityReadEntry(ctx, conn); derr != nil {
				return derr
			}
			return w.markSecurityReadDegraded(ctx, conn, err)
		}
		if err != nil {
			return fmt.Errorf("forge: security-read mint for connection %s: %w", conn.ID, err)
		}
		if err := UpsertSecurityReadToken(ctx, w.Secrets, w.Sealer, &conn, secTok, w.now()); err != nil {
			return err
		}
	}
	return nil
}

// markSecurityReadDegraded turns the security-read opt-in back off on a
// PERMANENT mint failure, stamping the actionable reason. The connection
// itself stays usable (its forge token is unaffected) — only the alerts
// lane is switched off, so an operator re-enables it after fixing the grant
// instead of the worker re-hitting the forge every ten minutes.
func (w *RefreshWorker) markSecurityReadDegraded(ctx context.Context, conn Connection, cause error) error {
	conn.SecurityReadEnabled = false
	conn.StatusReason = fmt.Sprintf(
		"security-read disabled: %v — approve 'Dependabot alerts: read' on the installation, then re-enable it",
		cause)
	conn.UpdatedAt = w.now()
	return w.Connections.Update(ctx, conn)
}

// dropSecurityReadEntry withdraws a connection's org entry from the
// dependabot_tokens map when the connection can no longer mint (degraded /
// revoked): a token in that map is a promise of freshness, and a connection
// that stopped refreshing must not leave a silently-dying one behind.
func (w *RefreshWorker) dropSecurityReadEntry(ctx context.Context, conn Connection) error {
	if !conn.SecurityReadEnabled || conn.Kind != KindGitHubApp {
		return nil
	}
	// RunOnce's ctx carries NO tenant (it sweeps every tenant), and the
	// degraded early-return happens BEFORE refreshOne scopes it — so scope
	// here too. Without it the tenant-scoped store refuses every read and
	// the withdrawal could never land in cloud, re-erroring on every tick.
	ctx = store.WithTenant(ctx, conn.TenantID)
	if err := RemoveSecurityReadToken(ctx, w.Secrets, w.Sealer, &conn); err != nil {
		return fmt.Errorf("forge: withdraw security-read entry for stalled connection %s: %w", conn.ID, err)
	}
	return nil
}

func (w *RefreshWorker) rewriteManagedSecret(ctx context.Context, secretID, token string) error {
	gs, err := w.Secrets.Get(ctx, secretID)
	if err != nil {
		return fmt.Errorf("forge: load managed secret for rewrite: %w", err)
	}
	sealed, err := secrets.SealGenericSecret(w.Sealer, secretID, []byte(token))
	if err != nil {
		return err
	}
	gs.SealedSecret = sealed
	gs.Last4 = secrets.Last4(token)
	gs.Fingerprint = secrets.FingerprintSHA256(token)
	return w.Secrets.Update(ctx, gs)
}

func (w *RefreshWorker) markRevoked(ctx context.Context, conn Connection) error {
	now := w.now()
	conn.Status = StatusNeedsReauth
	conn.UpdatedAt = now
	if err := w.Connections.Update(ctx, conn); err != nil {
		return err
	}
	// A revoked credential can no longer refresh the org token either —
	// withdraw it (same rationale as the degraded path).
	return w.dropSecurityReadEntry(ctx, conn)
}

// markDegraded flips a connection to StatusDegraded on a permanent config
// mismatch (ErrPermissionsNotGranted), stamping GitHub's own message plus the
// remediation onto StatusReason. refreshOne then skips the connection on every
// later tick, so the reason surfaces once rather than re-logging each cycle. A
// reconnect/re-provision resets Status to StatusActive, clearing it.
func (w *RefreshWorker) markDegraded(ctx context.Context, conn Connection, cause error) error {
	now := w.now()
	conn.Status = StatusDegraded
	conn.StatusReason = fmt.Sprintf(
		"%v — an org admin must re-approve the installation with the updated permissions, or the connection should be removed",
		cause)
	conn.UpdatedAt = now
	return w.Connections.Update(ctx, conn)
}
