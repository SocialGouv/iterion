# Settings precedence & provenance

Three launch-relevant knobs share the same five-level precedence chain,
highest priority first:

```
run override  >  node DSL  >  workflow DSL  >  env  >  default
```

| Knob | Run override | Node field | Workflow field | Env | Default |
|------|--------------|------------|----------------|-----|---------|
| Compression | CLI `--compress` / studio select | `compress:` (agent/judge; tool nodes are opt-in only) | `compress:` | `ITERION_COMPRESS` | `on` when a rewriter plugin is enabled and its binary present, else `off` |
| Permission gate | CLI `--permission` / studio select | `permission:` | `permission:` | `ITERION_PERMISSION` | `off` |
| Backend | CLI `--backend` / studio select | `backend:` | `default_backend:` | `ITERION_DEFAULT_BACKEND` | credential auto-detection ([docs/backends.md](backends.md)) |

The first **non-empty** level wins. A non-empty value that fails to
parse surfaces an error naming that level — it never silently falls
through to a lower level the operator didn't pick (see
`permission.ResolveModeSourced`).

## Where you see it

The studio Launch dialog captions each Run-settings select with the
resolved value and its provenance:

```
effective: ask · from workflow
effective: off · from env
effective: on · from run override
```

plus a warning when at least one node pins its own value — a run
override never reaches those nodes:

```
effective: claw · from workflow · some nodes pin their own (override won't affect them)
```

## How it's wired

- `rewrite.ResolveWithDefaultSourced` and
  `permission.ResolveModeSourced` return the winning level alongside
  the mode — the same resolvers the runtime uses, so the caption can't
  drift from execution behavior.
- `POST /api/runs/preview-cost` returns an optional `effective` block
  (`{compress, permission, backend}`, each
  `{effective, source, node_pinned}`) resolved **below** the
  run-override level — workflow → env → default. The studio layers the
  operator's own select on top client-side (that's the only way
  `run override` appears in a caption).
- Node-level provenance is summarized as the `node_pinned` boolean
  rather than enumerated per node: the Launch dialog decides one run
  override, so "will my override take everywhere?" is the actionable
  question.

## Scope (lite)

Provenance covers the three **mode** knobs. The permission
`allow:`/`ask:`/`deny:` rule lists are additive across levels (workflow
lists + run-level `--permission-allow/...`), not overridden, so they
have no single "winning level" to report — rule-list provenance is a
deliberate non-goal for now.

Related: [permissions.md](permissions.md) · [plugins.md](plugins.md)
(compression) · [backends.md](backends.md) (auto-detection).
