[← Documentation](README.md)

# Environment variables

Operational `ITERION_*` environment variables read directly by the engine
and its backends. These are tuning dials and escape hatches — most runs need
none of them. For the three launch knobs (compression, permission gate,
backend) and their five-level precedence chain, see
[settings-precedence.md](settings-precedence.md).

Values are read at the point of use; an unset, empty, or unparseable value
falls back to the listed default.

## Backends and models

| Variable | Effect | Default |
|---|---|---|
| `ITERION_VERIFIED_ACTION_MODEL` | Model spec for the [Verified Action](adr/044-adaptive-recovery-for-deterministic-action-nodes.md) recovery agent. Precedence: a node's `recovery.model` (env-expanded) → this var → package default. | package default |
| `ITERION_CONFLICT_RESOLVER_MODEL` | Model for the merge-conflict-resolver agent ([review-merge-gate.md](review-merge-gate.md)). Overrides the auto-detected pick. | auto-detected claw model |
| `ITERION_CLAUDE_CODE_MAX_TOOL_ERRORS` | Aborts a `claude_code` session after this many **consecutive** tool errors (any success resets the count) — guards against degenerate tool-error loops. `0` disables the guard. | `25` |
| `ITERION_CLAUDE_CODE_THINKING_DISPLAY` | Controls the `claude_code` thinking-block display flag: unset/other → `summarized` (readable summary); `omitted` → the CLI's latency-optimised default; `off` → stop passing the flag (required for `claude` CLIs older than the flag). | `summarized` |
| `ITERION_CLAW_COMPACT_THRESHOLD_RATIO` | Context-window fraction (`0 < r ≤ 1`) at which the `claw` router compacts the conversation. Used only when the workflow does not set the field. | engine default |
| `ITERION_CLAW_COMPACT_PRESERVE_RECENT` | Number of most-recent messages kept verbatim when `claw` compacts. Used only when the workflow does not set the field. | engine default |

Backend selection and provider routing use `ITERION_DEFAULT_BACKEND`,
`ITERION_BACKEND_PREFERENCE`, `ITERION_OPENAI_USE_OAUTH`, and
`ITERION_CODEX_VERSION` — documented in [backends.md](backends.md).

## Sandbox

| Variable | Effect | Default |
|---|---|---|
| `ITERION_SANDBOX_PULL_TIMEOUT` | Caps `<runtime> pull` for the docker driver (Go duration, e.g. `20m`) so a stalled registry or blocked DNS cannot pend a run indefinitely. | `10m` |

The sandbox on/off default, cloud override, host-state mounts, and the
default image are `ITERION_SANDBOX_DEFAULT`, `ITERION_SANDBOX_OVERRIDE`, and
`ITERION_SANDBOX_HOST_STATE` — documented in [sandbox.md](sandbox.md).

## Runtime and runner

| Variable | Effect | Default |
|---|---|---|
| `ITERION_SKIP_MCP_HEALTH` | Truthy → do not abort the run when a declared MCP server fails its startup health-check; log a warning and continue. Equivalent to the `iterion run --skip-mcp-health` flag. Useful when an HTTP/OAuth MCP server is unreachable in this environment but the run does not depend on it. | off (abort on failure) |
| `ITERION_BRANCH_CANCEL_GRACE` | Grace period (Go duration, e.g. `30s`) a cancelled fan-out branch is given to unwind before the collector stops waiting on it — raise it for backends that need longer to abort. | `5s` |
| `ITERION_GIT_AUTHOR_NAME` | Commit-author name seeded into a cloud-runner clone's local git config (no `~/.gitconfig` is mounted in the sandbox). The push-token identity is the preferred attributed path; this fires token-less. | `iterion-runner[bot]` |
| `ITERION_GIT_AUTHOR_EMAIL` | Commit-author email for the same cloud-runner clone. The default uses a reserved `.invalid` domain (RFC 2606) so the commit maps to no real account. | `iterion-runner@bot.iterion.invalid` |

## See also

- [settings-precedence.md](settings-precedence.md) — compression / permission / backend precedence.
- [backends.md](backends.md) — backend, provider, and OAuth-forfait variables.
- [sandbox.md](sandbox.md) — sandbox default, override, and host-state variables.
- [notifications.md](notifications.md) — `ITERION_WEBPUSH_VAPID_{PUBLIC,PRIVATE}_KEY`.
