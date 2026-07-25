# ADR-077 — Declarative per-bot config-share surface

Status: accepted · 2026-07-18

Extends [ADR-076](076-scoped-config-share-editor.md) (the scoped config-share
editor). ADR-076 shipped the isolation core; this ADR records how a bot
**declares** what a share may touch, so the primitive generalizes beyond its
first user (feed-watch/Vigie).

## Context

ADR-076 shipped config-share with the shareable surface — `config_path`,
`allowed_paths`, `visible_paths` — **supplied by the operator at mint** (or by
the studio create dialog, which hardcoded feed-watch's shape: a `veille.yaml`
default and client-side `categories.{cat}.{field}` path templating).

That violates iterion's universal-bots bar (a catalog bot must be adoptable
with zero bespoke code): a second bot wanting config-share would need the SPA's
mint form edited to know its config file and fields, and the operator would
have to hand-type literal JSON paths — error-prone (a typo widens the grant)
and untied to anything the bot committed to git. The knowledge of *what is
shareable* belongs with the bot, versioned and PR-reviewed, not in the operator's
head or the SPA.

## Options considered

1. **Status quo — operator-supplied paths.** No bot artifact. Fails the
   universal bar (per-bot SPA edits), and the grant is only as correct as the
   operator's typing. Rejected.
2. **A separate `bots/<bot>/config-share.schema.json`** (JSON-Schema with
   `x-share-editable` / `x-share-visible` annotations + per-field constraints).
   Richest — carries JSON-Schema validation and an operator preview — but
   introduces a new file + a new schema-annotation format to author and load,
   and duplicates constraints the runtime already validates. Heavier than the
   MVP needs. Deferred (kept as the growth path for richer per-field
   constraints).
3. **A `config_share:` block in the existing `manifest.yaml`** declaring
   `config_path` + `editable_paths` / `visible_paths` **path templates**, with
   a `{category}` placeholder for per-category configs; reuse the existing
   deterministic validators (`ValidatePaths`, the `feeds`/`editorial` field
   guards). Chosen.

## Decision

A bot declares its config-share surface with a `config_share:` block in its
`manifest.yaml`:

```yaml
config_share:
  config_path: feed-watch.json
  editable_paths:
    - "categories.{category}.feeds"
    - "categories.{category}.editorial"
  visible_paths:
    - "categories.{category}.digest_title"
```

- **The mint derives, and pins, from the block.** Given `bot_id` (+ a
  `category` when the templates carry `{category}`), the mint resolves the bot's
  manifest, expands `{category}`, and pins `config_path` + `allowed_paths` +
  `visible_paths` on the `Share`. The operator supplies only who/where + the
  category — never a path. A share **can never exceed** the declared surface.
- **The block is authoritative; explicit paths are the fallback.** When a bot
  declares a block, client-supplied paths are ignored (the block is the
  contract). A bot with no block — or a loose `.bot` not resolvable on the
  server — falls back to today's explicit operator paths, so the change is
  backward-compatible and cloud-safe.
- **Per-share least privilege via field subsetting.** A mint may narrow a
  derived grant to a SUBSET of the bot's declared editable fields
  (`editable_fields: ["feeds"]`, by leaf name). A non-selected field is neither
  writable nor visible — a feeds-only curator can't touch or even read the
  editorial prompt (the LLM-injection surface). Selecting a field the bot never
  declared editable is a 400, not a silent widen. Enforced in `DeriveGrant`; the
  studio form renders a data-driven checkbox per declared field.
- **Deterministic guards stay.** `configshare.DeriveGrant` validates the
  category (`^[A-Za-z0-9_-]+$` — no dot, no traversal), fails closed on an
  unresolved placeholder (never pins a literal `{…}` segment), and its output
  still passes through `ValidatePaths` (literal / no-overlap / no-forbidden).
  The read-side `pruneForbidden` and write-side allow-list walk from ADR-076 are
  unchanged — this ADR only decides *where the surface comes from*, not how it
  is enforced.
- **The studio form is data-driven.** `config_share` is mirrored onto
  `botregistry.Entry` (so it reaches the SPA via `/api/v1/bots`); the
  "Config-share links" card reads it to render the editable/visible fields and
  hides itself for bots that declare none — no hardcoded per-bot shape.

## Consequences

- **A second bot adopts the whole editor by adding the block alone** — no Go,
  no SPA change. This is the universal-bots contract for config-share.
- The shareable surface is **git-reviewed** and colocated with the bot, matching
  ADR-076's "config stays versioned/PR-reviewable" premise. Changing what is
  shareable is a manifest edit in a PR, not operator tribal knowledge.
- Field validation is still keyed by leaf segment name (`feeds`, `editorial`):
  a bot reusing those names inherits the SSRF/size guards; a bot with a **novel**
  editable field passes structurally (the path allow-list still gates it, but no
  value-shape check runs). Richer per-field constraints declared in the block
  (option 2's JSON-Schema) are the growth path.
- **Per-share field subsetting is supported** (least privilege): a share exposes
  exactly the declared editable fields the operator selects for it, defaulting to
  the full set. The `feeds` / `editorial` risk asymmetry (editorial is the
  injection surface) is the motivating case.

## Accepted follow-ups (not blockers)

- Richer per-field constraints in the block (option 2's JSON-Schema), so a novel
  field gets a value-shape check, not only a path gate.
- Category existence check at mint (read the file and reject a category absent
  from it) — today an unknown category yields an empty projection, harmless but
  not caught early.

Reference: [docs/config-share.md](../config-share.md),
[pkg/configshare/schema.go](../../pkg/configshare/schema.go) (`DeriveGrant`).
