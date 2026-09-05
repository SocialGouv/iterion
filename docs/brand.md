# Brand — the iterion-bot mascot, everywhere iterion shows a face

The mascot on the official [`iterion-bot`](https://github.com/iterion-bot)
GitHub account is iterion's identity: the product logo, the favicons, the
desktop icon, **and the avatar of every account a bot posts through**. This
runbook covers both halves — the asset pipeline, and how each forge identity
gets the face (automatically where the forge has an API, by hand where it has
none).

## The assets — one source, generated copies

| Where | What | Consumer |
|---|---|---|
| `assets/brand/iterion-bot.png` | **master**, plain — the mascot on a transparent background, 460×460, pixel-equal to the `iterion-bot` account's avatar | every account avatar |
| `assets/brand/iterion-bot-circle.png` | **master**, badge — dark disc + ring, 1254×1254 | every product surface |
| `pkg/brand/*.png` | plain 460, lossless (the master's own pixels; < 200 KiB, GitLab's limit) + circle 512, `go:embed` | `forge.AvatarSetter` uploads; `GET /brand/iterion-bot.png` + `/brand/iterion-bot-circle.png` (public) |
| `studio/src/assets/iterion-mark.png` | circle 256 | the studio's `BrandMark` (sidebar, landing, shells) |
| `studio/public/{android,apple,ms,favicon}-*.png`, `favicon.ico` (16/32/48) | the favicon pack; the Apple icons on a flat navy (iOS renders alpha as black), the rest transparent | `index.html`, `manifest.json`, `browserconfig.xml`, `service-worker.js` |
| `docs/public/iterion-logo.png`, `docs/images/iterion-logo.png`, `docs/public/favicon.ico` | circle 512 | VitePress nav + hero, the README banner |
| `docs/public/og.png` | 1200×630 card | OpenGraph / Twitter preview |
| `build/appicon.png`, `cmd/iterion-desktop/appicon.png` | circle 1024 | Wails derives `.ico`/`.icns`; `desktop-release.yml`, the `.deb`/AppImage stage, the Helm chart `icon:` |

Every row but the masters and `og.png` is **generated**:

```sh
task brand:gen     # scripts/brand/generate.sh — magick + pngquant + oxipng from devbox
task brand:check   # regenerates into a temp dir and cmp's each copy (part of `task check`)
task brand:og      # docs/scripts/og-card.html → docs/public/og.png (needs `task test:e2e:ui:install`)
```

`brand:check` runs in `task check`, not in CI (it needs the devbox toolchain,
like `pi-ext:check`); a toolchain bump (`devbox update`) can change the
bytes, and the answer is the same as for a master change: regenerate and
commit. Change the mascot = replace a master, run `brand:gen` **then `brand:og`**
(the OG card embeds the regenerated docs logo and has no guard of its own),
commit everything they touched. Never edit a generated copy: `brand:check`
fails on it, and on a committed copy the script no longer produces. The
output is deterministic (stripped metadata, fixed quantiser), which is what
makes the check a byte comparison.

## Forge identities — who gets the face, and how

The identity a bot posts with is the **connection's account**
([forge-permissions.md](forge-permissions.md)). Whether iterion can put the
avatar on it depends on the forge:

| Connection | Avatar | How |
|---|---|---|
| GitLab **PAT** whose account GitLab flags `bot: true` — a group/project access token's bot user, a service account | **automatic at connect time** (`PUT /user/avatar`, GitLab ≥ 17.0) | the connect records `account_kind: bot` and `avatar_applied_at`; a refusal lands on `avatar_error`, never fails the connect |
| GitLab / Forgejo **PAT** of a dedicated account the forge does not flag (a hand-made `iterion-bot` user; every Forgejo account — no bot flag there) | **on demand**, with the operator's word | studio → Integrations → the connection card → *Apply iterion-bot avatar* (confirms, then `force`), or `iterion remote forge connections avatar <conn-id> --force` |
| **OAuth** connection (any forge) | **never** | it authenticates as the person who authorized it; iterion does not rebrand personal accounts, no override |
| **GitHub App** (`iterion-forge-*`, `iterion-watch-*`) | **manual** | GitHub has no logo API and the manifest cannot carry one: the studio shows the hand-off after creation (download link + the App's *Display information* page) and keeps a *Logo ↗* link on the App row |
| GitHub **PAT / user account** | manual | no avatar API either; `iterion-bot` already wears it |

Apply state on a connection: `account_kind`, `avatar_applied_at`, `avatar_error`
(`GET /api/teams/{id}/forge/connections`). The action:
`POST /api/teams/{id}/forge/connections/{conn_id}/avatar` `{variant?: plain|circle, force?}`
— 422 with `logo_url` + `logo_circle_url` on GitHub (plus `manage_url` when
the App is one iterion created), 422 on a revoked connection (reconnect
first), 409 `needs_force` on an unflagged account, 502 with the forge's
reason when the upload is refused. Audit event on both the automatic and
the explicit path: `forge.connection.avatar_applied` (`automatic: true` at
connect time).

**Escape hatch:** `ITERION_FORGE_BRAND_AVATAR=off` on the server disables the
connect-time upload deployment-wide; the explicit action stays.

### GitHub App — the manual upload, step by step

1. Download [`/brand/iterion-bot.png`](https://iterion.fabrique.social.gouv.fr/brand/iterion-bot.png)
   (plain, matches the `iterion-bot` account) or the badge
   `/brand/iterion-bot-circle.png` from any iterion server — or take
   `assets/brand/iterion-bot.png` from the repo.
2. Open the App's settings: the *Logo ↗* link on the App row (Integrations →
   Manual setup → Forge OAuth apps), or GitHub → the org → Settings → Developer
   settings → GitHub Apps → the App → **General**.
3. Under **Display information**, *Upload a logo*, pick a badge background
   colour if you like, save. Comments and commits signed by
   `<slug>[bot]` carry the avatar from then on.

To do once on the SocialGouv org: every `iterion-forge-*` / `iterion-watch-*`
App iterion created there, and the `iterion` OAuth App used for SSO.

### GitLab group bot accounts already connected (the PIC)

A connection created before this feature carries no `account_kind`: the
first apply asks the forge who the token is (`WhoAmI`), records what it
learns, and judges the account — a group-token bot user needs no `--force`.
Apply once per connection; the upload goes through the sealed token:

```sh
iterion remote forge connections avatar <conn-id>          # bot user of the GAT
iterion remote forge connections avatar <conn-id> --force  # an unflagged dedicated account
```

The GAT must be **alive** (the upload authenticates as its bot user); a
revoked/expired token answers `forge: credential rejected`. Verify on GitLab:
`glab api users/<bot-user-id> --jq .avatar_url`.

### Commits

Nothing to do: the runner already signs commits with the account's canonical
noreply address ([pkg/runner/git_identity.go](../pkg/runner/git_identity.go)),
so a commit shows the App's logo / the bot user's avatar once that is set.
