---
name: policy-template
description: Starter review-policy.md (the policy_path var pins the real path) for a gitops repo: what may widen or tighten the auto-approvable class, and what can never be widened
---

# Review policy — what merges without a human here

This file is read by the gitops review bot (gitops-warden) from the TARGET
branch of every merge request, before it classifies the diff. Edit it on
`main`; the change protects itself — an MR that edits this file is escalated
to devops review, never self-applied.

The bot ships with a default policy. This file may only:

- **widen** the auto-approvable class with more value-level patterns, and
- **tighten** anything.

It can never approve secrets, datastores, file additions, CI changes or
anything the default class calls structural — those need a devops review no
matter what this file says (see the bot's review-policy skill for the full
contract).

## Auto-approvable here (in addition to the bot's defaults)

Uncomment and adapt:

```yaml
# patterns:
#   - match: "resources.requests.memory"     # example: widening is value-level only
#   - match: "annotations.monitoring.*"
```

## Extra escalations for this repo

```yaml
escalate:
  paths:
    # - "apps/legacy-app/**"   # example: everything under this path needs a human
```

## Contacts

Escalations request review from the logins pinned on the integration (the
devops on-call). To change them, ask devops — they are not set in this file.
