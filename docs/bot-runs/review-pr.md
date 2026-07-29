# Revi — `review-pr` run bilans

Read-only code reviewer. Revi reviews with one selected family by default
(`review_mode: mono`) or independent Claude + GPT reviewers when dual mode is
explicitly selected; findings are normalised/de-duplicated, and one issue per
finding is published to the native board (label `source:revi`). With `--var
pr_url` it also posts an inline forge review and an optional deterministic
commit-status gate. Never edits or commits. See
[bots/review-pr/](../../bots/review-pr/).

## 2026-07-08 — GitHub PR webhook e2e on iterion cloud prod

- Status: **validated — full end-to-end via the inbound webhook.**
- Versions: bot review-pr 0.2.0 (post the `emit`→`converge` rename below) · iterion cloud prod `:edge` @ 93bc604+
- Method: cloud prod (ovh-prod). Connected a GitHub forge (PAT) on a fresh team,
  enabled Revi on a test repo (`SocialGouv/iterion-e2e-mathkit`), opened a PR
  with an intentional defect (`subtract` skipping the module's `assertFinite`
  input-validation invariant). The `pull_request` webhook launched Revi on a
  cloud runner (no sandbox).
- Result: both reviewer families ran (reviewer_claude/claude-code +
  reviewer_gpt/gpt-5.5), `converge` merged them, `publish_review` posted a GitHub
  review (COMMENTED) — "2 findings (1 medium, 1 low; 1 cross-confirmed)" + 2
  inline comments (src/calc.mjs:26 medium correctness, test/subtract.test.mjs:8
  low tests). Both families independently caught the planted defect;
  cross-confirmation worked.
- Engine hardening surfaced by this run:
  - **The bot didn't parse in prod** (`agent emit:` shadowed the reserved `emit`
    node keyword, ADR-051 → E002 → webhook 502). Fixed by renaming the node to
    `converge`; added a CI guard (`TestCatalogBotsParseAndCompileClean`) that
    fails on any catalog bot that doesn't parse+compile — the gap that let it
    ship (both catalog-loading tests skipped on parse failure).
  - **Webhook idempotency poisoned by a failed launch**: the initial `opened`
    delivery 502'd but still consumed the idempotency key, so redeliveries
    returned `duplicate` (empty run_id) forever. Fixed: a StatusLaunchError row
    is now retryable. Only a NEW head sha (close/reopen after a push) unblocked
    the validation.
- Lessons for next run: `synchronize` does NOT re-trigger Revi by design
  (opened/reopened only) — to re-review, close/reopen or push a new head sha.
  Revi posts as the PAT's account (`devthejo` here); a dedicated bot account
  would read cleaner.

## 2026-06-13 — review the campaign diff (run 019ec0e8)

- Status: **validated — high value.**
- Versions: bot review-pr 0.2.0 · iterion 7fea84cd (binary refreshed mid-campaign)
- Method: `POST /api/runs`, `base_ref=9197bcfd` (review the campaign's own fresh
  commits `9197bcfd..HEAD` — the `scan_shards`/`botregistry` fixes + the bilans),
  `severity_threshold=low`, `post_to_board=true`. Read-only, no sandbox. Backends:
  `claude_code` (reviewer_claude, emit) + `claw` gpt-5.5 (reviewer_gpt). ~37k tokens,
  ~$1.18, 151 steps, status `finished`.
- Result: `diff_precheck` (found changes) → fan-out **reviewer_claude ‖ reviewer_gpt**
  (parallel, confirmed) → `emit` → **1 deduped board issue** (`source:revi`,
  `severity:medium`, `type:correctness`). No commits (read-only, as designed).

### Value (genuinely high — caught a real second-order bug)
- The single finding is excellent: **"Cloud request-construction failures block until
  shard timeout" at `cmd/iterion/scan_shards.go:458`** — i.e. Willy's fix `4c525a6e`
  (handle the dropped `http.NewRequestWithContext` error) is *masked* by `awaitTerminal`,
  which polls a run document that never exists for a never-launched shard, hanging until
  `--timeout` (default 2h) instead of failing fast. Precise anchor, correct mechanism,
  actionable fix sketch. **Verified against the code and fixed** (`59cfedcc`, with a
  regression test). The pre-existing `ITERION_SERVER_URL`-unset / read-workflow paths
  had the same latent hang.
- **No noise:** the diff was mostly docs (≈280 of 387 lines) + two small code changes;
  Revi flagged 0 in the clean botregistry dedup, 0 in docs, and 1 real issue in the
  changed Go. Cross-family dedup worked; severity/type/confidence labels are clean.
- **Dogfood dynamic worth keeping:** a *breadth* bot (Revi) caught an incompleteness in
  a *depth* bot's (Willy) committed fix. Running review-pr over each loop bot's output is
  a cheap, high-signal second line of defence.

### Findings / misses
- The finding came from the **gpt** reviewer only (confidence `medium`) — Claude's
  reviewer didn't independently raise it. Single-family findings are real but lower-
  confidence; the cross-family agreement signal didn't fire here (still correctly
  published at the `low` threshold). No false positives.
- Minor: the `emit`/`reviewer_*` node outputs aren't surfaced in `run.json.checkpoint`
  in a easily-parsed shape (had to read the board to see findings) — cosmetic.
- **Repo scatter (low — repo-agnostic):** `report_path` defaults to
  `.review-pr/findings.md`, so Revi drops an **untracked `.review-pr/` dir into the
  target repo root** (not gitignored). Per CLAUDE.md "Catalog bots are repo-agnostic",
  a default that writes into the target tree should be gitignore-friendly. Fixed here by
  adding `.review-pr/` to iterion's `.gitignore`; for a pure dry-run pass
  `--var report_path=/tmp/revi-findings.md`. (A nicer bot-side default would append the
  dir to the target's `.gitignore`, or write under a path the operator already ignores.)

### Engine hardening
- `awaitTerminal` pre-dispatch-failure hang — **fixed `59cfedcc`** (+ regression test
  `TestAwaitTerminal_PreDispatchFailureDoesNotHang`). Directly attributable to this run.

### Lessons for next run
- Revi is a strong, low-noise read-only reviewer; point `base_ref` at the commit before
  the work to review a clean range (`base..HEAD`). Default `post_to_board=true` lands one
  issue per finding under `source:revi` — fine for real triage, set `false` for a pure
  dry-run.
- Use Revi as a routine second pass over Willy/Featurly/Billy output — it catches
  second-order issues the implementer's own review loop can miss.
