package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A verdict the forge refuses for a RATE LIMIT is not a verdict lost: the
// forge said when to come back. Dropping it made the run's silence read as a
// death once the limit lifted — the reconciler answered the head with a
// synthetic "review died" failure, and the relaunch lane paid for a second
// review of a revision the first one had already judged (#2002: 23 synthetic
// failures and 17 paid relaunches on 2026-09-30, all on an exhausted App
// installation budget). A failure on the forge's side, or one it never
// answered, is the same: the verdict is computed and only its post failed.
//
// So the publish endpoint keeps such a verdict on the run's grant
// (deferGateVerdict), and the reconciler posts it once the wait is over
// (replayGateDeferral) — and until then answers the run's silence with
// nothing: no synthetic failure, no relaunch, not even a read. A replay is
// one more decision on its head, ordered by the time it was made
// (claimGateDecision): a newer verdict posted meanwhile is never overwritten.

// gateDeferral is one verdict whose post the forge refused for a while: what
// to post, where, when it was decided, when to try again, and how many posts
// it has had.
type gateDeferral struct {
	Gate      publishReviewGate `json:"gate"`
	ReviewURL string            `json:"review_url,omitempty"`
	Repo      string            `json:"repo"`
	Number    int               `json:"number"`
	Decision  gateDecision      `json:"decision"`
	RetryAt   time.Time         `json:"retry_at"`
	// Attempts counts the posts tried, the refused original included.
	Attempts int `json:"attempts"`
}

// gateDeferBackoff is the first wait when the forge named none; it doubles
// with each attempt. gateDeferMaxWait caps every wait — the forge's own reset
// included: a primary REST budget resets within the hour, and a reset named
// further out (a typed answer may carry one up to a month away) is not waited
// out blind past the grant's life.
const (
	gateDeferBackoff = 5 * time.Minute
	gateDeferMaxWait = time.Hour
)

// gateDeferMaxAttempts bounds the posts one verdict gets, the refused original
// included. Past it the run is answered like any run that left no verdict.
const gateDeferMaxAttempts = 12

// gateRetryAt is when to post again a verdict refused for a rate limit or a
// transient failure, at its attempt-th post: when the forge said its wait
// ends, or — when it said nothing, or named an instant already past — a
// backoff doubling with the attempt; never more than gateDeferMaxWait away.
func gateRetryAt(now, resetAt time.Time, attempt int) time.Time {
	if resetAt.After(now) {
		if limit := now.Add(gateDeferMaxWait); resetAt.After(limit) {
			return limit
		}
		return resetAt
	}
	wait := gateDeferBackoff
	for i := 1; i < attempt && wait < gateDeferMaxWait; i++ {
		wait *= 2
	}
	return now.Add(min(wait, gateDeferMaxWait))
}

// noteRateLimit records on out that the forge refused for a rate limit, and
// when it said its wait ends.
func (out *gateOutcome) noteRateLimit(err error) {
	var se *forge.StatusError
	if errors.As(err, &se) && se.RateLimited() {
		out.rateLimited = true
		out.resetAt = se.ResetAt
	}
}

// noteTransient records a refusal worth retrying: the forge failed on its own
// side (5xx), or never answered — the call timed out, was cut, or its caller
// went away. Anything else is an answer that a retry repeats (forbidden, not
// found, a permission the installation withholds, a rejected request).
func (out *gateOutcome) noteTransient(err error) {
	var se *forge.StatusError
	var ne net.Error
	switch {
	case errors.As(err, &se):
		out.transient = se.Upstream5xx()
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled),
		errors.Is(err, io.ErrUnexpectedEOF), errors.As(err, &ne):
		out.transient = true
	}
}

