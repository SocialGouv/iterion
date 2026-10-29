package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// The merge-queue half of the gate.
//
// A required check is satisfied on ONE revision, and a merge-queue entry is a
// revision no bot ever sees: GitHub builds `gh-readonly-queue/<base>/pr-<n>-…`
// and fires `merge_group`, which is not a pull request, so the review lane
// never runs and the context the ruleset requires is simply absent there. The
// group then waits for a status that cannot arrive and is dequeued on timeout
// — measured 2026-07-28 on PR #307, 62 minutes in AWAITING_CHECKS with every
// entry behind it stuck too.
//
// WHY THE ENGINE rather than the workflow that mirrors this today
// (.github/workflows/merge-queue-gate.yml, kept as the documented fallback):
// a required check pinned by NAME alone is satisfied by any credential able to
// post a commit status, which is the premise #1589 refuses. Authenticating the
// producer means pinning the ruleset to the App that posts the verdict — and a
// workflow posts as GitHub Actions, never as iterion's App, so a ruleset
// pinned to iterion and a verdict mirrored by Actions are mutually exclusive.
// Posting from the engine makes the two compatible: the same App answers on
// the pull request head and on the queue revision.
//
// ORDER MATTERS and it is the whole risk, so it is written down rather than
// implied (docs/auto-maintenance.md, "the merge-queue caveat"): the engine
// must post on queue revisions BEFORE the pin is written, or every entry
// stalls on a context the pinned App never produced.

// mirroredQueueVerdict is the description this lane writes. It is NOT
// gateInFlightDescription even for a mirrored `pending`: that description is
// the server's own in-flight CLAIM, read back by isGateInFlight and repaired
// by the reconciler when the run owning it dies. No run owns a queue
// revision, so a claim there would be a pending nothing can ever resolve.
func mirroredQueueVerdict(number int, headSHA string) string {
	return fmt.Sprintf("mirrored from PR #%d head %s", number, shortSHA(headSHA))
}

