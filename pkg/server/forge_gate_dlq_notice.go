package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A run parked on the dead-letter queue is not paused: the queue exhausted
// its deliveries and nothing on the platform wakes it. Its gate is repaired
// by the reconciler like any dead review (a synthetic failure on the head,
// one relaunch per head), and the developer reading the PR has to be told
// that — not "resumes automatically at 21:07", which is what a park keyed on
// a leftover retry_after used to say (#669 part 4).
//
// Keyed on the persisted FailureCode alone, never on RetryState: the code is
// the typed WHY the two DLQ writers stamp, and a retry_after has no bearing
// on whether a parked message comes back.

// gateDLQNoticeMarker tags the comment so a reader (or a de-dup pass) can
// find iterion's own DLQ notices without parsing prose. Distinct from the
// pause marker: the two say opposite things about what happens next.
const gateDLQNoticeMarker = "<!-- iterion:gate-dlq-parked -->"

// noticeGateDLQParked posts the DLQ notice on the PR a parked run gates.
// Best-effort in every branch, like the pause notice: the repair the
// reconciler performs next does not depend on the comment landing, so a
// comment that cannot be posted never becomes an error. Silent (Debug) on
// every miss — a run that gates nothing is the common case.
func (s *Server) noticeGateDLQParked(ctx context.Context, run *store.Run) {
	if s == nil || run == nil || run.FailureCode != store.FailureDLQParked {
		return
	}
	target, ok := s.gateNoticeTarget(ctx, run, "DLQ notice")
	if !ok {
		return
	}
	body := gateDLQNoticeBody(run)
	if _, err := target.commenter.CommentIssue(ctx, target.repo, target.number, body); err != nil {
		s.gateNoticeDebug(run, "DLQ notice", "%v", err)
		return
	}
	if s.logger != nil {
		s.logger.Info("forge gate: run %s parked on the DLQ — operator notice posted on %s (the reconciler repairs the check; replay or discard via iterion remote admin dlq)",
			run.ID, target.prURL)
	}
}

// gateNoticeTarget resolves where a gate notice about run may be posted and
// who posts it: the PR named by pr_url, through the connection the run's
// publish grant names — with the grant's scope re-enforced (repo, tenant,
// host) exactly as the publish endpoint and the reconciler enforce it,
// because pr_url is a LAUNCH VAR and a grant for repo A must not let
// iterion's forge identity comment on any repo B the connection reaches.
// A run holding no grant, owing no verdict (no gate_context, or the gate
// pinned off), or whose grant/connection/forge cannot be resolved yields
// ok=false, each miss logged at Debug with its reason.
type gateNoticeTarget struct {
	commenter forgeIssueCommenter
	repo      string
	number    int
	prURL     string
}

func (s *Server) gateNoticeTarget(ctx context.Context, run *store.Run, what string) (gateNoticeTarget, bool) {
	if s.forgePublishTokens == nil || s.forgeConnections == nil {
		return gateNoticeTarget{}, false
	}
	prURL := strings.TrimSpace(runInputString(run, "pr_url"))
	token := strings.TrimSpace(runInputString(run, forgePublishVarToken))
	if prURL == "" || token == "" {
		return gateNoticeTarget{}, false // holds no publish grant: nothing to tell anyone
	}
	// Holding a grant is NOT owing a verdict — the server mints one for ANY
	// bot launched with a pr_url. Same two conditions the reconciler uses.
	if strings.TrimSpace(runInputString(run, "gate_context")) == "" || runGateDisabled(run) {
		return gateNoticeTarget{}, false
	}
	grant, ok := s.forgePublishTokens.lookup(token)
	if !ok {
		s.gateNoticeDebug(run, what, "its publish grant is expired or revoked")
		return gateNoticeTarget{}, false
	}
	host, repo, number, err := forge.ParsePullURL(prURL)
	if err != nil {
		s.gateNoticeDebug(run, what, "its pr_url does not parse: %v", err)
		return gateNoticeTarget{}, false
	}
	if !strings.EqualFold(strings.TrimSpace(repo), strings.TrimSpace(grant.Repo)) {
		s.gateNoticeDebug(run, what, "its grant covers %s — refusing to comment outside the grant's repo", grant.Repo)
		return gateNoticeTarget{}, false
	}
	conn, err := s.forgeConnections.Get(store.WithoutTenantFilter(ctx), grant.ConnectionID)
	if err != nil {
		s.gateNoticeDebug(run, what, "its connection %s is unreadable: %v", grant.ConnectionID, err)
		return gateNoticeTarget{}, false
	}
	if conn.TenantID != grant.TeamID {
		s.gateNoticeDebug(run, what, "its connection %s belongs to another tenant", grant.ConnectionID)
		return gateNoticeTarget{}, false
	}
	if connHost := hostOfURL(conn.BaseURL()); connHost == "" || !strings.EqualFold(connHost, host) {
		s.gateNoticeDebug(run, what, "its connection points at %q, not %q", hostOfURL(conn.BaseURL()), host)
		return gateNoticeTarget{}, false
	}
	commenter, err := s.issueCommenterFor(ctx, conn)
	if err != nil || commenter == nil {
		s.gateNoticeDebug(run, what, "no comment client for %s: %v", conn.Provider, err)
		return gateNoticeTarget{}, false
	}
	return gateNoticeTarget{commenter: commenter, repo: repo, number: number, prURL: prURL}, true
}

func (s *Server) gateNoticeDebug(run *store.Run, what, format string, args ...any) {
	if s.logger == nil {
		return
	}
	s.logger.Debug("forge gate: %s for run %s on %s not posted: "+format,
		append([]any{what, run.ID, runInputString(run, "pr_url")}, args...)...)
}

// gateDLQNoticeBody renders the comment for the developer whose PR it lands
// on: what happened, that waiting does nothing, and what iterion does next
// versus what needs a human. Role-neutral on purpose — the required check
// the run owed is what makes it worth saying, whichever bot owed it.
func gateDLQNoticeBody(run *store.Run) string {
	var b strings.Builder
	b.WriteString(gateDLQNoticeMarker)
	b.WriteString("\n⏸️ **Run parked on the dead-letter queue — automation has stopped for it.** Nothing is wrong with this pull request.\n\n")
	b.WriteString("iterion exhausted its redelivery budget for this run and parked its message on the DLQ; nothing resumes it on a schedule. ")
	b.WriteString("The check this run owed is marked **failed** on this head, and iterion relaunches the bot **once** on this head where the repository's automation allows it — that fresh run posts its own verdict. ")
	b.WriteString("If no relaunch follows, or it dies too, an operator replays or discards the parked message with `iterion remote admin dlq`, or re-triggers the bot (push again, or comment its command).\n")
	if cause := gatePauseCause(run); cause != "" {
		fmt.Fprintf(&b, "\n> %s\n", cause)
	}
	return b.String()
}
