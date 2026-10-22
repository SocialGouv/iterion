# Revi on GitHub — App binding and rollout runbook

Bind `review-pr` (Revi) to one GitHub repository through a team GitHub App
connection, then prove the delivery → run → review → gate chain with read-backs.
Read this before the first binding, before adding a required check, or when a
review request or synchronization launches nothing.

The procedure needs a team admin in iterion and a GitHub actor who can manage
the repository hook and invite a collaborator. App installation and repository
selection additionally require a GitHub organization owner. Replace every
`<placeholder>`; in particular, keep the iterion `<webhook-id>` distinct from
GitHub's own `<github-hook-id>`.

> **Cost warning.** PR open, reviewer request and (once enabled) PR synchronize
> can each launch a real, paid LLM review. Obtain the credential owner's
> approval and set the applicable budget before the acceptance steps. The
> [dated production measurements](bot-runs/review-pr.md#2026-09-05--what-a-revi-review-costs-33-production-reviews-and-the-model-that-fits-them)
> are observations from one bot version and population, including their caveats
> about unfinished expensive runs — not a price promise or a budget guarantee.

## 1. Install the App and verify its effective grants

Only a GitHub organization owner can install the App or widen the repository
selection of an existing installation. Install it on the organization, selecting
only the intended repositories, and connect that installation to the target
iterion team. For an existing App, approve every newly requested permission on
the installation; changing the App definition alone does not widen it.

The installation must grant the current iterion GitHub App profile these
repository permissions for the Revi/provisioning path:

| GitHub permission | Required GRANTED level | Why |
|---|---|---|
| Metadata | Read | Mandatory GitHub App baseline |
| Contents | Read and write | Runtime App profile; Revi consumes its read side to clone and read the diff |
| Pull requests | Read and write | Publish the review and withdraw the completed bot reviewer request |
| Commit statuses | Read and write | Publish the `gate_context` commit status |
| Issues | Read and write | Runtime App profile; Revi consumes its read side for forge-native ticket conformance |
| Repository webhooks | Read and write | Let provisioning create and repair the repository hook |

The Contents/Issues write levels belong to iterion's shared runtime profile;
they do not make Revi a fixer. Revi does not need Administration. See
[forge permissions](forge-permissions.md#least-privilege) for the identity and
permission model.

Find the connection, select its team, then refresh it after the installation
has accepted the permission set:

```sh
iterion remote forge connections --team <team-id>
iterion remote teams switch <team-id>
iterion remote forge refresh <connection-id>
```

`forge refresh` has no `--team` flag and uses the active team. The `--team`
flags on the surrounding commands do not persist, so the switch is required
before this command.

The refresh re-probes the installation and forces a fresh management-token mint.
It then reads `LastMintedPermissions` for the TOKEN column. Read both columns
with that distinction:

- **GRANTED** is what the live installation allows;
- **TOKEN** is the permission snapshot of the last runtime token recorded by
  this process, which can be absent or stale. It is not fresh evidence from
  this refresh. The newly minted management token is a distinct layer; this
  column does not describe its gate write.

Require the installation levels above in GRANTED, in particular
`statuses: write`. A runtime token's profile expects `statuses: read`, not
`statuses: write`; neither level in the recorded TOKEN snapshot proves a
management gate write. Do not block pre-provisioning on TOKEN: its absence or
staleness is not a refusal reason. If GRANTED lacks a required installation
permission, stop before the paid test and have an organization owner repair
the installation approval before refreshing again. A review can still publish
while missing GRANTED `statuses: write` leaves the management layer unable to
post the gate. The published status on the same PR head SHA in step 6.3 is
that write's effective proof.

Before provisioning, read back the repositories the connection can administer:

```sh
iterion remote forge connections repos <connection-id> --team <team-id>
```

The answer must contain `<owner>/<repo>`. If it does not, an organization owner
must add the repository to the installation; then repeat the refresh and this
read-back. Do not run `repo-bots create` until the target repository is listed.

## 2. Provision Revi and record both identifiers

```sh
iterion remote forge repo-bots create --team <team-id> --data '{
  "connection_id": "<connection-id>",
  "repo": "<owner>/<repo>",
  "bot_ids": ["review-pr"],
  "launch_vars": {
    "gate_context": "revi/review"
  }
}'
```

Record **`integration_id`** and **`webhook_id`** from the response. Read the
integration back and verify the repository, `review-pr`, both identifiers and
the exact `launch_vars.gate_context` value:

```sh
iterion remote forge repo-bots --team <team-id>
```

`gate_context` is only the name of the commit status iterion writes. It does
**not** make that status required; the separate branch-protection ruleset does
(see step 5).

Do not put `review_request_logins` or `review_on_sync` in the `repo-bots create`
payload, including under `launch_vars`: they are operator-owned webhook config,
not integration settings. Set them explicitly in the next step.

## 3. Arm reviewer requests and re-review on push

```sh
iterion remote webhooks update <webhook-id> --team <team-id> --data '{
  "review_request_logins": ["iterion-bot"],
  "review_on_sync": true
}'

iterion remote webhooks get <webhook-id> --team <team-id>
```

The read-back must show `review_request_logins` containing exactly the intended
GitHub User login (`iterion-bot` here) and `review_on_sync: true`. These settings
live on the webhook and survive integration re-provisioning; the explicit sync
setting is also marked as pinned.

- `review_request_logins` enables the native reviewer-request lane and lets the
  actor guard recognize that User's writes. A GitHub App itself cannot be a
  requested reviewer.
- `review_on_sync` re-reviews a new PR head after the synchronize debounce
  (three minutes by default). Each resulting review consumes budget.

## 4. Invite the reviewer User and prove acceptance

Invite the GitHub **User** `iterion-bot` with Read access (`pull` is the API's
name for that role):

```sh
gh api --method PUT repos/<owner>/<repo>/collaborators/iterion-bot \
  -f permission=pull
```

Authenticate as `iterion-bot`, open
`https://github.com/<owner>/<repo>/invitations`, and accept the invitation. Do
not substitute the App login or accept with a human account. Read the resulting
permission back from an administrative GitHub session:

```sh
gh api repos/<owner>/<repo>/collaborators/iterion-bot/permission \
  --jq '.permission'
```

Expect `read` before requesting a review. The reviewer request itself is part
of the paid acceptance in step 6.

## 5. Keep the status name and the required-check rule separate

For a first rollout, observe the status as advisory before making it blocking.
To enforce it later, a repository admin must add the exact `gate_context` value
(`revi/review` above) to the target branch ruleset's **required status checks**.
Changing the ruleset alone does not teach iterion to publish a status; changing
`gate_context` alone does not change merge enforcement.

Read the ruleset back after that separate change and confirm the exact context,
without removing the existing required checks:

```sh
gh api repos/<owner>/<repo>/rulesets/<ruleset-id>
```

## 6. Run the same-repo acceptance probe

### 6.1 Ping must authenticate and filter, never launch

Identify the GitHub repository hook whose URL ends in
`/api/webhooks/github/<webhook-id>`; its numeric id is `<github-hook-id>`, not
the iterion webhook id. Ask GitHub to test it, then read iterion's delivery
audit:

```sh
gh api --method POST repos/<owner>/<repo>/hooks/<github-hook-id>/tests
iterion remote webhooks deliveries <webhook-id> --team <team-id>
```

Expected: the `ping` delivery reaches **`filtered`**, with no `run_id`. That is
the success case for an out-of-scope event, not a failed hook. An authentication
failure is a different result and must be repaired before opening the test PR.

### 6.2 Open a non-draft, same-repo PR

Create a branch **in `<owner>/<repo>` itself**, commit one small disposable
documentation change, push it, and open a non-draft PR against the repository's
default branch:

```sh
git switch -c <revi-test-branch>
# Make and commit one small disposable documentation change here.
git push -u origin <revi-test-branch>
gh pr create --repo <owner>/<repo> --base <default-branch> \
  --head <revi-test-branch> --title "test: Revi rollout" \
  --body "Disposable same-repo Revi binding probe."
```

Do not use `<fork-owner>:<branch>` as the head. Ordinary Revi lanes refuse fork
PRs, including command and re-review lanes, and no webhook setting lifts that
boundary. Drafts also do not auto-launch; use `ready_for_review` if the PR was
created as a draft.

### 6.3 Follow delivery → run → review → gate

```sh
iterion remote webhooks deliveries <webhook-id> --team <team-id>
iterion remote runs get <run-id>
gh api repos/<owner>/<repo>/pulls/<pr-number>/reviews
gh pr checks <pr-number> --repo <owner>/<repo>
```

The PR `opened` delivery may transiently be `accepted`; wait for **`launched`**
and record its `run_id`. Wait for that run to terminate, then verify all three
outputs on the **same head SHA**:

1. Revi's review is published on the PR;
2. the run's result explains the published verdict;
3. a `revi/review` commit status exists and its success/failure matches the
   deterministic blocking-finding count.

A red `revi/review` with genuine findings is still a successful binding test:
the review and the gate agree. If the review exists without the status, check
GRANTED `statuses: write` and `gate_context`, then the management-layer publish
path; the runtime TOKEN's expected `statuses: read` is not the gate-write proof.
The status's presence on the current head is that proof. If the status exists
but does not block a deliberately failing PR, inspect the required-check
ruleset instead of changing the bot.

### 6.4 Exercise the native reviewer request

After the first review has published, request the User reviewer once:

```sh
gh pr edit <pr-number> --repo <owner>/<repo> --add-reviewer iterion-bot
iterion remote webhooks deliveries <webhook-id> --team <team-id>
```

This deliberately pays for another review. Expect a `pull_request` /
`review_requested` delivery to launch a new run, a fresh review on the current
head, and the completed `iterion-bot` request to be withdrawn so the gesture can
be repeated. If nothing launches, re-read the collaborator permission and the
webhook's `review_request_logins`; do not create a second integration to work
around either one.

Close the disposable PR after acceptance. Closing it stops its active runs and
disarms their usage-window retries.

## Fast diagnosis

| Observation | First check |
|---|---|
| `ping` is `filtered` | Expected; it must not launch |
| PR is filtered | Same-repo head, non-draft state, repository/bot scope |
| Review posts but gate is absent | GRANTED `statuses: write`, then `gate_context` and the management-layer publish path |
| Gate is red/green but never blocks | Required status check in the branch ruleset |
| Reviewer request launches nothing | Invitation accepted, Read permission, `review_request_logins` read-back |
| A push launches nothing immediately | `review_on_sync` read-back, then the synchronize debounce window |
