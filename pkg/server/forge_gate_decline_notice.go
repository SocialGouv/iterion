package server

import (
	"fmt"
	"strings"

	"context"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A bot that ended DECLINED did not fail: it read its task, concluded the
// task's own premise was wrong, and deliberately changed nothing. The case
// that named this (#706): the merge-queue auto-heal launched a fixer on a
// GREEN pull request an unrelated flaky test had ejected from the queue. The
// bot's plan node concluded "no code issue in the diff — this is a re-queue,
// not a fix" and recorded, as its own step 0, that a queue build was in flight
// and that pushing would cancel it. Its mission then told it to rebase and
// force-push. Run to completion it would have destroyed the merge it was
// dispatched to protect.
//
// Everything below keys on the persisted FailureCode ALONE — never on a bot
// id, a bot's manifest, or the lane that launched it. Any bot may decline; the
// engine knows the shape of the outcome and nothing about who produced it.
//
// Two consequences, both of them things that would otherwise be wrong:
//
//   - No relaunch. A refusal is not a dead run to repair. The head did not
//     move, so nothing else in the run's world changed either: re-dispatching
//     re-derives the same answer, at the same price, for as long as the
//     trigger keeps firing.
//   - A notice. Precisely because nothing moved, the pull request carries no
//     trace of the decision: the reviewer's check sits red and the fixer
//     appears to have done nothing at all. The bot's own reason is the only
//     thing anyone can act on, so it has to reach the author.

// declinedFailureCode is the code a BOT stamps on its own `fail` node to
// refuse its task on the merits. Deliberately NOT one of the engine's
// store.FailureCode constants: that block is exactly the set a workflow may
// NOT mint (store.ReservedFailureCodes, enforced by C248), because every one
// of them is engine control flow somewhere. This one runs the other way — the
// bot writes it, the platform reads it — so it is declared where it is read,
// alongside the two lanes that honour it, and belongs to the same family as
// the other bot-minted codes (PLAN_BUDGET_EXHAUSTED, LOT_NOT_ACTIONABLE).
//
// Any bot may adopt it by ending on `fail <name>: code: DECLINED`; nothing
// here learns which bot did, which is what keeps the engine bot-agnostic.
const declinedFailureCode store.FailureCode = "DECLINED"

// gateDeclineNoticeMarker tags the comment so a reader (or a later dedup
// pass) can find iterion's own decline notices without parsing prose.
// Distinct from the pause and DLQ markers: those say "this will come back",
// this one says "this is the answer".
const gateDeclineNoticeMarker = "<!-- iterion:fixer-declined -->"

// noticeFixerDeclined posts the decline on the pull request the run gates.
// Best-effort in every branch, like its sibling notices: no repair depends on
// the comment landing, so a comment that cannot be posted never becomes an
// error. Silent (Debug) on every miss — a run that gates nothing is the common
// case, and a bot that declines outside a pull request has nobody to tell.
func (s *Server) noticeFixerDeclined(ctx context.Context, run *store.Run) {
	if s == nil || run == nil || run.FailureCode != declinedFailureCode {
		return
	}
	target, ok := s.gateNoticeTarget(ctx, run, "decline notice")
	if !ok {
		return
	}
	if _, err := target.commenter.CommentIssue(ctx, target.repo, target.number, gateDeclineNoticeBody(run)); err != nil {
		s.gateNoticeDebug(run, "decline notice", "%v", err)
		return
	}
	if s.logger != nil {
		s.logger.Info("forge gate: run %s declined its task — notice posted on %s (nothing was pushed; no fix pass launched)",
			run.ID, target.prURL)
	}
}

// gateDeclineNoticeBody renders the comment for the developer whose pull
// request it lands on: that a bot ran, that it deliberately changed nothing,
// its reason, and what that leaves them to do. Role-neutral on purpose — what
// makes it worth saying is that an automated run touched this pull request and
// its answer was "no", whichever bot it was.
func gateDeclineNoticeBody(run *store.Run) string {
	var b strings.Builder
	b.WriteString(gateDeclineNoticeMarker)
	b.WriteString("\n🚫 **A bot was dispatched to this pull request and declined the task.** Nothing was pushed, and no branch or check was changed.\n\n")
	b.WriteString("It read the change and concluded that what it was asked to fix is not there — so acting would have meant pushing a commit nobody needed, on a head somebody may already be building. ")
	b.WriteString("iterion does not retry a decline: re-dispatching reaches the same conclusion. ")
	b.WriteString("If the premise was right after all, say so on this pull request (or re-trigger the bot with the detail it was missing); if it was wrong, this needs a human, not another run.\n")
	if reason := strings.TrimSpace(run.Error); reason != "" {
		fmt.Fprintf(&b, "\n> %s\n", reason)
	}
	return b.String()
}
