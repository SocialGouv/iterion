# Scoped config-share editor

The config-share editor lets an operator hand a **non-operator** a bookmarkable
URL that edits **exactly one config file's declared fields, in one repo, and
nothing else in iterion**. It was built for the veille use case — letting the
design/a11y team edit their own RSS `feeds[]` and `editorial` prompt without a
GitHub account or studio access — but the primitive is bot-agnostic.

## The shape

A **share** is a grant minted by an operator (team admin) for a `(bot × repo ×
config-file × category)`. It pins, at mint time and never from a request body:

- `repo_url` + `repo_ref` (the branch) + `config_path` (the file),
- `allowed_paths` — the literal dotted JSON paths the holder may **write**
  (e.g. `categories.a11y.feeds`, `categories.a11y.editorial`),
- `visible_paths` — the paths the holder may **read** (a superset, e.g. also
  `categories.a11y.digest_title` for context).

The operator gets back a URL `…/config/<id>#<token>` and the `iws_` token
**once**. The editor opens it, edits the allowed fields in a shell-less page,
and Save commits the change straight to the repo through the forge's contents
API — an atomic `if-match` write, no clone, no bot run.

### The shareable surface is declared by the bot

`config_path`, `allowed_paths` and `visible_paths` are not hand-typed by the
operator — they are **derived from the bot's own manifest** at mint time. A bot
declares what a share may touch with a `config_share:` block in its
`manifest.yaml`:

```yaml
config_share:
  config_path: feed-watch.json
  editor_title: "Éditeur de veilles"          # optional: shell heading (else "Config editor")
  editor_description: "Sources, éditorial et fréquence de vos veilles."
  editable_paths:
    - "categories.{category}.feeds"
    - "categories.{category}.editorial"
  visible_paths:
    - "categories.{category}.digest_title"
```

A `{category}` placeholder makes the surface per-category: the operator mints a
share by naming a category (`a11y`), and the mint expands `{category}` →
`a11y`, pins `config_path`, and computes `allowed_paths` / `visible_paths` from
the templates. A share **can never exceed** what the bot committed to git, and a
second bot adopts the whole editor by adding this block alone — no server or SPA
change (the studio's "Config-share links" card reads the block to render a
data-driven mint form, and hides itself for bots that declare none). The
`{category}` value is validated (`[A-Za-z0-9_-]+` — no dots or traversal), and
the derived paths still run through the same literal/no-overlap/no-forbidden
`ValidatePaths` guard. A bot with no `config_share:` block (or a loose `.bot`
not resolvable on the server) falls back to explicit operator-supplied paths.
See [`pkg/configshare/schema.go`](../pkg/configshare/schema.go)
(`DeriveGrant`).

## Security model

The editor is the anti-injection boundary for a feature that hands a token to an
untrusted party, so isolation is enforced at every layer:

- **Synthetic identity.** The share token authenticates a principal of
  `auth.KindShare`. Every operator RBAC gate (`canViewTeam` / `canManageTeam` /
  `canViewOrg` / `canManageOrg`) rejects a synthetic identity up front, even one
  carrying a matching team/role — so the token can never reach a team, run,
  secret, or board endpoint it wasn't granted.
- **Header-only token.** The token is accepted **only** as
  `Authorization: Bearer` — never a cookie or query param — so a cross-site page
  can't forge a call (structurally CSRF-immune).
- **Read projection.** `GET /config` returns a **fresh object** built from the
  share's `visible_paths` only. Other categories, other fields, sink webhook
  keys, and unrelated top-level keys are never on the wire.
- **Write allow-list (fail-closed).** `PATCH /config` walks the patch before any
  merge: any key at any depth outside `allowed_paths`, any prototype-pollution
  key (`__proto__`/`constructor`/`prototype`), or a hard-forbidden segment
  (`sinks`) rejects the **whole** request with a uniform `400` (no path echo).
  The allowed leaves are deep-merged onto the server-read file, re-validated
  (feeds are http(s)-only, no userinfo, no IP literal, capped; editorial is
  size/control-char/fence checked), and written with `if-match` on the blob SHA
  — a stale write gets a `409` with the fresh projection for a diff, **never** a
  silent overwrite.
- **Mint guards.** `config_path` is rejected if it traverses (`..`) or targets a
  protected area (`.git/**`, `.github/**`, `Dockerfile`, `.env*`); `repo_ref`
  must be explicit; `repo_url` must be a clean `owner/name`. Minting a
  category share whose category is **absent from the config file** (a common
  typo — `design` vs the real `design-systems`) is rejected with a clear error
  rather than producing an editor with no fields — a best-effort projected read
  (a forge outage never blocks the mint, since the operator is trusted).
- **Commit hygiene.** The commit message, branch, and a fixed
  `iterion-share-editor[bot]` author are all server-derived — an edit can't
  retarget the branch or forge attribution.
