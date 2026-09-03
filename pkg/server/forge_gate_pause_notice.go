package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A review that parks on a provider quota is NOT dead: the runner armed a
// durable retry, and the gate reconciler next door deliberately leaves such
// a run alone — the armed retry is the promise. But nothing said so ON THE
// PULL REQUEST. The required check sits on the in-flight claim ("review in
// progress"), which is exactly what it says an hour before the retry fires
// and exactly what it says if nothing ever comes back: the developer cannot
// tell a review that will resume from one that died, and has no idea how
// long to wait.
//
// So the park speaks, once, where the developer is looking. The comment
// carries the three facts nothing else supplies: that the pause is a quota
// (not a bug in their branch), WHEN the retry fires, and whether waiting is
// actually enough — an account SPEND ceiling reopens when an admin raises
// it, not on a schedule, so the armed retry may exhaust its attempts
// against a wall.
//
// Fired from the run-outcome EVENT only, never the sweep: the sweep
// re-offers the same run every minute for an hour, and this posts a
// comment. One park = one outcome event = one comment; a second park
// (the retry hit the same wall) legitimately produces a second one.

// gatePauseNoticeMarker is the invisible provenance tag on the comment.
// A future reader (or a de-dup pass) can find iterion's own pause notices
// without parsing prose.
const gatePauseNoticeMarker = "<!-- iterion:gate-paused -->"

// noticeGatePausedForRetry posts the pause notice on the PR a parked run
// gates. Best-effort in every branch: the run's retry is already armed and
// durable, so a comment that cannot be posted must never turn into an
// error the caller has to handle. Silent (Debug) on every miss — a run
// that gates nothing is the common case, not an anomaly.
func (s *Server) noticeGatePausedForRetry(ctx context.Context, run *store.Run) {
	if s == nil || run == nil || run.RetryState == nil || run.RetryState.RetryAfter == nil {
		return
	}
	if s.forgePublishTokens == nil || s.forgeConnections == nil {
		return
	}
	prURL := strings.TrimSpace(runInputString(run, "pr_url"))
	token := strings.TrimSpace(runInputString(run, forgePublishVarToken))
	if prURL == "" || token == "" {
		return // not a gating run: nothing to tell anyone
	}
	debugf := func(format string, args ...any) {
		if s.logger != nil {
			s.logger.Debug("forge gate: pause notice for run %s on %s not posted: "+format,
				append([]any{run.ID, prURL}, args...)...)
		}
	}
	grant, ok := s.forgePublishTokens.lookup(token)
	if !ok {
		debugf("its publish grant is expired or revoked")
		return
	}
	host, repo, number, err := forge.ParsePullURL(prURL)
	if err != nil {
		debugf("its pr_url does not parse: %v", err)
		return
	}
	conn, err := s.forgeConnections.Get(store.WithoutTenantFilter(ctx), grant.ConnectionID)
	if err != nil {
		debugf("its connection %s is unreadable: %v", grant.ConnectionID, err)
		return
	}
	// The same scope re-enforcement the publish path and the reconciler
	// apply: pr_url is a launch var, so the grant's tenant and host are
	// what bound where this token may speak.
	if conn.TenantID != grant.TeamID {
		debugf("its connection %s belongs to another tenant", grant.ConnectionID)
		return
	}
	if connHost := hostOfURL(conn.BaseURL()); connHost == "" || !strings.EqualFold(connHost, host) {
		debugf("its connection points at %q, not %q", hostOfURL(conn.BaseURL()), host)
		return
	}
	commenter, err := s.issueCommenterFor(ctx, conn)
	if err != nil || commenter == nil {
		debugf("no comment client for %s: %v", conn.Provider, err)
		return
	}
	body := gatePauseNoticeBody(run, time.Now().UTC())
	if _, err := commenter.CommentIssue(ctx, repo, number, body); err != nil {
		debugf("%v", err)
		return
	}
	if s.logger != nil {
		s.logger.Info("forge gate: run %s parked on a provider quota — pause notice posted on %s (retry at %s)",
			run.ID, prURL, run.RetryState.RetryAfter.Format(time.RFC3339))
	}
}