// deferGateVerdict keeps on the grant a verdict whose post the forge refused
// for a rate limit or a transient failure, so the reconciler posts it once the
// wait is over. Only a PINNED verdict is kept: one with no audited revision
// would land, hours later, on whatever head the pull request has then. Being
// the newest decision of its run, it replaces the grant's record of an earlier
// posted verdict — unless a verdict decided later already waits there, which
// makes this one moot. The outcome's error says which.
func (s *Server) deferGateVerdict(token, repo string, number int, gate *publishReviewGate, reviewURL string, decision gateDecision, out *gateOutcome) (owned bool) {
	if out == nil || out.posted || (!out.rateLimited && !out.transient) || gate == nil || s.forgePublishTokens == nil {
		return false
	}
	if !isCommitID(strings.TrimSpace(gate.AuditedSHA)) {
		return false
	}
	retryAt := gateRetryAt(s.gateNow(), out.resetAt, 1)
	d := &gateDeferral{Gate: *gate, ReviewURL: reviewURL, Repo: repo, Number: number, Decision: decision, RetryAt: retryAt, Attempts: 1}
	kept := false
	found, err := s.forgePublishTokens.update(token, func(g *ForgePublishGrant) {
		kept = false // the update may run again on a contended write
		if g.Deferred != nil && g.Deferred.Decision.newerThan(decision) {
			return
		}
		g.Deferred, g.Verdict = d, nil
		kept = true
	})
	switch {
	case err != nil || !found:
		if s.logger != nil {
			s.logger.Warn("forge gate: %s on %s#%d was refused (%s) and could not be deferred (found=%v, %v) — the run will be answered as unanswered",
				gateContextOf(gate), repo, number, out.errText, found, err)
		}
		return false
	case !kept:
		out.errText = "a verdict decided later already waits to be posted: " + out.errText
		return true
	default:
		out.errText = fmt.Sprintf("deferred until %s: %s", retryAt.UTC().Format(time.RFC3339), out.errText)
		return true
	}
}

// gateContextOf is the check a gate request names, defaulted as the publish
// endpoint defaults it.
func gateContextOf(gate *publishReviewGate) string {
	if c := strings.TrimSpace(gate.Context); c != "" {
		return c
	}
	return defaultGateContext
}

// replaceDeferral swaps the grant's deferral for next — nil clears it — only
// while it is still d, at d's attempt: a newer publish may have replaced it, or
// another replica booked the same attempt, and a replay must neither resurrect
// what one cleared nor spend an attempt twice. swapped says whether it did.
func (s *Server) replaceDeferral(token string, d *gateDeferral, next *gateDeferral) (swapped bool, err error) {
	_, err = s.forgePublishTokens.update(token, func(g *ForgePublishGrant) {
		swapped = false // the update may run again on a contended write
		if g.Deferred != nil && g.Deferred.Decision.ID == d.Decision.ID && g.Deferred.Attempts == d.Attempts {
			g.Deferred = next
			swapped = true
		}
	})
	return swapped, err
}

