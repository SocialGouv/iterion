package runner

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// oauthRefreshHTTPTimeout bounds a single token-refresh round-trip. The loop
// is torn down via a stop channel (not a context) to match the codebase's
// goroutine-teardown idiom, so the in-flight refresh is bounded by this alone.
const oauthRefreshHTTPTimeout = 30 * time.Second

// oauthRefreshLead is how far ahead of expiry a materialised forfait token is
// proactively refreshed. The Claude Code OAuth access token lives ~8h; a run
// that outlives it would otherwise hand the `claude` CLI an expired token
// mid-workflow. 10 min of slack covers clock skew + the refresh round-trip.
const oauthRefreshLead = 10 * time.Minute

// startOAuthRefreshers launches one background goroutine per materialised
// OAuth-forfait credential file that keeps the file's access token fresh for
// the LIFETIME of the run: it refreshes the token (using the refresh_token
// already in the file + the public client id) and rewrites the file in place
// shortly BEFORE the token expires, so neither a fresh per-node `claude`
// subprocess nor the CLI's own session refresh ever sees an expired token.
// This is the per-run complement to the server's OAuthRefreshWorker (which
// keeps the cloud STORE fresh for the NEXT run, but can't touch a file a
// runner already materialised at claim time).
//
// The goroutines stop when `stop` is closed (run end / cleanup). Only the
// Claude Code (Anthropic) forfait is handled, and NOT because codex lacks a
// usable client id — a codex credential names its own (see
// secrets.CodexCredentialsView.OAuthClientID), so that half is merely the
// easy half. Codex is excluded because of OWNERSHIP.
//
// A codex run draws its blob from a SHARED tier (platform/team/pool: the
// platform record is one meter for the whole deployment), so every
// concurrent run materialises a copy of the SAME credential. This loop
// refreshes a run-local FILE and never writes back to the OAuthStore — and
// OpenAI rotates the refresh token on use, so the first run to refresh
// would invalidate the token still held by the store, by the server's
// OAuthRefreshWorker, and by every sibling run. One long run would poison
// the deployment's codex forfait until a human re-connected it, which is a
// strictly worse failure than the expiry this loop exists to avoid.
//
// That is not a hypothesis: it is the measured incident in
// docs/bot-runs/feed-watch.md ("rotating refresh tokens make dual-client
// use self-destructive — each refresh invalidates the other holder"), whose
// remediation is stated as "one session, one record, one refresher", and
// the reason pkg/backend/delegate/pi_codex.go goes to such lengths to keep
// a codex refresh "the exception it should be rather than something every
// node does". The single canonical codex refresher is the server-side
// OAuthRefreshWorker, which rotates the record everyone reads from.
//
// Admitting codex here needs a centralised single-flight refresh against
// the canonical record plus distribution of the new blob to live runs —
// tracked as audit row B2 (native:fc0c51d4), not a `case` on this switch.
// Anthropic is safe on the SAME shape only for want of a measured
// counter-example; see the note on refreshAnthropicFile.
func (r *Runner) startOAuthRefreshers(stop <-chan struct{}, runID string, files map[string]string) {
	hc := &http.Client{Timeout: oauthRefreshHTTPTimeout}
	for kind, path := range files {
		if secrets.OAuthKind(kind) != secrets.OAuthKindClaudeCode {
			continue
		}
		errtrack.Go("runner.refreshAnthropicOAuth", func() { r.refreshAnthropicLoop(stop, hc, runID, path) })
	}
}

// refreshAnthropicLoop sleeps until oauthRefreshLead before the materialised
// token's expiry, refreshes + rewrites it, then repeats against the new
// expiry. A token already within the lead window (or past it) is refreshed
// immediately — so this also covers the "materialised a near-expiry token at
// claim time" pre-run case. Best-effort: a hard failure backs off a minute
// and retries; a missing refresh_token ends the loop (nothing we can do).
func (r *Runner) refreshAnthropicLoop(stop <-chan struct{}, hc *http.Client, runID, path string) {
	for {
		exp, refreshTok, err := readAnthropicExpiry(path)
		if err != nil || refreshTok == "" {
			return // file gone / unparseable / no refresh_token — leave it to the CLI
		}
		wait := time.Until(exp.Add(-oauthRefreshLead))
		if wait < 0 {
			wait = 0
		}
		select {
		case <-stop:
			return
		case <-time.After(wait):
		}
		if err := refreshAnthropicFile(hc, path); err != nil {
			if r.cfg.Logger != nil {
				r.cfg.Logger.Warn("runner: oauth-forfait refresh run=%s: %v", runID, err)
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Minute):
			}
			continue
		}
		if r.cfg.Logger != nil {
			r.cfg.Logger.Info("runner: oauth-forfait token refreshed run=%s", runID)
		}
		// Sandboxed runs read a seeded in-container COPY of this file
		// (CLAUDE_CONFIG_DIR) — push the refreshed credentials through the
		// sandbox exec seam so a long-lived in-pod CLI session outliving
		// the access token can still self-refresh (ADR-082 Phase 3
		// blocker 3). No-op without an active real sandbox.
		r.propagateForfaitToSandbox(runID, path)
	}
}

// readAnthropicExpiry returns the access token's expiry + the refresh_token
// from a materialised Claude Code .credentials.json.
func readAnthropicExpiry(path string) (time.Time, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, "", err
	}
	v, err := secrets.ParseAnthropicView(b)
	if err != nil {
		return time.Time{}, "", err
	}
	return time.UnixMilli(v.ClaudeAIOauth.ExpiresAt), v.ClaudeAIOauth.RefreshToken, nil
}

// refreshAnthropicFile exchanges the file's refresh_token for a fresh access
// token and rewrites the .credentials.json in place (0600). The HTTP call is
// bounded by its own timeout context (hc.Timeout also applies).
//
// Note the asymmetry with codex (see startOAuthRefreshers): this writes the
// run-local file only, never the OAuthStore, so IF Anthropic also rotated
// the refresh token on use, the same "one credential, many refreshers"
// hazard would apply to a shared claude_code forfait. Nothing in this tree
// establishes either way — RefreshAnthropic merely tolerates a rotated
// token — and no incident has been observed, so this long-standing
// behaviour is left as it is rather than changed on a guess. It belongs
// with the same audit row (B2 / native:fc0c51d4) as the codex fix.
func refreshAnthropicFile(hc *http.Client, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	v, err := secrets.ParseAnthropicView(b)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), oauthRefreshHTTPTimeout)
	defer cancel()
	res, err := secrets.RefreshAnthropic(ctx, hc, secrets.DefaultAnthropicOAuthClientID, v.ClaudeAIOauth.RefreshToken)
	if err != nil {
		return err
	}
	out, err := secrets.ApplyAnthropicRefresh(b, res)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}
