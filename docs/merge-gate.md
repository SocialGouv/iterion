# Merge gate — Revi arbitrates, determinism disposes

Revi (`bots/review-pr`) can post a **deterministic merge-gate status** on a
PR so an unresolved blocking finding keeps the PR out of the merge queue —
without ever letting an LLM be the yes/no arbiter of a merge.

The split is deliberate:

- **Revi (LLM) proposes** — two independent reviewers (Claude + GPT) find
  issues, merge + de-duplicate them, and post them as inline PR comments.
- **Determinism disposes** — the bot computes a **count** of findings at or
  above a severity floor and the server posts a `revi/review` **commit
  status** (`success` when the count is 0, else `failure`). The gate is a
  count, never the review verdict. Non-blocking [`questions`](#questions)
  never count.
- **A human arbitrates** — a false positive is cleared by pushing a fix
  (which re-reviews) or, for a disputed finding, by a maintainer override
  (see [Overriding](#overriding-a-finding)).

This mirrors the repo's standing doctrine: **gates stay deterministic**
(see `CLAUDE.md` → "Improvement loops must converge"). The reviews
themselves stay non-blocking advice (`forge.NewReview` never
approves/requests-changes); the entire gate lives in the separate commit
status.

## How it works

```
PR opened / pushed ──▶ Revi runs (2 reviewers → merge → publish)
                              │
                              ├─ inline comments  (advisory)
                              └─ revi/review status on the head SHA
                                    success  ⟺  0 findings ≥ gate_severity
                                    failure  ⟺  ≥1 finding  ≥ gate_severity
```

1. **Trigger.** Revi auto-reviews on PR `opened`/`reopened`. For the gate to
   track fixes, enable **`ReviewOnSync`** on the webhook so a push
   (`synchronize`) re-reviews the new head — otherwise the required check
   would never appear on the pushed SHA and the merge would deadlock.
2. **Verdict.** The bot's deterministic `publish_review` node counts findings
   whose severity is at or above `gate_severity` (default `high`), and sends
   `{enabled, blocking_count, threshold, total_findings}` in the publish
   payload.
3. **Status.** The server (`/api/v1/forge/publish-review`) resolves the PR
   head SHA and posts the `revi/review` commit status through the team
   connection's **live** forge client (a GitHub App mints a fresh token per
   call — no workspace credential, no ~1h token freeze). Forge-agnostic:
   GitHub commit-status / GitLab commit status / Forgejo commit status all
   expose the same primitive ([`pkg/forge/status.go`](../pkg/forge/status.go)).
4. **Enforcement.** List `revi/review` in the repo's **required status
   checks** (branch-protection ruleset). Until you do, the status is a
   harmless advisory check you can preview on every PR.

The status write is **additive**: if it fails (missing capability, forge
error, no head SHA), the review still publishes and the failure is reported
in the response (`gate_error`) and logged — never a silent no-op, never a
failed publish.

## Configuration

Bot vars ([`bots/review-pr/main.bot`](../bots/review-pr/main.bot)):

| Var | Default | Meaning |
|-----|---------|---------|
| `gate_enabled` | `true` | Post the `revi/review` status. `false` = advisory-only reviews. |
| `gate_severity` | `high` | Severity floor that blocks (`low`<`medium`<`high`<`critical`). `high` = high+critical block; low/medium advise. |

Webhook config ([`pkg/webhooks/types.go`](../pkg/webhooks/types.go)):

| Field | Default | Meaning |
|-------|---------|---------|
| `review_on_sync` | `false` | Re-review on each push so the required status re-evaluates on the fixed head. **Required for a blocking gate.** |
| `block_fork_prs` | `false` | Filter fork PRs from any auto-launch. **Recommended ON whenever `review_on_sync` is enabled on a public repo** (see caution). |

> **Caution — budget with `review_on_sync`.** The sync lane re-runs the two
> LLM reviewers on **every push** (each new head SHA), gated only by the
> webhook's `AuthorAllowlist` (empty = any author) and per-head idempotency —
> there is no author-trust gate on this lane. On a public repo a fork
> contributor pushing repeatedly can drive repeated full reviews, bounded only
> by the org launch gate + webhook rate limit. Enable **`block_fork_prs`**
> (and/or set an `AuthorAllowlist`/`MinAuthorRole`) alongside `review_on_sync`
> so untrusted fork PRs don't auto-re-review.

## Activating the blocking gate on a repo

The code posts the status unconditionally (advisory). To make it **block**:

1. Enable `review_on_sync` on the repo's inbound webhook.
2. Add `revi/review` to the branch-protection ruleset's required status
   checks (for this repo, the "main protected — merge queue" ruleset — see
   [merge-policy.md](merge-policy.md)). Example with `gh`:

   ```sh
   # inspect the current ruleset, add "revi/review" to required_status_checks,
   # then PUT it back:
   gh api repos/OWNER/REPO/rulesets/<id>
   ```

Repo admins keep their merge-queue bypass, so a stuck gate is never a hard
block for an admin.

## Overriding a finding

Three ways, in order of preference:

1. **Push a fix** — the status re-reviews on the new head (needs
   `review_on_sync`) and flips green when the finding is gone.
2. **`/revi approve [reason]`** — a **maintainer** comments this on the PR to
   force-green the `revi/review` status on the current head, for a finding
   they dispute. Trust-gated: the commenter must hold real repo rights
   (`MinAuthorRole` / `AuthorAllowlist`, verified live via the forge
   permission API) — an arbitrary contributor cannot wave a finding through.
   The status carries "approved by @user: reason" and links to the comment as
   the audit trail. It does **not** launch a re-review. *(GitHub + Forgejo
   today; GitLab `/revi approve` on a note is a follow-on.)*
3. **Admin merge-queue bypass** — the last resort, always available to repo
   admins.

## <a name="questions"></a>Questions vs findings

Revi separates two channels:

- **findings** — issues it would block a merge on. These feed the board, the
  inline comments, and the gate count.
- **questions** — non-blocking, load-bearing assumptions the diff now relies
  on that the reviewers could not verify end-to-end ("EditorView no longer
  opens the file — does EditorTabHost guarantee it does?"). They make a
  0-finding review **falsifiable** (they show what was checked and where the
  residual risk hides) and are surfaced in the report + the PR review summary
  body. They **never** become findings, reach the board, or gate a merge.
