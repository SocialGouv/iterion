# Forge security-read — org-wide Dependabot alerts for bots

The security-read flow gives a bot read access to a GitHub org's
**Dependabot alerts** without any long-lived credential: the forge
GitHub App mints short-lived `vulnerability_alerts:read` installation
tokens and iterion maintains them in a well-known team secret the bot
reads. Built for [`bots/vuln-watch`](../bots/vuln-watch/) (Senti); any
bot can declare the same secret.

## The contract: the `dependabot_tokens` secret

One team generic secret named **`dependabot_tokens`**
(`forge.SecurityReadSecretName`), plaintext = a JSON map of lowercase
org login → token:

```json
{"socialgouv": "ghs_…", "dnum-socialgouv": "ghs_…"}
```

`"*"` is accepted as a fallback key for every org. The **shape is the
contract, not the mint**: a deployment without the GitHub App flow
fills the same secret by hand with fine-grained PATs
("Dependabot alerts: read-only", one per org).

Mind which store you write, they are different secrets:

```sh
# CLOUD (a team secret, what a cloud run resolves):
iterion remote api POST /api/teams/<team-id>/secrets \
  --data '{"name":"dependabot_tokens","value":"{\"my-org\":\"github_pat_…\"}"}'

# LOCAL / desktop runs only (~/.iterion/secrets.json):
iterion secret set dependabot_tokens '{"my-org": "github_pat_…"}'
```

The consuming bot mounts it `as: file` and reads it only in its
deterministic poll step (vuln-watch has no LLM node at all).

## The App-managed path (cloud)

Permission model — mirrors the delivery-permissions opt-in
([pkg/forge/github/app_client.go](../pkg/forge/github/app_client.go)):

- `SecurityReadInstallationPermissions()` =
  `{vulnerability_alerts: read, metadata: read}` — a separate profile,
  never folded into the runtime forge token (which stays repo-narrowed
  Contents/PR/etc.). Alert data names every vulnerable dependency of
  every repo, so only this dedicated token ever carries the grant.
- New Apps request it when created with
  `AppManifestOptions.AllowSecurityRead`.
- Existing Apps: add **"Dependabot alerts: Read-only"** in the App's
  settings on GitHub (Permissions & events), then **an org admin
  approves the pending permission request on every installation**
  (org Settings → GitHub Apps → the App → Review request).

Per-connection opt-in — nothing mints until a team admin enables it on
the github_app connection:

```sh
iterion remote api PATCH /api/teams/<team>/forge/connections/<conn_id> \
  --data '{"security_read_enabled": true}'
```

The enable endpoint mints **immediately**: a missing grant answers
`422` with the remediation named, and nothing is persisted — you learn
on the spot, not at the next hourly run. From then on the forge
[RefreshWorker](../pkg/forge/refresh.go) re-mints the org's token in
the same cycle that rotates the connection's forge token (both are
~1h installation tokens) and merges it into `dependabot_tokens` under
the installation's org login. Disabling removes the org's entry (and
deletes the secret when the map empties).

Multiple orgs = one connection per org (a GitHub App is installed per
org), each opted in; the worker merges them all into the one secret.

## Health and diagnostics

- `GET /api/teams/{id}/forge/connections/{conn_id}/health` reports
  `security_read_enabled` and `missing_security_permissions` (names
  `vulnerability_alerts` when the installation never approved it; the
  fix is the `manage_install_url` page).
- A failing hourly re-mint surfaces as one server warn per refresh
  cycle (`forge: security-read mint for connection …`) — it never
  blocks the connection's own forge-token refresh.
- Bot side: vuln-watch fails its run explicitly when a configured org
  has no usable token, and names the org + both remediation paths
  (App opt-in / hand-set PAT). A 401/403 from GitHub names the token
  rotation.

## Verification checklist (first wiring)

1. App settings carry "Dependabot alerts: Read-only"; each org
   approved the pending request (installation page shows it granted).
2. The App is installed on **All repositories** of the org — the
   org-level alerts endpoint reads across the installation's repo
   scope, and a partial install silently narrows what Senti sees.
3. `PATCH …/connections/{id}` with `{"security_read_enabled": true}`
   answers 200 (a 422 names the missing grant).
4. Probe the endpoint with the minted map (from a run, or a PAT):
   `GET /orgs/<org>/dependabot/alerts?per_page=1` → 200.
5. Launch vuln-watch with `--var dry_run=true` and read the prepared
   messages in the run artifacts.
