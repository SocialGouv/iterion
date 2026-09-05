package server

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// An unattended gate lane — the dead-gate relaunch, the auto-fix pass — may
// retry a launch that failed to START under its (PR, head, bot) claim key: a
// deploy window, a queue blip, a bot resolution that heals. It may not retry
// it forever. A launch_error row is retryable by design (a forge redelivery
// after a transient failure must still launch), and the per-head claim only
// binds once a launch SUCCEEDS — so on 2026-08-26 a lane re-attempted the
// same deterministic failure on every sweep tick for ~90 minutes, one
// launch_error delivery (and, at the time, one orphan queued run) per tick.
//
// The budget lives on the delivery row the claim key already names
// (Delivery.Attempts / Delivery.FailedAt, stamped by the shared launch tail),
// so every replica reads the same count and nothing new has to be elected.
// Human-driven redeliveries are NOT budgeted: an operator retrying after a
// fix must be able to.
const (
	// maxUnattendedLaunchAttempts is how many times a lane tries a launch
	// under one claim key before it escalates and stops.
	maxUnattendedLaunchAttempts = 3
	// unattendedLaunchBackoffBase is the wait after the first failure; it
	// doubles per failure (5m, then 10m), so three attempts span a quarter
	// of an hour — long enough to outlast a rolling deploy, short enough to
	// fit inside the gate sweep's 60-minute lookback that carries the
	// re-offers.
	unattendedLaunchBackoffBase = 5 * time.Minute
)

type launchRetryVerdict int

const (
	// launchRetryFresh: no launch_error row under the key — a first attempt,
	// or a settled (launched) claim the caller handles as it always did.
	launchRetryFresh launchRetryVerdict = iota
	// launchRetryDue: a prior failure whose backoff has elapsed — try again.
	launchRetryDue
	// launchRetryWait: a prior failure still inside its backoff — do nothing.
	launchRetryWait
	// launchRetryExhausted: the budget is spent — escalate, then stay quiet.
	launchRetryExhausted
)

func unattendedLaunchBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	return unattendedLaunchBackoffBase << (attempts - 1)
}

// unattendedLaunchRetry decides what a lane may do with the claim row under
// its key at `now`. Only launch_error rows are budgeted.
func unattendedLaunchRetry(prior webhooks.Delivery, found bool, now time.Time) (launchRetryVerdict, time.Duration) {
	if !found || prior.Status != webhooks.StatusLaunchError {
		return launchRetryFresh, 0
	}
	if prior.Attempts >= maxUnattendedLaunchAttempts {
		return launchRetryExhausted, 0
	}
	if prior.FailedAt == nil {
		return launchRetryDue, 0 // a row written before the stamp existed
	}
	if remaining := prior.FailedAt.Add(unattendedLaunchBackoff(prior.Attempts)).Sub(now); remaining > 0 {
		return launchRetryWait, remaining
	}
	return launchRetryDue, 0
}

// unattendedLaunchVerdict reads the claim row under key and applies the
// budget. One indexed store read and no forge traffic — it runs on every
// sweep offer of a dead run, ~once a minute for an hour, on every replica.
func (s *Server) unattendedLaunchVerdict(ctx context.Context, key string) (prior webhooks.Delivery, found bool, verdict launchRetryVerdict, wait time.Duration) {
	if s == nil || s.webhookDeliveries == nil || key == "" {
		return webhooks.Delivery{}, false, launchRetryFresh, 0
	}
	d, err := s.webhookDeliveries.GetByIdempotencyKey(store.WithoutTenantFilter(ctx), key)
	found = err == nil
	verdict, wait = unattendedLaunchRetry(d, found, s.gateNow())
	return d, found, verdict, wait
}

// gateEscalation is one "automation is out of moves" notice for a (PR, head):
// a board card plus the same text as a PR comment. The card id is
// deterministic (UUIDv5 of lane/team/repo/PR/head): the gate sweep runs
// unelected on every replica, and two replicas racing past a List-based dedup
// would each file the card AND each post the comment — the store's unique-id
// insert is the only primitive here that serialises cross-replica. The dedup
// label stays on the card for querying.
type gateEscalation struct {
	teamID  string
	conn    forge.Connection
	repo    string
	number  int
	headSHA string
	// lane is the dedup namespace ("gate-dead", "gate-autofix"); label is
	// the query label its cards carry.
	lane  string
	label string
	title string
	body  string
}

// fileGateEscalation files the card and, only when THIS call created it,
// posts the PR comment — the card dedup is what bounds the comment to once
// per (PR, head). Best-effort by construction: the PR already carries the
// status that made the lane act, so a board that cannot be written costs
// visibility, never correctness. Returns whether this call filed the card.
func (s *Server) fileGateEscalation(ctx context.Context, e gateEscalation) bool {
	if s.cfg.CloudBoardFor == nil {
		s.logWarn("%s: no board on this deployment — %s#%d gets no card; the PR status carries what there is", e.lane, e.repo, e.number)
		return false
	}
	board := s.cfg.CloudBoardFor(e.teamID)
	if board == nil {
		s.logWarn("%s: no board for team %s — %s#%d gets no card", e.lane, e.teamID, e.repo, e.number)
		return false
	}
	dedup := fmt.Sprintf("%s:%s#%d@%s", e.lane, e.repo, e.number, shortSHA(e.headSHA))
	if existing, err := board.List(native.ListFilter{Labels: []string{dedup}}); err == nil && len(existing) > 0 {
		return false
	}
	cardID := "native:" + uuid.NewSHA1(uuid.NameSpaceURL, []byte("iterion://"+e.lane+"/"+e.teamID+"/"+dedup)).String()
	if _, err := board.Create(native.Issue{
		ID:     cardID,
		Title:  truncate(e.title, 120),
		Body:   e.body,
		Labels: []string{e.label, dedup},
	}); err != nil {
		// A create refused on the deterministic id means another replica won
		// the race a moment ago — the escalation exists, nothing to add. Any
		// other failure is a real board problem: say so.
		if existing, lerr := board.List(native.ListFilter{Labels: []string{dedup}}); lerr == nil && len(existing) > 0 {
			return false
		}
		s.logWarn("%s: board card create failed for %s#%d: %v", e.lane, e.repo, e.number, err)
		return false
	}
	if s.logger != nil {
		s.logger.Info("%s: board card filed for %s#%d@%s", e.lane, e.repo, e.number, shortSHA(e.headSHA))
	}
	s.commentGateEscalationOnPR(ctx, e)
	return true
}

// commentGateEscalationOnPR posts the escalation on the pull request itself —
// the one surface the PR's audience is guaranteed to see. The commit status
// only offers 140 characters and the board card lives on the integration's
// team board, which the people waiting on THIS merge may never open (a card
// alone sat unseen for 7 days while a security PR stayed blocked).
func (s *Server) commentGateEscalationOnPR(ctx context.Context, e gateEscalation) {
	rc, err := s.reviewClientFor(ctx, e.conn)
	if err != nil {
		s.logWarn("%s: no review client for %s (%v) — escalation stays board-only", e.lane, e.repo, err)
		return
	}
	if rc == nil {
		s.logWarn("%s: provider %s cannot post PR comments — escalation stays board-only", e.lane, e.conn.Provider)
		return
	}
	if _, err := rc.CreatePullReview(ctx, e.repo, e.number, forge.NewReview{Body: e.body}); err != nil {
		s.logWarn("%s: escalation comment on %s#%d failed: %v", e.lane, e.repo, e.number, err)
		return
	}
	if s.logger != nil {
		s.logger.Info("%s: escalation comment posted on %s#%d", e.lane, e.repo, e.number)
	}
}
