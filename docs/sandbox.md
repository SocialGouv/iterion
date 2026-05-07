# iterion sandbox

The iterion sandbox provides per-run isolation for coding agents and
shell tool nodes via a Docker (or Podman) container. It is **opt-in**:
workflows that don't declare `sandbox:` and runs that don't pass
`--sandbox` execute exactly as they did before this feature shipped.

## Quick start

The shortest path to a sandboxed run:

1. Add (or reuse) a `.devcontainer/devcontainer.json` in your repo.
2. Set `sandbox: auto` on your workflow:

   ```iter
   workflow review:
     worktree: auto
     sandbox: auto
     entry: plan
   ```

3. Run the workflow as usual. iterion will pull the image, start the
   container, route claude_code and tool nodes through it, and tear
   the container down on exit.

To enable sandboxing without touching the workflow source, pass
`--sandbox=auto` to `iterion run`:

```bash
iterion run review.iter --sandbox=auto
```

## How it works

### Lifecycle

```
runStart
  ├─ resolveSandboxSpec       (CLI > workflow > global default)
  ├─ Driver.Prepare           (validate spec, pull image if missing)
  ├─ startNetworkProxy        (HTTP CONNECT proxy on 127.0.0.1)
  ├─ Driver.Start             (docker run --detach with sleep infinity)
  ├─ executor.SetSandbox(run) (engine pushes the handle into the executor)
  ├─ ... node executions stream through `docker exec` ...
  └─ defer cleanup            (Stop + Remove container, Shutdown proxy)
```

A **single container** hosts the entire run. Multiple `docker exec`
calls amortise the create+start cost over every claude_code, codex,
or tool node invocation. The container's PID 1 is `sleep infinity` —
iterion deliberately ignores the image's CMD/ENTRYPOINT in favour of
treating the container as a long-lived "ssh-like" target.

### Workspace bind-mount

The host worktree (when `worktree: auto`) or repo (when `worktree: none`)
is bind-mounted RW at `/workspace` inside the container. This is the
container's working directory by default. Override via
`workspaceFolder` in `.devcontainer/devcontainer.json`.

### Network policy

When a sandbox is active, an iterion-managed HTTP CONNECT proxy runs
on the host (127.0.0.1, ephemeral port). The container receives the
proxy URL via standard `HTTPS_PROXY` / `HTTP_PROXY` env vars and
reaches it via the `host.docker.internal` alias.

The proxy enforces a rule list compiled from the workflow's
`network:` block (when present) prefixed by a named preset. The
default preset is **`iterion-default`** which allows the LLM
endpoints (anthropic, openai, openrouter, bedrock, googleapis,
azure, mistral) plus package registries (npm, PyPI, golang proxy)
plus code hosts (github, gitlab, bitbucket) plus apt mirrors.

The proxy does NOT MITM TLS — only the SNI / Host header is
inspected. The Anthropic SDK's cert pinning continues to work
unchanged.

Pattern syntax (last-match-wins evaluation):

| Pattern              | Matches                                  |
| -------------------- | ---------------------------------------- |
| `api.anthropic.com`  | exact case-insensitive host              |
| `*.example.com`      | exactly one DNS label (`foo.example.com`) |
| `**.example.com`     | one or more labels (`a.b.example.com`)   |
| `**`                 | any host (the "open" sentinel)            |
| `1.2.3.4`            | IPv4 literal exact match                  |
| `10.0.0.0/8`         | CIDR range                                |
| `!pattern`           | exclusion (negation)                      |

Modes:

| Mode        | Behaviour for unmatched hosts             |
| ----------- | ----------------------------------------- |
| `allowlist` | deny (the default)                        |
| `denylist`  | allow                                     |
| `open`      | accept everything (skips the proxy entirely) |

**IP literals are refused by default in allowlist mode** even when
their hostname is allowed, which closes the cloud-metadata exfiltration
vector (169.254.169.254 etc.). Add explicit IP rules to relax.

Blocked requests surface to the run as a `network_blocked` event in
`events.jsonl`:

```json
{"type": "network_blocked", "data": {"host": "evil.site", "reason": "policy denial", "run_id": "..."}}
```

## Configuration surface

### `.iter` workflow

Today only the simple form is parsed:

```iter
workflow x:
  sandbox: auto      # read .devcontainer/devcontainer.json
  # OR
  sandbox: none      # explicit opt-out (overrides global default)
```

Per-node overrides accept the same simple form on `agent`, `judge`,
`tool`:

```iter
agent shell_helper:
  sandbox: none      # this node runs on the host even though the
                     # workflow has sandbox: auto
```

