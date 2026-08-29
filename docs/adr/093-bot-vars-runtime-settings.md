# ADR-093 — Bot-var overrides: `${ITERION_X:-default}` resolved from the DB before the pod env

- **Status**: Accepted
- **Date**: 2026-08-29
- **Extends**: ADR-090 (runtime operational settings), the `platformcfg` families (bot_roles, sandbox)
- **Family**: `bot_vars` (`pkg/platformcfg`)

## Context

Bots parameterize their model pins, reasoning effort and tunables as
`${ITERION_X:-default}` expansions (`ITERION_VIBE_MODEL_CLAUDE`,
`ITERION_MODERNIZE_EFFORT`, …). Those expansions read the runner pod's
env, so re-tuning a bot in production meant a Helm values change and a
rollout — while credentials, bot bundles, usage caps, role bindings and
the sandbox image had all already moved to the CLI→API→DB surface. The
operator asked for the same for bot vars, live within the settings TTL.

## Decision

A fourth `platformcfg` family, `bot_vars`: one document holding a
validated `map[ITERION_*]value`. It reaches the expansions through ONE
choke point — `ir.ExpandEnvWithDefault` consults an overlay
(`ir.SetEnvOverlay`, installed once at process boot by the server and
runner cmds, backed by a TTL resolver) before `os.Getenv`. Precedence:
**setting > pod env > the .bot's `:-` default**. An empty overlay value
counts as unset (clearing is removing the key, never pinning "").

The record stays typed and validated in `platformcfg` (the ADR-090
rejection of a generic KV table): only the value SPACE is a map, because
the keys are declared by bots, not the engine. `Validate` refuses names
outside `ITERION_[A-Z0-9_]+`, credential-shaped names
(KEY/TOKEN/SECRET/PASSWORD/PRIVATE/CREDENTIAL — a var that suggests a
secret must never transit a non-secret surface), the infra namespaces
(Mongo/NATS/JWT/secrets/S3/blob/Valkey/OIDC/OAuth/public-URL/bootstrap),
and the families that already have their own audited surface (usage cap,
sandbox default image). Values are single-line, non-blank, bounded.

Writes are super-admin only (`PUT /api/admin/settings/bot-vars`, merge
per key: string = set, `null` = clear, absent = untouched), audited with
old→new per touched key (values are non-secret by construction), and
invalidate the mutating replica's resolver; every other replica
converges within `platformcfg.DefaultTTL`. Runs claimed after that
expand the new value — no restart anywhere.

## Consequences

- `iterion remote admin vars [set NAME VALUE | rm NAME]` is the operator
  surface; the GET echoes stored+origin+propagation bound.
- Local CLI runs keep byte-identical env-only behaviour (nil overlay).
- Vars read by shell scripts INSIDE a sandbox container (container env)
  are out of scope: the DSL-expanded fields (model, effort, timeouts,
  tool commands) are all runner-side and covered.
- A redelivered message re-expands with the CURRENT settings (same
  posture as the usage caps; unlike the sandbox image, nothing is pinned
  on the RunMessage — "effective from the next run" is the contract).
