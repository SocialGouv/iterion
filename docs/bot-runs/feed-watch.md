[← Bot runs index](README.md)

# feed-watch (Vigie) — run log

Newest first. One section per dogfooded run.

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
