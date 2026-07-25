package runner

import (
	"context"
	"net/http"
	"os"
	"time"

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
// Claude Code (Anthropic) forfait is handled — it is the one with a known
// public OAuth client id; codex files are left to the CLI / store worker.
func (r *Runner) startOAuthRefreshers(stop <-chan struct{}, runID string, files map[string]string) {
	hc := &http.Client{Timeout: oauthRefreshHTTPTimeout}
	for kind, path := range files {
		if secrets.OAuthKind(kind) != secrets.OAuthKindClaudeCode {
			continue
		}
		go r.refreshAnthropicLoop(stop, hc, runID, path)
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
