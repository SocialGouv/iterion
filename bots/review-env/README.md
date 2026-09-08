# review-env (Envy) 🌐

Deploys the **current workspace's already-CI-published image** to the
operator-attached platform and hands back a **live https URL** — a real
review environment (real TLS, real ingress, real DNS) that gives
end-to-end tests, screen/accessibility captures and human review the
realism a localhost approximation cannot.

One job, extracted so any flow can lease it: run it standalone, or as a
subbot of a larger campaign. The [app-dev bot](../app-dev/)'s opt-in
deploy phase shares the same skill and the same credential — this bot is
that capability alone, without the feature-development flow around it.

## The interchangeable-skill contract

This DSL names **no cloud and no PaaS**. The platform lives
in an operator-attached skill named `deploy-target`. One
Kubernetes-shaped hint survives in the `deploy` agent's own system
prompt (`kubectl get secret` / `rollout status` / ImagePullBackOff as a
measurement) — guidance for the common case, not a dependency:

- an org-private **plugin** contributes `skills/deploy-target.md` — the
  platform playbook (authenticate, provision, reference the image, apply
  deploy artifacts, wait for readiness, derive the public URL);
- the operator **enables exactly one** deploy-target plugin per instance
  (`~/.iterion/plugins.yaml`), and installs **one credential**;
- **swapping infrastructure = swapping that pair.** The bot never
  changes; a different cluster, a different cloud, a different PaaS is a
  different plugin. No `deploy-target` skill attached → the deploy
  refuses loudly (`deployed=false`, the note names the missing skill) —
  it never improvises a platform.

## The credential — by reference, never read

Declared as a file secret named **`deploy_credential`** (the same store
name the app-dev bot uses, so one installation serves both):

- mounted read-only at `{{secrets.deploy_credential}}` — that rendered
  path is the reliable reference. The secret's `env:` declaration
  (`DEPLOY_CREDENTIAL`) is injected into **tool** nodes, not into a
  delegated agent's shell, so `$DEPLOY_CREDENTIAL` may well be unset in
  the `deploy` agent; export it from the path rather than concluding the
  credential is missing;
- the bot and the skill pass it to tools by path/env only — never
  opened, printed, encoded or summarised; its bytes are redacted from
  logs;
- what the file **contains** (a kubeconfig, a token, anything) is the
  attached skill's business — this bot never knows;
- resolution is by name from the local/cloud secret store (see
  [docs/secrets.md](../../docs/secrets.md)); on cloud, bind it to the
  bot as a team-scoped secret.

## What the bot proves, and how

`deployed` and `healthy` are the agent's report. **`live` is measured**:
a deterministic gate probes the reported URL from outside the agent —
standard library, real certificate verification, expected status and a
non-empty body — and the run converges only on the conjunction
`deployed && healthy && live`. A URL that only answers with TLS checks
disabled is not live. On success a deterministic tool emits the
`[iterion] preview_url=… kind=deploy` directive, so the studio surfaces
the URL from stdout, never from prose.

The image is **the repo's own CI's**: a review environment must serve
what the forge built from the pushed commit, or it reviews something
nobody can reproduce. No published image → an honest
`deployed=false` naming what is missing, never a build here and never a
guessed reference. A dirty workspace is reported for the same reason:
the URL serves the pushed tree, not local edits.

## Using the URL for end-to-end realism

The returned `deployed_url` is a drop-in base URL:

- point a behavioural net's capture configuration at it to replay the
  oracle against a really-served environment;
- point browser-based end-to-end suites and accessibility scans at it;
- hand it to a reviewer as the environment for a branch.

```sh
cd <target-repo>
iterion run <path>/bots/review-env --var slug=myapp-mr42
# → deployed_url: https://… (kind=deploy directive surfaced in the studio)
```

| var | default | meaning |
|---|---|---|
| `slug` | `""` | DNS-safe environment name; reuse = in-place redeploy. Empty derives it from the repo name |
| `image_ref` | `""` | Exact published reference to deploy. Empty makes the agent read it back from the forge/registry the repo declares |
| `expected_status` | `200` | what the live URL must answer |
| `max_deploy_retries` | `2` | redeploy attempts on a not-live verdict |