The block form (inline image / mounts / network) is on the roadmap
but not yet shipped — use `sandbox: auto` with a
`.devcontainer/devcontainer.json` for full configuration.

### CLI

```bash
iterion run foo.iter --sandbox=auto    # one-shot override
iterion run foo.iter --sandbox=none    # force off
iterion run foo.iter                   # use workflow + global default
iterion sandbox doctor                 # report driver + capabilities
```

### Environment / project config

- `ITERION_SANDBOX_DEFAULT` — global default (`""`, `none`, or `auto`).
  Lowest precedence. Workflows and CLI override.

### Precedence (highest → lowest)

1. Per-node `sandbox:` declaration (DSL)
2. CLI `--sandbox` flag
3. Workflow-level `sandbox:` declaration (DSL)
4. `ITERION_SANDBOX_DEFAULT` env var
5. Implicit `none` (no sandbox)

## Backend compatibility

| Backend       | Sandbox status                                        |
| ------------- | ----------------------------------------------------- |
| `claude_code` | **fully sandboxed** (CLI runs inside the container)   |
| `codex`       | partially sandboxed (host CLI; codex has its own internal sandbox) |
| `claw`        | **sandboxed via runner sub-process** (Phase 4 V1) — see below |
| Tool nodes    | **fully sandboxed** (`sh -c` runs inside the container) |
| MCP servers   | partially sandboxed (host-side stdio; container-side MCP servers in V2) |

### Claw backend in sandbox

The `claw` backend runs LLM + tools in-process by default. When a
sandbox is active, iterion forwards each claw call to a hidden
`iterion __claw-runner` sub-process inside the container, so the
LLM's tool calls (Bash, file edits) execute inside the sandbox
boundary instead of escaping to the host.

**Container requirement**: the container image must ship the
`iterion` binary on PATH. The production Dockerfile installs it; for
local sandboxes built from third-party images you can mount the host
binary in (subject to architecture matching) via `runArgs`:

```jsonc
// .devcontainer/devcontainer.json
{
  "image": "node:20-bookworm",
  "runArgs": [
    "-v", "/usr/local/bin/iterion:/usr/local/bin/iterion:ro"
  ]
}
```

**V2-1+ wire format**: bidirectional NDJSON envelopes between
launcher and runner (see `pkg/backend/delegate/envelope.go`). Each
line is one envelope of typed payload (`task`, `tool_call`,
`tool_result`, `ask_user`, `ask_user_answer`, `session_capture`,
`session_replay`, `event`, `result`). The launcher's
[delegate.Multiplexer] dispatches runner-initiated envelopes
(tool_call, ask_user, …) to handlers wired against the engine's
existing tool registry / MCP manager / ask_user channel; the runner
builds proxy ToolDef closures that round-trip each invocation back
across the channel.

**Status of V1 limitations** (tracked in `.plans/sandbox-v2-plan.md`):

- ✅ **MCP-routed tools** are now visible to claw nodes inside the
  sandbox (V2-2). The launcher passes ToolDef metadata over the
  wire as [delegate.IOToolDef]; the runner builds proxy ToolDefs
  whose Execute closures emit `tool_call` envelopes; the launcher's
  multiplexer dispatches each call back to the original closure
  (which may close over the MCP manager, the engine's tool
  registry, or any custom dispatcher).
- ✅ **Mid-tool-loop ask_user** resume now works inside the sandbox
  (V2-3). The launcher-side ask_user ToolDef returns
  [*delegate.ErrAskUser] as it always has; the multiplexer encodes
  the typed payload into a [delegate.AskUserToolFail] field on the
  tool_result envelope; the runner-side proxy rebuilds a typed
  *ErrAskUser so the LLM loop's existing pause/resume path triggers
  identically inside and outside the sandbox.
- ✅ **Compaction-retry across the IPC** now works (V2-4). The
  runner ships a [model.SessionCaptureSink] that emits
  `session_capture` envelopes after every save into its local
  nodeSessionStore; the launcher's [delegate.MultiplexerHandler.OnSessionCapture]
  mirrors the snapshots into the host's nodeSessionStore so
  CompactAndRetry sees the latest history. On the retry spawn, the
  launcher seeds a `session_replay` envelope before the task
  envelope, the runner stashes the snapshot, then loads it into its
  local store once the task arrives so applySessionMessages
  prepends the replayed prior messages to the LLM's first call.

## Drivers

