package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// OAuthRefreshWorker proactively rotates OAuth-forfait access tokens
// before they expire, so neither an interactive run nor an automated
// (webhook/dispatcher/cron) run ever reads a stale credential. This is
// the analogue of forge.RefreshWorker for the forfait store.
//
// It covers BOTH personal and org-scoped records uniformly: org records
// are ordinary OAuthRecords keyed under OrgOwnerKey(tenant), so they
// surface from ExpiringBefore like any other — and the org credential is
// exactly the one that powers 24/7 automation, so keeping it fresh is the
// whole point.
type OAuthRefreshWorker struct {
	Store             OAuthStore
	Sealer            Sealer
	HTTP              *http.Client
	AnthropicClientID string
	CodexClientID     string
	// Lead is how far ahead of expiry a record is refreshed (a record
	// expiring within Lead is rotated now). Defaults to 30m.
	Lead time.Duration
}

// RunOnce refreshes every record expiring within Lead. It is best-effort:
// a single record's failure (provider rejection, missing client id) is
// logged via the returned error aggregate but does not abort the sweep.
// Returns the number of records successfully refreshed.
func (w *OAuthRefreshWorker) RunOnce(ctx context.Context) (int, error) {
	if w.Store == nil || w.Sealer == nil {
		return 0, nil
	}
	lead := w.Lead
	if lead <= 0 {
		lead = 30 * time.Minute
	}
	cutoff := time.Now().Add(lead).UTC()
	recs, err := w.Store.ExpiringBefore(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("secrets: oauth refresh sweep: %w", err)
	}
	var (
		refreshed int
		firstErr  error
		failures  int
	)
	for i := range recs {
		rec := recs[i]
		// Skip kinds we have no client id for — leave the record to
		// the manual refresh / re-connect path rather than erroring.
		if rec.Kind == OAuthKindClaudeCode && w.AnthropicClientID == "" {
			continue
		}
		// Codex is NOT skipped for a missing configured id: the
		// credential names its own client, so RefreshRecord derives one.
		// Skipping here used to be silent, and its cost was measured — a
		// forfait went unrefreshed for ten days on a deployment that had
		// simply never set the id, and the only symptom was a run
		// failing its first LLM call with "authentication token is
		// expired", far from the cause.
		//
		// This worker is the SINGLE canonical refresher of a codex
		// credential, and deliberately so: the blob is shared (the
		// platform record is one meter for the whole deployment), OpenAI
		// rotates the refresh token on use, and a second refresher
		// therefore invalidates this one's token. That is why the runner's
		// per-run loop refuses the kind (see runner.startOAuthRefreshers)
		// and why nothing else here may start rotating it.
		//
		// Removing the skip was necessary but NOT sufficient: this loop
		// only ever sees what ExpiringBefore returns, which requires
		// access_token_expires_at to exist. Stamping it from the access
		// token's own `exp` claim — at connect and after each refresh — is
		// what actually puts a codex record in front of this code.
		//
		// Still open (audit row B2 / native:fc0c51d4): a sandboxed codex
		// reader is permitted to rotate its own writable copy in place, so
		// a long run remains a second holder this worker cannot see.
		// A payload without a refresh token can never be refreshed; only
		// a re-connect renews it. Not a failure — skipping keeps the sweep
		// quiet instead of erroring on the same record every cycle.
		if rec.NotRefreshable {
			continue
		}
		if err := RefreshRecord(ctx, w.Sealer, w.HTTP, w.AnthropicClientID, w.CodexClientID, &rec); err != nil {
			if errors.Is(err, ErrNotRefreshable) {
				// Self-heal legacy records sealed before NotRefreshable
				// existed so future sweeps skip them without decrypting.
				// A partial write: the flag is the only thing learned here,
				// and the record read a round trip ago may already be stale.
				if uerr := w.Store.UpdateTokens(ctx, rec.UserID, rec.Kind, OAuthTokenUpdate{NotRefreshable: true}); uerr != nil {
					failures++
					if firstErr == nil {
						firstErr = fmt.Errorf("mark not-refreshable %s/%s: %w", rec.UserID, rec.Kind, uerr)
					}
				}
				continue
			}
			failures++
			if firstErr == nil {
				firstErr = fmt.Errorf("refresh %s/%s: %w", rec.UserID, rec.Kind, err)
			}
			continue
		}
		if err := w.Store.UpdateTokens(ctx, rec.UserID, rec.Kind, OAuthTokenUpdateFrom(rec)); err != nil {
			failures++
			if firstErr == nil {
				firstErr = fmt.Errorf("persist %s/%s: %w", rec.UserID, rec.Kind, err)
			}
			continue
		}
		refreshed++
	}
	if failures > 0 {
		return refreshed, fmt.Errorf("secrets: oauth refresh: %d/%d failed: %w", failures, len(recs), firstErr)
	}
	return refreshed, nil
}