- **Uniform failures + audit.** Every auth failure collapses to `401
  {"error":"invalid_share"}` (the id space isn't probeable). Every call lands a
  `Delivery` audit row (source IP, UA, before/after SHA, changed paths) — the
  forensic trail after a token leak. Rotate = immediate cutoff.
- **Isolated SPA client.** The shell-less `/config/:id` view uses its own fetch
  client (`credentials:"omit"`, Bearer-only) — it never touches the studio's
  cookie-carrying `apiRequest` or `/auth/refresh`, so an operator opening the
  link can't be signed out cross-tab. An eslint rule enforces the import
  boundary. The token is stripped from the URL (`history.replaceState`) and kept
  in per-tab `sessionStorage`; `editorial` renders as a plain `<textarea>` (no
  markdown, no `dangerouslySetInnerHTML`).

The paired bot hardening (`bots/feed-watch`, PR #225) is the other half: the
`editorial` an editor writes feeds the synthesize agent's LLM prompt, so that
agent runs under a `permission: deny` gate (WebFetch only — no Bash/Read/Write),
a deterministic link-firewall rejects any digest URL not drawn from the
collected items, and feed fetching refuses SSRF/LFI targets. See
[docs/bot-runs/feed-watch.md](bot-runs/feed-watch.md).

## API

Public (self-authenticating; `Authorization: Bearer <iws_ token>`):

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/config-share/{id}/meta` | scope: bot/repo/category + allowed/visible paths |
| GET | `/api/config-share/{id}/config` | the file projected to `visible_paths` + blob `sha` |
| PATCH | `/api/config-share/{id}/config` | body `{patch, sha}` → `{sha, changed}` / `409 {config, sha}` / `400` |

Operator (JWT, `canManageTeam`, audited):

| Method | Path | Purpose |
|---|---|---|
| POST | `/api/teams/{id}/config-shares` | mint (returns the token + URL once) |
| GET | `/api/teams/{id}/config-shares` | list |
| POST | `/api/teams/{id}/config-shares/{sid}/rotate` | re-mint the token |
| DELETE | `/api/teams/{id}/config-shares/{sid}` | revoke |
| GET | `/api/teams/{id}/config-shares/{sid}/deliveries` | audit trail |

The studio surfaces the operator side on a bot's home page (a "Config shares"
card, gated on `server_info.config_shares_enabled`).

Signed-in **config-editor** (the least-privilege `config_editor` team role,
ADR-078 — a real cookie session, `canEditConfigShares`-gated, not a token):

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/teams/{id}/config-editor/shares` | list the team's shares (reduced view + bot `editor_title`) |
| GET | `/api/teams/{id}/config-editor/shares/{sid}/config` | the projected config + `sha` |
| PATCH | `/api/teams/{id}/config-editor/shares/{sid}/config` | edit the fields (same allow-list/if-match as the token path) |
| GET | `/api/teams/{id}/config-editor/shares/{sid}/schedule` | the cadence (cron + next fire) of the share's category schedule, if any |
| PATCH | `/api/teams/{id}/config-editor/shares/{sid}/schedule` | edit **only** the cron of that category's schedule |

**Cadence.** The recurrence of a category's digest is not repo config — it is a
first-class schedule in iterion's schedule store (visible in the Schedules
view). The config-editor may read and adjust **only the cron** of the schedule
bound to its share's `(bot, category)` (`vars.category == share.category`); a
different category's schedule is unreachable, and a missing schedule returns
`404` (creating a category's schedule + delivery sinks stays an operator
action). The cadence never leaves iterion, so it needs no repo write. The
studio renders it as a "Cadence" card (cron presets + next-run) in the
config-editor shell, whose heading uses the bot's `editor_title`.

## Forge write path

The write resolves the team's `forge_token` generic secret — the **same PAT the
bot pushes state with** — and drives the GitHub contents API through
`forge.FileClient`. Local/desktop uses an in-memory share store; cloud wires a
Mongo store (`configshare.NewMongoStore`, TTL'd audit).

## MVP scope + follow-ups

Shipped: GitHub provider, Bearer-only token (no cookie exchange), 14-day default
TTL, one share per category, the team `forge_token` PAT for writes, the per-bot
`config_share:` manifest block that declares the shareable surface (the mint
derives `config_path` + `allowed_paths` + `visible_paths` from it and the studio
card renders a data-driven form, so a **second bot adopts the whole editor by
adding the block alone** — no Go or SPA change), **per-share field subsetting**
(an operator mints, say, a feeds-only share; a non-selected field is neither
writable nor visible), the signed-in **config-editor role** (ADR-078) with a
bot-declared **`editor_title`**, and **scoped cadence editing** (a config-editor
tunes the cron of its category's schedule from the same shell).

Follow-ups (not blockers): a repo-narrowed github-app installation token (tighter
blast radius than the team PAT); a one-shot code-per-handout exchange (so a
pasted URL burns once); richer per-field JSON-Schema constraints in the block
(beyond the built-in `feeds`/`editorial` validators); GitLab/Forgejo
`FileClient`; a preview/test-run button.
