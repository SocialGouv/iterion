# Ticket conformance — plugging tracker tickets into a review

Revi (`bots/review-pr` ≥ 0.6.0) can verify that a PR actually delivers
what its tracker ticket(s) ask for: it fetches the referenced tickets
(Jira Cloud, Jira Server/DC, GitHub or GitLab issues), checks the diff
against the demand and acceptance criteria, posts a per-ticket verdict
(covered / partial / not covered / unverifiable) in the review summary
and the markdown report, and files genuine gaps as findings of category
`requirements` — which gate the merge like any other finding, per the
repo's `gate_severity`.

The feature is **dormant by default**: without a `tracker_api_base` var
the bot behaves exactly as before. Tracker specifics live in
[bots/review-pr/skills/ticket-context.md](../bots/review-pr/skills/ticket-context.md)
(universal-bots doctrine — no tracker enum in the DSL).

## How the reviewers find the tickets

- Explicit: `--var ticket_refs="PROJ-123 PROJ-456"`.
- Extracted (the webhook default): from the PR title/body (they arrive
  as `scope_notes`) and the source branch name — Jira keys
  (`PROJ-123`), `#N` refs, pasted ticket URLs.

A fetch failure or zero extractable refs yields an explicit
`unverifiable — <reason>` verdict; it never fails the review and never
silently disappears.

## Wiring a team (cloud instance)

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

Repos/teams without the binding are untouched: the secret is
`optional: true` and the vars default empty.

For a local CLI run: `iterion secret set tracker_token`, then
`iterion run bots/review-pr/main.bot --var pr_url=… --var
tracker_api_base=… [--var ticket_refs=…]`.

### Limits to know

- A binding is per **(team, bot)** — one `tracker_token` per team for
  `review-pr`, hence one tracker credential per team. Different Jira
  tokens per repo ⇒ use one service account with access to all the
  relevant projects, or split the repos across teams.
- The reviewers read the token as a FILE path and never print its
  value; layers 0–2 of [secrets.md](secrets.md) apply. Ticket content
  is treated as untrusted data (anti-injection clause in the skill).

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
