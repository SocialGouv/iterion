# Cloud LLM credentials — provisioning a cloud run's model access

How a **cloud** run (queue → runner pod) gets its LLM credential, and how to
provision one so a run stops failing with `401` / `429`. Written from a real
session that lost hours rediscovering this; if you touch cloud cred flow,
keep it current.

## The one-paragraph model

A cloud run resolves its LLM credential **per-run** from the launching
**team/org**, then injects it into the runner as env. If the team has nothing,
the run gets no key and claw fails with `401 x-api-key header is required`.
Three credential kinds exist, and **which backend you use decides which kinds
are legal**:

| Credential | Stored via | Injected as | `claw` backend | `claude_code` backend | `pi` backend |
|---|---|---|---|---|---|
| **BYOK API key** (`sk-ant-api…`, `sk-…`) | `iterion remote api-keys create --provider <p> --from-file/-env` | Provider API-key env (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, etc.) | ✅ | ✅ (also works) | ✅ for Anthropic, OpenAI, xAI, z.ai, and OpenRouter |
| **Anthropic OAuth-forfait** (Claude sub, `sk-ant-oat…`) | `POST /api/me/oauth/claude_code/credentials` (paste `credentials.json`) | Bearer + oauth beta | ⚠️ allowed, warns (bills EXTRA USAGE) | ✅ (it *is* Claude Code) | ❌ the uploaded Claude credential directory is not bridged into pi's agent dir/env yet |
| **OpenAI ChatGPT-forfait** (Codex `auth.json`, `auth_mode: chatgpt`) | `POST /api/me/oauth/codex/credentials` (paste `auth.json`) | ChatGPT-backend OAuth | ✅ (allowed) | n/a | ✅ bridged — iterion seeds a throwaway pi agent dir from the credential, since pi's `openai-codex` provider is OAuth-only and reads no env var (host `~/.codex` works the same way) |

Kimi and Grok are outside this sealed-credential matrix: their delegates rely
on the CLI's own inherited environment/config. Codex consumes its own
uploaded Codex credential.

