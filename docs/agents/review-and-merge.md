# Before merge — the required loop, and how a change reaches `main`

Every change lands through a pull request whose `revi/review` gate is green.
The loop is:

**local adversarial round → fix → re-attack the fix → push → `/revi` → green**

1. **Run a local adversarial round on the diff BEFORE pushing.** A subagent
   whose posture is to break the change, not to bless it; every finding **and
   every fix it proposes** verified before a line is written. Protocol:
   [adversarial-review-loop.md](adversarial-review-loop.md). Measured on this
   repo: five consecutive gate verdicts at ≥ 1 medium on a fresh line (~6 h of
   queue) against one 15-minute local round followed by a first verdict at
   0 findings.
2. **The gate closes the loop; the local round never does.** A sterile local
   round means "time to push", not "done". Revi's `questions` channel is
   non-blocking, but each question gets a doc fix or a written refusal — never
   silence.
3. **The developer fixes the findings** — by hand, or through another local
   round. **Do not comment `/billy`**: see [the pause](#billy-is-paused) below.

## The merge queue

**`main` is protected by a merge queue** (ruleset "main protected — merge
queue"). PRs merge THROUGH the queue (`gh pr merge <n> --auto --squash`), which
rebuilds each on `main` + earlier-queued PRs and merges only if that combined
tree is green — closing the semantic inter-PR conflict class (two PRs green
apart, red combined). Repo **admins bypass** the queue for hotfixes (direct
push / `--squash` without `--auto`). Required checks: `test`, `race`,
`vendor-check`, `mongo-conformance`, `golangci`, `revi/review`.
`nats-conformance` remains advisory until an admin adds it to ruleset
18857412. Full details + revert command:
[../merge-policy.md](../merge-policy.md).

## The Revi merge gate

**Revi merge gate.** Revi (`bots/review-pr`) posts a
deterministic `revi/review` commit status on a PR head — `success` when 0
findings meet `gate_severity` (default `high`), else `failure`. Add that
context to another repository's required checks to make its verdict block the
merge; it is already required here by ruleset 18857412. The verdict is a COUNT
computed in the bot, never an LLM judgment; the review comments stay
non-blocking advice. Pairs with the webhook
`review_on_sync` opt-in (re-review each push so the status tracks the fixed
head) and Revi's falsifiability `questions` channel (non-blocking assumptions,
never gate). The forge-agnostic write path is `forge.CommitStatusClient`
([../../pkg/forge/status.go](../../pkg/forge/status.go)); the endpoint posts it after the
review ([../../pkg/server/forge_publish.go](../../pkg/server/forge_publish.go)). See
[../merge-gate.md](../merge-gate.md) — which also covers the two bots
sharing one context on the same PR, and the per-repo **opt-in** zero-touch lane
(`auto_fix_on_gate_failure`) where a red gate launches the repo's fixer once per
head sha, off by default so the developer keeps the choice.

`review_on_sync` is what makes `/revi` a *loop* rather than a one-shot: each
push re-reviews the new head, so the status tracks the code you actually fixed.
It stays on.

## <a name="billy-is-paused"></a>Billy is paused (2026-09-15)

The fixer campaign (`bots/branch-improve-loop`, Billy) is a whole-session
claude_code agent whose verify gate re-runs this repo's full build+test
(~10 min a pass) — the most expensive thing in the loop, drawn from the shared
forfait / platform credential that funds this repo's runs. The zero-touch lane
was spending it with nobody typing a command, so `auto_fix_on_gate_failure` is
**off** on this repo.

`/billy` still answers, deliberately: a pass someone chooses to pay for stays
available. What changed is the default — findings are the developer's to fix,
through the local loop, and nothing spends a campaign on its own.

The mechanics and the paid-for gotchas are unchanged and still worth reading
before a deliberate pass: [../revi-billy-loop.md](../revi-billy-loop.md).

### Re-arm when this repo's team spends its own BYOK key

Condition: the team's runs resolve **its own** provider key rather than the
shared tier — see [../byok.md](../byok.md) and
[../cloud-llm-credentials.md](../cloud-llm-credentials.md). Then:

```sh
# Identity first: an outbound forge action is not retractable.
gh api user --jq .login          # expected: devthejo
iterion remote status

# 1. Read the integration: its id AND its COMPLETE bot_ids list.
iterion remote api GET /api/teams/<team-id>/forge/repo-bots

# 2. Flip the lane. `bot_ids` is REQUIRED and must be complete (omitting it
#    is a 400, not "keep as is"); `auto_fix_on_gate_failure` omitted means
#    "leave the current choice alone", so it has to be written explicitly.
iterion remote api PATCH /api/teams/<team-id>/forge/repo-bots/<integration-id> \
  --data '{"bot_ids":[<complete list read back at step 1>],
           "auto_fix_on_gate_failure":true}'

# 3. Read it back — a write receipt is not a read-back.
iterion remote api GET /api/teams/<team-id>/forge/repo-bots
```

The typed CLI has no update verb: `iterion remote forge repo-bots` offers
`list | preview | create | delete` only
([../../cmd/iterion/remote_webhooks.go](../../cmd/iterion/remote_webhooks.go)),
so the PATCH goes through the `remote api` escape hatch. The server route is
`PATCH /api/teams/{id}/forge/repo-bots/{integration_id}`
([../../pkg/server/forge_provisioning_routes.go](../../pkg/server/forge_provisioning_routes.go)).

Setting `false` in that same payload is how the lane was turned off; the
procedure is symmetric.

**Proof of the flip is behavioural, not declarative.** The config read-back
says what was stored; what settles it is the next red gate — a fixer run
appearing (or not) in `iterion remote runs list`.

## Release and changelog

- **tests.yml** — on push/PR: gofmt, go vet, unit tests, e2e tests
- **release.yml** — on git tags (v*): multi-platform builds (linux/darwin/windows × amd64/arm64), GitHub release
- **version.yml** — conventional changelog via release-it, version from `package.json`.
  release-it writes the new section into [CHANGELOG.md](../../CHANGELOG.md) as part of the
  release commit itself (`infile` + `git add . --update`), so the file cannot drift
  from the tags — never hand-edit it. It holds the **current major only**; earlier
  ones are archived under [docs/changelog/](../changelog/) because GitHub stops
  rendering markdown past 512 KB. Each entry carries a collapsed `why` excerpt taken
  from the commit body — the rendering lives in
  [scripts/changelog-writer.mjs](../../scripts/changelog-writer.mjs), shared by release-it
  ([.release-it.mjs](../../.release-it.mjs)) and the regenerator (`task changelog:gen`), so
  a rebuilt section is byte-identical to a released one. Re-run `task changelog:gen`
  after a major bump, or when it warns the file is nearing the ceiling.
