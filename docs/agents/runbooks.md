# Operational runbook index — capture what a session cost you to discover

The discovery entry point for "how do I configure / operate / debug X on
iterion". Each entry names **when to read it**, which is its whole value: an
agent that does not know the runbook exists cannot look it up.

Referenced from [CLAUDE.md](../../CLAUDE.md). Extend the index in the same
change that adds the runbook.

## Operational-knowledge reflex — capture what a session cost you to discover

When a work session burns real time **discovering how to configure or operate
iterion** (a non-obvious parameter, a cloud cred flow, an env toggle, an infra
gotcha, a "why is prod doing X"), that discovery MUST land back in the repo so
the next occurrence is instant. Don't leave it in a chat transcript. Wire it
across the three surfaces by role:

- **This page** — the *reflex* itself (this section) + the "read it when" line
  in the **operational runbook index** below, added in the same change as the
  runbook. This is the discovery entry point, and
  [CLAUDE.md](../../CLAUDE.md) routes here. The router itself takes at most
  *one line*, and only when an agent would never find the runbook otherwise —
  a paragraph added there belongs in the tree instead.
- **`docs/`** — the *content*: one focused runbook per topic (the how + the
  gotchas + the cookbook). Link it from the index.
- **A skill** (`bots/whats-next/skills/…` or a project skill) — the *discovery
  trigger*: when an agent asks "how do I configure/provision X on iterion",
  the skill fires and points straight at the runbook.

Keep each addition succinct and grounded in the real commands/paths that
worked. A five-minute write-up now saves the next session (or the next dev)
the hours this one spent.

**Operational runbook index** (the discovery entry point — extend it):
- [docs/cloud-llm-credentials.md](../cloud-llm-credentials.md) — provisioning
  a cloud run's LLM credential (BYOK vs Anthropic OAuth-forfait vs OpenAI
  ChatGPT-forfait, the CGU guard, `ITERION_OPENAI_USE_OAUTH`, the
  `/api/me/oauth/*` endpoints; fixes `401`/`429` on cloud runs) — including
  the **org tier** (an organization's own keys/forfaits lent to the teams its
  `credential-audience` names — the answer to "share one key across our
  product teams" that used to mean copying it into each of them, N writes per
  rotation) and the **platform tier**: the deployment's own DB-backed
  fallback keys/forfait (`iterion remote admin llm …`, studio Admin → LLM
  credentials), rotated with one call instead of a k8s-secret edit +
  redeploy, and now **gated by an opt-in audience**
  (`iterion remote admin platform-credentials`) so a tenant with nothing of
  its own no longer draws on it in silence — and the
  one-credential activation of the campaign bots' **cross-model plan
  review** (provision the codex OAuth forfait → `plan_review` resolves
  `on` at the next launch, nothing else to configure) — and
  `ITERION_CLOUD_REQUIRE_LLM_CREDENTIAL`, which refuses at publish (HTTP
  `422`, a board give-back) a run no tier can fund instead of queueing one
  that dies at its first call. Read it also when **connecting** a Claude
  forfait (a bare `claude setup-token` is accepted directly — the server wraps
  it as `credentials.json`, assuming a one-year validity the token does not
  carry, and fingerprints the TOKEN so a re-upload keeps one meter and its
  name), when asking **which key
  paid for a run** (the `cloudpublisher: … used/SKIPPED … fp=` lines, the only
  place the credential, the window and the reopening are named — a run's own
  error names none of the three), and before trusting a **fallback**: a Claude
  blob carries no account id, so one subscription connected twice is two
  fingerprints and two meters, and a fleet can look redundant while sharing a
  single provider window.
- [docs/web-search.md](../web-search.md) — sovereign web search tiers
  (SearXNG → Firecrawl) + the `ITERION_WEB_SEARCH` resolver.
- [docs/credential-pool.md](../credential-pool.md) — mutualising
  contributors' unused Claude/ChatGPT quota: the pledge (ceilings, sharing
  window, kill switch), the audience policy deciding who may draw, the
  fourth credential tier in `cloudpublisher`, and why the run's own
  `max_cost_usd` is the enforcement.
