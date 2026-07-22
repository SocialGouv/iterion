# ADR-082 — Sandbox by default

- Status: accepted (Phases 1–2 shipped; Phase 3 planned)
- Date: 2026-07-22
- Deciders: devthejo

## Context

Sandboxing was opt-in: a workflow had to declare `sandbox:` (or the
operator pass `--sandbox`) to get container isolation, and in cloud the
Helm chart pins `ITERION_SANDBOX_OVERRIDE=none` — the runner pod is the
isolation boundary, so every per-bot `sandbox:` block is inert in
production. Consequences observed during the 2026-07-21 Doki cloud
dogfood:

- An unsandboxed run shares the executing host's filesystem and env —
  in cloud that means the runner's control-plane credentials (NATS/
  Mongo/S3), the materialized Anthropic forfait, forge tokens, and the
  residue of previous runs. The blast radius of a prompt-injected agent
  is the whole runner.
- The launch UX surfaced this as a per-launch "Launch without sandbox?"
  confirmation — the wrong altitude: whether a bot may run unsandboxed
  is a policy/declaration decision, not an operator click, and the
  modal fired for the common case (no block declared) rather than the
  exceptional one (explicit opt-out).
- The security posture was inverted: isolation was the exception, host
  execution the silent default.

## Decision

Sandboxing becomes the DEFAULT posture; opting out is explicit,
visible, and discouraged. Bots stop carrying `sandbox:` settings —
isolation is the platform's job; a bot only declares its TOOLCHAIN
(devbox.json, or a devcontainer in the target repo), never its
isolation level. Rollout in three phases:

### Phase 1 — engine + UX default (shipped, PR #252)

- Product entry points (`iterion run`/`resume`, studio/server,
  dispatcher) install a global sandbox default via
  `runtime.ResolveGlobalSandboxDefault()`: `ITERION_SANDBOX_DEFAULT`
  when set, else `auto`. The ENGINE stays neutral — an Engine built
  without `WithSandboxDefault` (tests, embedders) keeps the historical
  behaviour. `runview.Service` grows a `WithSandboxDefault` option the
  product constructors fill.
- The ambient default degrades instead of bricking: outside a git repo
  it is quietly not applicable; without a container runtime the run
  proceeds unsandboxed with a visible `sandbox_skipped` event. An
  explicit request (CLI flag / workflow block) still hard-errors.
- Explicit `sandbox: none` triggers warning diagnostic **C128**
  (workflow and node level), and the studio confirm dialog now fires
  only for it; an absent block shows `Sandbox: auto (default)`.

### Phase 2 — fleet realignment (shipped)

Remove `sandbox:` blocks from catalog bots. Tool access moves to the
declared-toolchain channels, per the repo's existing rule ("a bot that
needs tools declares them in devbox.json"):

- The generic default image (`iterion-sandbox-slim`) stays the floor:
  git + Node + claude CLI + devbox/Nix + the iterion binary.
- Each bot's extra tools (go, python3, gh, glab, scanners…) come from
  its `devbox.json` (already honoured on both the sandboxed and
  unsandboxed paths) and/or the target repo's own devcontainer/devbox.
- An image pin survives only where Nix cannot provide the toolchain
  (to verify per scanner for `-sec`) or a base layer is needed before
  any step runs.

Shipped state: the six `-full`-pinned coding bots (feature-dev,
feature-gap-fill, test-coverage, branch-improve-loop,
whole-improve-loop, app-dev) and devbox-setup carry no `sandbox:`
block at all; the forge-posting bots declare pinned `gh`/`glab` in
their `devbox.json` (app-dev keeps `crane`/`yq` there too). The five
scanner bots (sec-audit-source, sec-audit-deps, supply-shield,
supply-shield-cve, dep-update-guard) keep exactly the `-sec` image
pin — the scanner toolchain is the "Nix cannot provide it cheaply"
case — plus, for the supply-shield pair, the forge-token env
passthrough that Phase 3 replaces with file secrets.
secured-renovacy keeps a `-full:edge` pin (its targets' devcontainers
rely on the un-honoured `features:` block — the documented `auto`
trap) plus its load-bearing env (git identity, out-of-worktree
toolchain caches); everything else (claude install gates, `~/.claude`
mounts, `CLAUDE_CONFIG_DIR`, `user:`, `network: open`) is gone —
platform defaults cover them.

