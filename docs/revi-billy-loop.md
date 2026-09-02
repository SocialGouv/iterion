# Revi → Billy on this repo — the operating habit

When Revi (`bots/review-pr`) reviews a pull request of **this repository** and
leaves findings, the habit is to **comment `/billy` on the PR** and let the
fixer work — not to hand-fix the findings in an interactive session. iterion is
a code factory; its own PRs are the first place its review→fix loop must earn
its keep. Every `/billy` run here is a dogfood run: monitor it, fix the
frictions it surfaces (bot or engine), and write the bilan.

This runbook is the *habit*; the mechanics live in
[merge-gate.md](merge-gate.md) (gate, reconciler, two-bots-one-context) and
[bots/branch-improve-loop/README.md](../bots/branch-improve-loop/README.md)
(the campaign shape).

## The command

On the PR, comment:

```
/billy
```

(aliases: `/improve`, `/branch-improve-loop`; optional free text after the
command lands in `scope_notes`). The commenter must hold **maintainer+** on the
repo (`min_replier_role` on the command — verified live via the forge
permission API, not from the payload).

There is deliberately **no PR-open auto-launch for Billy**: opening a PR only
ever auto-REVIEWS it (Revi). Billy runs on a deliberate command — and, since
2026-08-28, on a **red gate**: the zero-touch lane (`auto_fix_on_gate_failure`
— a red gate launches the fixer by itself,
[merge-gate.md#autofix](merge-gate.md#autofix)) is **enabled on this repo**,
the manual habit having proven smooth. A red `revi/review` relaunches the
fixer without a comment, bounded by the lane's own brakes (one attempt per
head sha, five unattended passes per PR, the hold label). `/billy` remains
the way to start a pass when the gate is green or the lane's passes are
spent. One gotcha when flipping the flag: the `repo-bots` PATCH requires the
FULL `bot_ids` list in the payload (omitting it is a 400, not "keep as is").

**Corollary of the lane**: before hand-fixing a red PR, check no fixer run is
already in flight on it (`iterion remote runs list` or the gate's `pending`
link) — a manual push while the fixer works recreates the mid-run-push
collision the session discipline below warns about.

## What the command seeds — you type nothing else

The webhook tail resolves everything from the PR and the repo integration:

- **`prior_review`** — the latest review of this PR, findings + ready-made
  replacements, seeded through the kind-matched hand-off
  (`consumes: kind: review` in Billy's manifest ←
  `produces:` in Revi's; [pkg/server/webhooks_handoff.go](../pkg/server/webhooks_handoff.go)).
  Billy **re-checks every finding against the current diff** rather than
  trusting a stale verdict, and still runs fine when no review exists.
- **`push_branch` / `pr_url`** — his commits are pushed onto the PR's source
  branch, and his finding **ledger** (per finding id: fixed /
  refused-with-argument / deferred) is posted as a comment ON the PR.
- **`gate_context` + publish grant** — from the integration's `launch_vars`
  (`revi/review` here), so he can post his own gate count on the head he
  pushed.

## What to expect on the PR

1. Billy verifies each prior finding, fixes the real ones **one commit per
   fix** (build+test before each commit), and pushes onto the PR branch.
2. He posts the ledger comment and a gate status **on the head he pushed** —
   a count, never a judgement; a green from Billy says so in its description.
3. His push is a `synchronize`, so Revi **re-reviews the new head** and its
   independent verdict supersedes minutes later. A finding Billy contested
   (with an argument, in the ledger) is handed to the next review as pushback —
   and to you: a contested finding keeps the gate red until a human decides
   (`/revi approve [reason]`, maintainer-gated).

   That third step is **not free** — it needs `review_on_sync` on the repo's
   webhook config, which is **off by default** (a push is otherwise an
   on-demand re-review, deliberately budget-frugal). It is the same switch the
   merge gate needs, so a repo whose `revi/review` is a required check has it
   on; a repo that only ever auto-reviews on open does not, and Billy's push
   there ends the loop until someone comments `/revi`. Check it before
   concluding the loop is broken:
   `iterion remote api GET /api/teams/<team-id>/webhooks` → `review_on_sync`.

## Session discipline (the gotchas)

- **Don't work on the PR branch while Billy runs** — he pushes onto it. After
  his push, `git pull` before resuming any local work on that branch.
- **Only invoke him on PRs you own** (or with the author's accord): he rewrites
  their branch.
- **Monitor, don't fire-and-forget**: the run console link is on the `pending`
  gate status; or `iterion remote runs` / the `remote_run_log` MCP tool.
  Proof of a good run = ledger comment + commits on the branch + gate status
  on the new head + Revi's re-review landing after it.
- **He posts nothing?** Check the run's inputs for `forge_publish_url` and
  `gate_context` before blaming the bot ([merge-gate.md](merge-gate.md)).
- **Usage caps park runs**: a review or fix run that dies on
  `usage cap: … window` is `failed_resumable` with the usage-window retry
  armed — it resumes at the provider reset by itself
  ([usage-caps.md](usage-caps.md)). Don't relaunch it by hand.
- **"Don't hand-fix" assumes Billy can run.** When the *weekly* cap is
  hard-blocking, the reset can be days out and the habit has no path: fix the
  findings yourself, say so on the PR against their finding ids, and write the
  bilan for the launches that failed. Same when he burns his duration cap
  without banking a commit — on a repo whose verify gate re-runs the whole
  build+test (~10 min a pass here), 2h30 buys few passes, so a run sitting at
  `running` with nothing pushed is worth cancelling rather than waiting out
  (measured 2026-08-30: 2h31 for zero commits, see
  [bot-runs/branch-improve-loop.md](bot-runs/branch-improve-loop.md)).

## Dogfood duty

Every `/billy` run on this repo gets a dated bilan in
[docs/bot-runs/branch-improve-loop.md](bot-runs/branch-improve-loop.md)
(newest-first — status, method, result, value, frictions, lessons). A friction
found here is a defect to fix in stride — in the bot
(`bots/branch-improve-loop/`) or the engine — that is the point of running the
loop on ourselves.