A **fourth** source is the **org tier**: the parent organization's own keys
and forfaits, lent to the teams its **credential audience** names. It sits
below everything the team resolved and above the pool, because that is what
"shared inside the org" means — a team that brought its own key spends it, a
team the org lends to spends the org's, and neither takes a stranger's
donation while either is available. See
[the org tier section](#the-org-tier--one-key-several-product-teams).

A **fifth** source exists when the deployment runs a credential pool: a run
that resolves none of the above may draw on a **contributor's lent
subscription** (`pkg/credpool`). It reaches the runner as an ordinary OAuth
blob — indistinguishable from a personal forfait — but is metered against
the lender's own ceilings and the run's `max_cost_usd` is clamped to what
remains of them. See [credential-pool.md](credential-pool.md).

A **sixth** source is the **platform tier**: the deployment's own
credentials, stored sealed in the database under reserved scopes
(`secrets.PlatformTenantID` for API keys, `secrets.PlatformOwnerKey` for
forfait blobs) and managed by super-admins via `iterion remote admin llm …`
or the studio's Admin → LLM credentials console. The publisher fills, per
wire family, only the slots the four tiers above left empty — the DB-backed
form of the runner-pod env fallback (`ANTHROPIC_API_KEY`,
`CLAUDE_CODE_OAUTH_TOKEN` from the `iterion-forfait`/`iterion-llm` k8s
secrets), which **remains the final backstop** below it. Since it serves
every tenant that has nothing of its own, who may draw on it is now an
explicit, **opt-in** audience — see
[Gating the platform tier](#gating-the-platform-tier). The dedicated
rotation section is at the end of this doc.

## Decision shortcut

- **Sovereign `web_search` / any claw feature** → needs claw. An Anthropic
  OAuth-forfait now works there (it bills your EXTRA USAGE balance, not your
  plan — see below), but a **BYOK API key** or the **OpenAI ChatGPT-forfait**
  is the predictable-cost choice.
- **Only have a Claude subscription (OAuth)** → use the **`claude_code`
  backend** (native WebSearch/WebFetch + forwarded MCP). The uploaded cloud
  credential also reaches `claw` (against extra usage), but is not currently
  bridged into pi. Claude Code itself spends the plan normally.
- **Have ChatGPT Plus/Pro + Codex signed in** → connect the codex `auth.json`
  and run **claw + an `openai/*` model** — sovereign features work. **pi** also
  reaches it, on an `openai-codex/*` model: that provider is OAuth-only, so
  iterion seeds a per-run agent dir from the same credential rather than
  passing an env var.

## Subscription OAuth on a third-party backend: a spend question, not a ToS one

This used to be a hard refusal (`GuardThirdPartyOAuth` →
`ErrOAuthForfaitInThirdParty`), read as a Consumer-Terms boundary. Anthropic's
API settled it: the token IS accepted from a third-party app and billed
against the subscription's separate **extra-usage** balance rather than the
plan's limits. So the question is not "may I" but "which pot am I spending".

`secrets.GuardSubscriptionOAuth` ([pkg/secrets/credentials.go](../pkg/secrets/credentials.go),
called by `claw` and `pi`) therefore **warns once per node** instead of
refusing — the operator is spending a balance they may not expect — and
refuses only under **`ITERION_FORBID_SUBSCRIPTION_OAUTH=1`**. On pi this guard
covers a subscription token already available through its ambient/local auth;
it does not bridge the cloud-uploaded Claude credential into pi.

**Set that flag on a shared or cloud instance.** There, consuming one
operator's extra-usage balance is a cost decision taken on behalf of everyone
using the instance. The flag closes the per-run credential path AND the env
path (it clears `ANTHROPIC_AUTH_TOKEN` when the value is a subscription token
— `sk-ant-oat…` — leaving a z.ai facade key or gateway bearer in that same
variable untouched), and it is forwarded into the sandbox so the refusal holds
inside the container too.

OpenAI's ChatGPT-forfait has never had an equivalent restriction.

## Gotchas that cost real time

- **A team BYOK `openai` key beats the forfait, and no env var overrides it.**
  `Registry.ResolveWithContext`
  ([pkg/backend/model/registry.go](../pkg/backend/model/registry.go)) hands a
  non-empty tenant key straight to the provider factory — the forfait path is
  never consulted on that branch. So a quota-dead team key yields
  `429 exceeded your current quota` while a valid forfait sits unused, and the
  **only** fix is removing the key (`iterion remote api-keys list` →
  `delete <id>`). `ITERION_OPENAI_USE_OAUTH=1` does **not** help here: that
  force-flag is read only by the local disk factory (the desktop/CLI path that
  reads `~/.codex`), not by the cloud per-run path.
- **With no team key, the forfait already wins over the pod key** — no flag
  needed. `ResolveWithContext` tries the per-run codex forfait *before* falling
  back to the env-var resolver that reads the pod's `OPENAI_API_KEY` (from the
  `iterion-llm` secret). Only the disable knobs are global:
  `ITERION_OPENAI_USE_OAUTH=0` or any `OPENAI_BASE_URL` turns the forfait path
  off (set them under the chart's top-level `config.extraEnv`).
- **An Anthropic OAuth token stored as a BYOK `anthropic` key fails as
  `invalid x-api-key`** — it's sent as `x-api-key`, but an OAuth token needs
  `Authorization: Bearer`. BYOK is for real API keys only.
- **A keyless team → `401 x-api-key header is required`** (nothing injected),
  distinct from `invalid x-api-key` (something injected, wrong shape).
- **Codex `auth.json` must be in ChatGPT mode — there is nothing to strip.**
  The upload is parsed into `CodexCredentialsView`
  ([pkg/secrets/oauth.go](../pkg/secrets/oauth.go)), which declares only
  `auth_mode` + `tokens.*` + `last_refresh`; an `OPENAI_API_KEY` sitting in the
  file is dropped by the JSON decoder and can never shadow anything. The real
  failure is a blob that isn't ChatGPT-mode: `IsChatGPTMode()` requires
  `auth_mode: "chatgpt"` **and** `tokens.access_token` **and**
  `tokens.account_id`, and when it is false the forfait is silently skipped.
  Re-run `codex login` with "Sign in with ChatGPT" and upload the file
  unedited.
- **A codex forfait has exactly ONE refresher, and a record connected by
  an older build may be invisible to it.** OpenAI rotates the refresh
  token on use, so two holders refreshing the same credential invalidate
  each other — the measured incident in
  [bot-runs/feed-watch.md](bot-runs/feed-watch.md), whose remediation reads
  "one session, one record, one refresher". The single refresher is the
  server-side `OAuthRefreshWorker`; runner pods deliberately do **not**
  refresh codex (`runner.startOAuthRefreshers` takes claude_code only),
  and each deployment wants its own `codex login` session rather than one
  shared with an operator's laptop.
  "One refresher" is **enforced on the record, not assumed of the
  deployment** — that worker runs in every server replica with no leader
  election. Each refresh (the sweep's and the manual
  `POST …/oauth/{kind}/refresh`) first takes a compare-and-swap claim on
  the record (`refresh_claim_owner` + `refresh_not_before`, a 2-minute
  lease) and commits only while it still holds it. Consequences you can
  observe: a manual refresh answers **409** while another one is in
  flight; a re-connect during an exchange wins, and the refresh that was
  in flight discards its tokens (409, "replaced while the refresh was in
  flight") instead of overwriting the credential you just uploaded; and a
  replica that dies mid-refresh costs one sweep, not a stuck credential —
  the lease simply expires.
  A third 409 says **cool-down**, and names the instant it ends: the last
  refresh succeeded but the token it returned states no readable deadline,
  so the sweep backs off an hour rather than re-running the exchange (and
  rotating the refresh token) every tick. Nothing is in flight and
  retrying does not help — re-connect the credential, which clears the
  cool-down, or wait for the instant in the message.
  For a credential whose provider never states a deadline that hour is not
  a hiccup, it is the **steady state**: the record is always due, so it is
  renewed hourly for as long as it is connected. That is the price of it
  staying alive — the alternative is to stop renewing it and let it die at
  an hour nobody can predict, which is what used to happen. A credential
  that states its deadline is renewed once per token lifetime instead, so
  it is worth connecting one that does.
  That worker sees the records `DueForRefresh` returns: those expiring
  within its lead, **and those whose deadline is unknown**. The second
  half matters because `access_token_expires_at` is best-effort — it is
  stamped from the access token's own `exp` claim at connect and after
  each refresh, but real `~/.codex/auth.json` blobs carry no `expires_in`,
  so records connected by an OLDER build carry no expiry at all. While the
  sweep required the field to exist those records were skipped forever:
  a run failing its first LLM call with `authentication token is expired`
  while the studio showed the credential present (measured: ten days).
  They are now renewed on the next pass, which is also when they get a
  deadline. Re-uploading still works and is instant:
  ```bash
  iterion remote admin llm oauth set codex --from-file ~/.codex/auth.json
  ```
  A credential whose token states no readable deadline logs a Warn at
  connect (`stored WITHOUT an access-token expiry`) naming what happens
  next: the sweep treats it as due and it learns its deadline there. One
  that ALSO carries no refresh token logs the version that has no way out
  — nothing can renew it, so nothing can stamp it either, and every run
  drawing it dies on its first LLM call. Re-run `codex login` and upload
  again; `iterion remote api GET /api/admin/llm/oauth/connections` lists
  them (`refreshable: false`, no `access_token_expires_at`).
  You do **not** have to wait a sweep for a credential you connected
  already expired: the connect fires one refresh immediately (best-effort,
  off the request), and the server also sweeps once at boot rather than
  only on its next tick — a restart used to re-phase that ticker, leaving
  a token the previous replica was about to rotate dying for a full
  period. Both show in the log as `oauth-forfait refresh …: rotated N
  token(s)`.
- **A run with no credential at all is QUEUED, not refused, by default.**
  The publisher logs one Warn (`no credential resolved for run=… tiers
  consulted: byok, oauth-forfait, pool, platform`) and the runner falls
  back to its pod's ambient env — or fails at the first LLM call, a pod
  and some minutes after the launch was accepted.
  `ITERION_CLOUD_REQUIRE_LLM_CREDENTIAL=1` (server env) refuses such a
  run at publish time instead: HTTP `422` with `error_code:
  NO_LLM_CREDENTIAL` naming the providers to provision, and on the board
  a launch refusal retried with the dispatcher's backoff
  ([dispatcher.md](dispatcher.md)). The rule is **per route**, read
  through the same walk the credential stamp uses: only a run whose
  EVERY LLM route pins a provider the deployment knows (a `provider/`
  model prefix or a `provider:` hint) and NONE of them is funded by any
  tier is refused; one funded pinned provider is enough, and a route the
  walk cannot attribute (a `claude_code` node with no `model:`, `auto`, a
  hint outside the vocabulary such as `kimi-code/…`) is never refused,
  because the runner may still fund it from its env. Turn it on when the
  runner deployment carries no ambient LLM env — then a credential-less
  run can never start, and refusing it early is free information.

## Ingestion gate — what a `400` on provisioning means

Both provisioning paths refuse, at write time, a credential whose SHAPE could
not authenticate — the two paid failures were a terminal transcript pasted as
an accessToken and a bare record the CLI reads as "Not logged in", each
accepted silently and each burning a fleet of runs on 401s before the cause
was found (#627). The gate is
[`secrets.ValidateTokenShape` / `ValidateAPIKeyShape`](../pkg/secrets/credential_shape.go):

- **A bearer token is one run of visible characters.** Any white-space
  (ASCII or not — a no-break space glued on by a rendered page counts), any
  control, format or non-printing rune, NUL, invalid UTF-8: refused, naming
  the rune and its position, never the value.
- **Providers whose credential is a JSON document** (`bedrock`, `vertex` —
  [`Provider.CredentialIsJSON`](../pkg/secrets/byok.go)) must send a JSON
  **object**; the token rule does not apply to them.
- **A pasted `claude_code` credentials.json** must carry `expiresAt` and a
  non-empty `scopes` (absent → the CLI considers itself logged out). An
  expired `accessToken` is refused **only when the record has no
  `refreshToken`**; with one, the record is accepted and the refresh worker
  renews it. The browser flow builds its own blob from the token exchange and
  is held to the token-shape check only (an exchange may answer without an
  expiry or a scope).
- **Codex `auth.json`**: the token-shape check on `tokens.access_token`.

The **local** store has the same door: `iterion secret set` runs the matching
rule for the value's shape (token / JSON / PEM), with `--kind` to name it and
`--kind raw` to opt out — see
[secrets.md](secrets.md#ingestion-shape-gate---kind).

A refusal answers `400` with the reason, and leaves a trace an operator can
find after the fact: a `Warn` in the server log and an audit event naming the
field and the reason — never the value. Nothing is stored on a refusal. Which
log the event lands in follows the credential's owner:

| Owner | Action | Readable by |
| --- | --- | --- |
| Team / org credential | `byok.refused`, `oauth.org.refused` | the team's admins (`/api/teams/{id}/audit`) |
| Platform fallback | `platform.llm_key.refused`, `platform.llm_oauth.refused` | super-admins (`/api/admin/audit`) |
| **Personal (`/me`)** | `byok.refused`, `oauth.personal.refused` | the admins of the **caller's active team** |

A personal refusal crosses into the team's log on purpose: a personal forfait
can be pledged to the org's [credential pool](credential-pool.md), so a
refused rotation of it stops funding runs the org depends on. Only the
refusal crosses — a successful personal connect is never audited — and the
row carries the field and the reason class, never the material. With no
active team on the session there is no tenant to key the row on, and the
server-log `Warn` is the only trace.

## Provisioning cookbook (via `iterion remote`, authenticated)

```sh
# BYOK — value read from a file, never printed:
iterion remote api-keys create --provider anthropic --name mykey --from-file ~/anthropic.key
iterion remote api-keys create --provider openai   --name mykey --from-file ~/openai.key

# Anthropic OAuth-forfait (cloud upload reaches claude_code and claw;
# pi bridge pending) — send the WHOLE credentials.json. The server REFUSES
# (400, see "Ingestion gate" below) a claude_code paste missing expiresAt or
# scopes (the shape the CLI reads as "Not logged in") and a body without a
# refreshToken whose token has already expired; a body with a refreshToken
# is accepted even when the token has expired — the refresh worker renews
# it on its next pass:
iterion remote api POST /api/me/oauth/claude_code/credentials \
  --data "@$HOME/.claude/.credentials.json"
# (Linux/WSL path. On macOS Claude Code keeps these in the Keychain, so there
#  may be no file to read until you export it. Run any `claude` command first
#  so the file is fresh: a stale export carries a refreshToken and connects,
#  but the first refresh is what makes it serve.)

# OpenAI ChatGPT-forfait (claw + openai/* model) — the Codex auth.json.
# Note `"@$HOME/…"`, not `@~/…`: the shell only expands a tilde at the START
# of a word, and ReadDataArg does no expansion of its own, so `@~/...` is read
# literally and fails with "no such file or directory".
iterion remote api POST /api/me/oauth/codex/credentials --data "@$HOME/.codex/auth.json"
# then, if a team BYOK openai key shadows it: delete that key (see the gotchas)

iterion remote api GET /api/me/oauth/connections     # verify (check refreshable)
```

Scope — the two credential kinds resolve **differently**:

- **BYOK keys** are team-scoped, personal-first: `(team, you, provider)` then
  `(team, "", provider)` ([pkg/secrets/byok.go](../pkg/secrets/byok.go)). A run
  under a keyless team won't see another team's keys.
- **OAuth forfaits** resolve **user-first with an org fallback**
  ([pkg/server/cloudpublisher/publisher.go](../pkg/server/cloudpublisher/publisher.go): `addOAuth(ownerID,
  "user")` then `addOAuth(OrgOwnerKey(tenantID), "org")`), so a personal
  `/api/me/oauth/…` connection follows *you* across teams — switching the
  active team neither gains nor loses it.

**Automated runs need the org-scoped upload.** A webhook / dispatcher / cron
run is owned by a synthetic identity with no personal forfait, so it only ever
sees the org record. Provision those on the team:

```sh
iterion remote api POST /api/teams/<team-id>/oauth/codex/credentials \
  --data "@$HOME/.codex/auth.json"
iterion remote api GET /api/teams/<team-id>/oauth/connections
```

### Paste a `claude setup-token` directly

The codex examples above paste a file that is already the right shape.
Anthropic's usually is not: `claude setup-token` prints a **bare token**
(`sk-ant-oat…`, no JSON), and for a team or the platform tier that is normally
all an operator has — nobody logs a shared account into a local CLI just to
export its `credentials.json`.

Send it as-is; the server wraps it:

```sh
iterion remote api POST \
  "/api/teams/<team-id>/oauth/claude_code/credentials?account_label=<account email>" \
  --data "@$HOME/.secrets/claude-setup-token"     # the bare sk-ant-oat… token
```

What the wrap assumes, and why it is not silent:

- **`expiresAt` = now + 1 year** (`secrets.SetupTokenAssumedLifetime`). The
  token carries no expiry of its own, and the choice is asymmetric: no expiry
  at all is what the CLI reads as *"Not logged in"* — stored happily, serves
  nothing — while too short retires a live credential in silence and too long
  only means the provider refuses loudly at the call. So it errs long. The
  server logs the assumption at ingestion, and the connection listing shows the
  resulting `access_token_expires_at`.
- **Scope `user:inference`**, since an empty scope list is the other half of
  what reads as "Not logged in".
- **No `refreshToken`**, which is correct: the record comes back
  `refreshable: false` and the refresh worker leaves it alone.
- The **fingerprint is taken over the token**, not over the wrapper — so
  re-uploading the same token keeps its legacy meter and `account_label`.
  It does not verify the account: setup tokens lack `user:profile`. Use the
  browser connection when a stable provider account identity is needed.

A blob that is neither JSON nor a well-formed `sk-ant-oat…` token still earns
the same typed refusal as before, and a token that picked up a newline or a
space from a copy-paste is refused at ingestion rather than killing every
downstream call with an opaque "Header has invalid value".

### The API response does not prove a run will use it

A successful upload returns a fingerprint. That says the record is stored, not
that anything resolves to it — tiers, window skips and per-kind precedence all
sit between the store and a run. The proof is one line in the **server** log at
publish time:

```
cloudpublisher: oauth-forfait(org) used run=… owner=org:<team> kind=claude_code fp=<new>
cloudpublisher: credentials GRANTED for run=… — oauth-forfait(oauth:claude_code fp=<new>)
```

and its counterpart when a tier declines:

```
cloudpublisher: oauth-forfait(org) SKIPPED for run=… fp=<old> — provider refused the
five_hour window (0% used) (reopens …); falling through to the next credential tier
```

Those lines name the credential, the window and the reopening. A run's own
error message names none of the three — so read the publisher lines first when
asking "which key paid for this, and why not the other one". The cheapest way
to get one on demand is to `resume` a run parked on a usage window: a resume
re-resolves credentials, so it both proves the wiring and unblocks the run.

**The run itself now carries which TIER paid** (`credential_tiers` on
`GET /api/runs/{id}`, shown as a `paid by …` badge on the run header). It is
stamped with `cred_fingerprints`, as one unit, at launch and at **every
resume** — a resume can be funded by a different tier than the launch, so a
field that recorded only the first answer would send you to the wrong door.
It survives log rotation, which the lines above do not:

```json
{ "cred_fingerprints": ["1cf39b47…"], "credential_tiers": ["oauth-forfait"] }
```

Beside it, `pinned_providers` records which providers the run's routes NAME,
and it is the opposite kind of field: stamped **once, at launch**, and
replayed by every resume. It is what lets a shared tier fund a pinned slot on
an already-served wire family (above), and freezing it is the point — a bot
whose source gained or lost a `provider:` pin between launch and resume must
not change what the in-flight run is funded with. Absent on runs launched
before the field existed, which reads as "nothing pinned".

It is **plural** because a run is: one attempt can spend a team forfait on
its implementer and the platform's codex key on its plan review, and naming
one of them "the tier" would be wrong about the other. It carries no slot
names — for the (slot, tier, fingerprint) triple, the GRANTED line above
remains the place to look, and the two cannot drift because they are computed
by the same function.

### Verified accounts share one provider meter

A Claude `credentials.json` carries no account identity. At connection time,
the server now reads Anthropic's OAuth profile when the credential has
`user:profile`. The account and organization UUIDs form a stable SHA-256
fingerprint (`account:anthropic:…`) that survives token rotation, reconnects,
and changes of Iterion owner or rank. Usage readings follow that account
across personal, team, org, pool and platform scopes. A newly verified account
with no readings is probed before its first admission; a failed provider probe
retains the existing fail-open behavior and the mid-run guard.

The listing returns `account_verified`, `account_email`, `account_checked_at`
and, when identification failed, `account_error`. UUIDs and bearer tokens are
never returned. `same_account_ranks` names other connections of the same kind
visible to that owner; it does not enumerate other tenants or donors. Studio
shows the verified email and warns when several entries share one quota.
Editable labels and coincident reset times are not proof of a shared account.

Setup tokens without profile scope and failed profile lookups retain the
legacy payload identity and report the missing verification. A failed lookup
may preserve a previously verified identity only when the stored bearer token
is exactly the same. Replacing it with an unidentified token clears that old
identity. Existing blob hashes cannot be backfilled from labels: reconnect
through the browser to identify those accounts.

A refresh identifies the returned Claude bearer again: a pasted access token
and refresh token are not proof that both belong to the same account. Tokens
and the verified identity are committed together. If that profile lookup fails,
the already rotated tokens are kept, but the old account association is cleared,
the meter becomes unverified and `account_error` explains the missing identity.
A later successful lookup restores the account meter. Refresh also checks that
the stored credential still matches its claimed snapshot before contacting the
provider; a concurrent reconnect cannot be overwritten by that stale snapshot.

This closes the verified-account form of the incident measured on 2026-09-08:
three teams held different credential hashes but drew on one subscription.
Several verified connections of that account now share its provider window;
they are still one source of capacity.

**Rollout:** deploy readers/runners before the server starts issuing these
fingerprints. New readers also merge older, scoped writes carrying the same
verified fingerprint, selecting the newest observation per window. Older
readers cannot read the new shared key. Historical blob-hash readings expire
normally and are not relabeled. The OAuth metadata is additive and no queue
envelope or AST schema changes in this feature.
Equal-time observations use a deterministic tie-break: refusal, higher usage
and refusal streak, then the later reset when those values agree. An unchanged
ledger cannot alternate between a blocked and open window due to cursor order.
The API hides stale profile fields left by a writer that does not know them.

## The fallback chain — several forfaits behind one tier

A tier is not limited to one forfait per kind. Each connection carries a
**rank**: `0` is the primary, `1` and up are fallbacks, and that order is the
order they are tried in. It is the answer to the failure the section above
describes — the tier's account shuts its window and the run has nowhere to go
inside its own tier.

**The resolution already worked this way; it just had nowhere to continue.**
`resolveAndSealCredentials` walks the owner's records and *skips* a forfait
whose provider window is closed (the `oauth-forfait(…) SKIPPED … falling
through to the next credential tier` line). With one record per kind that skip
fell straight out of the tier; with a chain it lands on rank 1 of the same
tier first. Records come back ordered by kind then rank, so nothing had to
learn what a rank is — `ListByUser` hands them over in try order.

Provision a fallback by naming its rank. It is always explicit, never
inferred, so no operator lands on a fallback by accident:

```sh
# Platform tier — a second Claude account behind the deployment's own.
iterion remote admin llm oauth set claude_code --rank 1 \
  --account-label "backup@example.org" --from-file ~/.secrets/claude-setup-token

# Org tier — same idea, for the org's shared forfait.
iterion remote orgs oauth set codex --rank 1 --from-file ~/.codex/auth.json

# The team tier has no typed subcommand yet; the raw endpoint takes the same query.
iterion remote api POST \
  "/api/teams/<team-id>/oauth/claude_code/credentials?rank=1&account_label=backup@example.org" \
  --data "@$HOME/.secrets/claude-setup-token"
```

Every action addresses exactly the link `--rank` names — `set`, `connect`,
`name`, `refresh` and `delete` alike. Read the chain back with the ordinary
listing, which reports each link's rank:

```sh
iterion remote admin llm oauth            # → connections[].rank, 0 first
iterion remote admin llm oauth name claude_code --rank 1 --account-label backup
iterion remote admin llm oauth delete claude_code --rank 1
```

Four things worth knowing before building one:

- **Nothing moves when the feature arrives.** Rank 0 keeps the record id it
  always had, so every credential connected before chains existed *is* the
  primary, and the migration runs itself at startup. An invocation that names
  no rank sends the byte-identical request it sent before.
- **A chain of the same account is not a fallback.** The trap of the previous
  section still applies: two connections of one verified Anthropic account
  share one fingerprint and meter and shut together. Studio flags duplicate
  accounts within the visible owner. Unverified credentials remain unknown.
- **`refresh --rank N` renews that link and no other.** This matters more than
  it looks: the provider RETIRES the refresh token it is handed, so a refresh
  aimed at the wrong record spends a live credential's token and leaves the
  dying one exactly as dying.
- **Only the primary can be lent to the credential pool.** A pledge is keyed
  on (owner, source, kind) with no rank, so `pool lend` offers rank 0. Lending
  a specific fallback is not wired — see
  [docs/credential-pool.md](credential-pool.md).

## Preview the effective fallback order

The team's **Model subscriptions** page includes a fallback preview. Choose
a bot and either your personal launch or an existing webhook. The server
derives the real owner and the webhook's key pins, rather than treating the
person reading the page as the owner of an automated run. The CLI uses the
same endpoint:

```sh
iterion remote credentials preview --team <team-id> --bot review-pr --json
iterion remote credentials preview --team <team-id> --webhook <webhook-id> --json
```

A webhook with several allowed bots and no default also needs `--bot`.
`POST /api/teams/{id}/credentials/preview` takes
`{"source":{"kind":"personal"},"bot_id":"review-pr"}` or
`{"source":{"kind":"webhook","id":"…"},"bot_id":"…"}`. It rejects
arbitrary owner fields and sources belonging to another team. Personal
previews use the bot defaults; they do not include model overrides that an
operator has entered in a different launch form.

The observation shows candidates in the resolver's order, grouped by provider
wire. `selection` distinguishes selected slots, later candidates and tiers not
consulted by the current launch. `state` separately reports observed quota,
capacity, a required probe or restoration of a closed credential to wait for
quota. A restored candidate is not available capacity. A selected key with
`route_only: true` funds only the routes that name its provider — sealed
beside the credential holding its wire family, it is not what an unpinned
node spends — and a key the facade policy keeps off the wire's default says
so in its `reason`. Pool donation is
considered only when the whole credential bundle is empty; a pool grant then
excludes the platform tier. The preview still shows those alternatives and
why they are not consulted, without reserving them.

`account_group` correlates verified accounts only within one response. It
does not reveal provider UUIDs, private donor labels or platform account
emails. Refusals without a utilization number omit `percent`, and unknown
capacity omits the corresponding numeric fields. A donor allowance, when
present, is the prospective allocation for this run, after concurrent
commitments; it is not the donor's total balance.

Every result is dated. It opens no secret, probes no provider, creates no run
and acquires no capacity. Actual materialization, a fresh provider observation
or a concurrent admission can change the chosen credential. Runner environment
credentials and workflow secrets remain outside this database observation.
The selection rules are shared with the real publisher and checked against
its sealed bundle in tests.

## Activating the cross-model plan review (one credential, nothing else)

The plan-phase campaign bots (feature-dev, app-dev,
branch-improve-loop, whole-improve-loop, feature-gap-fill,
test-coverage, e2e-coverage — the authoritative list is
`bots/plan_phase_test.go`) carry an opt-in **peer-reviewed plan phase**
([ADR-091](adr/091-fallback-skip-route-and-plan-peer-review.md)): a
cross-family reviewer (default `claw` + `openai/gpt-5.6-sol`) critiques
the plan before the campaign implements. `plan_review: auto` resolves at
launch from the run's credentials — **on cloud, from what actually seals
into the run's bundle** (BYOK → oauth-forfait → pool → platform tier),
so activation is exactly one provisioning step and zero bot config:

```sh
# User/org tier — the Codex auth.json (ChatGPT forfait):
iterion remote api POST /api/me/oauth/codex/credentials --data "@$HOME/.codex/auth.json"
# …or team-scoped for automated (webhook/cron/dispatcher) runs:
iterion remote api POST /api/teams/<team-id>/oauth/codex/credentials --data "@$HOME/.codex/auth.json"
# …or deployment-wide (platform tier, super-admin):
iterion remote admin llm oauth set codex --from-file ~/.codex/auth.json
```

The next launch of a declaring bot logs
`plan review: on · llm families: claude,gpt` and runs the phase. Verify
with the usual smoke: a one-node `claw` + `openai/gpt-5.6-sol` bot via
`iterion remote runs launch --follow`. To force or refuse the REVIEW per
run: `--var plan_review=on|off` — the plan itself is still authored
either way (the campaign then gets it stamped `plan_provenance:
… NOT peer-reviewed …`); skipping planning altogether is the separate
`--var plan_phase=off`. Deployment-wide brake:
`ITERION_PLAN_REVIEW=off` on the **server** (studio/API) env — NOT the
runner: the resolution happens at publish, the runner consumes
already-resolved vars, so setting it on the runner Deployment is a
no-op. It wins over auto, loses to an explicit `--var`; any
set-but-unrecognised value reads `off` (a brake fails safe). This is
the surface for webhook/cron lanes that have no per-run form, since a
platform-tier OpenAI credential flips `auto` on for every tenant. (A
run riding the runner's env-fallback credentials gets no injection at
all — the bots' `auto` default then reads as off, fail-safe.) Mid-run peer-forfait exhaustion follows
`--var plan_review_policy=wait|skip` (wait = the run parks
failed_resumable and the usage-window retry resumes it when the window
reopens; skip = continue without the review, loudly stamped).
All plan-phase campaign bots default to `skip` — the peer is an optional
enrichment, and a dead second-family credential must never park a
campaign (`wait` is the per-run deliberate-spend opt-in). Gotcha:
the ChatGPT-forfait wire gates models by the codex-cli `version:` header
— if gpt-5.6 is refused with a model-availability error, set
`ITERION_CODEX_VERSION` to a recent codex-cli version (gpt-5.5 needed
≥ 0.130).

## Name the account behind every credential

A verified profile supplies the account email and a default display name.
Custom labels remain useful for the role of a credential; they never replace
provider identity. When the publisher picks a credential it logs a fingerprint:

```
cloudpublisher: oauth-forfait(org) used run=… kind=claude_code fp=700acc7b00f
```

Answering "whose subscription paid for that run?" from hex alone means
grepping server logs and correlating by hand — measured on 2026-09-03,
with three different fingerprints across three owners in one log window.

So name it at connect time, and rename the ones already connected:

```sh
# at install (paste path; the browser flow takes the same query param)
iterion remote api POST "/api/teams/$TEAM/oauth/claude_code/credentials?account_label=jothedev" --data @blob.json

# rename later — metadata only, the sealed credential is untouched
iterion remote api PATCH "/api/teams/$TEAM/oauth/claude_code" --data '{"account_label":"jothedev"}'

# platform tier (super-admin), same idea
iterion remote admin llm oauth set claude_code --from-file ./creds.json --account-label "iterion platform"
iterion remote admin llm oauth name claude_code --account-label "iterion platform"
```

The listing then answers the question directly — `account_label` beside
the `fingerprint`, which is the SAME string the logs print, so a log line
and the API join without a human in the middle. The studio shows both on
every connection card (Settings → Subscriptions, a team's Model providers
tab, Admin → LLM credentials) with a *Name account* action, and both
connect forms take the name up front — pre-filled with the current one,
so a routine rotation through the studio keeps its name unless you
change it.

**The name follows the fingerprint.** A re-connect that names no account
keeps the previous label only when it provably re-connects the same
subscription — Codex fingerprints derive from the account id, and a verified
Claude profile supplies a stable identity outside the blob. A custom name
survives reconnects of that account; a derived email follows a changed profile
email. Unidentified Claude credentials preserve a name only for the same
payload identity. Any other unnamed re-connect drops the label rather than inherit
it: the same owner key re-pointed at a different forfait — the swap
measured on 2026-09-03, SocialGouv's key replaced by a personal one on
the same team — would otherwise answer "whose subscription paid?" with
the wrong person. Pass `account_label` when you rotate, or `name` it
afterwards; an unnamed row is a visible gap, a wrongly named one is a
confident lie.

Renaming is a metadata write at the store (`SetAccountLabel`), never a
read-modify-write of the record: the sealed payload a concurrent refresh
just rotated is not carried back over. The refresh paths are symmetric —
the background worker, the manual `POST …/refresh`, and the
`not_refreshable` self-heal all persist through `UpdateTokens`. That patch
owns the tokens and their verified identity, not unrelated metadata. A derived
email label changes only if its stored value still matches the previous email,
so a rename committed *during* a provider round trip is not reverted.
`Upsert` remains the connect path's, which
legitimately replaces the whole record.

**And what each named credential COST** is a separate ledger:
`iterion remote usage --by-credential` (per team) and `GET
/api/admin/credentials/usage` (the platform tier, or one fingerprint across
every tenant it served). Each amount is typed `metered` or `estimate` — a
forfait's dollar figure is what its calls would have cost metered, not money
— and the two totals come back apart for that reason. See
[quotas-and-limits.md](quotas-and-limits.md#per-credential-usage--what-did-this-key-cost).

## The org tier — one key, several product teams

**The problem it removes.** Before it existed, sharing one key across an
org's product teams meant COPYING it into each team: N writes per rotation,
N places to forget one, and no way to tell whose spend was whose. That was
not theoretical — the production instance had one Claude forfait duplicated
across two teams before this shipped.

An org key is an ordinary `ApiKey` row and an org forfait an ordinary
`OAuthRecord`, stored under a reserved scope (`secrets.OrgTierTenantID` /
`OrgTierOwnerKey`, prefix `orgtier:`), so the whole store — tenant filter,
defaults, rotation, `MarkUsed`, the refresh worker — is reused with zero
schema change. That prefix is deliberately **not** `org:`: that one is taken
by `OrgOwnerPrefix`, which despite its name keys a **team** forfait
(`OrgOwnerKey`'s argument is a tenant id). Reusing it would collide an org
credential with a team one in a single owner namespace, silently.

```sh
# The org's shared keys and forfaits (org admin):
iterion remote api-keys list   --scope org --org <org-id>
iterion remote api-keys create --scope org --provider anthropic --name "SDPC shared" \
  --from-file ~/anthropic.key
iterion remote orgs oauth --org <org-id>                       # list forfait connections
iterion remote orgs oauth set claude_code --from-file ~/.claude/.credentials.json

# Who may spend them — the audience:
iterion remote orgs credential-audience                        # show
iterion remote orgs credential-audience --teams t1,t2          # name teams
iterion remote orgs credential-audience --all-teams true       # every team of the org
iterion remote orgs credential-audience --teams ""             # revoke every named team
```

Semantics worth knowing:

- **The zero value admits NOBODY.** Lending a key is an explicit act, so a
  team that was never named funds its own runs or does not run. This is the
  opposite of the platform tier's default, and the asymmetry is the design:
  an org key is lent by someone who chose to lend it; the platform key is
  what the deployment already runs on.
- **It never shadows the team's own credential.** The fill is per WIRE
  FAMILY, so an org key cannot land next to a key the team already holds in
  another shape — the delegates rank a ctx API key above a ctx OAuth dir on
  one wire, and a second credential there would silently serve every call.
- **A team of another org is refused** when setting the audience (422). It
  is an authorization list, and the publisher trusts it by design.
- **The audience read fails CLOSED.** An unreadable org document skips the
  tier rather than admitting a team its admins never named — safe precisely
  because the pool, the platform tier and the pod env sit below it.
- **Metering follows the ORG, not the borrower.** Slots the tier filled are
  marked `RunBundle.OrgSourced`, the usage-cap meter keys on
  `usagecap.OrgScope(orgID)`, and `pkg/credusage` records them under
  `TierOrg`. Keying one org subscription per borrowing team would open one
  ledger per team, and what one team measured — a refusal, a window at 95% —
  would reach none of the others.
- Every mutation lands in that **org's** audit log, not a tenant log under a
  sentinel nobody can read (`iterion remote audit org`).

## Gating the platform tier

`fillFromPlatform` used to read no tenant policy at all: any team with no
credential of its own drew on the deployment's shared keys in silence. The
`platform_credentials` settings family gates that, on the ADR-090 doctrine
(env/const default, DB record as runtime override, ≤30 s TTL resolver,
super-admin API/CLI).

```sh
iterion remote admin platform-credentials                          # show
iterion remote admin platform-credentials set --orgs <org-id>      # name who may draw
iterion remote admin platform-credentials set --enforce true       # turn the gate on
iterion remote admin platform-credentials set --enforce false      # back to open
iterion remote admin platform-credentials set --keys-first true    # shared tiers: keys before forfaits
iterion remote admin platform-credentials set --facade-default never  # z.ai/Moonshot never the anthropic wire's default
```

`keys_first` and `facade_default` ride the same record but are not part of
the audience: they order the credentials of the org and platform tiers — see
"Forfaits before keys on a shared wire" below. Their defaults come from
`ITERION_PLATFORM_KEYS_FIRST` and `ITERION_PLATFORM_FACADE_DEFAULT` (a value
the server cannot read refuses boot); a stored value overrides them, `""`
clears it back to the env's, and the GET shows both (`keys_first_effective`,
`facade_default_effective`). `--keys-first ""` (`"keys_first": ""` or `null`)
clears it the same way.

- **Enforcement is OPT-IN.** An absent record — or one whose `enforce` is
  off — admits everyone, so the migration is a no-op AND naming a team does
  not by itself cut every other tenant off from the deployment's only
  credential. That failure would be discovered as a fleet of 401s, one write
  after an innocent-looking edit.
- **Orgs are the useful grain**: a team is created inside an org without
  asking the platform, so admitting the org admits teams the list never
  names.
- Enforcing an audience that names **nobody** is refused at write time: it
  is reachable by accident (enable enforcement, forget the lists) and its
  symptom is every credential-less run failing at its first LLM call.
- **A degraded settings read fails OPEN** — the opposite of the org tier's
  rule, deliberately. Failing closed on the tier a deployment runs on turns
  a settings blip into a fleet-wide outage. A refusal that does happen is
  logged with the tenant named, because a run that quietly receives no
  credential fails at its first LLM call with a provider error and nothing
  downstream would say the audience was why.

## A team's LLM fallback policy

The audience gate above decides WHO may draw on the platform tier. The
per-team policy decides WHETHER the shared tiers are the fallback chain at
all: `Team.LLMFallback`, written only through the super-admin route

```sh
iterion remote api PUT /api/admin/teams/<team-id>/llm-fallback \
  '{"llm_fallback":"none"}'
```

- `""` or `"platform"` (the default) — the shared tiers (org, pool,
  platform) stay the fallback chain, exactly as before.
- `"none"` — the sovereign posture: the launch consults ONLY the team's own
  credentials (its BYOK keys, its OAuth forfaits); no shared tier ever seals
  into the team's bundle; and an LLM route nothing of the team's funds
  REFUSES at launch, naming the providers to provision. One funded provider
  does not excuse another pinned route — the shared tiers would have been
  its only other funding, and the policy exists to make that impossible.

Unlike the audience gate, this policy fails CLOSED: the launch reads the
team FRESH at every resolution, and a store error — or an unknown value —
refuses naming it. A typo'd boundary field must not silently re-open the
shared tiers. Env-funded runs (every route on the runner's gateway) and
unattributable routes answer to no policy; a tool-only run spends nothing.

The setting is a boundary, not a preference: it is audited on every write
(`team.llm_fallback_set`, tenant trail + org mirror), and survives the other
team writers because it is a `PatchTeam $set` like the pool mapping. See
also [sovereign pools](sovereign-pools.md).

Two ordering facts worth naming. **Deploy first, then set**: a publisher
older than this field ignores the unknown BSON key, so a team set to `none`
before every publisher is upgraded walks the shared tiers in silence — flip
the teams after the release lands. And the policy is read FRESH at every
resolution including resumes: loosening `none` back to `platform` is
audited, but a resume of a run launched under `none` may then re-seal
shared credentials into its bundle — by design, and now said out loud.

## The routing block — adaptive routing's platform level

The same record carries the platform level of the adaptive-routing policy
(ADR-121, `pkg/llmroute`): which `(harness, credential)` pairs a run may
occupy and in what order, which failure classes may fire a mid-run switch,
what a launch does with a shared key whose cap refused it, and whether
backend pins are requirements.

```sh
iterion remote admin platform-credentials                                   # show (stored + effective)
iterion remote admin platform-credentials set --routing-pair-order claude_code+claude_forfait,claw+anthropic_key
iterion remote admin platform-credentials set --routing-triggers usage_window,auth
iterion remote admin platform-credentials set --routing-refused-pinned-key park    # the pre-policy restore
iterion remote admin platform-credentials set --routing-strict true
iterion remote admin platform-credentials set --routing-clear                      # back to the env dials
```

A `--routing-*` write REPLACES the stored block wholesale (levels are whole
records, never merges); the server refuses a block the fold cannot read —
pairs outside the `harness+credential` vocabulary, triggers outside the
closed set (`usage_window`, `unavailable`, `transient_exhausted`, `auth`;
`budget`/`schema` are not categories and `any`/`unclassified` are excluded
by rule), or an explicitly empty list (omit the field to inherit; a level
that must not switch locks the field instead).

Defaults come from the env dials — `ITERION_PLATFORM_PAIR_ORDER`,
`ITERION_PLATFORM_TRIGGERS`, `ITERION_PLATFORM_REFUSED_PINNED_KEY`,
`ITERION_PLATFORM_ROUTING_STRICT` (refused at boot when unreadable) — and
the built-in ones rank last. `refused_pinned_key` defaults to **`forfait`**
(ADR-121 § Arbitrated 1): a shared-tier key refused or capped at launch,
whose wire family another credential holds, stays out of the bundle — the
routes naming its provider spend that holder (claw bills the forfait as
extra usage) instead of parking on the key's own refusal. `park` is the
pre-policy behavior, kept settable and lockable. Facade keys (z.ai,
Moonshot) are out of the knob's scope entirely: no forfait alternative
exists for them, so they always come back.

Every launch snapshots the resolved policy on the run document
(`llm_route_policy`, with each field's provenance) — "why did this run
route this way" is answerable without replaying the launch. The bot
(`routing:` manifest block), the bot binding and the run-level launch
fields join this fold in the next slices, below the platform level, which
can lock a field against all of them.

## Platform credentials — rotate the deployment's fallback without a redeploy

The credential every tenant-less run used to inherit from the runner pod's
env (`CLAUDE_CODE_OAUTH_TOKEN` via the `iterion-forfait` k8s secret,
`ANTHROPIC_API_KEY`/`OPENAI_API_KEY` via `iterion-llm`) can now live in the
database instead: super-admin-managed rows the publisher resolves at every
launch **and every resume**, seals into the per-run bundle, and hands to the
runner exactly like a tenant credential. Rotation is one CLI call — no k8s
secret edit, no rollout:

```sh
# Provider API keys (metered):
iterion remote admin llm api-keys                      # list (metadata only)
iterion remote admin llm api-keys create --provider anthropic --name prod \
  --from-file ~/anthropic.key
iterion remote admin llm api-keys rotate <key-id> --from-env NEW_KEY
iterion remote admin llm api-keys delete <key-id>

# Forfait blobs (claude_code credentials.json / codex auth.json):
iterion remote admin llm oauth                         # list connections
iterion remote admin llm oauth connect claude_code     # browser code flow
iterion remote admin llm oauth set claude_code --from-file ~/.claude/.credentials.json
iterion remote admin llm oauth set codex --from-file ~/.codex/auth.json
iterion remote admin llm oauth refresh claude_code
iterion remote admin llm oauth delete claude_code
```

Semantics worth knowing:

- **Position in the chain**: after tenant BYOK, user/org forfaits and the
  mutualised pool; before the env fallback. A pool-granted run is never
  double-served (the donation would be shadowed while still consuming the
  donor's quota), and a slot is only filled when the run holds nothing on
  the same **wire family** — a platform `anthropic` key never shadows a
  tenant's own `claude_code` forfait (the delegate ranks a ctx API key above
  a ctx OAuth dir on the same wire).
- **…except a slot the workflow PINS**, which is fundable on its own name
  even when another credential already fills its wire family. A run whose
  node says `provider: "moonshot"` (or, on claw, `model: "moonshot/kimi-k2"`)
  gets the shared tier's Moonshot key beside the Anthropic one it is already
  served by — otherwise that node is refused while its funding sits one row
  away, which is what happens to every second provider on a shared wire.
  Three properties make it safe, and they are what the rule is:
  - the one-key-per-family rule is **unchanged for a run that pins
    nothing** — no pin, no extra slot, byte-identical behaviour;
  - a key filled this way serves **only the routes that name its
    provider**. It rides a separate channel (`RunBundle.PinnedAPIKeys`), so
    the delegates' default precedence cannot see it: the unpinned nodes of
    the same run keep the credential they had, including a tenant forfait
    that the wire order would otherwise have put behind a facade key;
  - the pinned set is **frozen at launch** (`store.Run.PinnedProviders`) and
    replayed by every resume, so a source edited between launch and resume
    cannot change what the run is funded with.

  A route the launch cannot resolve — a subbot's inner nodes, an `auto`
  hint, a `{{vars.…}}` provider — contributes no pin, so its slot stays
  unfillable and its node is refused by name. `iterion remote credentials
  preview` applies the same rule, so what it lists is what the launch will
  grant.
- **Forfaits before keys on a shared wire** (platform AND org tiers): when a
  shared tier holds both a forfait and an API key on one wire family — a
  Claude forfait and a z.ai key, say — the **forfait takes the family** and
  the key funds only the routes that name its provider. The first credential
  to fill a family is what every unpinned node of the run spends, and a
  subscription the deployment already pays for beats a key billed per token
  (the pool's own order, [credential-pool.md](credential-pool.md)); filled
  the other way round, a platform z.ai key would take the anthropic wire of
  every team without a credential of its own and serve their claude nodes
  GLM. What follows from it:
  - **A facade key is not the wire's default beside a Claude credential**
    (`facade_default`, `auto` by default). z.ai and Moonshot keys ride the
    anthropic wire but answer a claude id with their own model. Under `auto`
    such a key is the wire's default on NO tier of the run while ANY tier of
    the run holds an Anthropic-native credential — a Claude forfait in any
    window state, or an `anthropic` key the launch's bot may spend. The rule
    spans the run since #1998: an org whose Claude forfait is closed keeps
    the platform tier's z.ai key off the wire too, and the run parks on the
    forfait, because a claude id answered GLM in silence is the failure mode
    the policy exists for. A z.ai-only deployment (no tier holds a Claude
    credential) keeps serving every node.
    `facade_default: tier` is the per-tier rule #1956 first shipped: a tier
    that itself holds no native credential falls through to its facade key —
    capacity over label, for operators who prefer a served run on another
    vendor to a parked one.
    `never` keeps a facade key off the default everywhere (the routes that
    name it only); `always` lets it take any free family, so a closed forfait
    falls through to it. A tier whose store cannot answer "do you hold a
    Claude credential?" is read as holding none, said on the server log.
    One tier holding a Claude forfait F and a z.ai key Z (`tier` reads every
    row as if that tier were the only one):

    | `keys_first` | `facade_default` | F on ANY tier | anthropic wire's default | Z |
    |---|---|---|---|---|
    | false | auto | open | F | routes naming `zai` |
    | false | auto | closed | F, restored — the run parks on it | routes naming `zai` |
    | false | auto | none on every tier | Z | default |
    | false | tier | closed on THIS tier, none on it | Z — this tier's fall-through | default |
    | false | always | closed | Z | default |
    | false | never | any | F (restored when closed) | routes naming `zai` |
    | true | auto | open | F — Z is a facade | routes naming `zai` |
    | true | always | open | Z — keys first | default |

    A key of the wire's own vendor (an `anthropic` key) is not a facade:
    `keys_first` alone orders it against the forfait, and a closed forfait
    yields the wire to it.
  - **A closed forfait yields the wire — to a key the policy lets take it.**
    A forfait refused by its provider, or blocked by an operator cap — soft
    or hard: a soft cap lets no NEW run start ([usage-caps.md](usage-caps.md))
    and a launch is one — is skipped, and a key the rules above admit fills
    the wire it leaves free. That is the capacity fall-through.
  - **A run in flight waits for its forfait.** A run whose forfait closes
    mid-run parks until that forfait reopens — its usage-window retry arms
    on the forfait's reset — while the next launches skip the closed
    forfait. Waking the parked run onto a key early would need a reopening
    instant per wire family on the run; the run records one instant for all
    of them, and stamping it "now" would make every park of the run retry at
    the floor, on wires a re-resolution cannot move.
  - **A pin on the forfait's own provider is funded beside it, pinned-only.**
    claw bills a Claude forfait as extra usage (and refuses it under
    `ITERION_FORBID_SUBSCRIPTION_OAUTH=1`) and pi has no bridge to it, so a
    shared tier's `anthropic` key pinned for an `anthropic/…` node is what
    that node spends. claude_code under a `provider: anthropic` hint spends
    the run's own default key first, then the forfait, and only then such a
    pinned key — the subscription's work stays on the subscription. claw on
    an `openai/…` route spends the run's own openai key, then the ChatGPT
    forfait (on plan) while the runner lets it serve one
    (`ITERION_OPENAI_USE_OAUTH` not `0`, no `OPENAI_BASE_URL` — both cross
    into the sandbox), then a key pinned for the route — in process and in
    the sandbox alike; on `anthropic/…` its key stays before the Claude
    forfait.
  - **A pinned key no route spends holds nothing.** A key sealed for routes
    only is stamped — and so takes a slot of its concurrency ceiling — when
    some route may spend it. A route whose backend declares that it spends
    the run's forfait of that provider first (`delegate.RegisterForfaitFirst`:
    claude_code for `anthropic`, codex for `openai`), beside a sealed forfait
    of that provider, never reaches the key; any route the walk cannot vouch
    for counts as spending it — a backend resolved at dispatch or read from
    the environment (the runner expands it with its own), a model it cannot
    read, a supervisor (it calls its model in process, key before forfait),
    and claw, whose openai order depends on runner settings the server
    cannot see.
  - **The pool lends to what the run cannot fund.** It is asked only for
    providers no key sealed for the run's routes already funds: a donation
    beside such a key would take its routes over, a stranger's credential
    spent where the run's own tier held one.
  - **A GLM model on the anthropic wire is a z.ai route** — bare `glm-*` on
    claude_code or pi, `anthropic/glm-*` on claw. It pins `zai`, spends the
    z.ai key (the default one, or the one pinned beside a forfait), and its
    spend is booked there; it never reaches the Claude forfait, which
    api.anthropic.com cannot serve it on — in process and in the sandbox
    alike, and claw's subscription guard does not apply to it. A GLM node
    with no hint gets the `zai` one only when a z.ai key is reachable (the
    run's, or `ZAI_API_KEY`), so an ambient z.ai setup (`ANTHROPIC_BASE_URL`
    on api.z.ai) keeps working. The z.ai key goes to the operator's
    `ANTHROPIC_BASE_URL` when one is set — a z.ai-compatible endpoint, a
    proxy — on claw as on claude_code, and to z.ai's endpoint otherwise. A
    GLM spec naming another provider (`openrouter/z-ai/glm-4.6`) stays that
    provider's route.
  - **Tier order comes before instrument order.** The rules order the
    credentials of ONE tier; the org tier still fills before the platform
    one, so an org's credential holds the wire over the platform's for that
    org's teams. The restore step keeps the same order: when every tier
    refused its credentials, the one handed back per wire family is the
    earliest tier's — the tenant's own before the org's, the org's before
    the platform's — in that tier's fill order. A refused shared key comes
    back in the channel its fill would have sealed it in: beside the
    credential holding its family, only for the routes that name its
    provider — they park on their own refusal instead of reaching a
    credential that cannot serve them — and never over a key another tier
    already sealed for those routes. Nor does the tenant's own refused key
    displace a key a shared tier sealed for its provider's routes: those
    routes are served, so it waits until every tier had its turn and comes
    back only into a family none refilled, for a run some route of which
    may spend that key's slot as the run's default — a claude_code route
    whose chain is not only hints it honours (or is read from the
    environment), a claw route on the anthropic wire for the Anthropic and
    z.ai keys, a route the walk cannot resolve — as the wire's last park
    point, over that route key, which would never be spent beside it. The routes naming its provider then park with the run; a wire
    left empty would fail the default-reading routes on a no-credential
    error nothing retries, or spend the runner pod's ambient env. A run
    whose every route names the provider keeps the shared key.
  - **What the run spends is what the ledger books.** The per-credential
    spend ledger books a claude_code route on the credential its session
    named (the `fingerprint` on `delegate_finished`), so a node pinned
    `provider: zai` is charged to the z.ai key, not to the forfait holding
    the wire, a claw `openai/…` route to what claw spent under the runner's
    settings, a pi `anthropic/…` or `openai/…` route to that provider's key
    (the default one, then one pinned for the route — never a forfait nor a
    facade), and a kimi, grok or opencode route to nobody — they spend their
    own config. The usage-cap pre-flight reads each route's credential the
    same way ([usage-caps.md](usage-caps.md)) — kimi and grok it does not
    meter, opencode, which may pick the pod's ambient Anthropic credential,
    it judges on the pod's ambient meter — and meters the run on its DEFAULT
    credential's ledger — a pinned key keeps its owner's.
  - **The escape hatches**, deployment-wide and on both shared tiers:
    `iterion remote admin platform-credentials set --keys-first true` puts
    keys back in front, and `--facade-default always` lets a facade key take
    a free family again (`keys_first` / `facade_default` on `PUT
    /api/admin/settings/platform-credentials`; the env defaults above).
  - **Known limits.** A claw or pi route whose model the walk cannot read
    (`{{vars.model}}`, a model from the env) names no provider, so no shared
    key is sealed for it — give it a readable model, or put keys first. A
    `${…}` in a model or a provider is expanded with the SERVER's env to
    decide what to fund; a runner whose env says otherwise may route
    elsewhere. The accounting errs toward counting: a route it cannot vouch
    for (an env-read backend, claw on openai) stamps a pinned key it may
    never spend, which then holds a slot of that key's concurrency ceiling.
    A supervisor with a model of its own is funded like a claw node: a
    shared tier's key pinned for its provider serves it before the team's
    forfait (a Claude forfait on claw is billed as extra usage). A
    pool-granted run skips the platform tier, pinned-only fills included,
    and the pool's own order does not read the facade policy. Direct
    generations (`interaction_model`, recoveries, the review companion) read
    the process env, not the run's credentials. pi's `openai-codex`
    provider is not an iterion provider id: its routes widen the
    resolution, and their spend of the ChatGPT forfait is booked on
    nobody. A claw `anthropic/…` node run in process spends the Claude
    forfait where the ledger and the usage-cap pre-flight read the run's
    z.ai key first — the sandbox's order: a route carries no sandbox
    verdict.
- **Rotation reach**: new launches and resumes re-resolve, so a
  `failed_resumable` run picks the fresh value on resume. In-flight runs
  keep the sealed snapshot they launched with.
- **Refresh for free**: a platform forfait stored via the browser flow (or a
  full `credentials.json` with its refresh token) is renewed by the same
  background refresh worker as user/org records.
- **Usage-cap scope stays whole**: bundle slots the platform filled are
  marked (`RunBundle.PlatformSourced`) so `ITERION_USAGE_CAP_*` metering
  keeps pooling them on the single platform meter instead of fragmenting it
  per tenant ([usage-caps.md](usage-caps.md)).
- **Migration**: push the values currently in the k8s secrets through the
  commands above, then the env vars become a pure backstop you can empty at
  leisure. An empty platform store keeps today's behaviour byte-identical.
- Every mutation lands in the platform audit log (`/api/admin/audit`).

## Re-pointing the OAuth endpoints (OEM CLI, proxy, air-gapped IdP)

The forfait flow talks to a reverse-engineered vendor surface, so every
endpoint it uses is env-overridable per deployment. **Move them as a set** —
the first three drive the browser connect, the fourth drives what happens
after it, and a deployment that moves three of four connects against one host
and refreshes against another (each is read at the call site through
[`envOr`](../pkg/secrets/oauth_authcode.go), so nothing is cached across a
restart):

| Env var | Default |
| --- | --- |
| `ITERION_OAUTH_FORFAIT_ANTHROPIC_AUTHORIZE_URL` | `https://claude.ai/oauth/authorize` |
| `ITERION_OAUTH_FORFAIT_ANTHROPIC_REDIRECT_URI` | `https://platform.claude.com/oauth/code/callback` |
| `ITERION_OAUTH_FORFAIT_ANTHROPIC_SCOPES` | `user:profile user:inference user:sessions:claude_code user:mcp_servers` |
| `ITERION_OAUTH_FORFAIT_ANTHROPIC_TOKEN_URL` | `https://console.anthropic.com/v1/oauth/token` — the auth-code exchange **and** the refresh worker |
| `ITERION_OAUTH_FORFAIT_ANTHROPIC_PROFILE_URL` | `https://api.anthropic.com/api/oauth/profile` — the account lookup. Move it with the token endpoint: this is the leg that sends a **bearer** outbound, on every connect and every refresh |
| `ITERION_OAUTH_FORFAIT_CODEX_TOKEN_URL` | `https://auth.openai.com/oauth/token` |

The two client ids (`ITERION_OAUTH_FORFAIT_{ANTHROPIC,CODEX}_CLIENT_ID`) ride
the config loader rather than the call site; full table in
[oauth-forfait.md](oauth-forfait.md#configuration).

## Related

- Backends + provider routing + the OpenAI-via-ChatGPT-forfait section:
  [backends.md](backends.md).
- The sovereign `web_search` backend ladder (a claw feature — hence needs a
  claw-legal credential): [web-search.md](web-search.md).