// forgeIssueCommenter is the ONE method this notice needs. Narrow on
// purpose (the same discipline as forgeGateClient): forge.IssueClient
// bundles comments with list/create/update, and a provider adapter that
// only knows how to comment must still be able to carry the notice.
type forgeIssueCommenter interface {
	CommentIssue(ctx context.Context, repo string, number int, body string) (forge.CommentRef, error)
}

// issueCommenterFor resolves the connection's comment capability, mirroring
// gateClientFor: a provider that cannot comment yields (nil, nil) rather
// than an error, because "this forge has no comment API wired" is not a
// failure of the run.
func (s *Server) issueCommenterFor(ctx context.Context, conn forge.Connection) (forgeIssueCommenter, error) {
	if s.forgeIssueCommenterFor != nil {
		return s.forgeIssueCommenterFor(ctx, conn)
	}
	admin, err := s.forgeAdminFor(ctx, conn)
	if err != nil {
		return nil, err
	}
	c, ok := admin.(forgeIssueCommenter)
	if !ok {
		return nil, nil
	}
	return c, nil
}

// gatePauseNoticeBody renders the comment. Written for the developer whose
// PR it lands on, not for an operator: what happened, when it resumes, and
// whether waiting suffices.
func gatePauseNoticeBody(run *store.Run, now time.Time) string {
	at := run.RetryState.RetryAfter.UTC()
	var b strings.Builder
	b.WriteString(gatePauseNoticeMarker)
	b.WriteString("\n⏸️ **Review paused — the LLM provider's quota is exhausted.** Nothing is wrong with this pull request.\n\n")
	fmt.Fprintf(&b, "iterion parked the review and will resume it **automatically at %s**%s",
		at.Format("15:04 UTC on 2006-01-02"), humanizeIn(at, now))
	if run.RetryState.Attempts > 0 {
		fmt.Fprintf(&b, " — attempt %d", run.RetryState.Attempts)
	}
	b.WriteString(". The verdict lands here when it does; a new push restarts it sooner.\n")
	if cause := gatePauseCause(run); cause != "" {
		fmt.Fprintf(&b, "\n> %s\n", cause)
		if isSpendCeilingCause(cause) {
			b.WriteString("\n**Waiting may not be enough here**: a spend ceiling reopens when an\n" +
				"account admin raises it (or when the month rolls over), not on a\n" +
				"provider window schedule.\n")
		}
	}
	return b.String()
}

// gatePauseCause extracts the provider's own sentence from the run error —
// the part that tells a reader WHICH wall was hit. Bounded: the run error
// can carry wrappers and a stack of context, and a comment is not a log.
func gatePauseCause(run *store.Run) string {
	msg := strings.TrimSpace(run.Error)
	if msg == "" {
		return ""
	}
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = strings.TrimSpace(msg[:i])
	}
	const maxCause = 300
	if len(msg) > maxCause {
		msg = strings.TrimSpace(msg[:maxCause]) + "…"
	}
	return msg
}

// isSpendCeilingCause reports whether the parked cause is an account SPEND
// ceiling rather than a time window. The distinction is the only part of
// the notice a developer can act on (ask an admin) versus wait out.
func isSpendCeilingCause(cause string) bool {
	return strings.Contains(strings.ToLower(cause), "spend limit")
}

// humanizeIn renders the wait as a parenthetical so the reader does not
// have to subtract clocks. Empty when the instant is already past (a retry
// eligible now, waiting on a runner slot).
func humanizeIn(at, now time.Time) string {
	d := at.Sub(now)
	if d <= 0 {
		return ""
	}
	switch {
	case d < time.Minute:
		return " (in under a minute)"
	case d < time.Hour:
		return fmt.Sprintf(" (in ~%d min)", int(d.Round(time.Minute).Minutes()))
	default:
		return fmt.Sprintf(" (in ~%.1f h)", d.Hours())
	}
}
