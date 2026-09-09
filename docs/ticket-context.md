# Ticket conformance — checking a PR against the ticket it claims

Revi (`bots/review-pr` ≥ 0.6.0) verifies that a PR actually delivers
what its ticket(s) ask for: it reads the referenced tickets (the forge's
own issues, or Jira Cloud / Server-DC), checks the diff
against the demand and acceptance criteria, posts a per-ticket verdict
(covered / partial / not covered / unverifiable) in the review summary
and the markdown report, and files genuine gaps as findings of category
`requirements` — which gate the merge like any other finding, per the
repo's `gate_severity`.

**Since 0.9.0 the check is ON by default and needs no configuration.**
`ticket_context` (`auto` | `off`, default `auto`) picks the source:

| what is configured | source | who fetches |
|---|---|---|
| `tracker_api_base` set | that external tracker (Jira & co) | the reviewer, with the bound `tracker_token` |
| nothing (the common case) | the FORGE's own issues, derived from `pr_url` | the deterministic `ticket_fetch` node, with the run's `forge_token` |
| `--var ticket_context=off` | none — the section is skipped | — |

Everything below the next section is about the **external tracker**
wiring; forge-native mode needs none of it. Tracker specifics live in
[bots/review-pr/skills/ticket-context.md](../bots/review-pr/skills/ticket-context.md)
(universal-bots doctrine — no tracker enum in the DSL).

## How the reviewers find the tickets

In order:

1. Explicit: `--var ticket_refs="PROJ-123 PROJ-456"` (or `#12`).
2. Forge-native only — **what the forge says this PR closes**: GitHub's
   `closingIssuesReferences`, GitLab's `closes_issues`. A formal link
   beats any regex over prose, and it is the only source that can carry
   a blocking `requirements` finding, along with (1).
3. Scanned from the PR title/body and the source branch name — Jira
   keys (`PROJ-123`), `#N` and `owner/repo#N` refs, pasted ticket URLs.
   A reference the text merely *mentions* is context for the review, not
   a promise to deliver: it never becomes a blocking finding.

A fetch failure or zero extractable refs yields an explicit
`unverifiable — <reason>` verdict; it never fails the review and never
silently disappears. **A PR that references no ticket is normal** — the
absence of a reference is never a finding.

### Forge-native mode — what it needs, and what it never does

It needs the `forge_token` the repo's provisioning already binds, and
nothing else. Two properties are deliberate:

- **No agent is asked to use that token.** It is the connection's runtime
  credential and it is write-capable, while a reviewer ingests the diff
  and every linked issue body — text an outside contributor writes. So
  the fetch is a deterministic node (`ticket_fetch`) and the reviewers
  receive ticket TEXT only. The external-tracker path keeps its
  agent-side `curl` recipes: `tracker_token` is a read-only credential
  an operator bound on purpose, egress-pinned to the tracker host.
  *Residual:* `as: file` secrets are mounted run-wide and the engine
  lists every mounted path in each agent's system prompt with its own
  "do not read the contents" rule, so the file stays reachable by an
  agent that defies both that rule and the bot's. What this buys is the
  difference between an injected reviewer FOLLOWING its instructions and
  one having to break them; closing the rest needs per-node secret
  scoping in the engine.
- **A GitHub Enterprise token stays on the enterprise.** The API base is
  derived from the PR URL — `https://<host>/api/v3` for REST and
  `https://<host>/api/graphql` for GraphQL, never `api.github.com`.

Without a bound `forge_token` the node reads anonymously: public issues
still work, private ones come back `unverifiable`.

## Wiring a team to an EXTERNAL tracker (cloud instance)

Only for Jira & co — forge-native mode needs none of these steps.
Everything is team-scoped; a team admin can do all of it self-service
in the studio, or an org admin / SRE does it for them.

1. **Tracker credential** — create a *read-only* service account on the
   tracker (Jira: browse-projects permission on the relevant projects
   only; the Jira-side project scoping is the primary boundary).
   - Jira Cloud: an API token for the service account (auth = Basic
     `email:token`).
   - Jira Server/DC: a Personal Access Token (auth = Bearer).
2. **Team secret** — store the token as a team generic secret (studio
   Secrets view, or `POST /api/teams/{id}/secrets`). Name it e.g.
   `jira_readonly`.
3. **Bot binding** — bind it to `review-pr` under the workflow name
   `tracker_token`, with `allowed_hosts` set to the tracker host
   (studio Bot bindings view, or
   `POST /api/teams/{id}/bots/review-pr/bindings` with
   `{"secret_id": …, "secret_name_for_workflow": "tracker_token",
   "allowed_hosts": ["jira.example.org"]}`). `allowed_hosts` is
   ENFORCED egress policy: the token cannot leave toward any other
   host (TLS-inspection DLP, see [secrets.md](secrets.md)).
