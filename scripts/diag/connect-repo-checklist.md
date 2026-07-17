# Connect-repo diagnostic checklist (cloud)

Run this checklist BEFORE touching connect-flow code when "connecting a
repo doesn't work" is reported on a cloud instance. Most causes are
config or forge-side state, not iterion code.

## 1. Server config sanity

```sh
curl -s https://<host>/api/server/info | jq '{mode, version, commit, forge_github_app_configured}'
kubectl -n <ns> get configmap iterion-config -o yaml   # values are non-secret
kubectl -n <ns> get secret iterion-auth -o jsonpath='{.data}' | jq 'keys'  # KEYS only
```

- `ITERION_PUBLIC_URL` must equal the browser origin exactly (scheme +
  host). The GitHub App manifest embeds callback URLs derived from it;
  a mismatch breaks the manifest callback and the OAuth callback.
- `ITERION_COOKIE_DOMAIN` empty (host-only) is correct for a single-host
  deploy. `ITERION_COOKIE_SECURE=true` behind TLS.
- `forge_github_app_configured=false` only means no PLATFORM-level App
  (`ITERION_FORGE_GITHUB_APP_*`); teams can still create their own App
  via the manifest flow. The ConnectForm must not steer users into a
  dead mode when this is false.

## 2. Existing forge state (as the team admin, via cookie jar)

```sh
curl -sb cookies.txt -H "Origin: https://<host>" https://<host>/api/teams/<team>/forge/connections
curl -sb cookies.txt -H "Origin: https://<host>" https://<host>/api/teams/<team>/forge/oauth-apps
curl -sb cookies.txt -H "Origin: https://<host>" https://<host>/api/teams/<team>/forge/repo-bots
```

- `status` on each connection: `active` | `needs_reauth` | `degraded`
  (+ `status_reason`). A `degraded` github_app connection usually means
  a permissions update was requested but not approved on GitHub.
- `access_token_expires_at` in the future + recent `last_refreshed_at`
  ⇒ the refresh worker is healthy.

## 3. GitHub App installation scope (the usual suspect)

```sh
curl -sb cookies.txt -H "Origin: https://<host>" \
  "https://<host>/api/teams/<team>/forge/connections/<conn>/repos"
```

`AppClient.ListRepos` calls `GET /installation/repositories` with an
UN-narrowed (permission-limited) installation token, so the result is
exactly what the GitHub-side installation covers. If the repo you want
to connect is missing here, the App was installed with "Only select
repositories" and the fix is on GitHub, not in iterion:

    https://github.com/organizations/<org>/settings/installations/<installation_id>

(or `https://github.com/apps/<slug>/installations/<installation_id>`).
Add the repo to the installation, then re-run the search.

Note: run-time tokens ARE narrowed to the provisioned repo set
(`narrowGitHubAppSecret`); that narrowing never affects ListRepos.

## 3bis. Widening a GitHub App installation via API — token-type maze

`PUT /user/installations/{installation_id}/repositories/{repo_id}` (and
the whole `/user/installations*` family) has a token-acceptance matrix
of its own, with misleading error messages (verified live 2026-07-17):

- `gho_…` (OAuth app token — what `gh auth login` mints) → 403
  "contact an Organization Owner" EVEN for an actual org owner.
- `github_pat_…` (fine-grained) → 403 "Resource not accessible by
  personal access token".
- `ghp_…` (classic PAT, `repo` scope, org-owner account) → **204**.

So: script the widening only with a classic `ghp_` PAT from an org
owner; otherwise use the installation settings page (the
`manage_install_url` the health endpoint returns). Either way iterion
needs no redeploy — `InstallationInfo` probes the live scope.

## 4. Cross-site round-trips

- The OAuth / App-install / manifest callbacks rely on a signed `state`
  plus an agent-binding cookie at `Path=/api/auth/oidc/` /
  `/api/forge/`. `SameSite=Lax` is compatible with top-level 302
  returns; a broken flow here shows as `?sso_error=` / 400 on the
  callback with a fresh session still anonymous.
- Manifest state TTL is 10 min — a slow GitHub form submit 400s.

## 5. Server logs

```sh
kubectl -n <ns> logs deploy/iterion --since=48h | grep -iE "forge|manifest|oauth" | grep -iE "error|fail|403|422"
```

---

## Findings — prod (iterion.fabrique.social.gouv.fr), 2026-07-17

- Config: sane. `ITERION_PUBLIC_URL` correct, cookies host-only+secure,
  GitHub SSO on, no platform GitHub App (expected).
- Team Ministères-Sociaux already has a healthy manifest-created App
  (`iterion-forge-61934180`, installation 145904609, token auto-refresh
  green) + an active webhook integration on SocialGouv/iterion.
- ROOT CAUSE of "cannot connect a repo": the GitHub installation covers
  ONLY SocialGouv/iterion, so `/connections/<id>/repos` returns one
  repo; searching any other repo yields an empty list with NO
  explanation and NO "add repositories to the installation on GitHub"
  affordance in the UI (the iterion-veille schedules were wired by
  script for exactly this reason). UX fix tracked in the connect-flow
  redesign; no server/config fix needed.
- Secondary: /integrations concentrates 4 conceptual steps + 5 tabs on
  one page; ConnectForm shows disabled radios with tooltips instead of
  steering; landing "Sign in" button is a no-op (no /login route) —
  both addressed by the UX overhaul waves.
- Unrelated warn in logs: oauth-forfait `claude_code` refresh for team
  3a29c5ee lacks client_id+refresh_token (BYOK/forfait config gap, not
  forge).
