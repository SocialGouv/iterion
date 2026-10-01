# Billy is paused — the zero-touch fixer lane, and its re-arm

State and re-arm procedure for this repository. The loop's rule that points
here: findings are the developer's to fix, never a `/billy` comment
([the loop](adversarial-review-loop.md)). Mechanics of a deliberate pass:
[../../revi-billy-loop.md](../../revi-billy-loop.md).

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
before a deliberate pass: [../../revi-billy-loop.md](../../revi-billy-loop.md).

**From bundle 0.9.3, Revi's review summary advertises a fixer only where a
repo declares one.** The escalation line is the `fixer_hint` launch var
([../../bots/review-pr/main.bot](../../../bots/review-pr/main.bot)): empty — the
default, and what this repo leaves it at while Billy is paused — omits the line
entirely; a repo with a fixer sets the sentence it wants, `{finding}` standing
for the first finding's id — see
[../merge-gate.md](../../merge-gate.md#review-tiers) for the `launch_vars` form. A
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
shared tier — see [../byok.md](../../byok.md) and
[../cloud-llm-credentials.md](../../cloud-llm-credentials.md). Then:

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
([../../cmd/iterion/remote_webhooks.go](../../../cmd/iterion/remote_webhooks.go) —
`list` is not a verb, it falls through to the usage error),
so the PATCH goes through the `remote api` escape hatch. The server route is
`PATCH /api/teams/{id}/forge/repo-bots/{integration_id}`
([../../pkg/server/forge_provisioning_routes.go](../../../pkg/server/forge_provisioning_routes.go)).

Setting `false` in that same payload is how the lane is turned off — but the
two directions **do not read back the same way**, so the symmetry stops at the
payload. `AutoFixOnGateFailure` is a plain `bool` tagged `omitempty`
([../../pkg/forge/repo_integration_store.go](../../../pkg/forge/repo_integration_store.go)),
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

