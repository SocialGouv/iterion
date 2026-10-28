# ADR-047 — Keep the forge token out of the cloned repo's `.git/config`

Status: **accepted → implemented 2026-07-20 by a different mechanism, amended 2026-10-28** (no kill-switch — see Amendment)

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

> Amended 2026-10-28. What shipped keeps the goal of this section — no
> credential in `.git/config` — through a different mechanism, and without the
> kill-switch below. This section stays as decided because its reasoning is
> still the reason; the Amendment is the mechanism of record.

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

**Kill-switch** — *withdrawn 2026-10-28, never implemented.* As decided:
`ITERION_RUNNER_FORGE_LEGACY_URL_TOKEN=1` reverts to URL-embedding, so a cloud
regression in the push path can be unblocked instantly. The Amendment says why
that lever is not wanted.

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
  off-by-default per the implementation PR. *Amended: it shipped on by default,
  with no kill-switch and no gate. Whether the Validation list below was ever
  walked is not recorded; what is recorded is that every repo-targeted cloud
  run has used this path since 2026-07-20.*
- The credential helper must be robust to the per-binding `MountPath` and to both
  sandbox drivers (docker bind-mount, k8s secret projection).

## Validation (the part that cannot be done from a local dev box)

1. **Featurly in-sandbox push still works** end-to-end on the preprod k8s runner
   (the validated `issues.labeled → featurly → linked PR` flow).
2. `.git/config` of a cloned run contains **no token** (only the helper).
3. The forge_token `MountPath` resolves correctly under **both** the docker and
   kubernetes sandbox drivers, and the helper reads it at push time.
4. Review-bot flows that use the token file directly (glab/gh) are unaffected.
5. ~~The kill-switch restores the legacy path.~~ **Withdrawn** — there is no
   kill-switch, and nothing to validate (see Amendment).

## References

- Finding: Seki `sec-audit-source` self-audit (deepsec), `docs/bot-runs/sec-audit-source.md`.
- Code: `pkg/runner/loop.go` (`injectGitToken`, `prepareRepoWorkspace`),
  `pkg/secrets/files.go` (file-secret MountPath), the SSRF sibling hardening in
  the same clone path (ADR/commit `28a8690e8`).

## Status note (2026-10-13)

No kill-switch exists. `ITERION_RUNNER_FORGE_LEGACY_URL_TOKEN` is read nowhere
in the module, and nothing else gates the path either: the runner installs the
credential store on every clone that carries a token, so an operator who
exports that variable changes nothing.

The mechanism differs from the decision above. The clone authenticates with the
token in its URL rather than through a transient `GIT_ASKPASS`
([`injectGitToken`](../../pkg/runner/loop_gitws.go)); then
[`installGitCredentialStore`](../../pkg/runner/loop_gitws.go) rewrites
`remote.origin.url` to the clean URL, writes the raw token into
`<clone>/.git/iterion-credentials` in git's credential-store format (mode 0600,
under `.git/` so a bot's `git add -A` cannot stage it), and sets
`credential.helper` to `store --file=<that path>`. `.git/config` therefore
holds a file path and no credential, but the file it names holds the token
itself inside the clone — the helper never reads the per-binding `MountPath`.
That file is also what keeps the credential live:
[`refreshGitCredentialsLoop`](../../pkg/runner/loop_secrets.go) rewrites it as
the forge token rotates, and
[`writeThroughSandboxGitCredential`](../../pkg/runner/sandbox_registry.go)
carries the rotation into a copy-based sandbox, whose kubernetes driver
re-points the helper at the pod-local file ([sandbox](../sandbox.md),
[ADR-082](082-sandbox-by-default.md)).

## Amendment (2026-10-28) — the mechanism of record, and no kill-switch

### The mechanism, as shipped

The status note above is the accurate description: the clone authenticates with
the token in its URL, then
[`installGitCredentialStore`](../../pkg/runner/loop_gitws.go) cleans
`remote.origin.url` and moves the credential into
`<clone>/.git/iterion-credentials` (git's credential-store format, mode 0600,
under `.git/` so `git add -A` cannot stage it), with `credential.helper` set to
`store --file=<that path>`. That is the mechanism of record. It keeps what the
Decision was for — `.git/config` holds a path, never a credential — and gives
up what the Decision wanted it for: the helper does not read the per-binding
`MountPath`, because a repo-targeted run has a forge token even when the
workflow declares no file secret, so there is no mount path to `cat`. The file
is also what keeps the credential live as the token rotates
([`refreshGitCredentialsLoop`](../../pkg/runner/loop_secrets.go),
[`writeThroughSandboxGitCredential`](../../pkg/runner/sandbox_registry.go)).

### The kill-switch is withdrawn, not pending

`ITERION_RUNNER_FORGE_LEGACY_URL_TOKEN` is read nowhere in the module, and
never was — the name has never appeared in a `.go` file in this repository's
history (`git log -S … -- '*.go'` is empty). It is withdrawn from the record
rather than implemented: the legacy
path it would restore — the token persisted in `.git/config`, readable by
attacker-authored repository content running in the same sandbox — is exactly
the cross-tenant credential theft this ADR closes. A bound whose override is
the vulnerability is not an escape hatch, and iterion's "a load-bearing limit
keeps a greppable escape hatch" applies at a safe altitude, where the hatches
already exist: `ITERION_SECRETS_REDACT*` for the sinks, and the `forge_token`
binding itself for whether a run gets a push credential at all. An auth-path
regression is unblocked by not binding the token, not by re-leaking it.

### What contains the file

The credential store is named by `git.CredentialStoreFile`
([`pkg/git/safety.go`](../../pkg/git/safety.go)) so its writer and the readers
that must refuse it share one name — a rename that moved only the writer would
silently un-protect the file. Refused, as a prefix of the basename so the
sibling an atomic replace writes (`.tmp` on the host, `.tmp.$$` in the
sandbox) is covered too:

- the claw workspace readers — `read_file`, `grep`, `glob`
  (`sensitiveWorkspacePath`, [`pkg/backend/tool/claw_workspace_read.go`](../../pkg/backend/tool/claw_workspace_read.go));
- a bundle manifest's `authoring.editable_files`, whose `workspace` scope IS
  the clone root (`authoringSensitivePath`, [`pkg/bundle/manifest.go`](../../pkg/bundle/manifest.go));
- the review-scope and workspace-file handlers, through
  `ValidateRelPath`, which rejects any `.git` path component
  ([`pkg/git/safety.go`](../../pkg/git/safety.go));
- the pod → host workspace export (`exportExcludes`,
  [`pkg/sandbox/kubernetes/driver.go`](../../pkg/sandbox/kubernetes/driver.go)).

### What is left, and what stays out of scope

The **rotated** value is not registered with the run's output secret guard: the
guard is built once per run and is documented immutable, so a bot printing the
file after a rotation can land the new token verbatim in the run's log and
events (the heuristic detector catches the `ghs_`/`ghp_`/`gho_`/`glpat-` shapes
— the net, not the guarantee). Tracked in #2756, with the découpe.

The in-sandbox exposure stays **out of scope** here, explicitly rather than
silently: the workspace is the clone root and in-sandbox git must read the file
for the validated in-sandbox push, so any `bash` node — or an external pull
request's own build scripts — can read a push-capable token. That is this
ADR's own follow-on above (no-push-credential clones for untrusted-input bots,
per-run scoped tokens), unchanged by this amendment.
