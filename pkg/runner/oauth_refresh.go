package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// oauthFollowInterval paces the runner's re-read of the store record a run's
// forfait was sealed from. The server rotates a record inside the half hour
// before its expiry (OAuthRefreshWorker), and the token a live CLI holds is
// revoked the moment it does: the materialised file must carry the new one
// within a minute, for the next spawn and for the delegate's retry.
const oauthFollowInterval = time.Minute

// startOAuthRefreshers keeps each materialised OAuth-forfait credential file
// valid for the LIFETIME of the run.
//
// ONE RECORD, ONE REFRESHER. A forfait is a store record the server's
// OAuthRefreshWorker rotates, and a refresh-token exchange revokes the token
// every other holder of the record still uses — measured on a claude_code team
// record: a live delegate refused "OAuth access token has been revoked" 16 s
// after the worker rotated it. So a run whose slot names its record
// (RunBundle.OAuthRecordRefs) never exchanges anything: it FOLLOWS the record
// and rewrites the file with each rotation (followOAuthRecordLoop).
//
// A slot that names no record (a bundle sealed by an older server), or a
// runner with no store, keeps the claude_code self-refresh
// (refreshAnthropicLoop), and says so. Codex is not refreshed at all: its CLI
// rotates its own refresh token, and admitting it needs a write-through of the
// followed record into the sandbox's codex home, which does not exist yet.
func (r *Runner) startOAuthRefreshers(stop <-chan struct{}, runID string, files, refs, fingerprints map[string]string, lent map[string]bool) {
	hc := &http.Client{Timeout: oauthRefreshHTTPTimeout}
	for kind, path := range files {
		if secrets.OAuthKind(kind) != secrets.OAuthKindClaudeCode {
			continue
		}
		fp := fingerprints[kind]
		// A lent slot is held to the subscription that was lent, which it can
		// only recognise by its fingerprint: without one it keeps the
		// self-refresh it always had.
		if ref := refs[kind]; ref != "" && r.cfg.OAuthForfaits != nil && r.cfg.Sealer != nil && (!lent[kind] || fp != "") {
			heldTo := ""
			if lent[kind] {
				heldTo = fp
			}
			// A bundle can wait in the queue past a rotation: catch up
			// before the run's first spawn reads the file.
			if !r.followOAuthRecord(runID, secrets.OAuthKind(kind), ref, fp, heldTo, path) {
				continue
			}
			errtrack.Go("runner.followOAuthRecord", func() {
				r.followOAuthRecordLoop(stop, runID, secrets.OAuthKind(kind), ref, fp, heldTo, path, oauthFollowInterval)
			})
			continue
		}
		if r.cfg.Logger != nil {
			r.cfg.Logger.Info("runner: oauth-forfait run=%s kind=%s has no store record to follow — refreshing its own copy", runID, kind)
		}
		errtrack.Go("runner.refreshAnthropicOAuth", func() { r.refreshAnthropicLoop(stop, hc, runID, path) })
	}
}

// followOAuthRecordLoop keeps the materialised credentials file equal to the
// store record the run was sealed from, re-reading it every `every`, and
// writes each change through into the run's sandbox. It never exchanges a
// token, and it ends when a lent slot's record no longer names the lent
// subscription.
func (r *Runner) followOAuthRecordLoop(stop <-chan struct{}, runID string, kind secrets.OAuthKind, ref, fp, heldTo, path string, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		if !r.followOAuthRecord(runID, kind, ref, fp, heldTo, path) {
			return
		}
	}
}

// errNotTheLentCredential ends the follow of a lent slot whose donor record
// no longer names the subscription that was lent.
var errNotTheLentCredential = errors.New("the donor's record no longer names the subscription that was lent")

// followOAuthRecord runs one follow pass and reports it: a refusal or a store
// error is logged, a change is written through into the run's sandbox. It
// returns false when the follow must end: a lent slot whose donor record now
// names another subscription keeps the token it holds — in-flight runs
// finish on the credential they were granted, never on one the donor
// connects after.
func (r *Runner) followOAuthRecord(runID string, kind secrets.OAuthKind, ref, fp, heldTo, path string) bool {
	changed, fpNow, err := r.followOAuthRecordOnce(kind, ref, heldTo, path)
	if err != nil {
		if r.cfg.Logger != nil {
			r.cfg.Logger.Warn("runner: oauth-forfait follow run=%s kind=%s record=%s: %v", runID, kind, ref, err)
		}
		return !errors.Is(err, errNotTheLentCredential)
	}
	if !changed {
		return true
	}
	if r.cfg.Logger != nil {
		r.cfg.Logger.Info("runner: oauth-forfait run=%s picked up the store's rotation of %s", runID, kind)
		if fpNow != fp {
			r.cfg.Logger.Info("runner: oauth-forfait run=%s kind=%s record=%s now carries fingerprint %q (sealed %q): a refresh re-stamp or a reconnect of the slot — following it; this run's usage readings stay filed under the sealed one",
				runID, kind, ref, fpNow, fp)
		}
	}
	r.propagateForfaitToSandbox(runID, path)
	return true
}

// followOAuthRecordOnce re-reads the record once and, when its payload moved,
// atomically rewrites the file; it returns the record's fingerprint. A record
// that is no longer the same kind is refused. Its fingerprint is not held to
// the sealed one: it is a meter key the refresh worker re-stamps on its own
// rotations (an unstamped record stamped, a subscription identified as an
// account, an account demoted to a local meter), and a reconnect of the slot
// is its owner's choice of credential for it. The id pins the owner and the
// rank, so the run follows that one slot — except a lent one: heldTo, when
// set, is the fingerprint of the subscription that was lent, and a record
// that names another is refused (errNotTheLentCredential).
func (r *Runner) followOAuthRecordOnce(kind secrets.OAuthKind, ref, heldTo, path string) (bool, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rec, err := r.cfg.OAuthForfaits.GetByID(ctx, ref)
	if err != nil {
		return false, "", err
	}
	if rec.Kind != kind {
		return false, "", fmt.Errorf("record is a %s forfait, the run was sealed with a %s one", rec.Kind, kind)
	}
	if heldTo != "" && rec.Fingerprint != heldTo {
		return false, rec.Fingerprint, fmt.Errorf("%w (fingerprint %q, lent %q): the run keeps the token it holds", errNotTheLentCredential, rec.Fingerprint, heldTo)
	}
	payload, err := secrets.OpenOAuthPayload(r.cfg.Sealer, rec.UserID, rec.Kind, rec.SealedPayload)
	if err != nil {
		return false, "", fmt.Errorf("unseal: %w", err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return false, "", err
	}
	if bytes.Equal(current, payload) {
		return false, rec.Fingerprint, nil
	}
	// Atomic replace: a CLI spawning now never reads a torn file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return false, "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, "", err
	}
	return true, rec.Fingerprint, nil
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
// It is the fallback for a slot that names no store record (see
// startOAuthRefreshers): it writes the run-local file only, and its exchange
// revokes the token any other holder of the same grant still uses.
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