### Phase 3 — cloud cutover (planned)

Lift the chart's `ITERION_SANDBOX_OVERRIDE=none` so cloud runs execute
in per-run sandbox pods (kubernetes driver + ADR-070 per-run credential
Secret). Known blockers to resolve first, all confirmed by code
inspection:

1. **Worktree git + push**: the k8s driver tar-copies the workspace
   into the pod's emptyDir; the parent `.git` object store and the
   clone's `iterion-credentials` store stay on the runner, so
   in-sandbox git (and `finalize_mr`-style `git push`) is not wired —
   needs the init-container/PVC mechanism (or a git-credential/HTTP
   proxy) called out in `sandbox_mounts.go`.
2. **ask-user transport** — RESOLVED: the engine now binds a per-run
   gateway-reachable ask-user MCP listener at `/api/v1/mcp/ask-user`
   (`pkg/askusermcp`, per-run `X-Iterion-Run` token, mirroring the
   board transport) and the claude_code delegate registers it as an
   HTTP MCP server for sandboxed interactive nodes instead of
   disabling the hook. Both docker and kubernetes drivers are covered
   (same `ProxyConfigurer` bind as the egress proxy / board listener);
   the PreToolUse hooks stay host-side so the interaction-store paths
   are unchanged. A bind failure degrades loudly per node.
3. **Forfait auth robustness**: in-pod claude auth rides a single
   exec-env `CLAUDE_CODE_OAUTH_TOKEN` (the `CLAUDE_CONFIG_DIR` file
   fallback points at a host path that doesn't exist in the pod);
   mid-run refresh only re-reads per spawn.
4. **Runner-identity bots** (review-pr/Revi, revi-converse): they use
   the pod's glab/gh + env credentials by design; under default-on
   they need the CLI in the image (`-full` has both) and the forge
   token delivered as a file secret — or a per-bot policy grant to
   stay unsandboxed.

Validation for the cutover: re-run the Doki repo-targeted cloud dogfood
sandboxed and compare against the 2026-07-21/22 unsandboxed baseline
(`docs/bot-runs/docs-refresh.md`).

## Alternatives considered

- **Flip the default inside the engine (`pickMode`)** — rejected after
  implementation: ~260 direct engine constructions in tests inherit the
  default, and on docker-equipped CI a git-repo test workspace starts a
  REAL container (observed live on
  `TestEngineRunner_ViaServiceMatchesDirect`). Policy belongs to
  product entry points; the engine stays a neutral mechanism.
- **Remove `sandbox: none` entirely** — rejected for now: the cloud
  runner's `ITERION_SANDBOX_OVERRIDE=none` model is load-bearing until
  Phase 3, and some flows genuinely need the executing host. The C128
  warning + inverted UX make the opt-out visible instead.
- **Default image `-full` instead of `-slim`** — deferred to Phase 2:
  `-full` covers more bots out of the box (python3, gh, glab) but is a
  heavy pull for casual local runs; the devbox.json channel keeps the
  floor slim while letting each bot declare exactly what it needs.

## Consequences

- Local runs of undeclared workflows are now isolated by default on
  docker hosts; hosts without docker keep working (visible skip).
- The per-launch sandbox confirmation disappears for the common case;
  security review focuses on explicit `sandbox: none` declarations,
  which C128 makes greppable and reviewable.
- Until Phase 3 lands, cloud behaviour is unchanged — the dogfood
  baseline stays comparable.
