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

Beyond the `${…}` expansions, the direct `os.Getenv("ITERION_*")`
model-pin sites (supervisor, verified-action, classifier, conflict
resolver, session board) are routed through the exported `ir.LookupEnv`
so the same setting reaches them; `mcp_server` command/args/url moved
from `os.ExpandEnv` to `ExpandEnvWithDefault`, which also gives those
fields the documented `:-` form. Other `os.Getenv` reads (boot config,
infra) stay env-only by design — the blocklist keeps their names
unwritable so routing more sites later can never turn a stored var into
an infra override; the list is namespace hygiene and depth, not the
containment boundary (that is the overlay's ITERION_-gated reach).

## Consequences

- `iterion remote admin vars [set NAME VALUE | rm NAME]` is the operator
  surface; the GET echoes stored+origin+propagation bound.
- Values are stored, echoed and audit-logged in clear: the name gate
  refuses credential-shaped NAMES, nothing can vouch for values — the
  CLI help says never to store a secret in a bot var.
- A var only takes effect where the engine resolves it through the
  expansion chain; a name nothing reads is accepted but inert.
- Local CLI runs keep byte-identical env-only behaviour (nil overlay).
- Vars read by shell scripts INSIDE a sandbox container (container env)
  are out of scope.
- Propagation is per NODE boundary, not per run: expansions happen as
  each node builds, so a run longer than the TTL can straddle a change
  (node 1 old value, node 30 new). Same posture as a cap tightened
  mid-run. A redelivered message re-expands with the CURRENT settings —
  nothing is pinned on the RunMessage.
- Concurrent PUTs are safe: the write is a CAS on `updated_at`; a lost
  race is a loud 409, never a silently dropped key.
