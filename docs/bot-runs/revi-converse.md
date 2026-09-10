# revi-converse — run bilans

Newest first. Template: [README.md](README.md).

## 2026-09-02 — GitHub review-thread + `/revi <question>` lanes, first live e2e (runs 01a063d4 / 01a063d5)

- Status: **validated**
- Versions: bot as of `d4611df0` (PR #626) · iterion `5c0a1ea2` (v3.93.0, prod)
- Method: real end-to-end on prod (`iterion.fabrique.social.gouv.fr`) against
  a live repo — test PR SocialGouv/questions-ecrites#65 (deliberately flawed
  python helper), auto-reviewed by Revi (5 inline findings, every planted flaw
  caught), then the two conversational lanes exercised as a human
  (`devthejo`), never simulated. PR closed unmerged + branch deleted after.
- Result — both lanes proved, first try:
  - **Reply-to-a-suggestion**: human reply in an inline thread at 20:34:00 →
    `revi_converse` launched at 20:34:02 (2 s) → the bot's grounded answer
    posted IN the same thread by iterion-bot at 20:34:53. **53 s end-to-end.**
  - **`/revi <question>` as a plain PR comment**: routed by the generic
    command registry (`when_args_present` → revi-converse), run launched in
    ~5 s, finished in 2 min — and the answer landed as a reply INSIDE the
    thread of the finding the question was about (the bot resolved which
    finding the question targeted by itself). Better anchoring than the flat
    PR comment we expected.
- Value: the GitLab conversation parity is now real on GitHub, and answer
  quality was strong — the bot honestly deflated a severity when challenged
  (env-only `EXPORT_DIR` ⇒ injection not user-reachable) while keeping its
  defence-in-depth reservations, instead of doubling down.
- Findings / misses: none in the runs themselves. The rollout mechanics all
  behaved as designed: re-provision ×13 regenerated hook + `event_allowlist`
  (3 events) from the converse bot's own manifest event, preserved
  `review_request_logins` (the 🔁 carry), and migrated the historical
  `{rate:1,burst:10}` to `{2,60}` unpinned — zero manual PATCH.
- Engine hardening (the road to this run): the Revi loop on PR #626 itself
  took **5 rounds / 13 findings, all real, all fixed** — among them a
  self-conversation loop on PAT connections opened by the round-3 WhoAmI fix
  (caught by round 4), the `RateLimitPinned` carry (a default bump must reach
  existing webhooks without erasing operator choices, pre-pin PATCHes
  adopted), and the review-comment firehose becoming its own opt-in manifest
  event so the nine `pull_request_comment` bots don't pay one delivery per
  inline comment against the un-raisable org monthly quota.
- Frictions filed: zero-touch Billy died 3× on the repo's `.mcp.json` sentry
  server (unbootable on runner pods, `native:b2e46831`) and once on the
  usage cap doing its job; the `/revi` command lane still has no fork guard
  (`forge.PullRef` carries no head repo — `native:4363d723`).
- Lessons for next run: replying to a top-level review comment can never
  trigger the lane (thread-opening comments are structurally filtered) —
  reply INSIDE a thread the bot participates in. On PAT/OAuth connections the
  posting identity is the token's own; the PAT owner cannot converse with the
  bot (their replies are self-filtered by design).
