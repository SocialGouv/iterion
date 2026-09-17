# Before merge — the required loop, and how a change reaches `main`

Every change lands through a pull request whose `revi/review` gate is green.
The documented exceptions are the admin bypass below — which allows a direct
push to `main`, not just a queue skip — and the release bot, whose
`chore: release` commits land on `main` with no PR and no gate.
The loop is:

**local adversarial round → fix → re-attack the fix → push → `/revi` → green**

1. **Run a local adversarial round on the diff BEFORE pushing.** A subagent
   whose posture is to break the change, not to bless it; every finding **and
   every fix it proposes** verified before a line is written. Protocol:
   [adversarial-review-loop.md](adversarial-review-loop.md). Measured on this
   repo: five consecutive gate verdicts at ≥ 1 medium on a fresh line (~6 h of
   queue) against one 15-minute local round followed by a first verdict at
   0 findings. That page also carries the **round budget** — a ceiling on cost
   by change size (≤ 8 files → 5 local rounds, 10 if the diff blocks · 9–25 →
   20 · > 25 → 50), never a
   target: what decides another round is whether the last one still returned
   verified high or critical findings.
2. **The gate closes the loop; the local round never does.** A sterile local
   round means "time to push", not "done". Revi's `questions` channel is
   non-blocking, but each question gets a doc fix or a written refusal — never
   silence. Symmetrically: at the **third** verdict with findings on the same
   PR, stop pushing — what is left is local-round work.
3. **The developer fixes the findings** — by hand, or through another local
   round. **Do not comment `/billy`**: see [the pause](#billy-is-paused) below.
4. **Every commit that ships reviewed work says what the review cost.**
   `Adversarial-Rounds:` and `Adversarial-Model:` trailers, `0 (trivial: …)`
   written explicitly when nobody attacked the change — format, what survives
   the squash, and rationale in
   [adversarial-review-loop.md](adversarial-review-loop.md#say-what-the-review-cost-in-the-commit).

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

**GitHub workflow delivery preflight** — Billy checks the runtime token’s actual `workflows:write` proof before analyzing a workflow-changing diff. `FORGE_PERMISSION_DENIED` requires credential configuration and a new launch; installation grants alone are not proof. See [../revi-billy-loop.md](../revi-billy-loop.md).

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

**From bundle 0.9.3, Revi's review summary advertises a fixer only where a
repo declares one.** The escalation line is the `fixer_hint` launch var
([../../bots/review-pr/main.bot](../../bots/review-pr/main.bot)): empty — the
default, and what this repo leaves it at while Billy is paused — omits the line
entirely; a repo with a fixer sets the sentence it wants, `{finding}` standing
for the first finding's id — see
[../merge-gate.md](../merge-gate.md#review-tiers) for the `launch_vars` form. A
catalog bot is a general-purpose tool: it may read which repository it is
reviewing, but it must not be scoped to one, so the escalation is the repo's to
declare.

**Until the production override is re-pushed, reviews here still carry
`Correction : /billy`.** A stored bundle outranks the baked catalog at every
launch surface, and the staleness warning cannot help: the deployed override is
version-suffixed (`0.9.2-codex-claw.N`) and a suffixed version is unorderable,
so `shadowsNewerVersion` never fires — a blind spot
`pkg/server/bot_override_staleness.go` names itself. Landing the change moves
`main`; re-pushing the override is what moves what a developer reads.

### Re-arm when this repo's team spends its own BYOK key

Condition: the team's runs resolve **its own** provider key rather than the
shared tier — see [../byok.md](../byok.md) and
[../cloud-llm-credentials.md](../cloud-llm-credentials.md). Then:

```sh
# Identity first: an outbound forge action is not retractable.
gh api user --jq .login          # expected: devthejo
iterion remote status            # instance + account (it does NOT print a team)

# 1. Read the integration. The bare form takes NO argument and resolves the
#    active team itself — which is how you learn the team id, since nothing
#    above prints one. Note `tenant_id` (= <team-id>), `id`, and the COMPLETE
#    `bot_ids` list.
iterion remote forge repo-bots --json

# 2. Flip the lane. `bot_ids` is REQUIRED and must be complete (omitting it
#    is a 400, not "keep as is"); `auto_fix_on_gate_failure` omitted means
#    "leave the current choice alone", so it has to be written explicitly.
iterion remote api PATCH /api/teams/<team-id>/forge/repo-bots/<integration-id> \
  --data '{"bot_ids":[<complete list read back at step 1>],
           "auto_fix_on_gate_failure":true}'
#    A `202` carrying `pending_approval: true` is NOT a failure and NOT a
#    success: turning the lane ON is an automation EXPANSION, so an org that
#    requires provisioning approval parks it for an admin and applies nothing
#    yet. The read-back below then still shows the key absent — queued, not
#    refused. Only a 200 means it landed.

# 3. Read it back — the PATCH response does NOT echo the field.
iterion remote forge repo-bots --json
```

The typed CLI has no update verb: `iterion remote forge repo-bots` offers
`preview | create | delete`, and lists with no argument at all
([../../cmd/iterion/remote_webhooks.go](../../cmd/iterion/remote_webhooks.go) —
`list` is not a verb, it falls through to the usage error),
so the PATCH goes through the `remote api` escape hatch. The server route is
`PATCH /api/teams/{id}/forge/repo-bots/{integration_id}`
([../../pkg/server/forge_provisioning_routes.go](../../pkg/server/forge_provisioning_routes.go)).

Setting `false` in that same payload is how the lane is turned off — but the
two directions **do not read back the same way**, so the symmetry stops at the
payload. `AutoFixOnGateFailure` is a plain `bool` tagged `omitempty`
([../../pkg/forge/repo_integration_store.go](../../pkg/forge/repo_integration_store.go)),
so `false` is never serialised: **ON reads as `"auto_fix_on_gate_failure": true`,
OFF reads as the key being absent entirely.** Absence is genuine rather than a
lost write — `Update` is a full-document `ReplaceOne`
(`mongoutil.ReplaceOneChecked`), so a previous `true` cannot survive the
replace — but on its own it is a weak signal: it looks identical to a field
name you typo'd, a server too old to know the field, and a repo that never
opted in. What makes it evidence is watching the **transition** on the same
endpoint: `true` before, absent after.

**And the flip is settled behaviourally, not declaratively.** The read-back
says what was stored; what proves it is the next red gate — a fixer run
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
