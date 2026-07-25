# Studio UX coherence audit — 2026-07

Snapshot of the systematic UX audit run against the studio (cloud-first
lens, the connect-repo wizard as the quality bar: guided steps, no dead
ends, every empty/error state carries an explanation + a CTA). 8 view
clusters were audited in parallel (87 code-evidenced findings); the
bulk shipped in the wave commits `048c8e8af…b24361e8c` (2026-07-17).

## What shipped (by theme)

- **Cloud tells the truth** — no affordance that 403s or points at a
  local filesystem: marketplace install → download/CLI path, /bots
  import/builder → marketplace, plugin toggles super-admin-gated
  (server + UI) with a shared-scope banner, board/pipelines/runs copy
  and CTAs mode-aware, gated features (/triggers /dispatcher
  /marketplace) render an explanation instead of silently falling
  through to Home.
- **Golden path** — cloud Home: get-started progression (connect →
  enable → launch), Repositories panel, repo-scoped runs panel, Nexie
  hero with live-session state; ⌘K gains connect + repo-switch
  entries; the active team is always visible in the org switcher.
- **People flows without email** — invitations hand out the accept URL
  (stacked, raw token behind a disclosure), org invites can grant a
  team, /invitations/accept accepts pasted links, RestrictedShell
  links the redemption, forgot-password explains the admin path, and
  super-admins can mint a one-shot temp password
  (`POST /api/admin/users/{id}/reset-password`).
- **Repo-first, functionally** — whats-next launches against the
  active repo (runner clones it; session keyed per repo; scope
  mismatch warned; discovery failures visible; spend chip), pipeline
  cards/tasks carry the forge linkage with an active-repo scope,
  bot home + trigger dialog know the target repo, run console shows
  the repo identity instead of pod paths. See
  [repo-scope.md](repo-scope.md).
- **Run console legibility** — node ids render humanized (raw id as
  suffix/tooltip), error hints are studio-first, launch inputs on the
  Input primitive.

## Remaining backlog (ranked)

Third pass (2026-07-18, commits `3df0716b9…e519c2fad`) closed the bulk
of what was left: CreateWebhookDialog is a 4-step wizard with
provider-driven event defaults (#3), the legacy ConnectForm/OAuth-apps
fold behind a "Manual setup (advanced)" disclosure and the WiringGuide
routes to the connect wizard (#4), Automations carries a
trigger-families explainer (#5), nodes accept a DSL `description:`
surfaced as the run-console label (#6), org-status enums are explained
(#7), and the #8 polish shipped (About links in web mode, shared
CronField with presets/preview in EnableRepoPanel). The same pass added
inline Resume + bulk cancel/delete on the runs list, explanatory
fallbacks for gated routes, cloud gating of the Editor affordances, and
platform `/admin/audit` + `/admin/dlq` pages. #2 (cloud schedules UI)
had already shipped as the Automations Schedules tab.

Fourth pass (2026-07-18, commits `95cf81de1…39ce1d264`) closed #2 and
#3 (run-console headers resolve the authored `description:` via
`useNodeLabel`; `/board/labels` + `/board/fields` gained explanatory
fallbacks) and moved to fresh, code-evidenced ground: dispatcher-free
Board vocabulary in cloud (ColumnDialogs/bulk actions say
"stage/staged" when `dispatcher_enabled` is off), cloud-truthful
Marketplace copy (no local install paths for tenants without a
workspace), humanized enums/ids across account/admin/integrations
(PAT team names, org-status badge labels + tones, connection
status/kind, invocation kinds, ADR references dropped), shared
`formatDateTime`/`formatDate` helpers replacing ~22 hand-rolled
`toLocaleString` renders, `formatRelative` future-instant support
("in 30d" instead of "-2564872s ago" on share expiries), destructive
confirms unified on `useConfirm`, a config-share delivery log drawer
(previously API-only), an edit-trigger dialog (whose PUT projection
also fixed `setTriggerEnabled` clobbering schedgate policy), and
feedback/a11y polish (download-failure toast, invitation loading
state, WatchPanel tracker gating, dead API clients removed).

Still open:

1. **Launch form as a guided flow** (declined for now) — the
   progressive-disclosure LaunchView (persona header, auto-managed
   vars, single Advanced fold) already broke the 8-section wall; a
   strict multi-step wizard would add clicks to the most frequent
   flow. Revisit only if evidence shows the single-page form still
   confuses.
2. **Editor disk affordances in cloud** (declined for now) — `/editor`
   is off the cloud nav but still URL-reachable; File-menu Save/Open
   present disk paths that fail with a generic toast there. Revisit if
   the editor gets a cloud save path.
3. Low-value API/UI symmetry leftovers: `server_info.pipeline_concurrency`
   duplicates the pipeline-board payload (unused by the UI); trigger
   editing is unverifiable on deployments with `triggers_enabled=false`.

## Method notes

Per-cluster auditors read code (file:line evidence mandatory, no
speculation) against a shared lens list: dead ends, unguided flows,
missing feedback, discoverability, primitive coherence, repo-scope
coherence, redundant friction. Findings were verified during
implementation — two were rejected on inspection (IssueModal footer
layout was misread; one duplicate). The deploy's live flags
(`server_info`) were injected into the audit so cloud dead-ends were
judged against reality, not code possibility.