- [docs/mcp-server.md](../mcp-server.md) — driving iterion from an MCP
  client (Claude Code, desktop, Cursor): `iterion mcp` setup, the
  `local_*`/`remote_*` tool families, detached-launch semantics,
  `--read-only`, and the `remote_api` escape hatch.
- [docs/usage-caps.md](../usage-caps.md) — capping the LLM
  subscription below the provider's own wall (`ITERION_USAGE_CAP_*`:
  soft on the 5h window, hard on the weekly one), where the numbers
  come from, and the KEDA emergency brake. The percentages are also
  **runtime-mutable without a restart** (`iterion remote admin caps set
  --five-hour 80 --week 70`, super-admin; DB record over the env
  defaults, ≤30s propagation to both deployments, `/healthz` echoes the
  effective values — ADR-090). Read it when bots are eating the forfait
  an operator also works on — and when every claude_code run is refused
  at admission on a window the provider's own dashboard shows near 0%: a
  stored reading is trusted for `ITERION_USAGE_CAP_TRUST_WINDOW` (3h)
  after it was observed, not until its reset instant, and `iterion
  remote admin usage-readings clear <fingerprint>` forgets one
  credential's readings on the spot (a run refused pre-flight never
  refreshes them by itself).
- [docs/merge-gate.md](../merge-gate.md) — the required check's full life:
  the in-flight claim at launch, the verdict, and the two triggers that
  guarantee a dead review still answers (outcome event + 1-min sweep).
  Read it when a gate looks stuck — "absent", "pending forever", a synthetic
  `review died`, or a repair that posts nothing and says why in the logs.
- [docs/merge-policy.md](../merge-policy.md) — how a change reaches `main`:
  the merge queue, the required checks, and the admin bypass. Read it when
  **nobody can merge** — three required checks (`test`, `vendor-check`,
  `golangci`) run on the organisation's self-hosted `arc-runners` scale set,
  which sits outside this repository and was dead unnoticed for over a year,
  so the first thing to try is the `CI_SELF_HOSTED=off` repository variable
  (a variable, not a commit: repairing by merging does not work when merging
  is what is broken). Also carries why a queue entry is a full CI cycle
  against a 20-job organisation cap, and the trap that promoting an advisory
  job to required without deleting its `merge_group` skip produces a silent
  FALSE GREEN rather than a stalled queue.
