# gitops-warden — binding, calibrating, and flipping a repo to enforce

The runbook for [gitops-warden](https://github.com/SocialGouv/iterion/tree/main/bots/gitops-warden)
(the bot) and [ADR-124](adr/124-gating-bot-merge-gestures.md) (the doctrine).
Read it when binding the bot to a gitops repository, before flipping any repo
from `dry_run` to `enforce`, or when a warden verdict looks wrong and you need
to know which half decided.

The gestures (approve / arm merge-when-pipeline-succeeds / request reviewers)
cross [the publish verdict block](merge-gate.md#gating-bots), which enforces
the guards engine-side; nothing here rests on the bot's good manners.

## 1. Bind a repository

Prerequisites: the engine release carrying the verdict block (#2238) is
deployed, and the bot bundle is on the instance (`iterion remote admin bots
push bots/gitops-warden`, or the release that bakes it).

1. **Forge credential.** GitLab: a group access token named `iterion-bot` on
   the repo's group — role **Maintainer**, scope **api** (it covers statuses,
   approvals, merge and reviewers), plus read_repository for clones. Record
   the bot user it creates (`group_<gid>_bot_…`): that account is the
   approval author on every MR. A group Owner creates the token.
2. **Connection** (team admin):
   `POST /api/teams/{team}/forge/connections`
   `{"provider":"gitlab","mode":"pat","forge_base_url":"https://<forge>","pat":"<token>","display_name":"…"}`,
   then `POST …/forge/connections/{id}/avatar` for the iterion-bot identity.
   Preflight the APPROVAL RIGHT before enforce (step 3) — a 403 here means
   the project's approval rules exclude bot users, which no amount of scopes
   fixes.
3. **Provision both bots** (warden + review-pr, first-class on one webhook):
   `POST /api/teams/{team}/forge/repo-bots`
   `{"connection_id":"…","repo":"<full path>","bot_ids":["gitops-warden","review-pr"],
     "launch_vars":{"reviewers":"<space-separated devops logins>",
                     "policy_path":".iterion/review-policy.md"}}`
   The response names the hook (merge_requests + note events, push_events
   false — `review_on_sync` derived true from the statuses scope covers
   pushes), the two bot rules, and the command map (`/revi`, `/warden`).
   Gate contexts stay separate: `revi/review` judges code quality,
   `gitops/conformance` judges the policy. `reviewers` names who the
   escalation requests; empty means the comment is the only page.

## 2. Author the policy with the team

The bot ships default classes ([skills/review-policy.md](https://github.com/SocialGouv/iterion/tree/main/bots/gitops-warden/skills/review-policy.md)).
A repo narrows or widens them — **value-level only** — via the file pinned as
`policy_path` (default `review-policy.md` at the repo root; the
`.iterion/review-policy.md` convention is a per-repo pin), committed on the
target branch. The starter is the bot's
[skills/policy-template.md](https://github.com/SocialGouv/iterion/tree/main/bots/gitops-warden/skills/policy-template.md).
The file cannot approve secrets, datastores, file additions or CI changes —
the fold enforces the structural class itself, and an MR editing the policy
escalates.

## 3. Calibrate in dry-run (the shipped posture)

Every real MR gets a verdict comment + gate, zero gestures. Exit criteria,
ALL of them:

- **≥ 20 verdicts** on real merge requests;
- **0 false approves** — every "would approve" independently checked by a
  human before the MR merged;
- **0 missed escalations** — nothing structural/secret/datastore-shaped was
  called simple;
- the **approve preflight** answered 201 on a scratch MR;
- the **peer-gate hold** observed once on a real MR (warden clean while
  `revi/review` red → no arm);
- devops signs the authored policy file.

Dry-run greens still poison calibration if the fold is wrong — a green gate
on a diff nobody read is the defect class, so spot-check greens too.

## 4. Flip to enforce

One PATCH, echo every key you keep (launch_vars are replace-with-refusal):

```sh
PATCH /api/teams/{team}/forge/repo-bots/{integration_id}
{"launch_vars":{"reviewers":"…","policy_path":"…","mode":"enforce"}}
```

Reversible the same way (`mode: dry_run`). The emergency disarm that needs
no release and no bot redeploy: add `"forge_publish_mutations":"false"` —
the grant mints no gesture capability at all, repo-wide, instantly.

## 5. What a verdict means

- **Green + enforce**: every file simple, platform guardrails respected,
  peers not red → the MR is approved and armed; the forge merges when its
  own conditions hold (pipeline green per the repo's settings). The bot
  never merges now, and never bypasses a required check.
- **Red**: the comment lists every reason (structural, doubts shown verbatim,
  LimitRange math, the managed equivalent for a self-hosted datastore) and
  requests the pinned reviewers. The red check is the page.
- **Refused** (`verdict_result.refused` non-empty): the engine declined a
  gesture — stale pin, peer held, capability missing. The bot's
  publish-health banners it; `/warden` on the MR re-runs.
