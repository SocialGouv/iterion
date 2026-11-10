# workflow-kick — Kicky

Dispatches **one GitHub Actions workflow** of the target repository on an
iterion schedule. It is the external trigger for a workflow whose own
`schedule:` cron GitHub fires late or not at all: between 27/10 and 10/11/2026
GitHub ran 41 of ~813 expected hourly `schedule` events of buildkit-operator's
`renovate.yml` (≈ 5 %, median gap 6.2 h), so a security update waited hours
for a Renovate pass ([#3130](https://github.com/SocialGouv/iterion/issues/3130)).

One deterministic tool node, **no LLM**, nothing written, nothing posted.

## How it dispatches without holding a credential

```
schedule tick ──► run (forge_dispatch_url + forge_publish_token)
                    │ POST {run_id, repo, workflow}
                    ▼
iterion server ── checks the grant ──► mints a GitHub App token for THIS call
                                         {actions: write, metadata: read},
                                         narrowed to the one repository
                                         │
                                         ▼
                 POST /repos/{owner}/{repo}/actions/workflows/{file}/dispatches
                 (ref = the repository's default branch, no inputs)
```

- The manifest declares `forge.token_scopes: { actions: write }`. That scope
  mints the `dispatch_workflow` capability into the run's **publish grant**; a
  bot that does not declare it never holds the capability.
- The grant is minted at the schedule tick, bound to the schedule's **repo
  integration** (its team, connection and repository), and carries that one
  capability.
- It is bound to **no pull request**, so the dispatch is all it does: the
  review and merge-gate endpoint, the pull-request read and the delivery
  preflight refuse it (`403 grant_bound_to_no_pull_request`) before any forge
  call. A schedule's run cannot post a review, nor a commit status such as the
  `revi/review` required check, on any head of the repository.
- The run's own forge token never carries the Actions permission: the server
  mints a separate installation token per dispatch and keeps it.

The server refuses, typed (`code` in its JSON answer) and before any forge
call:

| code | when |
|---|---|
| `workflow_dispatch_no_capability` | the grant lacks the capability — the bot's manifest does not declare `actions: write`, or `forge_publish_mutations` disarms it |
| `workflow_dispatch_bad_workflow_file` | `workflow` is not a plain `*.yml` / `*.yaml` file name |
| `workflow_dispatch_repo_outside_grant` | `repo` is not the grant's repository |
| `workflow_dispatch_run_not_grant_holder` | the calling run does not hold this grant |
| `grant_untrusted_run` / `grant_tenant_mismatch` | the run's workspace holds code the tenant did not write (ADR-162), or the run is another tenant's |
| `workflow_dispatch_unsupported` | the connection is not a GitHub App connection (GitLab, Forgejo, a GitHub OAuth or PAT connection) |
| `workflow_dispatch_repeat` | this run already dispatched (or attempted) this workflow — one per run and workflow, so a replay or a loop cannot fan out |

and reports the forge's own refusal after the call:
`workflow_dispatch_permission_missing` (the App installation lacks
`Actions: Read and write`), `workflow_dispatch_refused_by_forge` (an unknown
workflow answers 404, a workflow without `workflow_dispatch` or an unknown ref
422), `workflow_dispatch_unconfirmed` (no answer — the run may have been
queued). A refusal ends the run `WORKFLOW_NOT_DISPATCHED` with the code and the
server's reason, which the schedule's health surface shows; the next tick is
the retry.

## Wiring it on a repository

1. **The workflow** declares `workflow_dispatch` (the
   [auto-maintenance recipe](../../docs/auto-maintenance.md#2-renovate-opens-the-prs)
   `renovate.yml` does).
2. **The App permission.** The GitHub App behind the repository's connection
   holds `Actions: Read and write`; an org owner approves the pending request.
3. **Enable** `workflow-kick` on the repository's integration (Studio →
   Integrations, alongside the review bots). Provisioning creates its hourly
   schedule (`7 * * * *`) bound to that integration.
4. **Set the workflow**: patch the schedule's vars with `workflow` — e.g.
   `{"vars": {"workflow": "renovate.yml"}}` on
   `PATCH /api/teams/{id}/schedules/{sid}`, or Studio → Automations →
   Schedules. Until then each tick ends `NO_WORKFLOW`.

`repo` may stay empty: the bot reads it from the checkout's origin remote. The
server refuses it anyway if it is not the integration's repository.

## Vars

| var | | |
|---|---|---|
| `workflow` | required | the workflow file name (`renovate.yml`) |
| `repo` | optional | `owner/name`; empty = the checkout's origin |
| `forge_dispatch_url`, `forge_publish_token` | minted | the dispatch endpoint and its bearer, minted by the server with the grant |

## Output

`kick_output`: `dispatched`, `code`, `reason`, `repo`, `workflow`, `ref`,
`workflow_url` (the workflow's runs page) and `run_url` — the run's own page,
the link every report of this bot carries.