- [docs/revi-billy-loop.md](../revi-billy-loop.md) — the Revi → Billy loop,
  **paused on THIS repo since 2026-09-15** (cost; see
  [docs/agents/review-and-merge.md](../agents/review-and-merge.md)): what
  `/billy` seeds (prior-review hand-off, push-back, ledger, gate), the session
  gotchas (don't touch the branch while he runs, pull after his push), and the
  dogfood duty (bilan per run). Read it before a deliberate `/billy` pass, and
  before re-arming the zero-touch lane.
- [docs/probes-and-graceful-shutdown.md](../probes-and-graceful-shutdown.md) —
  what `/healthz` and `/readyz` promise on the server AND the runner, the
  lame-duck window (`ITERION_SHUTDOWN_DELAY`) that keeps a deploy or an HPA
  scale-down from refusing in-flight connections, why only Mongo gates
  readiness (a critical check on a shared backend turns a blip into a
  fleet-wide outage), the startup probe that covers a slow cloud boot, and
  the `terminationGracePeriodSeconds` arithmetic. Read it on 502s during a
  deploy, a CrashLoop at boot, or a runner that reads `Ready` while the
  queue sits still.
- [docs/worktree-pool.md](../worktree-pool.md) — where a long-lived
  store's disk goes: `worktree: auto` parks a FULL checkout per run under
  `<store>/worktrees/`, a failed run keeps its own for inspection, and
  nothing but `iterion clean` ever came back for it (measured: 355 MB each
  on this repo, 32 of them = 12 GB in forty minutes, on a `/tmp` tmpfs =
  RAM, which killed the machine). Covers the runtime bound
  (`ITERION_WORKTREE_POOL_MAX`, default 8), why it spares dirty and
  resumable checkouts, and the `iterion clean` invocation for the rest.
  Read it on "the disk is full", or before pointing `--store-dir` anywhere.
- [docs/forge-security-read.md](../forge-security-read.md) — giving a bot
  org-wide **Dependabot alerts** read access: the `dependabot_tokens` team
  secret (JSON map org→token — the shape is the contract), the GitHub App
  path (add "Dependabot alerts: Read-only" + org approval + per-connection
  `security_read_enabled` PATCH, refresh worker keeps it minted) vs the
  hand-set fine-grained-PAT path, and the health/422 diagnostics. Read it
  when wiring vuln-watch (Senti) or when its run fails on "no Dependabot
  token". Covers the **coverage trap** — the org-wide endpoint returns only
  what the installation can see, so a `selected`-scope install is silently
  near-blind — and the **watch-only App** (`security_read_only`, manifest
  permissions REPLACED by `metadata`+`vulnerability_alerts` read) that makes
  an All-repositories install safe. Its connection carries
  `purpose: security_read`, which is what keeps the refresh worker from
  minting it a runtime token (that mint would 422 → degrade → withdraw the
  token it exists to supply) and keeps the publish resolver from picking it.
- [docs/browser-security.md](../browser-security.md) — what protects the
  studio from the BROWSER side: which cross-origin requests are accepted, what
  makes a session cookie unforgeable, what the CSP allows. Read it before
  touching `authMiddleware`, the auth cookies, the origin allowlist, or
  anything that adds a response header. Its load-bearing fact is that the two
  public hosts do NOT have the same properties: `gouv.fr` is a public suffix,
  so `iterion.fabrique.social.gouv.fr` is same-site with ~47 sibling hosts and
  `SameSite=Lax` buys nothing there — which is why the CSRF boundary is a
  single **origin gate** in `authMiddleware` (state-changing `/api` + a
  foreign `Origin` ⇒ 403; an absent Origin is the CLI/runner/webhook and
  passes) rather than the per-handler `requireSafeOrigin` that had drifted to
  70 of 247 routes. Covers the two things that do NOT protect a POST (a
  `text/plain` body is never preflighted; withholding ACAO only stops the
  attacker READING the response), the `__Host-` cookie prefix and why it is
  conditional (a browser DISCARDS one whose terms are unmet, so emitting it on
  a plaintext studio locks everyone out), the measured CSP (`script-src
  'self'` holds; `style-src` needs `'unsafe-inline'` for the CSS-in-JS), why
  HSTS is the ingress's job, and the no-CDN rule for the SPA. Read it also
  when a client is being **refused and you cannot see why**: the gate logs one
  **`INFO`** per refusal naming method/path/Origin — grep at `info`, not
  `warn`, or you reproduce the very false negative the line exists to kill
  ("nothing is being wrongly refused" and "we cannot see one" were the same
  empty grep). `warn` is deliberately NOT used: it would ride errtrack's hook
  into Sentry's 100-entry breadcrumb ring, and the gate runs before auth, so a
  stranger could evict everyone's error context. Also
  `ITERION_ALLOWED_ORIGINS`, which names extra hosts so a multi-host mount
  stops depending on the ingress forwarding `Host` unchanged — the
  proportionate widening next to `ITERION_REQUIRE_ORIGIN=0`, which switches the
  gate off. It is a **first-party** trust grant, though: the same list also
  governs WebSocket upgrades and ACAO reflection, so a partner origin does not
  belong in it.
- [docs/platform-bots.md](../platform-bots.md) — iterating on any bot
  (incl. natives) on a cloud instance WITHOUT an image rollout: the
  platform bot-override tier (`iterion remote admin bots push bots/<slug>`,
  botsource rows under the `platform:` sentinel, resolution team →
  platform → baked at every launch surface, the runner's by-ref rebuild +
  version-drift guard, digest-audited), plus the runtime-mutable webhook
  role bots (`admin roles set --reviewer …`) and `sandbox: auto` default
  image (`admin sandbox set --default-image …`, pinned per RunMessage).
  Read it when a bot tweak seems to need a deploy, when a push must be
  reverted, or when a run fails on "version drift". Covers the **engine
  contract** a bundle may declare (`requires: { iterion: ">= X.Y.Z" }` in
  its manifest — [docs/bundles.md](../bundles.md#requires--the-engine-contract)):
  `push` refuses `409` when the deployment's floor (min of the server's
  build and the runner builds observed on recent runs) is below it,
  `--force` overrides loudly, the launch refuses with a terminal
  `BOT_REQUIRES_NEWER_ENGINE`, and `iterion validate` says the same
  locally (C250/C251). Read it when a push is refused, or when a bot that
  compiles dies at its first expression.
- [docs/dispatcher.md](../dispatcher.md#claim-lease--watchdog-native-board-adr-096) —
  the board **claim lease + watchdog** (ADR-096,
  `ITERION_BOARD_CLAIM_REAPER`, default off): the fenced leased claim
  (`claim_epoch`/`claim_lease_until`, heartbeat, owner-scoped CAS
  writes), the periodic cross-host reaper that reclaims expired leases
  by TRANSFER and routes the card by `DecideStuckCard`, the
  terminal-state sink + operator `Reopen`, and the two-release
  expand/contract rollout. Read it when a native-board card is stuck
  `in_progress` with a dead owner, or before enabling the reaper. Read it
  also on the OTHER native-board silence — **a card written on disk that
  `/board` and the dispatcher never show**: the store's index rides an
  inotify watch, and inotify is lossy by construction (`ENOSPC` at
  `fs.inotify.max_user_watches`, `EMFILE` at `max_user_instances`,
  `ErrEventOverflow` on a full kernel queue, a watch silently dropped when
  the directory is removed or renamed). Each of those used to freeze the
  index until the daemon restarted; each now falls back to a full
  `issues/` rescan every `ITERION_NATIVE_INDEX_RESCAN` (default `2s`,
  `off` to disable), taken outside the store mutex. The log names which
  mode a store is in — that line, not the board, is what tells you the
  fast path is gone.
- [docs/github-board-sync.md](../github-board-sync.md) — making a GitHub
  **Projects v2** board and the native board the same tickets (ADR-097): the
  permissions (App `organization_projects`, PAT `project`), `iterion issue
  import --project` locally vs `iterion remote board bind` on cloud, the
  operator-replaceable `--status-map`, the reconciliation interval + its one
  log line per pass, and the conflict rule. Read it when a card moved on one
  board and not the other — the three answers are almost always an unmapped
  state (inert by design), a terminal card (automation never reopens), or a
  card the project pass could not create because that repo's **issue sync**
  had not run (it names the repos). On cloud that sync is the server's own —
  `iterion remote forge integrations sync <id>` / `sync_issues_enabled` — not
  `iterion issue import`, which writes to a local store the instance never
  reads.
- [docs/ticket-context.md](../ticket-context.md) — **also the tenancy and
  governance runbook**: one org = the client, one team = one product team,
  team admins self-serve while org admins govern. Read its *Org governance
  controls* section for the provisioning approval queue and what it is FOR
  (`all` vs `shared_credentials` — a team spending its own BYOK answers to
  nobody, so only teams funded by a shared tier are parked), the per-team
  caps, the shared-credential audience, and the **team lifecycle**
  (`iterion remote teams update|status|delete|add-member` — rename, suspend,
  delete an empty team, and place an account that already exists instead of
  emailing it an invitation). Read *Moving a repo to another team* before
  splitting a tenant: the provisioner rebuilds the managed forge secret and its
  `forge_token` binding on the target, and **nothing else keyed on `Team.ID`
  follows** — an operator secret, a `tracker_token` binding, a config-share, a
  schedule, `sync_issues_enabled`. The launch that needed one fails mute, since
  a missing binding reads exactly like a feature nobody configured. Plus its
  original subject: plugging tracker
  tickets (Jira Cloud/DC, GitHub/GitLab issues) into a Revi review so it
  verifies the PR delivers what the ticket asks: the team wiring (team
  secret → `tracker_token` binding with `allowed_hosts` → per-repo
  `tracker_api_base` launch_var), the one-credential-per-team limit, and
  the org governance layer (provisioning approval queue +
  org-admin-delegated per-team caps). Read it when a team asks "can the
  reviewer check the code against our tickets?" or when a provisioning
  request seems stuck "awaiting org approval".
- [docs/outcome-router.md](../outcome-router.md) — the
  `ITERION_OUTCOME_ROUTER` switch: how a policy-carrying terminal run is
  decided by its launch-frozen contract (merge/relaunch/escalate), the
  activation watermark that keeps a flip from retro-routing 24h of
  history, the decision registry (lease, attempt cap,
  `GET /api/runs/{id}/route-decisions`), the `route_escalated` /
  `route_action_failed` ops alerts, and the rollout + emergency-stop
  procedure. Read it before flipping the switch on a deployment.
- [docs/sentry-feedback-loop.md](../sentry-feedback-loop.md) — reading
  production errors BACK from the platform Sentry (org `incubateur`, project
  `iterion`/62 on sentry2): the user-auth-token setup, the repo's `.mcp.json`
  (iterion + Sentry MCP servers, `SENTRY_ACCESS_TOKEN` env), raw-API recipes,
  and the error-watch sentinel design (detect→card→fix→resolve). Read it to
  triage a prod crash or wire an agent session to live errors.
- [docs/observability.md](../observability.md) — process logs, error
  tracking and tracing: the env vars (`SENTRY_DSN`, `SENTRY_ENVIRONMENT`,
  `SENTRY_TRACES_SAMPLE_RATE`, `ITERION_LOG_FORMAT`, `ITERION_LOG_LEVEL`),
  which surfaces default to JSON, what a Sentry/GlitchTip project receives
  (panics, fatal exits, error logs, run alerts — plus, when the sample rate
  is set, one transaction per API request and per in-process LLM call), the
  scrubbing, and the smoke tests. Read it when a deployment needs to answer
  "what crashed, how often, since which release" or "where did the time go".
- [docs/assistant-dock.md](../assistant-dock.md) — the studio assistant:
  which bot answers it (the manifest `chat:` block IS the registry — a
  second conversational bot is a bundle, not a studio release), the
  page-context chip, dragging a run/card/bot onto the composer, and
  assistant-vs-steering on a run page. Read it before adding a chat
  surface, or to know which bot answers where: Nexie owns `/whats-next`
  and only that route, the dock everywhere else is Copi.
- [docs/models.md](../models.md) — the model registry (`iterion models`,
  `GET /api/models`: known × usable × capabilities × pricing), the launch-time
  model/backend/effort overrides, and how to change the studio assistant's
  model (persisted per user; fixes "which model is this running on" and
  "the assistant feels dumber").
- [docs/brand.md](../brand.md) — the iterion-bot mascot as iterion's face:
  the asset pipeline (`assets/brand/` masters → `task brand:gen|check|og`
  regenerate favicons, the desktop icon, the docs logo, `pkg/brand`), and
  which forge identity gets the avatar how — automatic on a GitLab account
  flagged `bot` (`PUT /user/avatar` at connect time), on request for a
  dedicated Forgejo/GitLab account (`iterion remote forge connections
  avatar <id> [--force]`), never on an OAuth connection, by hand on a GitHub
  App (no logo API; the studio hands over the file + the settings page).
  Read it when a bot posts with a default avatar, or before touching a logo.
- [docs/bot-bundle-snapshots.md](../bot-bundle-snapshots.md) — cloud launches
  freeze workflow, resources and sibling subbots through the server authority;
  queue v13, bounded immutable snapshot transport, strict runner resolution and
  resume semantics. Read before changing cloud bundle or subbot resolution.
- [docs/cloud-deployment.md](../cloud-deployment.md#verifying-that-an-infra-apps-push-landed-argocd-sync-liveness) —
  how a build reaches a deployment: the server follows the moving `:edge`
  tag (a `rollout restart` picks it up), the runner is pinned BY DIGEST in
  the deployment's values (an explicit bump per engine build), the
  generation-aware rollout epoch, and how to verify an infra-apps push
  actually landed (Deployment generation, never pods — the ArgoCD stall of
  2026-09-05 sat 2h30 until the next push). Read it on "I pushed the config
  and nothing happened", or before shipping an engine fix to the runners.