| Driver       | When selected                              | Status   |
| ------------ | ------------------------------------------ | -------- |
| `docker`     | host has `docker` on PATH                  | Phase 1 ✅ |
| `podman`     | host has `podman` on PATH (no `docker`)    | Phase 1 ✅ (shares the docker code path) |
| `kubernetes` | running in-cluster (`ITERION_MODE=cloud`)  | Phase 5 V1 ✅ + V2-5 NetworkPolicy |
| `noop`       | always available; emits `sandbox_skipped` event when an active mode is requested but no real driver is usable | ✅ |

`iterion sandbox doctor` reports which driver is selected on the
current host and what capabilities it advertises.

## Cloud (`ITERION_MODE=cloud`)

When iterion runs in-cluster (`iterion server` + `iterion runner`
deployed via the Helm chart) and `runner.sandbox.enabled: true` is
set, each sandboxed run is hosted in its own **sibling pod** in the
runner's namespace.

Architecture:

- The runner pod detects the in-cluster service-account token and
  selects the `kubernetes` driver. The factory's preference order
  on `HostCloud` is `kubernetes → noop`.
- For each iterion run, the driver renders a Pod manifest from
  the resolved `sandbox.Spec` (image, env, user, workspaceFolder,
  postCreate) and applies it via `kubectl apply -f -`.
- The pod's PID 1 is `sleep infinity`; subsequent delegate calls
  (claude_code / claw / tool nodes) reach in via `kubectl exec`.
- Workspace is provided by an `emptyDir` volume mounted at
  `/workspace`. Phase 5 V1 doesn't clone source from a remote;
  the runner's WorkDir is the bind-mount source.
- Cleanup deletes the pod (and its emptyDir) on run exit.

Security defaults applied to every sibling pod:

| Setting                          | Value                              |
| -------------------------------- | ---------------------------------- |
| `restartPolicy`                  | `Never`                            |
| `automountServiceAccountToken`   | `false`                            |
| pod `securityContext.runAsNonRoot` | `true`                           |
| `seccompProfile.type`            | `RuntimeDefault`                   |
| container `allowPrivilegeEscalation` | `false`                          |
| container `capabilities.drop`    | `[ALL]`                            |
| `runAsUser` / `runAsGroup`       | from `sandbox.user` (numeric form) |

RBAC: the chart provisions a `Role` (namespace-scoped, NOT
ClusterRole) granting the runner `pods:get/list/watch/create/delete`,
`pods/exec:create/get`, `pods/log:get/list`, `pods/status:get`.
Enable via:

```yaml
# values-prod.yaml
runner:
  sandbox:
    enabled: true
```

V1 limitations (deferred to V2):

- **Per-run NetworkPolicy** is now synthesised (V2-5): every sibling
  pod gets a NetworkPolicy locking egress to the runner pod's IP
  (proxy) plus DNS to `kube-system / k8s-app=kube-dns`. **Enforcement
  requires a NetworkPolicy-aware CNI** — Calico, Cilium, weave-net,
  kube-router. Default kindnetd / EKS VPC CNI without policy add-on
  do **not** enforce; the resource still applies cleanly but is a
  no-op. The CONNECT proxy continues to enforce hostname allowlist
  at the application layer regardless of CNI.
- **`sandbox.build`** (Dockerfile-at-run-start) is **rejected in
  cloud mode** — see "BuildKit (local docker only)" below for the
  rationale and the cloud-side workaround.
- ✅ **`sandbox.mounts`** now honours PVC / ConfigMap / Secret
  entries (V2-7). Mount string format mirrors the docker driver
  with k8s-native types:
  ```
  mounts:
    - "type=pvc,source=cargo-cache,target=/cargo"
    - "type=configmap,source=app-cfg,target=/etc/app.json,key=app.json,readonly"
    - "type=secret,source=db-creds,target=/secrets"
  ```
  Bind mounts are explicitly rejected — pods have no host
  filesystem; the error message points authors at the PVC
  alternative. PVCs must exist in the namespace before the run pod
  is admitted; iterion does not provision them. Secrets always
  mount with `defaultMode=0400`.
- **Image-pull secrets** for private registries beyond the
  runner's own image are not propagated; declare them on the
  pod's namespace ServiceAccount as `imagePullSecrets` and they
  will apply to sibling pods automatically.

### BuildKit (local docker only) — V2-6

`sandbox.build:` is wired only on the docker driver. The driver
invokes `docker buildx build --load` against the host's Docker
daemon — BuildKit is already part of the daemon, so no separate
service is deployed; the resulting image lands in the local Docker
image store and the sibling container of the run consumes it via
`docker run` like any pre-built ref.

