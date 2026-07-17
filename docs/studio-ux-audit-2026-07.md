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

1. **Launch form as a guided flow** (H/L) — /runs/new is still 8
   stacked sections; the WizardForm treatment (target → inputs →
   advanced → review) is designed in the audit but not built.
2. **Cloud schedules UI** (H/L) — `/api/teams/{id}/schedules` CRUD is
   live server-side with zero studio surface; provisioned schedules
   never resurface after EnableRepoPanel commits them.
3. **CreateWebhookDialog** (H/M) — 15-field wall; fold into WizardForm
   steps with provider-driven defaults.
4. **Integrations wizard-return dedup** (H/M) — `?tab=forges` still
   re-exposes the legacy ConnectForm + OAuth-app registration next to
   the wizard-born Repositories section; demote/merge.
5. **Trigger-families explainer** (M/M) — one page explaining forge
   webhooks vs schedules vs board triggers vs run-completion, and
   which surface owns each.
6. **DSL node `description:`** (M/L) — humanized ids are the 80% fix;
   authored labels need a DSL field + IR + wire plumbing.
7. **Org page** (M) — duplicate org/team audit surfaces could merge;
   org-status enums (suspended/read_only) still unexplained in the
   admin drawer.
8. Assorted low-severity polish tracked in the audit transcript
   (SettingsDialog About links, Dispatcher WS banner spam local,
   cron humanization presets in EnableRepoPanel).

## Method notes

Per-cluster auditors read code (file:line evidence mandatory, no
speculation) against a shared lens list: dead ends, unguided flows,
missing feedback, discoverability, primitive coherence, repo-scope
coherence, redundant friction. Findings were verified during
implementation — two were rejected on inspection (IssueModal footer
layout was misread; one duplicate). The deploy's live flags
(`server_info`) were injected into the audit so cloud dead-ends were
judged against reality, not code possibility.
