# ADR-076 — Scoped config-share editor (self-service, per-field, per-repo)

Status: proposed · 2026-07-17

## Context

Operators want to let **non-operators** maintain a bot's configuration — the
motivating case is the veille (feed-watch) team editing their own RSS `feeds[]`
and `editorial` prompt — without handing out a GitHub account or studio access.
The config lives as a JSON file in a git repo (`feed-watch.json` in
`SocialGouv/iterion-veille`); the bot reads it at run time and commits its state
back.

The blunt options (a GitHub team with write on the repo; a studio team
membership) both over-grant: they expose git, the whole file, every category,
the sink webhook keys, and — for a studio membership — the team's runs, secrets
and boards. We want a grant scoped to **exactly the fields we choose, in one
file of one repo, and nothing else in iterion**, addressable by a URL we can
hand out and revoke.

A three-lens adversarial design review surfaced the load-bearing constraint:
the `editorial` an editor writes is injected verbatim into the synthesize
agent's LLM system prompt, and that agent (claude_code, `bypassPermissions`)
otherwise has full native `Bash`/`Read`/`Write`. **The editor endpoint is only
as safe as the bot it edits** — so the work is two coupled pieces: hardening the
bot (ADR-scope: the feed-watch `permission: deny` gate + SSRF-safe fetch +
deterministic link firewall, shipped in the paired change) and the editor
primitive itself (this ADR).

## Options considered

1. **GitHub team with repo write** — zero build, but exposes git + the whole
   repo, needs accounts, and can't scope below "the repo".
2. **Config in iterion's Mongo store, form CRUD** — clean form UX, but loses
   git history / PR-reviewability / colocation with the bot's state, and
   duplicates the source of truth.
3. **Scoped config-share primitive over the git file (chosen)** — a per-grant
   token + a server that projects reads and fail-closed-merges writes onto the
   real file through the forge contents API.

Within (3), sub-decisions:
- **Auth transport**: a long-lived per-share token accepted as `Authorization:
  Bearer` only (vs a cookie / a one-shot-code→cookie exchange).
- **Forge write credential**: reuse the team's `forge_token` PAT (the same one
  the bot pushes with) vs mint a repo-narrowed github-app installation token.
- **Genericity**: bake feed-watch's fields into the server vs carry the
  editable/visible paths on the grant record.

## Decision

Add `pkg/configshare` — a **generic, bot-agnostic** "scoped config-file share"
primitive — plus its server surface and a shell-less studio editor.

- A **share** grant (`configshare.Share`) pins, at mint time and never from a
  request body: `bot_id`, `repo_url`, `repo_ref`, `config_path`, `category`, and
  literal-dotted `allowed_paths` (writable) + `visible_paths` (readable). The
  token is an `iws_` string stored only as a salted hash.
- **Synthetic identity.** The token authenticates a principal of a new
  `auth.KindShare`. `auth.Identity` gains a `Kind` field, and every operator
  RBAC gate (`canViewTeam`/`canManageTeam`/`canViewOrg`/`canManageOrg`) rejects a
  synthetic principal up front — so a share token can never reach a team, run,
  secret or board endpoint. Backward-compatible (zero value = a real user); the
  inbound-webhook actor is stamped `KindWebhook` by the same mechanism.
- **Read = projection.** `GET /config` returns a fresh object built from
  `visible_paths` only — never a filtered pass over the file, so no other
  category/field/key leaks.
- **Write = fail-closed allow-list + if-match.** `PATCH /config` walks the patch
  before any merge (any key outside `allowed_paths`, any prototype-pollution
  key, any hard-forbidden segment → reject the whole request, uniform 400, no
  path echo), deep-merges the allowed leaves onto the server-read file,
  re-validates (feeds http(s)-only/no-userinfo/no-IP/capped; editorial
  size/control-char/fence), and writes through a new `forge.FileClient` (GitHub
  contents API) with `if-match` on the blob SHA. A stale SHA → 409 + fresh
  projection for a diff, never a silent overwrite. Commit message/branch/author
  are server-derived.
- **Transport = Bearer-only** (no cookie surface → structurally CSRF-immune).
  All auth failures collapse to a uniform 401. Every call lands a `Delivery`
  audit row. Rotate = immediate cutoff. Default TTL 14 days.
- **Forge credential = the team `forge_token` PAT** (resolved via
  `ResolveGenericWithBindings`, the same secret the bot pushes with).
- **Studio**: a shell-less `/config/:id` editor with its OWN fetch client
  (`credentials:"omit"`, Bearer-only, no shared `apiRequest`/`/auth/refresh` —
  an eslint boundary enforces the import isolation), token stripped from the URL
  (`history.replaceState`) into per-tab `sessionStorage`, editorial as a plain
  `<textarea>` (no markdown/HTML). Operator CRUD on the bot's home page.
- **Storage**: in-memory (local/desktop) + Mongo (cloud).

## Consequences

- A grant is minted per `(bot × repo × file × category)`; a second bot needs
  **no Go change** — only its `allowed_paths`/`visible_paths` (the server is
  generic; only the studio create-dialog knows feed-watch's field names today).
- The `Kind` field is a new, small, permanent RBAC concept every future
  synthetic-identity surface (share, webhook, and any next one) rides; a
  regression test locks "synthetic never passes an operator gate".
- The single unproven-in-CI edge is the real GitHub contents round-trip (the
  end-to-end tests run against a fake `FileClient`); a live dogfood on
  `iterion-veille` closes it.

### Accepted follow-ups (not blockers)

- A **repo-narrowed github-app installation token** for the write (tighter blast
  radius than a team PAT, which is broad if the server leaks it).
- A **one-shot code-per-handout** exchange so a pasted URL burns once (today the
  long-lived token in the URL leaks if pasted; mitigated by replaceState + short
  TTL + rotate).
- A per-bot **`config_share:` manifest block + JSON schema** so the studio form
  auto-derives fields (today the paths are supplied by the operator/dialog).
- GitLab/Forgejo `FileClient`; a preview/test-run button; per-org monthly write
  quota.

Reference: [docs/config-share.md](../config-share.md).
