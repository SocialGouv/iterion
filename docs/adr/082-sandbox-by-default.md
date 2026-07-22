# ADR-082 — Sandbox by default

- Status: accepted (Phase 1 shipped; Phases 2–3 planned)
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

### Phase 2 — fleet realignment (planned)

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

### Phase 3 — cloud cutover (planned)

Lift the chart's `ITERION_SANDBOX_OVERRIDE=none` so cloud runs execute
in per-run sandbox pods (kubernetes driver + ADR-070 per-run credential
Secret). Known blockers to resolve first, all confirmed by code
inspection:

1. **Worktree git + push** — RESOLVED (feat/sandbox-cloud-git). No
   init-container/PVC needed: the driver already tar-copies the *clone
   root* (real `.git` + `origin` + the `iterion-credentials` store)
   into the pod at the workspace path; the branch adds the missing
   halves — a post-populate fixup re-points the copied clone's
   `credential.helper` at the pod-local credential file (the recorded
   host path doesn't exist in-pod) and drops stale `.git/worktrees/`
   registrations, and the runner's mid-run git-credential refresher
   writes rotated forge tokens THROUGH into the pod copy via the new
   `sandbox.WorkspaceFileRefresher` exec seam (stdin-streamed). Local
   docker worktree runs keep the bind-mount mechanism untouched.
2. **ask-user transport**: `__mcp-ask-user` is stdio-only and disabled
   under any sandbox; needs an HTTP fallback mirroring the board MCP
   transport (`/api/v1/mcp/board` + per-run token).
3. **Forfait auth robustness** — RESOLVED (feat/sandbox-cloud-git).
   The materialised `.credentials.json` ships into the pod on the
   ADR-070 file-secret channel and is seeded into a WRITABLE
   `CLAUDE_CONFIG_DIR` (`/tmp/iterion-claude-config`) the claude_code
   delegate targets for sandboxed spawns; the per-spawn env token
   stays first-precedence, and the runner's forfait refresher rewrites
   both the Secret and the in-pod copy on each rotation.
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
