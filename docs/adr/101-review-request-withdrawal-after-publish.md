# ADR-101: The App withdraws the review request its own review answered

**Status:** Accepted (2026-09-17)
**Relates to:** `forge.ReviewerAssigner` (#604 / #605, the opening half of the same gesture), the per-product write attribution settled 2026-09-10

## Context

Asking for a re-review by **adding a bot identity as reviewer** is a lane the
engine can trigger (`webhooks.Config.ReviewRequestLogins`) but could not
**close**: nothing retracted the request after the review was published.

GitHub lifts a review request only when the **requested account** submits the
review. On a `github_app` connection the review is posted by
`<app_slug>[bot]`, never by the designated User account — and a GitHub App
**cannot be a reviewer at all** (forge restriction). So the request outlived
the review answering it: the "review requested" pastille stayed pending
indefinitely, and the gesture was not repeatable — an operator had to remove
then re-add the reviewer by hand between two asks.

Measured on 2026-09-17 while arming `review_request_logins = ["iterion-bot"]`
on the fleet's 15 GitHub bindings: the lane was armed on **none** of them, and
no `review_requested` event naming that account exists in the delivery
history. The mechanism had never run for real on GitHub, which is why the
missing half had never been felt.

## Decision

1. **A new optional forge capability, `forge.ReviewRequestWithdrawer`,
   implemented by github only** — the mirror of `ReviewerAssigner`, which is
   implemented by gitlab only. The two are opposite halves of one gesture, and
   each provider implements exactly the half its forge cannot perform itself:
   GitLab's reviewer request has to be **opened** (a posted note creates no
   reviewer), GitHub's has to be **closed**. GitLab is therefore a *deliberate
   non-implementation*, not a gap: there the reviewer role is precisely what
   makes the native re-request button exist, so withdrawing it would dismantle
   the affordance the self-assign just created. Both directions are pinned
   negatively by capability tests, so a provider cannot silently acquire the
   wrong half.

2. **Read-then-intersect, never a blind `DELETE`.** The implementation fetches
   `GET …/pulls/{n}/requested_reviewers` first and withdraws only the
   intersection with the armed logins, case-insensitively, echoing the forge's
   own casing. An empty intersection issues **no write at all**.

3. **The armed logins are resolved from the store, not threaded through the
   request.** `grant{TeamID,ConnectionID,Repo}` → `forgeIntegrations.GetByConnRepo`
   → `integration.WebhookID` → `webhookConfigs.Get` →
   `Config.NormalizedReviewRequestLogins()`, cross-checked against the grant's
   tenant the way `repoLaunchPolicy` does. Resolved **before** any forge
   round-trip, so an unarmed webhook — the overwhelming majority — costs two
   store reads and nothing on the wire.

4. **In the existing best-effort publish tail, with its own deadline.** It
   joins `selfAssignReviewer` inside the detached, `goSafe`-wrapped goroutine
   that already runs strictly behind the response and the merge-gate status.
   A withdrawal that fails — a connection short of `pull_requests: write`
   above all — logs a Warn naming the grant and leaves the published review
   and its gate status untouched. Each half takes its own 30 s deadline off
   the one detached parent.

5. **`NormalizedReviewRequestLogins` is one definition with two readers** —
   the anti-loop actor guard (`iterionBotLogins`) and this tail. A trim/`@`-strip
   applied on one side only would have the tail withdraw `@bot`, which GitHub
   does not know, while the guard trusts `bot`.

## Alternatives rejected

- **Give the designated User account a write credential so it posts the review
  itself.** This is the only path to GitHub's native **↻ "Re-request review"**
  button, which the forge renders only next to a reviewer who has *submitted*
  a review. Rejected: it introduces a second write identity and undoes the
  per-product write attribution settled on 2026-09-10 — a distinct
  arbitration, not a detail of this one. The consequence is accepted
  explicitly: the ↻ button stays out of reach, and the gesture made repeatable
  is "add the reviewer".
- **A blind `DELETE` of the armed logins.** Simpler by one round-trip, and
  wrong twice: GitHub 422s a removal naming an account that is not currently
  requested, so every second publish on the same PR would log a failure — and
  without reading the pending set the call cannot *prove* it never drops a
  reviewer it was not named, which is a destructive write against a third
  party.
- **Thread the logins through the publish request.** Rejected:
  `injectForgePublishVars` already carries six positional parameters, and only
  the webhook lane that armed the request would be covered — a `/revi`
  comment, a board launch or an API publish leaves an equally pending request.
- **Document the manual remove-then-re-add and ship nothing.** Rejected: that
  is the defect, stated as a workaround.
- **Withdraw on the budget-escalation notice too**
  (`forge_gate_launch_budget.go`, which posts through `CreatePullReview`
  without this tail). Deliberately **not** wired: that notice says a review
  could *not* be produced, so the pending request is an accurate record of a
  review still owed. The request is withdrawn by a review that **landed**,
  never by one that was refused.

## Consequences

The pastille clears after each published review, so two successive requests on
one head are two ordinary "add the reviewer" gestures and launch two reviews
(the second is salted by the PR's advanced `updated_at`, not swallowed as a
duplicate). No new credential and no new write identity: `pull_requests: write`
is already in the App manifest baseline (`RuntimeInstallationPermissions`).

The removal the App performs emits a `review_request_removed`, which cannot
relaunch anything: the re-request predicate only matches action
`review_requested` and `IsReviewable` excludes `review_request_removed`, so
the event is filtered before the actor guard is even consulted — the guard
(`<app_slug>[bot]` ∈ `iterionBotLogins`) is the second of two nets, not the
first.

On a connection without the grant the lane degrades to exactly today's
behaviour — a pending pastille and a manual re-add — with a Warn naming
`pull_requests: write`, never a failed review. Forgejo keeps an accepted gap:
its re-request lane is not wired either, and wiring the trigger comes first.
