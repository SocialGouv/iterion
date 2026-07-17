[← Bot runs index](README.md)

# feed-watch (Vigie) — run log

Newest first. One section per dogfooded run.

## 2026-07-17 — Cloud rollout: native scheduler on prod, veille runs on ephemeral runners

- Status: **validated (production cloud)** — the veille now runs on the
  `iterion.fabrique.social.gouv.fr` (ovh-prod) cloud instance via the NATIVE
  scheduler, against a git-backed workspace, posting to the prod Mattermost
  channels. Both modes proven end-to-end on a real cloud run.
- Versions: bot 1.0.0 (+3 cloud fixes) · iterion prod `33f6f425e`.
- Method: a new engine feature (#219) makes `cloudsched` run a stateful,
  repo-bound bot; the veille config + state live in a private repo
  (`github.com/SocialGouv/iterion-veille`), the runner clones it (forge_token) and
  the bot commits + pushes state each run (`state_commit=true`). Team-secrets
  (`webhooks` = the 3 prod Mattermost URLs read by-reference from the ovh-prod
  Huginn; `forge_token` = a fine-grained GitHub PAT) resolve by NAME for the bot
  (no bot-binding needed — `teamByName` is a resolution tier). 10 schedules
  registered on the Ministères-Sociaux team via the new
  `/api/teams/{id}/schedules` API.
- Result (proven on iterion-veille's git history):
  - `chore(feed-watch): collect +165 item(s)` @08:15Z — a scheduled run cloned
    the private repo, collected cyber feeds (zero-LLM), pushed state. Validates
    scheduler → clone → run → push.
  - `chore(feed-watch): digest cyber (165 item(s))` @10:00Z — a scheduled digest
    synthesized (claude_code + the team's Claude Code OAuth forfait), posted to
    the prod Mattermost fan-out, cleared the queue, pushed state. Since
    `commit_state` is gated `when posted`, the commit PROVES the Mattermost post.
- Engine feature shipped: **native cloud schedules for stateful repo-bound bots**
  (#219) — `ScheduledBot.RepoURL/RepoRef` threaded onto the scheduled LaunchSpec
  (the runner then clones + auth-pushes, same mechanics as the webhook path) +
  a team-scoped schedule CRUD API + `iterion remote schedules`. This closes a
  real gap: before it, the cloud scheduler fired bots against no repo, so no
  stateful bot could run on cloud.
- Three bot adaptations found by validating on cloud (each invisible on host):
  - **#220** — `git push` of state is now rebase-retry safe: concurrent cloud
    runs clone independently and race on push; a losing push left an uncleared
    queue → a duplicate digest next run. Each run touches a disjoint per-category
    subdir, so rebase auto-merges.
  - **#221** — dropped `capabilities: [board.create, board.read]` from
    `synthesize`: a declared capability forces the `iterion_board` MCP server
    active on every run; it is NOT active on a cloud runner, so the node failed
    at setup even with `post_to_board=false`. The digest's sink is the chat
    webhook; the speculative board-card path is removed.
  - **#222** — pinned `backend: claude_code` (was auto-detected): on a cloud
    runner with a Claude Code OAuth-forfait credential, auto-detect picked
    `claw`, which refuses the forfait as a third-party-SDK CGU violation.
    `claude_code` is the only backend allowed to use the forfait; overridable
    via `FEED_WATCH_BACKEND=claw` + an OpenAI key.
- Lessons for next run: **validating a bot on CLOUD reveals specs invisible on
  host** — the board MCP is host/studio-wired, backend auto-detection differs by
  credential environment, and runners are ephemeral (state must be git-backed,
  pushes must be concurrency-safe). Any bot destined for scheduled cloud runs
  should be validated there, not only on host. The `iterion runs prune` retention
  (2026-07-16) does not cover cloud (Mongo TTL on events only) — cloud run
  documents persist; a cloud retention pass is a separate follow-on.

## 2026-07-16 — Full production rollout: 2 Huginn scenarios, 9 categories, 6 live posts, schedules wired

- Status: **validated (production)** — the reference self-host veille is live on a
  real Mattermost channel and cron-scheduled.
- Versions: bot 1.0.0 · iterion `59b8f73a1` (post #213/#215/#217).
- Method: workspace `~/lab/fabrique/veille/`, config-driven. Collect zero-LLM on
  host python3; digest via claude_code (auto-detected, CLI default model), host
  (non-sandbox) runs, `--store-dir` = operator studio store. Webhook secret
  fetched by-reference from the live Huginn dev DB (rails runner → the single
  `mattermost_webhook_url_dev` credential → `iterion secret set webhooks`).
- Result:
  - **Scenario 1 (Veille Technique)** — 5 categories (cyber, ia, tsjs, gopyrust,
    java) all posted to `#huginn-dev`. Cyber validated by the operator ("stylé!");
    ia/tsjs/gopyrust/java posted in one run-complet (207/134/113/43 items each).
  - **Scenario 2 (Veille Design UX/UI)** — imported from Huginn scenario 12 (24
    agents) as 4 categories (design-sp, ux-metier, design-systems, a11y). Collect
    populated all four (85/168/53/67 items); a11y digest posted as the design
    validation (67 items cleared).
  - **Schedules installed** — 10 veille cron entries via `iterion schedule`
    (collect 2×/day; tech digests Mon 08:00; design digests Wed 08:00 — Huginn
    cadences), on `~/.local/bin/iterion` (see "installed binary" below).
- Value: **Huginn veille replaced by a single ~600-line bot + a JSON config**,
  qualitatively above the Huginn baseline — same-story grouping across sources,
  `web_fetch` of the lead articles (not just RSS titles), CERT-FR avis linked,
  semantic dedup against prior digests, explicit overflow reporting. Adding a
  category or a whole new veille is a config edit, no bot change.
- Findings / misses:
  - **WebsiteAgent HTML scrapes not ported** — the design scenario had 3
    non-RSS sources. `nldesignsystem.nl` was recovered via its `/blog/rss.xml`;
    `design.numerique.gouv.fr/articles` and `zeroheight.com/blog` (Next.js SPA)
    have no feed and are unported (noted inline in the config). feed-watch collect
    is RSS/Atom/RDF-only; HTML scraping is a future collect-source (the Firecrawl
    plugin already exists engine-side).
  - **Transient `Bash Exit code 1/2` inside claude_code synthesis** — the
    synthesize agent pokes at its input (grep/python one-liners on
    `post_to_board`/`items_count`) that occasionally exit non-zero; it recovers
    and every digest still posted. Worth a follow-up: tighten the synthesize
    system prompt / input schema so the agent doesn't shell out to inspect its
    own structured input.
- Engine hardening surfaced by this rollout:
  - **`iterion runs prune`** (#213) — the local store had no retention; recurring
    schedules made unbounded growth real. Terminal-status prune, worktree-safe.
  - **prune survives unreadable run dirs** (#215) — found smoke-testing prune on
    the real 244-run operator store (a partial/crashed run dir sank the sweep).
  - **`as: file` secrets on host runs** (#217, the big one) — file secrets only
    materialized inside a sandbox (`/run/iterion/secrets/` bind-mount). On a host
    run — exactly what `iterion schedule`/cron does — `{{secrets.X.path}}` pointed
    at a dangling container path, so the deterministic `notify` step 404'd every
    time. The executor now materializes file secrets to a per-run host tempdir
    (0700/0600, `sync.Once`, gated on `e.sandbox == nil`, cleaned on Close).
    Without this, NO secret-bearing bot could run under cron.
  - **`model: "{{vars.x}}"` literal-passthrough on claude_code** — resolved on
    claw but reaches the CLI verbatim on the delegation path (board native:73bfb3b4);
    worked around with the env form `${FEED_WATCH_MODEL:-}`.
  - **`worktree: auto` is the engine default** — fatal for a state-bearing bot;
    feed-watch declares `worktree: none` (authoring lesson for any stateful bot).
  - Design fix: only a **delivered** digest consumes the queue
    (`notify -> commit_state when posted`) — a dry-run must not eat the queue.
- Installed binary: the schedules run `~/.local/bin/iterion` (a fresh static
  build), deliberately NOT `/usr/bin/iterion` (v0.31.0, root-owned, months stale —
  refreshing it needs sudo, an operator step). Using `~/.local/bin` sidestepped
  the sudo gate; the ephemeral worktree binary would have been wrong to freeze
  into cron lines.
- Lessons for next run: the transient synthesize-Bash noise is the one rough
  edge to smooth; consider a Firecrawl-backed collect source to close the
  HTML-scrape gap; the veille currently posts dev+prod both to `#huginn-dev`
  (dev webhook) — the Huginn prod scenario fans out to `#veille-huginn-*` +
  `mattermost2_*`, portable by adding those webhook names to the `webhooks`
  secret and the config sinks when cutting over.

## 2026-07-16 — Huginn veille port: first full cycle (runs 019f699d / 019f699d-d407 / 019f69a1)

- Status: **validated** (collect ×2 + digest dry-run end-to-end on the real
  fabrique feeds) — real Mattermost post pending the `webhooks` secret
  (operator-only credential).
- Versions: bot 1.0.0 · iterion worktree `worktree-feed-watch-veille`
  (base dcaea1ab8 + feed-watch + runs-prune).
- Method: workspace `~/lab/fabrique/veille/` (config ported from
  `infra-apps/huginn/scenarios/veille-tech-dev.json` — 36 feeds, 5 catégories,
  briefs éditoriaux FR repris des prompts Huginn). Collect on host python3
  (zero-LLM, no credential). Digest: backend auto-detected → claude_code
  (CLI default model), `dry_run=true`, budget defaults (3 USD / 30m),
  `--store-dir` pointed at the operator studio store.
- Result:
  - collect #1: **623 items** across 33/36 feeds, 0 dup (bootstrap);
    3 dead/rate-limited feeds (threatpost, 2× hnrss intermittents)
    surfaced in the summary, non-fatal by design.
  - collect #2 (immediate): **0 re-ingested — 623/623 deduped**; 20 new =
    the hnrss feed that failed in #1 catching up (wanted behavior).
  - digest cyber: 165 queued → 150 in working set (15 overflow, surfaced
    in the message) → **79 items retained**, 45 393 tokens, 7m12s,
    10 952-char French digest; notify dry-run prepared 1 payload for
    `mattermost_dev → #huginn-dev`, delivered nothing.
- Value: the digest is qualitatively ABOVE the Huginn baseline — grouped
  multi-source stories (CERT-FR + BleepingComputer + THN folded into one
  entry with secondary links), actionable takeaways (patch versions, CVE
  ids, CISA deadline), correct 🔴/🟠/🟡 classification per the editorial
  brief, dated headline. Overflow explicitly reported.
- Findings / misses: none functional. Watch: first-ever digest is a
  bootstrap (150-item working set) — later daily digests will be ~10-30
  items; hnrss endpoints rate-limit intermittently (self-heal at next
  poll proven).
- Engine hardening surfaced by this run:
  - `worktree: auto` is the ENGINE DEFAULT — deadly for a state-carrying
    bot (gitignored state written in the run worktree is discarded at
    finalization; the first attempt also hard-failed on a
    zero-commit workspace: `git worktree add … invalid reference: HEAD`).
    Fix shipped: feed-watch declares `worktree: none` with a rationale
    comment. Authoring lesson: any bot whose product is workspace state
    must opt out explicitly.
  - `model: "{{vars.model}}"` is resolved on the claw path
    (examples/clarify) but reaches the claude CLI **literally** on the
    delegation path → node fails with "model {{vars.model}} unavailable".
    Board card native:73bfb3b4 (uniform resolution or a compile
    diagnostic). Workaround shipped: env form `${FEED_WATCH_MODEL:-}`.
  - Design fix from the dry-run: commit_state initially consumed the
    queue on ANY digest — a dry-run silently ate 165 items. The graph now
    gates it (`notify -> commit_state when posted`): only a DELIVERED
    digest consumes the queue / writes the archive; covered by an e2e
    subtest.
- Lessons for next run: set the `webhooks` secret then re-run the cyber
  digest for real (queue refilled with 165 items); wire the schedules
  AFTER the branch lands on main (paths in veille README); pair with
  `iterion runs prune` (new CLI) for retention; consider
  `--var post_to_board=true` on cyber once the team wants CERT-FR
  criticals as cards.
