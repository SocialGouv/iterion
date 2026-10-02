# Operational runbook index — capture what a session cost you to discover

The discovery entry point for "how do I configure / operate / debug X on
iterion". Each entry names **when to read it**, which is its whole value: an
agent that does not know the runbook exists cannot look it up. The detail
lives in the target doc, never here.

Routed from [the tree](../README.md). Extend the index in the same change that
adds the runbook.

## Operational-knowledge reflex

When a session burns real time **discovering how to configure or operate
iterion**, that discovery MUST land back in the repo, across three surfaces by
role: the runbook's **content** in `docs/`, its **"read it when" line** in the
index below (same change), and — only if an agent would never find it
otherwise — a **discovery trigger** skill. The root,
[AGENTS.md](../../../AGENTS.md), takes at most one line. Ground every addition
in the commands/paths that actually worked.

## Credentials & spend

- [cloud-llm-credentials](../../cloud-llm-credentials.md) — provisioning a cloud run's LLM credential: BYOK vs OAuth-forfaits, org/platform tiers, `--keys-first`/`--facade-default`, connecting a Claude forfait, "which key paid for a run", `credentials preview`. Read on 401/429 on cloud runs.
- [usage-caps](../../usage-caps.md) — capping a subscription below the provider's wall; runtime-mutable caps; a reading is trusted 3 h, not until its reset instant, and `usage-readings clear` cuts it. Read when bots eat the operator's forfait or runs are refused at admission.
- [credential-pool](../../credential-pool.md) — mutualising contributors' unused quota: the pledge, the audience policy, why `max_cost_usd` enforces.
- [web-search](../../web-search.md) — sovereign search tiers and the `ITERION_WEB_SEARCH` resolver.
- [models](../../models.md) — the model registry, launch-time overrides, the studio assistant's model.
- [current-bot-models](../../current-bot-models.md) — catalog defaults, CLI pins, live rollout of bot model vars.

## Merge, CI & gate

- [merge-policy](../../merge-policy.md) — the queue, required checks, admin bypass; on "nobody can merge", first try the `CI_SELF_HOSTED=off` repo variable. See also [measuring the queue](../../merge-policy.md#measuring-the-queue) (`task ci:queue-stats`) and [releases and the queue](../../merge-policy.md#releases-and-the-queue).
- [merge-gate](../../merge-gate.md) — the required check's full life; read when a gate looks stuck or a repair posts nothing.
- [revi-billy-loop](../../revi-billy-loop.md) — the Revi → Billy loop, **paused on this repo** ([why](../workflow/billy.md)); read before a deliberate pass or a re-arm.
- [auto-maintenance](../../auto-maintenance.md) — dependency loops nobody watches; read before arming `arm_automerge`, or when a loop "is configured" and merges nothing.
- [Revi's positive/negative audit](../../bot-runs/review-pr.md#2026-09-14--falsifiable-claw--gpt-review-proof-1203) — when a clean review looks suspicious.

## Cloud ops

- [cloud-deployment](../../cloud-deployment.md#verifying-that-an-infra-apps-push-landed-argocd-sync-liveness) — how a build reaches a deployment (server follows `:edge`, runner pinned BY DIGEST); verify by Deployment generation, never pods.
- [probes-and-graceful-shutdown](../../probes-and-graceful-shutdown.md) — `/healthz`/`/readyz` promises, the lame-duck window, why only Mongo gates readiness. Read on 502s during a deploy or a CrashLoop at boot.
- [platform-bots](../../platform-bots.md) — iterating a bot on cloud without an image rollout; the engine contract `requires:`; read on "version drift" or a refused push.
- [bot-bundle-snapshots](../../bot-bundle-snapshots.md) — cloud bundle/subbot resolution; read before changing it.
- [outcome-router](../../outcome-router.md) — the terminal-run routing contract; read before flipping `ITERION_OUTCOME_ROUTER`.
- [url-layout](../../url-layout.md) — which paths answer at the origin root vs `/studio`; read when a link lands somewhere unexpected.
- [observability](../../observability.md) — logs, error tracking, tracing: what a Sentry/GlitchTip project receives, the smoke tests.
- [sentry-feedback-loop](../../sentry-feedback-loop.md) — reading prod errors back from Sentry; triaging a crash, wiring an agent to live errors.

## Security & tenancy

- [browser-security](../../browser-security.md) — the studio's browser-side protections; the `gouv.fr` public-suffix fact behind the origin gate; refusals log at **`info`**, not `warn`. Read before touching auth cookies, origins or headers.
- [forge-security-read](../../forge-security-read.md) — org-wide Dependabot read for a bot; the coverage trap and the watch-only App. Read when wiring vuln-watch or on "no Dependabot token".
- [ticket-context](../../ticket-context.md) — tracker tickets in a review, **plus org/team governance**: provisioning approval, team lifecycle, "an account signs in and sees nothing", moving a repo between teams.

## Board & dispatch

- [board-epics](../../board-epics.md) — epics on the GitHub board: which view answers what, membership carried twice; re-sending options without their `id` silently clears them.
- [github-board-sync](../../github-board-sync.md) — GitHub Projects v2 ↔ native board; on a card moved on one side only: unmapped state, terminal card, or issue sync that never ran.
- [dispatcher](../../dispatcher.md#claim-lease--watchdog-native-board-adr-096) — the claim lease + watchdog (ADR-096), and the lossy-inotify index fallback. Read on a stuck `in_progress` card or a card the board never shows.

## Local runtime & discovery

- [repo-map-and-graph](../../repo-map-and-graph.md) — "where is… / what breaks if…" before grepping; the committed maps and `iterion map`.
- [state-of-the-art](../../state-of-the-art.md) — how *proven* each surface is.
- [worktree-pool](../../worktree-pool.md) — where a long-lived store's disk goes (`worktree: auto` parks a full checkout per run, 355 MB each here); read on "the disk is full" or before pointing `--store-dir`.
- [resume](../../resume.md#when-the-final-bank-push-fails) — final-bank retries and recovery evidence.
- [connector-identities](../../connector-identities.md) — connector regeneration identity locks and recovery after an interrupted replacement.
- [mcp-server](../../mcp-server.md) — driving iterion from an MCP client; `--read-only`, the `remote_api` escape hatch.
- [assistant-dock](../../assistant-dock.md) — which bot answers the studio assistant where (Nexie owns `/whats-next`, Copi the dock).
- [brand](../../brand.md) — the mascot assets and which forge identity gets the avatar how.
- [groups-iteration-subbots](../../groups-iteration-subbots.md#child-bundle-resources) — child skills/devbox missing or leaking into a parent.
