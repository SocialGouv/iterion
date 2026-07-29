# Secrets protection

Iterion runs agents that can be prompt-injected (the sec-audit bots read
untrusted repo content), shell out, and read files. Two distinct leak
surfaces are defended in layers:

1. **Exfiltration** — the agent sends a secret off-box.
2. **Observability leak** — a secret lands in clear in `events.jsonl`,
   artifacts, `run.log`, the studio/board stream, or `report.md`.

The engine is [`pkg/backend/secretguard`](../pkg/backend/secretguard):
a per-run `Guard` built by
[`model.BuildSecretGuard`](../pkg/backend/model/secretguard.go) from the
run's resolved credentials, sensitive host env vars, and the workflow's
declared `secrets:` block.

## Detection (incl. base64 and other encodings)

Two tiers:

- **Known-value taint (deterministic).** Iterion knows its secret
  values, so for each it precomputes every textual form — raw, base64
  (std + url, ±padding), hex (upper/lower), URL-escape, JSON-escape —
  and matches those literally via a single RE2 alternation. This is the
  reliable answer to "also detect base64": we match the base64 form of a
  secret we *hold*, we don't guess. Zero encoding false-negatives.
- **Heuristic (for unknown secrets).** The gitleaks-derived detector
  ([`tool/privacy/detector`](../pkg/backend/tool/privacy/detector)) +
  Shannon entropy, plus a recursive base64/hex decode pass that peels one
  layer off a blob and re-scans (catches an AKIA/JWT wrapped in base64
  that the agent read from a file iterion never registered).

## Layer 0 — sink redaction (default on)

`Guard.Redact` scrubs known values (any encoding) → their placeholder,
and unknown token shapes → `[redacted]`, at every **observational**
sink, before persistence:

- events.jsonl (all event types, via a redacting `AppendEvent` wrapper +
  `node_finished` output via the engine's `SecretScrubber`),
- run.log block bodies, tool sidecar blobs, turn-snapshot conversations.

**Deliberately NOT redacted:** persisted **artifacts** and the resume
**checkpoint**. These are load-bearing — they feed `{{outputs.X}}` /
`{{artifacts.X}}` and are re-read on resume; redacting them would corrupt
cross-node and cross-resume data flow. Their defence is Layer 1
(placeholders keep the real secret out of node output in the first place)
plus the run store being local/private.

## Layer 1 — placeholders + materialization (default on)

Declare secrets in the DSL; the agent only ever sees an opaque
placeholder `__ITERION_SECRET_<name>__`; iterion swaps in the real value
at the moment of execution.

```iter
secrets:
  github_token: "${GITHUB_TOKEN}"          # short form
  deploy_key:
    value: "${DEPLOY_KEY}"
    hosts: ["api.github.com", "github.com"] # egress scoping (Layer 2)
```

Reference as `{{secrets.deploy_key}}` in prompts and tool/shell commands.
Materialization happens immediately before exec, keeping the placeholder
form in every hook/log:

- **claw** `tool` nodes (shell + script) and the in-process tool loop
  (`executeToolsDirect`) call `Guard.Materialize` before exec.
- **claude_code** uses a `PreToolUse` hook returning `UpdatedInput` with
  the materialised tool input (the SDK-supported substitution path).
- Both consume `delegate.Task.MaterializeSecrets` (a closure set by the
  executor), so `pkg/backend/delegate` stays decoupled from secretguard.

Generic placeholder materialization is currently limited to `claw` and
`claude_code`. Pi, Kimi, Grok, and legacy Codex leave
`__ITERION_SECRET_*__` opaque, so use file secrets for those delegates when the
agent needs a non-provider credential.

Every backend still receives the **behavioural backstop**: a "## Secret
handling" system-prompt clause tells the agent not to read/exfiltrate
credential files and to pass placeholders through verbatim. This is never the
primary control — where supported, the structural boundary is the
materialization above.

### File secrets

Some credentials are safer and more ergonomic as files (`kubeconfig`,
cloud SDK config, deploy certs). Declare them with `as: file`:

```iter
secrets:
  kubeconfig:
    as: file
    # Optional in cloud: when omitted, iterion resolves a stored secret
    # named "kubeconfig" from /api/me/secrets or /api/teams/:id/secrets.
    value: "${KUBECONFIG_CONTENT}"
    mount_path: "/run/iterion/secrets/kubeconfig"
    env: "KUBECONFIG"
    hosts: ["api.cluster.example"]
```

For file secrets, `{{secrets.kubeconfig}}` and
`{{secrets.kubeconfig.path}}` render the mounted path, not the secret
content. The runtime writes the plaintext into a read-only file inside
the sandbox and injects `env` to point at that path when configured. The
agent prompt lists the mounted paths and explicitly instructs the agent
to pass the path/env var to commands, without opening, printing, encoding
or summarizing the file contents.

Default path when `mount_path` is omitted:
`/run/iterion/secrets/<sanitized-secret-name>`.

Custom `mount_path` values must be clean absolute file paths (no `..`,
duplicate separators, trailing slash, or `/`). Prefer the default
directory: the drivers create/mount it for the run. Custom file targets
depend on the parent directory already existing in the sandbox image.

#### `optional: true`

By default a declared secret with no resolved value (no `value:` expr,
no host env, no stored/bound secret) is a **hard launch error** — the
launch fails loudly, naming the secret (`secret "x" is declared required
by the workflow but resolves to nothing …`), and **no run record is
created**. The gate runs at launch on both paths — the cloud publisher
(`resolveAndSealCredentials`, before the run is persisted) and the local
/ in-process path (`runview.BuildExecutor`) — so a required credential
that resolves to nothing can never let a bot proceed unauthenticated
(push with no token, call an API with no key). A webhook-triggered launch
records the failure as `StatusLaunchError` on its delivery trail.

Mark a secret `optional: true` to skip it silently instead — for a bot
that only needs the credential on *some* runs:

```iter
secrets:
  forge_token:
    as: file
    optional: true   # mounted when bound/resolved; skipped otherwise
```

This is how a forge-agnostic reviewer (Revi) accepts an org's posting
token on an unattended webhook launch (the org binds its credential to
`forge_token`) while still running locally with host CLI auth when it
isn't provided. The agent reads the file path/env only — never the
contents.

#### Recipe: deploy to a real test instance + e2e loop

[`examples/deploy-e2e.bot`](../examples/deploy-e2e.bot) shows the full
pattern: a `kubeconfig` (with `env: KUBECONFIG`) and a host-scoped
`deploy_token`, both `as: file` + `optional: true`, mounted into a
`sandbox: auto` run where the agent builds, redeploys and validates the
deployment end-to-end (playwright MCP) in a bounded retry loop — passing
the secret *paths* to its commands, never reading the bytes.

Driver behaviour:

- Docker/Podman: writes payloads to private host temp files, mounts the
  default secret directory read-only (or custom file targets read-only),
  and deletes the temp directories at sandbox cleanup.
- Kubernetes: creates a per-run opaque Secret, mounts the default secret
  directory read-only (or custom file targets via `subPath`), and deletes
  the Secret with the sandbox pod.

**Mid-run refresh (ADR-069).** A file secret is a launch-time snapshot,
but a short-lived credential (e.g. a 1h GitHub App installation token)
would go stale on a long run that pushes/comments near the end. The cloud
runner re-reads each file secret's store record on a 5-minute cadence and,
when it rotated, propagates the fresh value into the running sandbox —
docker rewrites the bind-mount source file, kubernetes re-applies the
Secret (kubelet refreshes the projected volume within ~1min). This covers
the default directory-mounted secrets on both drivers. **Caveat:** a
kubernetes secret with a custom absolute `mount_path` is projected via
`subPath`, which kubelet does *not* auto-update, so that projection stays
at its launch value until pod restart; put refreshable tokens under the
default `/run/iterion/secrets` directory. Reads are tenant-scoped and the
value is never logged.

Cloud setup API:

- `GET/POST /api/me/secrets`
- `PATCH/DELETE /api/me/secrets/{secret_id}`
- `GET/POST /api/teams/{id}/secrets`
- `PATCH/DELETE /api/teams/{id}/secrets/{secret_id}`

Responses never include plaintext, only metadata (`name`, `last4`,
`fingerprint`, timestamps, scope). At publish time the cloud publisher
resolves declared secrets whose `value` is empty by name, seals them into
the per-run bundle, and the runner injects them into the sandbox runtime.
Secret names must be DSL identifiers (`[A-Za-z_][A-Za-z0-9_]*`) so they
can be referenced from `secrets:`.

## Layer 2 — TLS-inspection egress (default on for sandboxed runs)

For secrets the agent uses in its *own* TLS calls (e.g. claude_code's
Bash `curl`/`git push`), the sandbox egress proxy
([`pkg/sandbox/netproxy`](../pkg/sandbox/netproxy)) can terminate TLS and
rewrite the plaintext request (Deno-parity secret handling):

- A per-run **ephemeral CA** ([`ca.go`](../pkg/sandbox/netproxy/ca.go),
  in-memory, never persisted) mints per-host leaves. Its public cert is
  injected into the sandbox so in-container clients trust the leaves.
- **Substitution** ([`inspect.go`](../pkg/sandbox/netproxy/inspect.go)):
  `MaterializeForHost` swaps placeholder→value, but only toward a
  secret's approved `hosts:`.
- **Content DLP**: `ExfiltratesTo` blocks (403) a real secret value bound
  for a host it isn't scoped to — defeats domain-fronting the host
  allowlist can't see.

Inspection activates by default when a sandboxed run has known secrets;
it forces a proxy even under `network: open`. Why TLS inspection is safe
to do: Claude Code and the Anthropic/OpenAI SDKs are standard trust-store
clients with **no certificate pinning** (per the [official Claude Code
network-config docs](https://code.claude.com/docs/en/network-config) —
they work behind Zscaler/CrowdStrike/mitmproxy once the CA is trusted).
The default-transparent proxy is a cost choice, not a pinning constraint.

### Limitation: OAuth-forfait credentials are NOT substituted

For **OAuth-forfait** auth (Anthropic Claude Code OAuth, OpenAI
ChatGPT/Codex — the recommended credential model), egress substitution is
impractical: the CLI performs stateful token refresh, and the Consumer
Terms scope the forfait to Claude Code only (no API key to splice). Those
credentials are protected by **the network allowlist + Layer 0 redaction
+ the backstop clause**, not by Layer 2 substitution. Layer 2's
substitution/DLP value is for **declared workflow secrets** (a
`GITHUB_TOKEN`, a deploy key) and API-key mode.

### Status: live-validated in a docker sandbox (with trust-store caveat)

The MITM mechanism is hermetically tested end-to-end
([`inspect_test.go`](../pkg/sandbox/netproxy/inspect_test.go)) **and**
live-validated in a real docker sandbox (2026-06-08): a sandboxed `tool`
run with a `secrets:` entry scoped `hosts: ["example.com"]` confirmed
`inspect=true`, the per-run CA bind-mounted at `/run/iterion/egress-ca.pem`
and trusted in-container, a `--data {{secrets.X}}` call to the approved
host forwarded through the MITM to the real upstream (HTTP 405 from
example.com), and the same call to an unapproved host blocked by content
DLP (HTTP 403 + `secret exfiltration blocked` event). The real value
never appeared in the run store.

Trust injection by client (the docker driver sets all of these env vars
at the CA path, plus mounts the CA; in inspection mode every egress cert
is our leaf, so our-CA-only is correct):

| Client | Trust mechanism | Status |
|---|---|---|
| Node / Claude Code | `NODE_EXTRA_CA_CERTS` (additive) | live-validated — `fetch`/undici → example.com 200 through the MITM |
| curl | `CURL_CA_BUNDLE` | live-validated — approved→405, exfil→403, no `--cacert` needed |
| python ssl / requests | `SSL_CERT_FILE` / `REQUESTS_CA_BUNDLE` | env set (same mechanism as curl) |
| git | `GIT_SSL_CAINFO` / `SSL_CERT_FILE` | env set |

Remaining follow-ups:

- **Claude Code `WebFetch` specifically.** Plain Node `fetch`/undici
  honours `NODE_EXTRA_CA_CERTS` (validated above), but Claude Code's
  `WebFetch` tool has historically bundled its own undici dispatcher +
  does an `api.anthropic.com` domain-safety preflight. Confirm against a
  live `claude_code` run; if it trips, set `skipWebFetchPreflight: true`.
  The known `NODE_EXTRA_CA_CERTS`-ignored reports are Bun-runtime
  specific, not standard Node.
- **Kubernetes driver.** CA injection is implemented — `Driver.Start`
  creates a per-run Secret holding the public CA, the pod mounts it and
  the CA env vars point at it (`BuildCASecret` / `caInjection`,
  manifest-tested in `secrets_ca_test.go`), and
  `Capabilities.SupportsTLSInspection` is true. **Not yet
  cluster-validated** (needs a real cluster + a NetworkPolicy-aware CNI);
  the runner's RBAC must allow `secrets` create/delete in the sandbox
  namespace.

## Where the value comes from — the local secret store (desktop / non-cloud)

Layers 0–2 above protect a value *once iterion has it*. That value is
resolved into `Credentials.Generic[name]` at run start. Two sources feed it:

- **Cloud mode** — the auth-gated team/personal store (Mongo, `GenericSecretStore`)
  resolved by the publisher and shipped to the runner as a sealed per-run bundle.
- **Local mode (desktop / `iterion studio` / CLI)** — a **file-backed sealed
  store**, the desktop equivalent of the cloud store, reusing the same
  `GenericSecretStore` interface, `ResolveGeneric` resolution, and the whole
  Layer 0–2 pipeline. This is what replaces "put it in a `.env` and tell the
  agent to use it".

A declared secret with **no inline `value:`** resolves *by name* from this
store — so a bot declares what it needs, and the operator supplies it out of
band:

```iter
secrets:
  GITHUB_TOKEN:            # no value: → resolved by name from the local store
    hosts: [github.com]    # egress lock still applies (Layer 2)
```

### Storage, master key, scope

- **Files** — machine-global `~/.iterion/secrets.json` plus an optional
  per-project `<store-dir>/.iterion/secrets.json`. Both are AES-256-GCM sealed
  (the value is never on disk in clear) and written `0600`. The **project layer
  overrides the global by name** (precedence: project > global).
- **Master key** — 32 bytes held in the **OS keychain** (macOS Keychain /
  libsecret / Windows Credential Manager) when available; otherwise a
  **keyfile** `~/.iterion/secrets.key` (`0600`), created on first use with an
  explicit warning (no silent fallback). `ITERION_SECRETS_KEY` (base64)
  overrides both — parity with cloud, useful for CI. An existing keyfile is
  always preferred so a store sealed headlessly stays openable.

### Managing local secrets

CLI (values are read from a masked prompt, a stdin pipe, or `--from-env` —
never from `argv`, and never printed back):

```sh
iterion secret set GITHUB_TOKEN                 # masked prompt
iterion secret set STRIPE_KEY --from-env SK     # import from an env var
iterion secret set DB_URL --project --hosts db.internal
iterion secret list                             # names + last4 + scope only
iterion secret rm GITHUB_TOKEN
```

Studio: the **Secrets** view (gated on `server_info.secrets_enabled`) offers
the same CRUD over `/api/local/secrets` (unauthenticated single-operator
routes — the local studio is trusted to its loopback TTY user). Neither the
CLI nor the REST responses ever return a stored value.

The desktop app's provider-API-key keychain (`ANTHROPIC_API_KEY`, … under
`io.iterion.desktop`) is a **separate** concern — those are how iterion talks
to the LLMs, not what a bot uses inside a run.

## Environment kill-switches

| Var | Default | Effect |
|---|---|---|
| `ITERION_SECRETS_REDACT` | on | Master: off disables Layer 0 sink redaction (materialization still works). |
| `ITERION_SECRETS_REDACT_HEURISTIC` | on | off keeps known-value redaction but disables the gitleaks/entropy pass. |
| `ITERION_SECRETS_REDACT_DECODE` | on | off disables the recursive base64/hex decode pass. |
| `ITERION_SECRETS_REDACT_MIN_SCORE` | 0.7 | Heuristic confidence floor (the 0.6 generic high-entropy rule is excluded by default). |
| `ITERION_SECRETS_PLACEHOLDERS` | on | off renders `{{secrets.X}}` as the real value instead of a placeholder. |
| `ITERION_SANDBOX_TLS_INSPECT` | on | off disables Layer 2 TLS inspection (the escape hatch for a pinning client or broken CA injection). |

## Diagnostics

`C090` duplicate secret · `C091` secret/var name collision · `C092`
malformed egress host (Layer 2) · `C093` `{{secrets.X}}` references an
undeclared secret · `C094` invalid file-secret declaration · `C095`
invalid secret subfield reference (for example `.path` on a value
secret).