4. **Per-repo activation** — pin the vars on the repo integration's
   `launch_vars` (studio Integrations, or the provisioning API):
   - `tracker_api_base`: `https://jira.example.org` (or
     `https://myorg.atlassian.net`)
   - `tracker_user`: the service account email — Jira Cloud only;
     leave unset for Bearer-token trackers.

Repos/teams without the binding keep the forge-native behaviour: the
secret is `optional: true` and `tracker_api_base` defaults empty, which
is exactly what selects the forge's own issues.

For a local CLI run: `iterion secret set tracker_token`, then
`iterion run bots/review-pr/main.bot --var pr_url=… --var
tracker_api_base=… [--var ticket_refs=…]`.

### Validated wiring (reference)

The first production wiring, validated end-to-end on 2026-09-08 against a
private Jira Cloud (bilan: [docs/bot-runs/review-pr.md](bot-runs/review-pr.md)):

| | value |
|---|---|
| team | `PIC (GitLab)` — where the repo's integration lives |
| secret | `jira_dam_token` (Jira Cloud API token, `--from-file`) |
| binding | `review-pr` ← `tracker_token`, `allowed_hosts: [jira-mcas.atlassian.net]` |
| launch_vars | `tracker_api_base: https://jira-mcas.atlassian.net`, `tracker_user: <service account email>` |
| trigger | `/revi` on the MR — it applies the integration's `launch_vars`; a manual launch does **not** |

Note the team choice is a real decision: a binding is per `(team, bot)`, so
every repo of that team could read those tickets if someone set
`tracker_api_base` on it. When a product deserves its own credential
boundary, give it its own team (there is an empty `PIC DematAmiante` team
waiting for exactly that move).

> **Do steps 2 and 3 from the target team.** Until
> [#997](https://github.com/SocialGouv/iterion/issues/997) lands, a secret or
> binding created for another team — by path (`/api/teams/<other>/…`) or by
> `--team` — is written into the tenant of your *active* team instead. It is
> acknowledged (201), then invisible from both teams, and the run silently
> finds no credential (the review reports `unverifiable — tracker token file
> does not exist`). Run `iterion remote teams switch <team>` first, and
> confirm with a list call from that same team. Measured on prod on
> 2026-09-08 (see [docs/bot-runs/review-pr.md](bot-runs/review-pr.md)).

### Limits to know

- A binding is per **(team, bot)** — one `tracker_token` per team for
  `review-pr`, hence one tracker credential per team. Different Jira
  tokens per repo ⇒ use one service account with access to all the
  relevant projects, or split the repos across teams.
- The reviewers read the tracker token as a FILE path and never print
  its value; layers 0–2 of [secrets.md](secrets.md) apply. Ticket
  content is treated as untrusted data (anti-injection clause in the
  skill) — and in forge-native mode the fetch happens outside any model
  context, so an injected ticket has no credential to reach for.
- Setting `tracker_api_base` **replaces** forge-native mode for that
  repo (the external tracker is the more specific answer). The two are
  exclusive today: a repo that tracks work in Jira does not also get its
  forge issues checked on the same PR.

## Isolation & org layout (who manages what)

The tenancy model (ADR-048) already carries the governance:

- **1 org = the client organization** (SSO, roster, monthly budget,
  audit). **1 team = 1 product team** = the billing AND secrets
  boundary: repos, forge connection, tracker token, bindings all live
  in the team.
- **Team admins** self-serve: team secrets, bot bindings, repo
  provisioning via the connect wizard. Plain members only view.
- **Org admins** manage every team in the org, the org roster, and the
  governance controls below. SRE typically holds org admin (and
  platform super-admin).
- The transitional "SRE manages everything" mode needs no code: an org
  admin performs the team-scoped steps; promoting a team referent to
  team admin later flips the team to self-service.

### Org governance controls

- **Provisioning approval** (`Org.RequireProvisionApproval`, studio
  Org → Governance): when on, a *team admin's* repo-bot provisioning
  (new repo, or adding a bot to a connected repo) is parked in a
  pending queue — nothing is created on the forge — until an org admin
  approves or rejects it. Org admins provision directly. All three
  events (requested / approved / rejected) land in the audit log.
  Endpoints: `GET/POST /api/orgs/{id}/provision-approvals[…/approve|
  /reject]`, `GET /api/teams/{id}/provision-approvals`,
  `GET/PATCH /api/orgs/{id}/settings`.
- **Per-team usage caps** (studio Org → Governance): org admins set
  `max_concurrent_runs` and `launch_rate_per_min` per team of their
  org (`PATCH /api/orgs/{id}/teams/{team_id}/caps`) — enforced at
  launch by the existing gate. The org-level monthly run/cost/memory
  budget remains super-admin (platform ⇄ org contract).
