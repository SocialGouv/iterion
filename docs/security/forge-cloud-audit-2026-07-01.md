# Forge / cloud security audit — least-privilege (2026-07-01)

- **Scope**: the cloud multi-tenant path that connects iterion to git forges
  (GitHub first), the OAuth-app store, repo provisioning, and the credentials
  those flows produce and hand to bots.
- **Method**: multi-agent read of `pkg/forge`, `pkg/auth`, `pkg/server`,
  `pkg/secrets`, `pkg/runner`, `pkg/sandbox`, `pkg/identity`, `pkg/trigger`,
  followed by targeted verification of the highest-risk auth paths.
- **Commit audited**: `9b03ce6dd`.
- **Bottom line**: **no clear-cut remotely-exploitable vulnerability** was found
  (no cross-tenant leak, no auth-bypass, no CSRF/open-redirect, no token
  leakage). The material findings are **least-privilege weaknesses** — a broad,
  durable forge token shared across a connection's repos/bots, with no default
  egress lock — which *compound under prompt injection*. One privileged SSRF
  (org/team-admin) and minor hardening round it out.

## Verdict table

| # | Area | Verdict | Severity | Evidence |
|---|------|---------|----------|----------|
| A1 | OAuth connect CSRF / PKCE / state | **OK** | — | single-use `state` + PKCE + agent-binding cookie compared with `subtle.ConstantTimeCompare`; tenant from signed state, not URL/JWT — [forge_routes.go:443-492](../../pkg/server/forge_routes.go#L443) |
| A2 | redirect_uri | **OK** | — | `forgeOAuthRedirectURI()` is a server-derived constant used identically in authorize + exchange; post-connect redirect via `safeNext()` — [forge_routes.go:235](../../pkg/server/forge_routes.go#L235), [:459](../../pkg/server/forge_routes.go#L459), [:498](../../pkg/server/forge_routes.go#L498) |
| A3 | Webhook signature verification | **OK** | — | HMAC-SHA256, `hmac.Equal` (constant-time), length-checked, secret sealed with AAD `webhook_hmac_secret:<id>`, never stored plaintext — [webhooks/token.go:64-84](../../pkg/webhooks/token.go#L64) |
| A4 | Authorization gating | **OK** | — | every mutating forge/oauth-app/webhook/provisioning route gated `canManageTeam` (admin/owner), reads `canViewTeam` — [forge_provisioning_routes.go](../../pkg/server/forge_provisioning_routes.go), [forge_oauth_app_routes.go](../../pkg/server/forge_oauth_app_routes.go), [webhooks_routes.go](../../pkg/server/webhooks_routes.go) |
| A5 | Tenancy isolation | **OK** | — | all stores keyed on `TenantID`; defensive re-checks return `ErrConnectionNotFound` without leaking existence; runner Terms on tenant mismatch — [orchestrator.go:114-120](../../pkg/forge/orchestrator.go#L114), [runner/loop.go](../../pkg/runner/loop.go) |
| A6 | Secrets at rest | **OK** | — | AES-256-GCM + per-record AAD (`forge_conn:`, `generic_secret:`, `forge_oauth_app:`, `run_secrets:`); `SealedPayload`/`SealedSecret` are `json:"-"` — [connection_sealer.go](../../pkg/forge/connection_sealer.go), [secrets/generic.go](../../pkg/secrets/generic.go) |
| A7 | Token leakage (logs / API / errors) | **OK** | — | no token *value* logged; `oauth_routes.go:231/256` log owner+kind+expiry only; seal errors don't echo values |
| A8 | mode=auto forge admin token | **OK** | — | passed transiently to `CreateOAuthApp`, never sealed/stored — [forge_oauth_app_routes.go:117-151](../../pkg/server/forge_oauth_app_routes.go#L117) |
| A9 | github_app token staleness | **OK** (correctness, not security) | — | `AppRefresher` re-mints installation token; RefreshWorker selects `ExpiringBefore` and rewrites the managed secret 5m pre-expiry — [forge_routes.go:275-281](../../pkg/server/forge_routes.go#L275), [server_lifecycle.go:104](../../pkg/server/server_lifecycle.go#L104), [github/app_client.go:210](../../pkg/forge/github/app_client.go#L210) |
| **F1** | **Broad durable forge token shared per-connection** | **Weakness** | **HIGH** | `ensureManagedSecret` copies `conn.AdminToken()` verbatim into a durable connection-level `GenericSecret` reused by every repo/bot: OAuth-App = full user `repo`+`read:org`; github_app = whole-installation token (not per-repo) — [orchestrator.go:407-445](../../pkg/forge/orchestrator.go#L407) |
| **F2** | **forge_token has no default egress lock** | **Risk (compounds F1)** | **HIGH/MED** | `effectiveSecretHosts(nil, nil) → nil = allow-any`; managed secret sets no `AllowedHosts` and workflows rarely declare `hosts:` for `forge_token`, so the guard materializes the real token toward *any* host at shell-exec. Sandbox `network: open` by default. — [secretguard.go:123-152](../../pkg/backend/model/secretguard.go#L123) |
| **F3** | **Scopes too broad / opaque** | **Weakness** | **MED** | `read:org` requested by default though only needed for org-repo listing; requested scopes not surfaced in the OAuth-app UI — [github/oauth.go:19](../../pkg/forge/github/oauth.go#L19) |
| **F4** | **SSRF via self-hosted forge base URL** | **Vuln (privileged)** | **MED** | `CanonicalBaseURL` only normalizes scheme+host — no loopback/RFC1918/link-local/metadata rejection — yet the server makes requests to it (WhoAmI, token exchange, hook + app provisioning). Requires an authenticated team/org-admin, who in a SaaS is not a platform operator. — [types.go:167](../../pkg/forge/types.go#L167) |
| **F5** | `managed_secret_id` exposed in API responses | **Info** | **LOW** | implementation-detail id in JSON (useless without the master key) — [repo_integration_store.go](../../pkg/forge/repo_integration_store.go) |

## Is it safe today?

Against an **external / cross-tenant** attacker: yes — the connect flow, tenancy,
sealing, authorization, and webhook verification hold. There is no path found for
one tenant to use another's forge credentials, hijack a connection via CSRF, or
read a token from the API/logs.

The residual exposure is **blast-radius under compromise**: if a bot is
prompt-injected (the exact threat the permission-gate work targets), it runs with
a forge token that (F1) is broader than the one repo it was provisioned for and
(F2) can egress anywhere by default. That combination is what the Phase-2
hardening removes. F4 is a genuine SSRF but is gated behind team/org-admin, so it
is a privileged-insider vector, not an anonymous one.

## Hardening applied (this pass)

- **H2 / F2 — DONE.** The managed forge secret is created with
  `AllowedHosts = [forge host]` ([orchestrator.go `forgeTokenEgressHosts` +
  `ensureManagedSecret`](../../pkg/forge/orchestrator.go)); the secret's own
  egress lock now travels through every resolution tier (seeded in
  `buildGenericResolution`, intersected — never broadened — with bindings) so
  the Tier-0 webhook override no longer leaks it as allow-any. Chain:
  secret → `GenericResolution.AllowedHosts` → `RunBundle.GenericSecretHosts` →
  runner `GenericHosts` → `secretguard`. Parent-domain match means `github.com`
  covers api./codeload./uploads. Tests:
  `TestResolveGenericWithBindings_SecretOwnEgressLock`,
  `TestProvision_SingleBot`.
- **H1 / F1 — PARTIAL.** `MintInstallationToken` now takes
  `InstallationTokenOptions{Repositories, Permissions}` and iterion pins every
  installation-token mint to the least-privilege `RuntimeInstallationPermissions`
  set (never the installation's full grant), and the **runtime** token
  (`AppRefresher`) is scoped to the connection's **provisioned repo set**
  (`forgeConnRepoNames`) — no longer the whole installation. Tests:
  `TestMintInstallationToken_NarrowsScope`. *Deferred (follow-on):* per-single-
  repo scoping of the connect-time creation token requires re-shaping the
  connection-level managed secret + refresh model; staged to avoid regressing
  the working token-rotation path. The github_app path was already
  least-privilege in the dimensions that matter to the "don't propagate a
  user's broad rights" concern (app identity ≠ user identity; fixed minimal
  permissions; operator-selected repos).
- **H3 / F3 — DONE.** `read:org` dropped from GitHub OAuth `DefaultScopes`
  ([github/oauth.go](../../pkg/forge/github/oauth.go)); `repo` alone still lists
  org repos via `affiliation=organization_member`. *Follow-on:* surface the
  requested scopes in the OAuth-app UI.
- **H5 / F4,F5 — DONE.** All forge outbound calls route through
  `forgeHTTPClient()` (a `httpdial.SafeTransport` client honoring
  `outboundStrict()`) — public-unicast validation on **every** dial (redirect
  hops included), rebinding-proof, blocking loopback/RFC1918/link-local/metadata
  in cloud / non-loopback-bind modes ([forge_routes.go](../../pkg/server/forge_routes.go)).
  `managed_secret_id` is now `json:"-"` on `Connection`, `RepoIntegration`, and
  `ProvisionResult`.

## Deferred follow-ons

- **H1-full** — per-single-repo creation-time token + moving the github_app
  runtime credential to per-integration minting (connection-managed-secret +
  refresh-model refactor).
- **H4** — make the GitHub-App connect path the recommended default over the
  broad OAuth-App `repo` path in the studio UI (UX only; does not change which
  token is minted).
- **H3-UI** — display requested scopes in the OAuth-app registration UI.
