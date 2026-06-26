# ADR-047 — Keep the forge token out of the cloned repo's `.git/config`

Status: **Proposed** (implementation pending cloud e2e — see Validation)

## Context

The cloud runner clones a tenant's repository with a per-org forge token so
commit-producing bots (Featurly/feature-dev, review bots) can fetch and push.
Today [`injectGitToken`](../../pkg/runner/loop.go) rewrites the clone URL to
`https://oauth2:<forge_token>@host/…` and `git` **persists that URL verbatim in
`<workspace>/.git/config`**. The workspace (including `.git/config`) is then
bind-mounted (docker) / tar-copied (k8s) into the sandbox where bots execute
**attacker-authored repository content** — e.g. an external PR under review runs
its own build/test scripts. Those scripts can read `.git/config`, recover the
bound per-tenant token, and exfiltrate it to the (allowlisted) forge host. This
crosses the sandbox trust boundary — **cross-tenant credential theft**.

This was surfaced repeatedly by iterion's own Seki (`sec-audit-source`) dogfood
self-audit (runs `019f02f4`, `019f034b`, `019f03df`; deepsec, medium/high).

The constraint that makes it non-trivial: **there is no `git push` in iterion's
Go code — bots push from *inside* the sandbox** using that persisted token. So
the credential cannot simply be stripped (that breaks the validated in-sandbox
push), and it cannot be hidden from untrusted code that runs in the *same*
sandbox as the push.

Key existing fact: the forge token is **already** delivered to the sandbox as a
deliberately-mounted 0600 secret **file** (`as: file`, e.g.
`/run/secrets/forge_token` or `/run/iterion/secrets/<name>` — the `MountPath` is
per-binding, [`pkg/secrets/files.go`](../../pkg/secrets/files.go)), separate from
the `.git/config` URL token. The `.git/config` token is an **unintended second
copy** that `git` creates from the URL.

## Decision

**Stop persisting the token in `.git/config`. Authenticate git via a credential
helper that reads the deliberately-mounted secret file at git-auth time.**

1. **Runner clone/fetch**: clone the *clean* URL (no userinfo token). Supply the
   runner's in-process token transiently via `GIT_ASKPASS` (an env-pointed
   helper script) or `-c credential.helper=…` on the command line — neither is
   written to `.git/config`.
2. **Persist a credential helper** (not the token) in the clone's *local*
   `.git/config` that, on `get`, prints `username=oauth2` +
   `password=$(cat <forge_token MountPath>)`. The `MountPath` is threaded from
   the resolved secret binding (it is per-binding, so it must be passed in, not
   hardcoded). `store`/`erase` no-op.
3. Result: `.git/config` contains only a `credential.helper` that references a
   **file path** — never the raw token. In-sandbox push works (the helper reads
   the same mounted secret the bots already use). Untrusted code reading
   `.git/config` finds a path reference; to get the token it must read the
   mounted secret file — which is the existing, separately-scoped sandbox-secret
   exposure, not a `.git/config` leak.

**Kill-switch**: `ITERION_RUNNER_FORGE_LEGACY_URL_TOKEN=1` reverts to today's
URL-embedding, so a cloud regression in the push path can be unblocked instantly.

### Follow-on (deeper trust separation)

The credential-helper change removes the *unintended* `.git/config` copy but the
token is still reachable by untrusted code that runs in the same sandbox as a
push-capable bot. The complete trust fix classifies bots by trust:
**untrusted-input bots** (reviewing an external PR's code) get **no push
credential mounted at all** — they post via the forge API or push to a fork;
only **trusted-author bots** (operating on the operator's own repo) get the push
secret. Alternatively, mint **short-lived, narrowly-scoped** tokens per run
(GitHub App installation tokens scoped to one repo/branch with a few-minute TTL)
so an exfiltrated token's blast radius and lifetime are bounded. Both are larger,
forge-specific changes tracked separately.

## Consequences

- `.git/config` no longer contains a credential — the literal finding is closed.
- The clone/push auth path changes; **must be validated in cloud e2e** before it
  is the default (see Validation). Until then it ships behind the kill-switch /
  off-by-default per the implementation PR.
- The credential helper must be robust to the per-binding `MountPath` and to both
  sandbox drivers (docker bind-mount, k8s secret projection).

## Validation (the part that cannot be done from a local dev box)

1. **Featurly in-sandbox push still works** end-to-end on the preprod k8s runner
   (the validated `issues.labeled → featurly → linked PR` flow).
2. `.git/config` of a cloned run contains **no token** (only the helper).
3. The forge_token `MountPath` resolves correctly under **both** the docker and
   kubernetes sandbox drivers, and the helper reads it at push time.
4. Review-bot flows that use the token file directly (glab/gh) are unaffected.
5. The kill-switch restores the legacy path.

## References

- Finding: Seki `sec-audit-source` self-audit (deepsec), `docs/bot-runs/sec-audit-source.md`.
- Code: `pkg/runner/loop.go` (`injectGitToken`, `prepareRepoWorkspace`),
  `pkg/secrets/files.go` (file-secret MountPath), the SSRF sibling hardening in
  the same clone path (ADR/commit `28a8690e8`).
