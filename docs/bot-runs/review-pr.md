# Revi — `review-pr` run bilans

Read-only code reviewer. Revi reviews with one selected family by default
(`review_mode: mono`) or independent Claude + GPT reviewers when dual mode is
explicitly selected; findings are normalised/de-duplicated, and one issue per
finding is published to the native board (label `source:revi`). With `--var
pr_url` it also posts an inline forge review and an optional deterministic
commit-status gate. Never edits or commits. See
[bots/review-pr/](../../bots/review-pr/).

## 2026-07-31 — the Revi→Billy hand-off is dead on the cloud path (runs 019fb9bc / 019fb9c6)

- Status: **partial — Revi validated end to end, the hand-off to the fixer proved
  non-functional in cloud.** The defect predates the declarative rework: the
  shipped `stampPriorReview` read the same artifact.
- Versions: review-pr 0.5.7 · branch-improve-loop 1.1.0 · iterion cloud prod
  v3.17.7 @ `af787562c` (verified to contain the hand-off work)
- Method: `SocialGouv/iterion-test-appy-e2e` PR #2, seeded with a real module and
  three planted defects (unsynchronised map written from a `Warm` fan-out;
  a failed fetch cached for the whole TTL; a loop-variable capture that is NOT a
  bug under the declared `go 1.22`). Revi auto-launched on PR open; `/billy` by
  comment afterwards. Repo provisioned with both bots, `gate_context` pinned.

### What worked, verified on the forge

- **Revi found the real bugs and refused the planted false positive.** critical:
  the concurrent map access, *"reproduced empirically … crashed in 4 of 5 runs"*;
  high: the cached failure, *"call 1 returns connection refused, call 2 returns
  body=\"\" err=<nil>"*. The loop-capture did **not** become a finding — it went
  to `questions` with the reason (`go.mod` declares 1.22, per-iteration
  semantics, verified empirically). The falsifiability channel did its job.
- **Stable finding ids are live**: `Ra34eca`, `R1dce3f`, plus the arbitration
  line the review now carries — *"Fix them yourself, or comment /billy … adding
  e.g. skip Ra34eca and your reason leaves that one alone."*
- A `replacement` was produced and rendered ("Proposed replacement:").
- The gate landed: `revi/review = FAILURE` on the head.
- Mono topology reported honestly, no cross-confirmation claimed.

### The defect: cloud runs persist no node artifacts, so nothing can be handed over

`/billy` launched with the right PR context (`pr_url`, `head_sha`,
`push_branch`) and **`prior_review` empty**.

Root cause, from prod: `GET /api/runs/<id>/artifacts` returns `[]` for **every**
run checked (Revi, Billy, an unrelated pi smoke), and the event log — 89 events,
`node_finished` ×9 — contains **zero `artifact_written`**. The hand-off resolves
through `LoadLatestArtifact(runID, <producing node>)`, so it has nothing to read.

This is not a regression of the declarative rework. The version it replaced read
`LoadLatestArtifact(runID, "converge")` the same way, so the `/billy` seed has
never worked on a cloud run — it was documented as shipped and, as far as this
run shows, never exercised live. It works in-process (the filesystem store the
unit tests use returns the artifact), which is why every test is green.

### Lessons for next run

- **A hand-off that reads artifacts needs the artifacts to survive the runner.**
  Before any further work on the review→fix chain, settle where a cloud run's
  node outputs live and make the producer read that. The checkpoint carries node
  outputs and IS synced; it is the obvious candidate.
- The forge identity matters: the first `/billy` was refused *"self comment
  (loop-guard)"* because the repo was provisioned on a **PAT connection whose
  account is the operator's own**. Re-provisioning onto the GitHub App
  connection fixed it. The guard was right; the provisioning was the mistake.
- A freshly provisioned repo shows `auto_fix_on_gate_failure` absent — the
  zero-touch lane is off unless asked for, confirmed on real config.

## 2026-07-30 — Revi had stopped publishing on every repo, and finished green doing it

- Status: **defect found and fixed** — the runs were fine, the publishing step
  was dead. Found while wiring `iterion/review` as a required check, which is
  the only reason it surfaced at all.
- Versions: bot review-pr 0.5.6 · iterion `main` @ `7b87b5f37` + `34bd00879`
- Method: `/revi` on buildkit-operator #4 (run `019fb403-530a`, 2m42s) and #7
  (`019fb403-5f28`, 3m39s), plus the iterion PRs of the day (#323 →
  `019fb408`, ~12min). Cloud prod, mono topology.
- Result: after the fix, all three posted their review **and** their commit
  status. Before it, every one of them finished `finished` having posted
  nothing. Verbatim from #323 — both fixes visible in one line:

  > ## Code review by Revi (iterion)
  > 4 finding(s) kept after threshold/cap. — medium: 2, low: 2
  > Reviewed by a single model family (mono topology): no finding is
  > cross-confirmed, and none is meant to be.

  with `revi/review=SUCCESS` on the head.

### The defect: a template that never resolved

`publish_review` built its guard input as `REVIEWED_SHA={{outputs.…}}` inside a
tool node's `command:`. That body is resolved by
[resolveCommandTemplate](../../pkg/backend/model/executor_tool.go), which
substitutes `{{input.X}}`, `{{vars.X}}`, `{{secrets.X}}` and `{{run.id}}` —
**`{{outputs.…}}` resolves only in edge mappings**. Written in a body it
survives as literal text.

So the stale-anchor guard compared the literal string `{{outputs.…}}` to the
PR's head sha, concluded the anchors were stale, and skipped the whole publish:
review, inline comments, and gate status. No error anywhere — the node
succeeded, the run finished, and the PR simply never heard from Revi. This had
been true **repo-wide**, on every review, for as long as the guard existed.

Had the required check been switched on before this was found, it would have
blocked every pull request on the repo — an outage caused by a check that was
never posted, on runs reporting success.

### Second defect on the same path: the guard took the gate down with it

Even a *genuinely* stale anchor set skipped the entire publish. But a stale
inline anchor only means the line numbers moved — it says nothing about the
verdict. Dropping the gate along with the comments turns a cosmetic problem
into a permanently absent required check. `stale_anchors` now drops the inline
comments and **keeps publishing** the summary and the status.

### Third: mono claimed a cross-family confirmation that never happened

The summary printed `N finding(s) cross-confirmed by both model families` even
in mono topology, where one family ran. Spotted by jo on the real comment on
buildkit-operator #6 — the reviewer was describing a corroboration it had no
way to perform. Mono now says so in as many words.

### Guards added

- `bots/catalog_command_refs_test.go` — catalog-wide: **no** `{{outputs.…}}` in
  any tool `command:`/`script:`/`postcondition:`, walking every `.bot`. The
  class, not the instance: the same silent no-op was available to every bot in
  the catalog.
- `bots/review_pr_stale_anchor_test.go` — drives the real publish body against
  a stub, shell-quoting substitutions the way the engine does, so the guard is
  exercised on the code that ships rather than on a paraphrase of it.

### Lessons for next run

1. **`{{outputs.…}}` in a command body is a silent no-op**, not an error. Any
   comparison against one is a comparison against a constant string — it will
   take whichever branch that constant happens to select, forever.
2. **A guard that suppresses output must never suppress the verdict.** Degrade
   the part that is unsafe (the anchors), keep the part a required check
   depends on.
3. A bot that publishes nothing looks exactly like a bot with nothing to say.
   Neither the run status, nor the logs, nor a green test suite distinguished
   them here — making the check *required* is what finally did.

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
