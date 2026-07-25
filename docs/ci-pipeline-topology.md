# CI image pipeline — topology

The image workflows are split so that **each deployable artifact's workflow
completion is that artifact's "ready to redeploy" signal**, and nothing a
deploy doesn't need ever sits on that signal. A CD watcher (an agent today, a
CD workflow tomorrow) keys on the completion of the workflow that owns the
image it redeploys.

## What deploys from what

| Prod target | Image / tag it tracks | Built by | Signal = completion of |
|---|---|---|---|
| server pods | `iterion:vX.Y.Z` (chart appVersion) | `image.yml` (tag → multi-arch + signed) | `image.yml` |
| runner pods | `iterion-runner-devbox:edge` (rolling main tag) | `runner-image.yml` (main → amd64) | `runner-image.yml` |

Sandbox images (`iterion-sandbox-*`) are pulled at bot-run time, not by any
deploy — they are off every deploy path.

> ⚠️ **Signal-contract change — a redeploy watcher must be reconfigured.**
> Before this split, `image.yml` built the server AND the runner, so its
> completion meant *both* were ready. Now `image.yml` completes when only the
> **server** is ready; the runner `:edge` is published ~2-3 min later by
> `runner-image.yml`. **A CD/agent that redeploys the runner must key on
> `runner-image.yml` completion, not `image.yml`** — otherwise it redeploys a
> one-cycle-stale runner. Server redeploy keys on `image.yml` as before.
>
> Minor knock-on: `trivy.yml` image-scan runs on `image.yml` completion and
> scans `iterion-sandbox-*:edge`, which the fresh refold republishes just
> after — so those sandbox scans lag by one cycle (non-blocking; the tool
> bases change only on `sandbox/**`, so the content is near-always identical).

## Workflows

```
image.yml            server `iterion`             push main (amd64 :edge) · tags (multi-arch :vX.Y.Z, signed) · PR
   └─ completion = SERVER redeploy signal
        │ workflow_run (main, success)
        ├─▶ runner-image.yml   runner `iterion-runner-devbox:edge`  (amd64)   └─ completion = RUNNER redeploy signal
        └─▶ sandbox-images.yml (finalize only, bases skipped)  refold sandbox :edge with the fresh binary

sandbox-images.yml   bases + finalizes            push sandbox/** (bases→finalize) · workflow_run(image.yml, main) (finalize) · PR
release-images.yml   runner + sandbox @ :vX.Y.Z   push tags v*  (await image.yml's server, then multi-arch + signed)
```

### Why the splits

- **Server is amd64-only on main** (`:edge`): OVH prod is amd64 (12/12 nodes),
  `:edge` is ephemeral, so the deployable tag is never gated by the slower
  arm64 leg. On a **tag** the server is multi-arch + signed in-line — arm64 IS
  the release artifact, and releases are infrequent.
- **Runner has its own workflow** so the server signal never waits on it (it
  bakes the server binary via `COPY --from`, i.e. it can only run *after* the
  server image). It is `workflow_run`-triggered off `image.yml` on main.
- **Sandbox finalizes** re-run on the same `workflow_run` (fresh binary onto
  the unchanged tool bases) and on `sandbox/**` pushes (fresh tools). Never on
  the deploy path.
- **release-images.yml** builds the `:vX.Y.Z` runner + sandbox tags that
  `image.yml` no longer builds on a tag. It is tag-push-triggered (so
  `github.ref` is the tag → correct checkout + semver tag naming), and its
  `await-server` job polls until `image.yml` has published `iterion:vX.Y.Z`
  before it `COPY`s the binary — so the two workflows never race to publish
  the server tag.

### workflow_run notes

- `workflow_run` triggers use the workflow definition from the **default
  branch** and fire for every `image.yml` completion (main, tag, PR). The
  downstream `prep` jobs guard to `conclusion == success && event == 'push' &&
  head_branch == 'main'` so only a successful main build refreshes the runner /
  sandbox `:edge`; tags go through `release-images.yml`, PRs publish nothing.
- No cycles: nothing re-triggers `image.yml`, so the `image.yml → {runner,
  sandbox}` fan-out terminates.