// replayGateDeferral posts the verdict deferred on the grant, the forge's wait
// being over, by the deferral's own terms — its pull request and its pin,
// whatever the run's inputs say: a bot may pin the commit it pushed rather
// than the one it was launched on, or a short form of it.
//
// The attempt is booked on the grant BEFORE the post: a grant that cannot be
// written would otherwise post — and spend the limited budget — on every pass.
//
// It reports done when the reconciler has nothing left to do for the run: the
// run's own verdict landed (recorded and settled like one the endpoint
// posted), a newer verdict owns the check the run owes (settled: that one
// answers it), or the replay waits again — the forge still limiting, or
// failing on its own side. Otherwise the run is handed back to the ordinary
// repair: a replay refused for good (the head moved, the pull request closed),
// one whose attempts are spent, or a verdict that answers another run's check
// on a shared grant.
func (s *Server) replayGateDeferral(ctx context.Context, run *store.Run, token string, grant ForgePublishGrant, d *gateDeferral) bool {
	if d.Attempts >= gateDeferMaxAttempts {
		if _, err := s.replaceDeferral(token, d, nil); err != nil {
			s.logWarn("forge gate: run %s's deferred verdict could not be cleared: %v", run.ID, err)
		}
		s.logWarn("forge gate: run %s's deferred verdict was refused %d time(s) — answering the run as unanswered", run.ID, d.Attempts)
		s.retireDeferredGrant(run, token)
		return false
	}
	next := *d
	next.Attempts++
	next.RetryAt = gateRetryAt(s.gateNow(), time.Time{}, next.Attempts)
	switch swapped, err := s.replaceDeferral(token, d, &next); {
	case err != nil:
		s.logWarn("forge gate: run %s's deferred verdict could not book its replay (%v) — not posting until it can", run.ID, err)
		return true
	case !swapped:
		return true // replaced or booked meanwhile: the next pass reads what is there now
	}

	var gate gateOutcome
	conn, err := s.forgeConnections.Get(store.WithoutTenantFilter(ctx), grant.ConnectionID)
	switch {
	case err != nil:
		gate = gateOutcome{errText: "connection " + grant.ConnectionID + " unreadable: " + err.Error(), transient: !errors.Is(err, forge.ErrConnectionNotFound)}
	case conn.TenantID != grant.TeamID || !strings.EqualFold(strings.TrimSpace(d.Repo), strings.TrimSpace(grant.Repo)):
		gate = gateOutcome{errText: "the deferral no longer matches its grant's tenant and repo"}
	default:
		gate = s.postGateStatus(ctx, conn, d.Repo, d.Number, &d.Gate, d.ReviewURL, d.Decision)
	}
	switch {
	case gate.posted:
		s.recordGateVerdict(token, gate, d.Decision)
		if s.logger != nil {
			s.logger.Info("forge gate: run %s's deferred verdict posted on %s#%d@%s → %s (post %d)",
				run.ID, d.Repo, d.Number, shortSHA(gate.sha), gate.state, next.Attempts)
		}
		// Settled only when it answers THIS run's owed check — its head, its
		// check, on a grant no second run publishes with (the attribution rule
		// settleOwnVerdict trusts). A shared grant's deferral may be another
		// run's verdict, and this run's own answer is then still owed.
		if verdictAnswersRun(run, grant, gate.sha, gate.context) {
			s.settleOwnVerdict(ctx, run, token, grant, &gateVerdict{SHA: gate.sha, Context: gate.context, State: gate.state})
			return true
		}
		s.retireDeferredGrant(run, token)
		return false
	case gate.rateLimited || gate.transient:
		if gate.resetAt.After(s.gateNow()) {
			rearmed := next
			rearmed.RetryAt = gateRetryAt(s.gateNow(), gate.resetAt, next.Attempts)
			if _, err := s.replaceDeferral(token, &next, &rearmed); err != nil {
				s.logWarn("forge gate: run %s's deferred verdict keeps its backoff, the forge's reset could not be recorded: %v", run.ID, err)
			}
		}
		return true
	case gate.superseded && verdictAnswersRun(run, grant, gate.sha, gate.context):
		// A verdict decided later owns the check this run owes: the head's
		// answer is that one's, posted or waiting to be, and nothing is left
		// for this run — the ordinary repair would read a newer review's
		// silence as this run's death.
		if _, err := s.replaceDeferral(token, &next, nil); err != nil {
			s.logWarn("forge gate: run %s's deferred verdict could not be cleared: %v", run.ID, err)
		}
		s.settleGateRun(run, gateSettledSuperseded, gate.sha)
		s.cutBackGrant(run, token)
		if s.logger != nil {
			s.logger.Info("forge gate: run %s's deferred verdict on %s#%d@%s is superseded by a newer one — not posted", run.ID, d.Repo, d.Number, shortSHA(gate.sha))
		}
		return true
	}
	if _, err := s.replaceDeferral(token, &next, nil); err != nil {
		s.logWarn("forge gate: run %s's deferred verdict could not be cleared: %v", run.ID, err)
	}
	s.logWarn("forge gate: run %s's deferred verdict was not posted (%s) — answering the run as unanswered", run.ID, gate.errText)
	s.retireDeferredGrant(run, token)
	return false
}

// retireDeferredGrant brings the grant of a run that owes no gate repair down
// to the ordinary post-run grace once its deferral is gone: the grant reaper
// declined to while the deferral stood, and nothing else revisits it.
func (s *Server) retireDeferredGrant(run *store.Run, token string) {
	if !runAwaitsGateRepair(run) {
		s.cutBackGrant(run, token)
	}
}
