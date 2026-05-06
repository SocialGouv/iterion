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

**V1 limitations** (tracked for V2):

- The runner registers iterion's standard claw tool set on its own
  side. `delegate.ToolDef.Execute` closures (which capture executor
  state) cannot be serialized across the IPC boundary, so:
  - **MCP-routed tools** declared by the workflow are not visible
    to claw nodes inside the sandbox.
  - The **mid-tool-loop ask_user** resume path is not supported on
    the sandboxed claw path. ask_user calls in the prompt-side
    fallback form still work.
- Session state (the conversation history captured for resume) is
  preserved across the IPC, but rehydration requires the same
  iterion binary version on both sides.

If your workflow uses claw nodes that depend on MCP or mid-loop
ask_user, switch them to `claude_code` (which routes natively
through the sandbox) or run without `--sandbox` until V2.

## Drivers

| Driver       | When selected                              | Status   |
| ------------ | ------------------------------------------ | -------- |
| `docker`     | host has `docker` on PATH                  | Phase 1 ✅ |
| `podman`     | host has `podman` on PATH (no `docker`)    | Phase 1 ✅ (shares the docker code path) |
| `kubernetes` | running in-cluster (`ITERION_MODE=cloud`)  | Phase 5 ⏳ |
| `noop`       | always available; emits `sandbox_skipped` event when an active mode is requested but no real driver is usable | ✅ |

`iterion sandbox doctor` reports which driver is selected on the
current host and what capabilities it advertises.

## Cloud (`ITERION_MODE=cloud`)

The runner pod itself is the de-facto sandbox in cloud V1: the
KubernetesDriver is stubbed and the resolver falls back to noop.
NetworkPolicy + securityContext defined in the Helm chart provide
isolation at the pod level, which iterion does not currently
augment with per-run policy. Phase 5 ships per-run Pod siblings
with NetworkPolicy generated from the workflow's spec. Until then,
declaring `sandbox: auto` in cloud mode emits `sandbox_skipped`
with `driver=noop` so operators see the gap explicitly.

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
