# Revi → Billy on this repo — PAUSED, and how to run a deliberate pass

> **Paused since 2026-09-15.** `/billy` is no longer the default answer to a
> red gate on this repo, and the zero-touch lane is **off**. A fixer campaign
> is a whole-session claude_code agent whose verify gate re-runs the full
> build+test (~10 min a pass), drawn from the shared forfait / platform
> credential — too expensive until the team spends its own BYOK key. Findings
> are the developer's to fix, through the local loop:
> [agents/review-and-merge.md](agents/review-and-merge.md) +
> [agents/adversarial-review-loop.md](agents/adversarial-review-loop.md).
>
> **This file stays current and worth reading** — for a pass someone chooses
> to pay for (`/billy` still answers), and for re-arming the lane. Where it
> says "the habit", read "the habit when Billy is armed".

When Revi (`bots/review-pr`) reviews a pull request of **this repository** and
leaves findings, an armed Billy is invoked by **commenting `/billy` on the
PR** rather than hand-fixing in an interactive session. iterion is
a code factory; its own PRs are the first place its review→fix loop must earn
its keep. Every `/billy` run here is a dogfood run: monitor it, fix the
frictions it surfaces (bot or engine), and write the bilan.

This runbook is the *habit*; the mechanics live in
[merge-gate.md](merge-gate.md) (gate, reconciler, two-bots-one-context) and
[bots/branch-improve-loop/README.md](../bots/branch-improve-loop/README.md)
(the campaign shape). For the three-way picture — Revi, Billy, **and**
Vetty (`dep-update-guard`) sharing one gate — see
[merge-gate.md's "Revi / Billy / Vetty — one gate, three roles"](merge-gate.md#three-roles):
what is wired (disjoint ownership, the ~4s claim window, the
`produces:`/`consumes:` hand-off), what is not yet (the pause notice
naming a parked run's role, a "fixer in flight" signal before its first
push), and the operator rules this file's session-discipline section below
also lives by.

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
ever auto-REVIEWS it (Revi). Billy runs on a deliberate command — that is now
the ONLY way he runs here. The zero-touch lane (`auto_fix_on_gate_failure` —
a red gate launches the fixer by itself,
[merge-gate.md#autofix](merge-gate.md#autofix)) was enabled on this repo from
2026-08-28 and is **off since 2026-09-15**: it was spending a full campaign on
the shared credential with nobody typing a command. Re-arming it, and the
`repo-bots` PATCH that does it, is in
[agents/review-and-merge.md](agents/review-and-merge.md#billy-is-paused). One
gotcha when flipping the flag either way: the `repo-bots` PATCH requires the
FULL `bot_ids` list in the payload (omitting it is a 400, not "keep as is"),
and an omitted `auto_fix_on_gate_failure` means "leave the current choice
alone" — it has to be written explicitly.

**Corollary, whatever the lane is set to**: before hand-fixing a red or ejected
PR, check no fixer run is already in flight on it — a manual push while the
fixer works recreates the mid-run-push collision the session discipline below
warns about. **`iterion remote runs list` is that check.** The PR's own
statuses are per-lane and none covers every fixer, so none replaces it: the
gate's `pending` link covers a reviewer and the zero-touch fixer,
`iterion/fix-in-flight` covers the auto-heal, and a `/billy` pass shows nothing
at all until its first commit
([merge-gate.md](merge-gate.md#what-is-not-wired)).

Turning the lane off does not retire the check: the **merge-queue auto-heal**
dispatches the same brancher bot with no comment when the queue ejects a PR
**for a healable reason**, and it never reads `auto_fix_on_gate_failure`
([merge-gate.md](merge-gate.md#auto-heal-and-when-it-stands-down)). A heal in
flight force-pushes the branch.

## <a name="what-the-command-seeds"></a>What the command seeds — you type nothing else

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

## <a name="what-to-expect-on-the-pr"></a>What to expect on the PR

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
   webhook config. The orchestrator **derives it ON** whenever a bot of the
   webhook gates merges (declares the `statuses` scope) and the gate is not
   disabled, unless an operator pinned the value
   ([pkg/forge/orchestrator.go](../pkg/forge/orchestrator.go),
   `ReviewOnSyncPinned`); a repo that only ever auto-reviews on open has it
   off, and Billy's push there ends the loop until someone comments `/revi`.
   Check it before concluding the loop is broken:
   `iterion remote api GET /api/teams/<team-id>/webhooks` → `review_on_sync`.

4. **While Billy runs, the PR looks untouched** — `revi/review` stays green
   on the OLD head and no status says a fixer is at work: the only signals
   are the run itself (`iterion remote runs list`, the run console) and,
   when he parks on a quota, the platform's pause notice. That notice is
   written for a review and currently mislabels a parked fixer ("Review
   paused … a new push restarts it sooner" — a push is exactly what NOT to
   do while he works; SocialGouv/iterion#650). Nothing is wrong: wait for
   his push, or for the `pending` claim that follows it.

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
- **A death is not a loss — the pod's commits are BANKED** on
  `iterion/run-<run-id>` (`final_branch` / `final_commit` on the run doc;
  the richer chain of successive attempts wins). But a **resume re-clones
  the branch head and restarts the campaign from scratch**, redoing the
  banked work (SocialGouv/iterion#652). If the retry cannot finish in the
  budget left, deliver the banked chain by hand: fast-forward it onto the
  PR branch AFTER the full validation (`task check`, `-race`, conformance —
  a chain committed in stride is not gate-verified and can be red), say so
  on the PR, and let `review_on_sync` re-review the head.
- **Never bare-resume a duration death on cloud.** The consumed duration
  axis rides the checkpoint, so `iterion remote runs resume` restarts and
  dies at the 110 % exit-grace ceiling (15 min of pod for nothing). The
  cloud resume endpoint has no budget-override fields; raise the cap by
  resuming with the bot source inline: `POST /api/runs/<id>/resume` with
  `{"source": <main.bot with a larger max_duration>, "force": true}` — the
  checkpoint restarts inside `campaign`, the plan phases are not re-paid.
- **When the WEEKLY cap parks the gate**, the reset can be days out and
  the review's `pending` claim blocks the PR that long. The documented
  maintainer override (`/revi approve`, [merge-gate.md](merge-gate.md))
  currently fails on a GitHub App integration (`set commit status: forge:
  insufficient scope`, SocialGouv/iterion#662): the fallbacks are the
  admin's own status write on the head (same context, "approved by
  @user: reason" in the description, the comment as `target_url`) or the
  admin merge-queue bypass.
- **A deliberate pass still needs Billy to be able to run.** When the *weekly*
  cap is hard-blocking, the reset can be days out and the pass has no path:
  fix the findings yourself, say so on the PR against their finding ids, and
  write the bilan for the launches that failed. Same when he burns his
  duration cap without banking a commit — on a repo whose verify gate re-runs
  the whole build+test (~10 min a pass here), 2h30 buys few passes, so a run
  sitting at `running` with nothing pushed is worth cancelling rather than
  waiting out (measured 2026-08-30: 2h31 for zero commits, see
  [bot-runs/branch-improve-loop.md](bot-runs/branch-improve-loop.md)). That
  cost profile is also why the pass is no longer the default — see the pause
  note at the top.

## Dogfood duty

Every `/billy` run on this repo gets a dated bilan in
[docs/bot-runs/branch-improve-loop.md](bot-runs/branch-improve-loop.md)
(newest-first — status, method, result, value, frictions, lessons). A friction
found here is a defect to fix in stride — in the bot
(`bots/branch-improve-loop/`) or the engine — that is the point of running the
loop on ourselves.

The 2026-09-03 run on the watchdog PR (#646) is the reference for the
banked-chain delivery and the weekly-cap wall:
[bot-runs/branch-improve-loop.md](bot-runs/branch-improve-loop.md).


## Workflow delivery permission preflight (#999)

Billy 1.8 checks the branch before any planning or campaign analysis. A
GitHub diff touching `.github/workflows/` (including rename sources,
intermediate changes subsequently reverted, staged changes and untracked
files) requires proof that the runtime token carries `workflows:write` and
`contents:write`. Other diffs proceed normally. GitLab merge requests are
outside this GitHub permission check.

The App mint response supplies the actual permission set. The server stores
it with the sealed managed token, its full SHA256 identity and expiry;
rotation replaces both together. The installation grant and the diagnostic
last-minted-per-installation cache never authorize the check. The run sends
only its token digest to the read-only, repository/team/host-scoped
`POST /api/v1/forge/delivery-preflight` callback under its existing run grant.
No permission is added to the App automatically.

A missing, expired, rotated or narrower proof refuses the run with
`FORGE_PERMISSION_DENIED` before analysis. The operator can deliberately
approve delivery permissions on the App, refresh its managed token and
launch a new run. Classic PAT/OAuth tokens can instead prove `workflow` plus
repository scopes through the actual token's GitHub `/user` response.
Fine-grained PATs and external App tokens without managed mint evidence
remain unverified; their permission cannot be inferred from token syntax.
A missing callback on an older server fails closed for workflow changes;
ordinary code changes retain their path. This uses existing bot variables
and a deterministic tool, so no queue-version change is required.

The check is an admission snapshot, not a guarantee against later credential
revocation or newly authored workflow files. A later push/bank refusal still
requires the bank failure alert tracked in #885 (PR #1194).

Proof: `TestFixerWorkflowDiffRefusesBeforeAnalysis` runs the compiled catalog
workflow and real shell tools while counting forbidden analysis calls;
`TestForgeDeliveryPreflightUsesActualTokenProof` covers the callback's scope
and token binding. No live App permission was changed during validation.
