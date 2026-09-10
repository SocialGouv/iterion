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
  `GET/PATCH /api/orgs/{id}/settings`. From the terminal:
  `iterion remote orgs settings` and `iterion remote orgs approvals
  [approve|reject <id>]`.
- **What the approval is FOR** (`Org.ProvisionApprovalScope`): `all` (the
  default, and every pre-existing org) parks every request; a team spending
  its own credentials answers to nobody for what it runs, so
  **`shared_credentials`** parks only the teams that bring none — the ones
  whose runs would be funded by the org tier, the pool or the platform.
  `iterion remote orgs settings --approval-scope shared_credentials`.
  "Funded" counts TEAM-scoped credentials only: the owner of a webhook,
  board or schedule launch is a synthetic identity with no personal
  credential, so one member's personal key funds none of the automated runs
  the provisioning under review would create. A degraded credential read
  fails the request 503 rather than resolving into a policy decision nobody
  made — "I could not tell" must never read as "they pay their own way".
- **Per-team usage caps** (studio Org → Governance): org admins set
  `max_concurrent_runs` and `launch_rate_per_min` per team of their
  org (`PATCH /api/orgs/{id}/teams/{team_id}/caps`) — enforced at
  launch by the existing gate. The org-level monthly run/cost/memory
  budget remains super-admin (platform ⇄ org contract).
- **Shared org credentials** (`iterion remote orgs credential-audience`):
  which of the org's teams may spend the org's own LLM keys. Its zero value
  admits nobody. See
  [cloud-llm-credentials.md](cloud-llm-credentials.md#the-org-tier--one-key-several-product-teams).

### Team lifecycle

Teams used to be create-and-list only, which made a naming mistake permanent
and left `Team.Status` readable by the launch gate but writable by nothing:

| Action | Who | Command |
|---|---|---|
| Rename | team admin | `iterion remote teams update --name X --slug x` |
| Suspend / resume | **org** admin | `iterion remote teams status suspended --reason "…"` |
| Delete an EMPTY team | org admin | `iterion remote teams delete` |
| Place an EXISTING account in a team | team admin | `iterion remote teams add-member <user-id> --role admin` |
| Place an EXISTING account in the org | org admin | `iterion remote orgs add-member <user-id> --role member` |

Two guards worth knowing. **Delete refuses a team that still owns
anything** — repo integrations, forge connections, api keys, active runs —
and names what is left: it is a refusal list, not a cascade, because the
only correct cascade is the org purge sweeper, and the failure a hand-rolled
one would cause is silent (a surviving webhook firing into a tenant nothing
can reach). **`teams add-member` still requires the org
membership**: that is the identity boundary a team grant sits inside, so
creating one silently would let a team admin pull a stranger into the org —
which is why `orgs add-member` exists as its org-admin twin, and why the two
are a pair. Without it the round trip was merely moved one level up: a user
with no org at all stayed reachable only by email. For an account that does
not exist **yet**, the email invitation remains the path.
