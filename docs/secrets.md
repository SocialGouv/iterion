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
  values, so for each it precomputes every textual form — raw (and,
  for a value ending with a newline, without it, raw and JSON-escaped: a
  tool printing a file drops it), base64 (std + url, ±padding), hex
  (upper/lower), URL-escape (Go's, the strict RFC 3986 form, Python's
  `quote()` default that keeps `/`, and the WHATWG forms a JS runtime
  writes: query, form-urlencoded, path, fragment, userinfo, component),
  JSON-escape (Go's, which escapes `&` `<` `>`, and as `JSON.stringify` or
  `jq` write it) — and matches those literally in one pass: a form of 4 KiB
  or more (a file secret) and a binary one as a plain string, the others in
  leftmost-longest RE2 alternations. At a position the longest form wins; a
  form that starts inside another and runs past it is replaced too, so no
  part of either shows. This is the reliable answer to "also detect
  base64": we match the base64 form of a secret we *hold*, we don't
  guess. What it does not match: a form it does not precompute — base64
  or hex wrapped at a column width (`base64`, `xxd -p`), a multi-line
  value printed in part or line by line (`grep`, `head`, a file view that
  numbers its lines), a binary (non-UTF-8) value that went through a
  channel decoding it as UTF-8 (the claude_code stream, a Node tool: its
  invalid bytes arrive as U+FFFD).
- **Heuristic (for unknown secrets).** The gitleaks-derived detector
  ([`tool/privacy/detector`](../pkg/backend/tool/privacy/detector)) +
  Shannon entropy, plus a recursive base64/hex decode pass that peels one
  layer off a blob and re-scans (catches an AKIA/JWT wrapped in base64
  that the agent read from a file iterion never registered).

## Layer 0 — sink redaction (default on)

`Guard.Redact` scrubs known values (any encoding) → their placeholder,
and unknown token shapes → `[redacted]`, at every **observational**
sink, before persistence:

- events.jsonl (the backends' events via a redacting `AppendEvent`
  wrapper, `node_finished` output via the engine's `SecretScrubber`),
- a node's error, which the engine writes to `run.json`, `run.log`, its
  own events (`node_recovery`, `run_failed`) and the completion webhook —
  scrubbed where the executor returns it (`ClawExecutor.Execute`), for
  every node kind: a failing command's stdout and stderr, an MCP error
  echoing its input,
- the engine's events that carry model- or operator-written text: the
  whole `human_input_requested` payload of a question the run pauses on (a
  human node's rendered instructions, a fan-out branch's pause, an async
  question), an LLM router's reasoning, a review's verdict, the answers
  recorded at a resume or at a review gate (the interaction and the
  checkpoint keep them whole: the run needs them). A placeholder in them is
  kept as is — the heuristic pass never redacts one, trailing sentence
  punctuation included,
- run.log block bodies, tool sidecar blobs, turn-snapshot conversations.

Values iterion only scrubs — the run's provider keys, the process's own
secret-named environment, and credentials a server mints for a run (the forge
publish grant) — are **redact-only**: their placeholder resolves nowhere, so
no agent, command or egress request can turn it back into the value, and it is
passed through unchanged. Only the secrets a workflow declares (Layer 1)
materialize.

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

```iter fragment
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
  the materialised tool input (the SDK-supported substitution path). The
  placeholders are swapped in the input's decoded strings, never in its JSON
  text; a shell's (or a monitor's) `description` stays in placeholder form —
  the CLI labels a background shell with it, or with its command when there
  is none, and relays that label to the agent when the shell ends.
- Neither materialises the input of a tool that keeps it rather than runs
  it — the node's report (claude_code's `StructuredOutput` stores the input
  it is called with), the session's task list (`TodoWrite`, `TaskCreate`,
  `TaskUpdate`; claw's `todo_write`, written to disk), a scheduled prompt
  (`CronCreate` writes it to `.claude/scheduled_tasks.json`,
  `ScheduleWakeup` sends it back to the model), a memory note
  (`memory_write`; claw's `memory_write`, the knowledge store the next run
  loads), a skill's arguments (`Skill`: the CLI injects the skill's body
  with them), claw's PII vault (`privacy_filter`), a question to the
  operator (`ask_user`), iterion's own MCP tools (a board issue, a run
  query): they stay in placeholder form. The auto-memory mirror persists
  `MEMORY.md` in placeholder form too (the file tools write it with the
  values).
- A command naming a secret is not compressed (claude_code's `rtk` hook,
  claw's agent loop, a tool node's `compress:`): the compressor runs the
  command it rewrites, and rtk records every command it runs in its
  history — the value would land there. A shell iterion compresses also
  runs with the rewriter's `run_env` (rtk's: `RTK_RECALL=0`, and its
  history database under `/dev/null`, a path nothing can create) — every
  shell of a node, whatever its mode, as the agent may run rtk itself —,
  exported by each command it compresses as well (a settings `env` or a
  shell rc cannot outrank it there), which turns off the stores where rtk
  keeps the commands it ran and the output lines it left out: a command
  carrying a secret some other way (its raw value, an environment variable),
  or an output quoting one, leaves nothing there either. On claude_code the
  run env is pinned in the CLI's `--settings` flag layer too, which a user's
  or a repository's settings `env` cannot outrank; a command iterion did not
  compress — rtk typed by the agent, or run by an operator's own rtk hook —
  still gets it from its environment only, which the operator's shell rc or
  `BASH_ENV` can replace.
- Both consume `delegate.Task.MaterializeSecrets` (a closure set by the
  executor), so `pkg/backend/delegate` stays decoupled from secretguard.
- Its mirror, `delegate.Task.UnmaterializeSecrets` (`Guard.Unmaterialize`),
  turns a known value — in any registered encoding — back into its
  placeholder in what iterion carries from the far side of that boundary into
  a prompt or the run store — a background task's label is the CLI's command
  after materialisation. It is not a sink pass: `ITERION_SECRETS_REDACT=off`
  leaves it on.
- A verified action's self-repair (`policy: recover`) quotes the failing
  command and its output to a model: both in placeholder form.
- Both turn known values back into placeholders in the output of every tool
  but the workspace readers — stopping a background shell names its command,
  a notebook edit its new source, a redirected fetch its URL, a task or a
  message what it was given, a foreground subagent's report whatever it saw,
  an MCP tool whatever it reports: **claude_code** through a `PostToolUse`
  hook (`updatedToolOutput`), **claw** before the result enters the
  conversation. The workspace readers' output — a file (`Read`,
  `read_file`), a search over files, an edit's snippet, a command's output
  (`Bash`, `bash`) — is left as is, unless the call itself carried a secret
  (a search's pattern, an image's URL: it may quote it back) or reads the
  CLI's background task outputs — a task's file (named by a glob or a
  variable too), their directory, or the CLI's tmp root (`claude-<uid>`): a
  command's output, read after the call that ran it. A path in the node's
  workspace is the workspace's own, whatever its name (a worktree is named
  by its run's UUID, like the CLI's session directory): an agent editing a
  line that holds a secret must see the value the file holds.
  **Known limitations, claude_code only:** a call a permission rule denies is
  refused with its materialised input quoted to the agent, an MCP tool's
  error result reaches it as is, a background subagent's report reaches
  the lead verbatim in its task notification, a monitor's events — each line
  its command prints — reach the agent as the CLI relays them, and after a
  compaction the CLI re-announces every background shell or monitor still
  running with the command it runs — materialised (a foreground command the
  CLI moves to the background past its timeout included) — no hook runs on
  any of them. A
  settings hook iterion does not own (the operator's user settings, the
  target repository's `.claude/settings.json`, an iterion `hooks` plugin)
  runs beside iterion's, and the CLI keeps the answer that lands last: one
  that rewrites a tool's input runs the command with the placeholder, one
  that rewrites a tool's output can send the value it received to the
  model. A read of a task's output by a relative path after a `cd` made in
  an earlier call, or through a variable or a parent of the CLI's tmp
  directory (`grep -r "$TMPDIR"`), is not recognised as one, and a background
  task's output file stays in the CLI's tmp directory after the session;
  and the CLI keeps a tool's
  original output when the replacement does not match its output schema
  (iterion keeps the replacement on schema where the original can be off it:
  a notebook's language, read from its own metadata). Keep a secret out of a
  command a deny rule may match, or an MCP call that may fail on it (a file
  secret).

Generic placeholder materialization is currently limited to `claw` and
`claude_code`. Pi, Kimi, Grok, and Codex leave
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

```iter fragment
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

```iter fragment
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
  `MaterializeForHostWithin` swaps placeholder→value, but only toward a
  secret's approved `hosts:` (a secret that declares none goes toward any
  host), and within the inspection bound.
- **Content DLP**: `ExfiltratesTo` blocks (403) a real secret value bound
  for a host it isn't scoped to — defeats domain-fronting the host
  allowlist can't see.
- **A model's conversation is never substituted.** The harness's own model
  calls (the claude CLI, the claw runner) leave the sandbox through the
  same proxy, their conversation naming secrets by their placeholders. A
  request to a model API keeps its body as sent: a provider's host
  (`api.anthropic.com`, `api.openai.com`, `openrouter.ai`, `api.x.ai`,
  `api.mistral.ai`, `api.z.ai`, `api.moonshot.ai`/`.cn`, the Gemini and
  Vertex AI hosts, Azure OpenAI, Bedrock runtime), a model API's path at
  any host (`/v1/messages`, `/chat/completions`, `/v1/completions`,
  `/responses` under any base (a gateway's, Copilot's, the ChatGPT
  forfait's `chatgpt.com/backend-api/codex/responses`), `/v1/embeddings`,
  `/images/generations`, `:generateContent`, `:rawPredict` and their
  streaming forms, Ollama's `/api/chat` and `/api/generate`, Bedrock's
  `/model/{id}/invoke` and `/converse`), or a host listed in
  `ITERION_SANDBOX_MODEL_HOSTS` (a gateway at a path of its own, in the
  network rules' syntax — `gw.corp`, `*.corp`, `**.corp`, an IP, a CIDR,
  `!` to exclude; a base URL or `host:port` names its host; an entry that
  is none of these, one matching every host (`*`, `**`), or a list of
  exclusions only, fails the run's start whenever its sandbox starts the
  proxy, the entry named by its position). Its
  headers are substituted as usual (an API key a tool gives as a
  placeholder), and content DLP applies to it like to any request. A
  tool that sends a secret in the body of a model API call gets the
  placeholder there: a model sees placeholders, never values
  ([`model.go`](../pkg/sandbox/netproxy/model.go)).
- **A scoped value in the conversation.** A secret scoped with `hosts:`
  whose value a workspace reader showed the agent (Read, Bash — they
  return what the workspace holds) is in the conversation from then on:
  content DLP refuses the run's next model call (403 `blocked by sandbox
  secret policy`, a `network_blocked` event). Keep such a file out of the
  agent's reads, or scope the secret to the model provider's host too.
- **A request goes where its tunnel was opened.** Policy, content DLP and
  substitution all key on the `CONNECT` target; a request naming another
  host inside the tunnel — its `Host` header, an absolute URL, another
  port — is refused (421, a `network_blocked` event) rather than forwarded
  there with the target's secrets. A client that routes a request to
  another host than its URL's through the proxy (`curl --connect-to`) is
  refused that way.
- **Plain HTTP too.** A plain-HTTP request through the same proxy gets the
  same content DLP as an inspected one — its method, URL, headers and body;
  a chunked request's trailers, which it does not scan, are dropped (on
  both paths).
  Its placeholders are never substituted: a value never goes out over clear
  text.
- **Clients that honour the proxy.** The drivers set `HTTPS_PROXY`,
  `HTTP_PROXY` and their lower-case spellings (curl, wget and git read only
  `http_proxy` for an `http://` URL), and `NO_PROXY`/`no_proxy` to the
  sandbox's own loopback — `localhost,127.0.0.1,0.0.0.0`, plus
  `host.docker.internal` on docker (the host's MCP listeners, reached
  directly). Docker ADDS those to the entries the spec carried, under both
  spellings: nothing enforces egress there, so an inherited corporate
  proxy's exceptions stay the operator's. A pod REPLACES them: its egress
  is locked by the synthesized NetworkPolicy, and an entry kept from the
  spec would carve a hole in the allowlist that policy enforces. No IPv6 loopback: clients read entries as patterns, and every
  spelling of it breaks a mainstream one — a bare `::1` reads to Ruby's
  Net::HTTP as any IPv4 address ending in `.1` and to Python's requests as
  any IPv6 address ending in `::1`, while `[::1]` and `::1/128` make httpx
  refuse to build a client. Where the driver enforces no egress
  policy — docker, or a kubernetes cluster whose CNI ignores
  NetworkPolicy — a client that ignores them reaches the network directly:
  the proxy governs the clients that use it.
- **A bounded body.** The proxy holds a request's body to scan and
  substitute it: one over the bound (64 MiB by default) is refused (413),
  never cut — a plain-HTTP upload included (an `http://` git push, an
  artifact `curl -T`), which the proxy now scans too. The bound holds for
  what substitution makes as well: a placeholder expands to its value, so
  a body that would pass the bound once substituted is refused, and so are
  header values whose substitution would add more than the bound.
  `ITERION_SANDBOX_INSPECT_MAX_BODY` moves the bound (a byte count, or
  `256MiB`, `1GiB`; the proxy holds several times that per request in
  memory — about five with the header budget, more as a request carries
  more distinct secrets, and once per request in flight);
  `ITERION_SANDBOX_TLS_INSPECT=off` lifts it with Layer 2.
- **What the scan sees.** Content DLP matches each registered form of a
  value as one contiguous run of the request's text — its method, URL,
  headers (a field name as sent when the value is all lower-case: Go
  canonicalises names, and the other case shapes are a follow-up) and body.
  A value split across two fields, or re-encoded in a way the guard does
  not register, is not seen: the gate stops a leak, not an agent that
  works around it.

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

```iter fragment
secrets:
  GITHUB_TOKEN:            # no value: → resolved by name from the local store
    hosts: ["github.com"]    # egress lock still applies (Layer 2)
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
iterion secret set DB_PASSPHRASE --kind raw     # not a token / JSON / PEM
iterion secret list                             # names + last4 + scope only
iterion secret rm GITHUB_TOKEN
```

#### Ingestion shape gate (`--kind`)

`secret set` refuses, at the paste, a value that could not possibly
authenticate — the same rule the cloud API runs on BYOK keys and OAuth blobs
([`pkg/secrets/credential_shape.go`](../pkg/secrets/credential_shape.go)), so
a terminal transcript pasted into the prompt fails here instead of surfacing
as a provider `401` in the middle of a run.

The local store is name-keyed and carries no kind of its own, so the shape is
either **read off the value** — a `-----BEGIN ` header → `pem`, a leading `{`
or `[` → `json`, anything else → `token` — or **named with `--kind`**:

| `--kind` | Rule |
| --- | --- |
| `token` | One run of visible characters: no white-space (ASCII or not), no control/format/non-printing rune, valid UTF-8. |
| `json` | Parses as a top-level JSON object or array, and carries at least one member. |
| `pem` | At least one complete `-----BEGIN`/`-----END` block decodes. |
| `raw` | No check — the explicit opt-out for a passphrase, a connection string, a blob. |

The refusal names the secret, the reason and the kind it was read as, and
never the value; `--kind raw` is in the message, so the remedy travels with
it. An unknown `--kind` is an error, not a silent pass-through to no checking.

Studio: the **Secrets** view (gated on `server_info.secrets_enabled`) offers
the same CRUD over `/api/local/secrets` (unauthenticated single-operator
routes — the local studio is trusted to its loopback TTY user), **including
the same gate**: the create/rotate request carries an optional `kind` (the
view's *Kind* picker, defaulting to "detect from the value"), the shape is
resolved by the same `secrets.ResolveSecretShape` the CLI calls, and a refusal
answers `400` naming the kind and the `raw` opt-out. Two doors into one store
must not disagree on what a value is. Neither the CLI nor the REST responses
ever return a stored value.

The desktop app's provider-API-key keychain (`ANTHROPIC_API_KEY`, … under
`io.iterion.desktop`) is a **separate** concern — those are how iterion talks
to the LLMs, not what a bot uses inside a run.

## Environment kill-switches

| Var | Default | Effect |
|---|---|---|
| `ITERION_SECRETS_REDACT` | on | Master: off disables Layer 0 sink redaction (materialization, and its mirror in what iterion writes into a prompt or the run store, still work). |
| `ITERION_SECRETS_REDACT_HEURISTIC` | on | off keeps known-value redaction but disables the gitleaks/entropy pass. |
| `ITERION_SECRETS_REDACT_DECODE` | on | off disables the recursive base64/hex decode pass. |
| `ITERION_SECRETS_REDACT_MIN_SCORE` | 0.7 | Heuristic confidence floor (the 0.6 generic high-entropy rule is excluded by default). |
| `ITERION_SECRETS_PLACEHOLDERS` | on | off renders `{{secrets.X}}` as the real value instead of a placeholder. |
| `ITERION_SANDBOX_TLS_INSPECT` | on | off disables Layer 2 — TLS inspection and the plain-HTTP content DLP alike (the escape hatch for a pinning client or broken CA injection). |
| `ITERION_SANDBOX_MODEL_HOSTS` | (none) | Your own model gateways, comma- or space-separated, in the network rules' syntax (a base URL or `host:port` names its host): Layer 2 leaves their request bodies in placeholder form, beside the built-in providers and model API paths. An invalid entry fails the run's start whenever its sandbox starts the proxy. |
| `ITERION_SANDBOX_INSPECT_MAX_BODY` | 64 MiB | The bound of a request body Layer 2 holds to inspect: a byte count or a KiB/MiB/GiB size (`256MiB`); a larger body is refused (413). It bounds what substitution makes too, and the request line and headers keep net/http's own bound. The proxy holds several times the bound per request in memory (about five with the header budget, more with several distinct secrets), once per request in flight. A value that is no positive size fails the run's start whenever its sandbox starts the proxy. |

## Diagnostics

`C090` duplicate secret · `C091` secret/var name collision · `C092`
malformed egress host (Layer 2) · `C093` `{{secrets.X}}` references an
undeclared secret · `C094` invalid file-secret declaration · `C095`
invalid secret subfield reference (for example `.path` on a value
secret).
