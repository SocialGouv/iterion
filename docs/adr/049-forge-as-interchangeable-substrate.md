# ADR-049 — Forge as an interchangeable substrate (iterion stays a layer, not a forge)

Status: **Accepted**

## Context

A recurring strategic question: should iterion couple itself to an open-source
forge (Forgejo) — by **embedding** it, **forking** it, building its **own
forge** derived from its code, or by remaining an **interfaceable layer** over a
forge and deepening the integration?

The codebase already draws a clean line:

- **iterion natively hosts the work-tracking primitive** — a first-class kanban
  in [`pkg/dispatcher/native/`](../../pkg/dispatcher/native/) (issues, board,
  labels, comments, custom fields, saved views, swimlanes, `events.jsonl`), with
  no forge dependency for local workflows.
- **iterion is strictly a forge *client* for the code** — [`pkg/git/`](../../pkg/git/)
  shallow-clones + worktrees (read-only status/diff/log); bots commit/push from
  inside their sandbox with a sealed OAuth token. There is **no** git-hosting
  primitive (no git http/ssh server, no hosted PR/review/branch).
- **The provider abstraction is already multi-provider and clean** —
  [`forge.Admin`](../../pkg/forge/admin.go),
  [`forge.IssueClient`/`forge.PullClient`](../../pkg/forge/issues.go),
  [`tracker.Tracker`](../../pkg/dispatcher/tracker/tracker.go), and the inbound
  webhook parsers in [`pkg/webhooks/`](../../pkg/webhooks/) — with GitHub,
  GitLab and Forgejo at near-parity, plus the event-driven trigger spine
  ([ADR-046](046-event-driven-runs-trigger-spine.md)).

So iterion already owns "work orchestration + tracking" and delegates the heavy,
commoditized part (git + PR + review + CI) to the forge. The boundary exists and
is healthy.

## Decision

**iterion remains an AI-native layer over an interchangeable forge.** We close
the gaps *inside the existing abstraction*; we do **not** fork a forge and we do
**not** build one.

Rationale:

- **Moat / focus.** iterion's differentiation is agent-workflow orchestration —
  the DSL, the trigger spine, the event→workflow-with-bound-creds→post-back loop. Git
  hosting is solved and commoditized (≥3 mature OSS forges). Engineering spent
  *owning* a forge is spent on the part competitors give away.
- **Forge-agnosticism is a feature.** Today iterion lands in an org already on
  GitHub Enterprise *or* GitLab self-hosted *or* Forgejo/Codeberg. Becoming or
  tightly-forking a forge forfeits the "works with your existing forge" value
  prop. The `forge.Admin` / `tracker.Tracker` abstraction is the asset to
  protect.
- **The gaps are incremental, not architectural.** Everything missing fits
  inside the existing interfaces (see Consequences) — "improve the integration"
  delivers most of the perceived benefit of "tighter integration" at a fraction
  of the cost.

### Options rejected

- **Fork Forgejo** — permanent merge burden against an actively-developed
  ~500k-LOC project (security patches, Gitea/Forgejo divergence) for marginal
  integration depth the REST API already expresses. No identified need requires
  forge internals.
- **Build our own forge from Forgejo's code** — worst option: enters the
  git-hosting business (smart-http/ssh, packfile/LFS storage + scaling, an
  internet-facing auth-critical security surface) orthogonal to the moat, and
  forces reconciling forge identity/permissions with iterion's own org/team/SSO
  model ([ADR-048](048-org-team-hierarchy.md)). Open-ended cost, diluted mission.
- **Embed Forgejo (batteries-included)** — *deferred*, not rejected: a
  **packaging** decision (helm/compose bundling a Forgejo instance auto-connected
  via the *existing* `forgejo` adapter, zero code coupling), to revisit only if a
  "batteries-included self-hosted" story becomes a priority. Forgejo would stay a
  black box behind `forge.Admin`.

## Consequences

The integration is deepened by filling these gaps **within the existing
interfaces** (this ADR's accompanying change implements them):

1. **Bot-driven PR lifecycle** — `PullClient` gains `CreatePull` / `UpdatePull`
   / `MergePull` (was read-only) across all three providers, surfaced as card
   actions ([`pkg/server/board_forge.go`](../../pkg/server/board_forge.go)) so a
   run ties its PR back to the source card.
2. **GitLab dispatch parity** — a new GitLab `Tracker` adapter
   ([`pkg/dispatcher/tracker/gitlab.go`](../../pkg/dispatcher/tracker/gitlab.go))
   so GitLab Issues are dispatchable (previously GitLab was reachable only via MR
   webhooks).
3. **Forge comment-back** — `IssueClient.CommentIssue` lets a bot reply on the
   source issue/PR (was inbound-only).
4. **Webhook introspection** — `Admin.ListHooks` + a per-integration hooks
   endpoint, to audit orphaned/divergent forge-side hooks.
5. **Near-real-time forge→board projection** — an inbound forge webhook now
   triggers an immediate incremental sync of the affected repo's integration
   (reusing `syncOneIntegration`), with the periodic worker kept as the
   reconciliation net.

The capabilities stay **optional** (type-asserted, `ErrNotSupported`) so a
provider/auth-kind that lacks one (e.g. a GitHub-App connection without
issue/PR scope) degrades gracefully.

### When to revisit

Reconsider the heavier options only if either holds:
- a core product requirement needs forge internals that **no** forge API
  exposes (none identified today), or
- the product pivots to an "all-in-one AI-native DevOps platform" where owning
  the forge UX end-to-end is the differentiation — in which case **embed**
  Forgejo as a black box and contribute upstream, **never** fork.

The AI-native collaboration experience ("a forge where agents are first-class
collaborators") is delivered as an **overlay** via the forge API + the studio
(linked-PR/CI panels, board sync) — it does not require owning the git substrate.
