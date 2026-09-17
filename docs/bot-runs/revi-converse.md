# revi-converse — run bilans

Newest first. Template: [README.md](README.md).

## 2026-09-17 — profile 2: the converse prompts reach the model with their paragraphs, nothing posted (run 01a0af4f-430c)

- Status: **validated for what it could reach** — a grounded answer drafted from a branch diff,
  the POST and its mandatory VERIFY refused by an unresolvable forge host, reported honestly.
- Versions: bot revi-converse 0.1.2 (`dsl: 2`, wave 3a of #1344) · iterion `db8dbb8eb` for the bots; the engine the branch binary `v3.154.1+d5b7db09f` carrying the first wave-1 runtime fix (main is at v3.157.0 with both), served through the host's Anthropic-compatible facade (z.ai) as the runs' provenance records · claude_code +
  claude-opus-5.
- Method: CLI `iterion run <bundle>/main.bot` launched FROM a scratch copy of the greet project on
  `feat/shout`, `--store-dir` the operator's workspace store, `--sandbox none`, `--var base_ref=main
  --var pr_url=https://forge.invalid/scratch/greet/pull/1 --var discussion_id=scratch-thread-1
  --var converse_question=… --var trigger_note=… --var replier=devthejo`, caps
  `--max-cost-usd 2 --max-duration 10m`. The `.invalid` host is deliberate: it lets the bot run its
  whole procedure without any way of posting on a real forge.
- Result: **finished**, ~1.5 min, **$0.32** (converse_agent, 37 614 tokens): the answer explains,
  from the diff, why `--shout` uppercases the whole greeting; `posted: false`, `skipped_reason`
  names the unresolvable host (curl exit 6), both the POST and the VERIFY re-fetch failed;
  `converse_health` bannered it.
- Value: the bot's graph live on profile 2; the run's `events.jsonl` carries the rendered
  converse prompt with `discussion thread.\n\n── SCOPE ──`, the paragraph break the profile-1
  lexer used to fold.
- Findings / misses: the posting path is untested here by design; a real end-to-end stays the
  2026-09 production run above.
- Engine hardening: none needed.
- Lessons for next run: an RFC 2606 `.invalid` forge host is the safe way to render a posting
  bot's prompts locally.

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