// handleGitHubMergeGroup answers a merge_group delivery by mirroring the pull
// request head's gate verdict onto the revision the queue built.
//
// It MIRRORS and never asserts: an entry can only be queued once its head is
// clean, so reading the real status is both honest and free — and if that
// invariant ever changes, the actual verdict propagates instead of a
// fabricated one. A head carrying no verdict for this repo's gate context gets
// NOTHING posted: inventing a failure would block what was never gated, and
// inventing a success would certify a revision nobody audited.
//
// The RESIDUE, stated rather than papered over: the verdict is read off the
// head the pull request has NOW, while the queue revision was built from the
// head it had at entry. The `merge_group` payload carries the queue head and
// the base sha, never the pull request head the entry was cut from, so the two
// cannot be compared here — the invariant relied on is GitHub's own, that a
// push to the branch invalidates the entry. This is exactly the fallback
// workflow's bound too (it resolves `pulls/<n>` the same way), so the engine
// and the fallback are sound or unsound together, which is the property worth
// keeping while both exist.
func (s *Server) handleGitHubMergeGroup(ctx context.Context, w http.ResponseWriter, cfg webhooks.Config, body []byte, payloadHash, srcIP string) {
	p, err := prforge.ParseMergeGroup(body)
	if err != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, webhookEventMeta{Kind: prforge.EventHeaderMergeGroup}, webhooks.StatusInvalid, payloadHash, srcIP, err.Error())
		httpError(w, http.StatusBadRequest, "invalid merge_group payload")
		return
	}
	meta := webhookEventMeta{
		Kind:         prforge.EventHeaderMergeGroup,
		Action:       p.Action,
		ProjectPath:  p.ProjectPath,
		SubjectID:    fmt.Sprintf("pr:%d", p.PRNumber),
		SubjectSHA:   p.QueueSHA,
		SenderHandle: p.SenderLogin,
	}
	filtered := func(reason string) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, reason)
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
	}

	// `destroyed` means the group is gone — merged, or the entry was removed.
	// A status posted then decides about a revision nobody is deciding on.
	if !p.ChecksRequested() {
		filtered("merge_group action " + p.Action + " asks for no verdict")
		return
	}

	// The repo's pinned context, through the one resolver every lane reads —
	// a second reading of "which check does this repo require?" would mirror
	// onto a context nothing waits for (the #1591 class, arrived at from the
	// queue side).
	//
	// The empty bot id is the question being asked: what does the REPOSITORY
	// require? The resolver's third tier is a bot's own declared default, and
	// no bot runs on a queue revision — a repo that pins no shared context has
	// no repo-wide required check for the queue to satisfy, so this lane stays
	// silent on it rather than inventing one. (The tier is unreachable with an
	// unnamed bot anyway: no catalogue entry is called "".)
	gateCtx := s.resolveGateContext(ctx, cfg, "")
	if gateCtx == "" {
		filtered("no gate_context is pinned on this integration — the queue has no repo-wide required check to mirror")
		return
	}
	// An explicit `gate_enabled` pin that leaves the bots advisory-only means
	// no verdict is owed anywhere; mirroring one would manufacture the very
	// required check the pin exists to avoid.
	if v, ok := cfg.OperatorLaunchVars["gate_enabled"]; ok && forge.GateValueDisables(v) {
		filtered("gate_enabled is pinned off on this integration")
		return
	}

	conn, ok := s.forgeConnectionForPR(ctx, cfg.TenantID, "", hostOfURL(p.RepoURL), p.ProjectPath)
	if !ok {
		filtered("no forge connection of team " + cfg.TenantID + " covers " + p.ProjectPath)
		return
	}
	gc, err := s.gateClientFor(ctx, conn, p.ProjectPath)
	if err != nil {
		filtered("gate client for " + p.ProjectPath + ": " + err.Error())
		return
	}
	if gc == nil {
		filtered("provider " + string(conn.Provider) + " has no commit-status capability")
		return
	}

	pr, err := gc.GetPullRequest(ctx, p.ProjectPath, p.PRNumber)
	if err != nil {
		filtered(fmt.Sprintf("resolve the head of %s#%d: %v", p.ProjectPath, p.PRNumber, err))
		return
	}
	head := strings.TrimSpace(pr.HeadSHA)
	if head == "" {
		filtered(fmt.Sprintf("the forge returned no head sha for %s#%d", p.ProjectPath, p.PRNumber))
		return
	}

	// Read, then decide. An UNREADABLE status list is not an absent verdict:
	// the two must not collapse, or an API hiccup becomes "this PR was never
	// gated" and the queue is told nothing about a revision it is holding.
	src, readable, err := gateStatusOn(ctx, gc, p.ProjectPath, head, gateCtx)
	if err != nil || !readable {
		filtered(fmt.Sprintf("the %s status on %s@%s could not be read (readable=%v): %v", gateCtx, p.ProjectPath, shortSHA(head), readable, err))
		return
	}
	if strings.TrimSpace(string(src.State)) == "" {
		filtered(fmt.Sprintf("%s#%d head %s carries no %s — the gate is not in use for it, so there is nothing to mirror",
			p.ProjectPath, p.PRNumber, shortSHA(head), gateCtx))
		return
	}

	desc := mirroredQueueVerdict(p.PRNumber, head)
	// Already mirrored: the same verdict, from the same head. GitHub appends
	// statuses, and a redelivery is normal, so re-posting would stack
	// identical rows on the queue revision for no added fact.
	if cur, curReadable, curErr := gateStatusOn(ctx, gc, p.ProjectPath, p.QueueSHA, gateCtx); curErr == nil && curReadable &&
		cur.State == src.State && strings.TrimSpace(cur.Description) == desc {
		filtered(fmt.Sprintf("%s=%s is already mirrored onto %s", gateCtx, src.State, shortSHA(p.QueueSHA)))
		return
	}

	st := forge.CommitStatus{
		State:       src.State,
		Context:     gateCtx,
		Description: desc,
		TargetURL:   src.TargetURL,
	}
	if err := gc.SetCommitStatus(ctx, p.ProjectPath, p.QueueSHA, st); err != nil {
		// Reported, never swallowed: the queue is now waiting on a context
		// this server was supposed to produce, and the fallback workflow is
		// the documented way out.
		if s.logger != nil {
			s.logger.Warn("forge gate: could not mirror %s=%s from %s#%d head %s onto queue revision %s: %v",
				gateCtx, src.State, p.ProjectPath, p.PRNumber, shortSHA(head), shortSHA(p.QueueSHA), err)
		}
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP,
			fmt.Sprintf("mirroring %s onto %s: %v", gateCtx, shortSHA(p.QueueSHA), err))
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusLaunchError})
		return
	}
	if s.logger != nil {
		s.logger.Info("forge gate: mirrored %s=%s from %s#%d head %s onto queue revision %s",
			gateCtx, src.State, p.ProjectPath, p.PRNumber, shortSHA(head), shortSHA(p.QueueSHA))
	}
	s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusAccepted, payloadHash, srcIP,
		fmt.Sprintf("mirrored %s=%s from head %s onto %s", gateCtx, src.State, shortSHA(head), shortSHA(p.QueueSHA)))
	writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusAccepted})
}