```iter
sandbox:
  build:
    dockerfile: "examples/sandbox_build.dockerfile"
    context: "examples"
    args:
      VERSION: "1.2.3"   # forwarded as --build-arg
  user: "1000:1000"
```

Runtime flow:

1. Engine calls `docker.Driver.Prepare(spec)` — only validates.
2. Engine sees `spec.Build != nil` and the driver implements
   `sandbox.Builder`, emits `sandbox_build_started`, and calls
   `Driver.Build(prepared, info)`.
3. `docker.Build()` shells out to `docker buildx build -f
   <ws/dockerfile> -t iterion-sandbox-build:<run-id> --load
   [--build-arg K=V ...] <ws/context>`.
4. On success, `sandbox_build_finished` fires (with `target` and
   `duration_ms`); `prepared.Spec.Image` is mutated to the
   freshly-built tag and `prepared.Spec.Build` is cleared.
5. `Driver.Start()` proceeds normally, pulling the tag from the
   local Docker image store.

Failure modes (definitive `failed`, no checkpoint):

- `RunInfo.WorkspacePath` empty — engine bug; should not happen.
- `docker buildx build` exits non-zero → the last 4 KB of stderr
  (typically the `ERROR: failed to solve` footer) is surfaced into
  the `sandbox_build_failed` event payload and the wrapping run
  error.

#### Why cloud doesn't have this

The kubernetes driver intentionally rejects `sandbox.build:`. Cloud
deployments already use sibling pods (V1) or the runner pod itself
as their isolation unit; building images at run-start in cloud
would require a buildkitd Deployment, an in-cluster registry, RBAC,
NetworkPolicy, rootless seccomp/AppArmor relaxation, etc. — significant
operational complexity for a use case that production cloud users
already cover via CI:

- Build the workflow's image in CI (GitHub Actions, GitLab CI…),
  push to a registry, pin by digest.
- Reference the digest from the workflow:
  ```iter
  sandbox:
    image: "ghcr.io/myorg/myimage@sha256:<digest>"
  ```

This pattern is more reproducible (the digest is signed and immutable),
faster (no per-run build), and uses existing operational infrastructure
(registries, CI cache, signing). `sandbox.build:` is therefore a
local-development convenience for iterating on the Dockerfile alongside
the workflow; cloud is the production path with pre-built artifacts.

Out-of-scope for V2-6 (tracked for V2-7+):

- **Tag-by-content-hash + cleanup** — the `iterion-sandbox-build:*`
  repo accumulates one tag per run on the host. V1 leaves cleanup
  to `docker image prune` against that repo; V2 may swap to
  digest-based reuse so identical Dockerfiles share an image.
- **podman support** — the docker driver also handles podman, but
  `podman build` lacks the `--load` semantics buildx provides; we'd
  need a small shim to mirror the local-image-store contract.

The kubernetes runner pod must inject the downward API env var
`ITERION_POD_IP` (sourced from `status.podIP`) so the engine knows
its own IP for both the network proxy advertisement and the
NetworkPolicy egress rule. The Helm chart wires this automatically
when `runner.sandbox.enabled=true`; raw manifests must declare:

```yaml
env:
  - name: ITERION_POD_IP
    valueFrom:
      fieldRef:
        fieldPath: status.podIP
```

## Troubleshooting

### `docker: pull <image>: Cannot connect to the Docker daemon`

The user account doesn't have access to the docker socket. Either
add yourself to the `docker` group (Linux), use `sudo`, or switch to
rootless podman.

### `mode=auto but no .devcontainer/devcontainer.json found`

The `auto` mode reads `.devcontainer/devcontainer.json` (or the
sibling `.devcontainer.json` in the repo root). Add one (the same
file VS Code Remote — Containers reads), or switch to `sandbox: none`.

### `sandbox: workflow contains a node using backend=claw`

The claw backend cannot be sandboxed yet (Phase 4). Either drop the
sandbox, or change the affected nodes to `backend: claude_code`.

### `network_blocked` events you don't expect

Either the default `iterion-default` preset is too restrictive for
your workflow (declare a `network:` block to extend), or the agent
is genuinely talking to a domain you didn't intend to allow. Check
`events.jsonl` for the host pattern that fired.

### Performance

Container create+start adds ~1.5–4 s on Linux SSDs and ~5–10 s on
Docker Desktop (macOS/Windows). For workflows with many short nodes
the overhead is meaningful. Mitigation: run multiple delegate calls
through the same long-lived container (already the case — iterion
creates one container per *run*, not per node).
