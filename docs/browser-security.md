# Browser security posture — CORS, CSRF, cookies, CSP

What protects the studio from the *browser* side: which cross-origin requests
are accepted, what makes a session cookie unforgeable, and what the CSP allows.
Read this before touching `authMiddleware`, the auth cookies, the origin
allowlist, or anything that adds a response header.

The load-bearing fact, first, because every rule below follows from it:

> **iterion cloud is served on two hosts, and they do not have the same
> security properties.**
>
> | host | registrable domain (the "site") | who else is same-site |
> |---|---|---|
> | `iterion.cloud` | `iterion.cloud` | only `*.iterion.cloud` — ours |
> | `iterion.fabrique.social.gouv.fr` | `social.gouv.fr` | **~47 sibling hosts** we do not control |
>
> `gouv.fr` is a [public suffix](https://publicsuffix.org/), so the second
> host's site is `social.gouv.fr` — shared with every other app on the
> platform (grafana, harbor, metabase, rancher, …).

Both hosts serve on purpose, not as a transition: a redirect cannot sit in
front of registered webhooks (GitLab turns a 301 POST into a GET and loses the
payload; GitHub counts a 301 as a failed delivery). See the ingress comment in
`values.ovh-prod.yaml`.

**Why that matters:** `SameSite=Lax` only restricts *cross-site* requests. A
request from `anything.social.gouv.fr` to the fabrique host is **same-site**,
so the browser attaches the session cookie to it — POST included. On that host,
SameSite buys nothing. Everything below is what actually holds the line.

## The CSRF boundary is the origin gate, not SameSite

`Server.originGateAllows` ([pkg/server/middleware.go](../pkg/server/middleware.go))
refuses `403` any request that is:

- under `/api/`, **and**
- a state-changing method (`POST`/`PUT`/`PATCH`/`DELETE`), **and**
- carrying an `Origin` the deployment does not recognise.

It lives in `authMiddleware` — one gate every path already traverses — because
the per-handler form it replaced (`requireSafeOrigin`) was opt-in and drifted to
**70 of 247** state-changing routes, leaving BYOK keys, team/org secrets, OAuth
forfaits, platform LLM credentials, forge connections and org administration
ungated. The per-handler calls remain as defence in depth.

An **absent** `Origin` passes. That is not a hole: it means a non-browser caller
(the CLI, runner pods, forge webhooks), which carries no ambient credential for
an attacker to ride.

What is recognised, via `isAllowedOriginReq`
([pkg/server/server_files.go](../pkg/server/server_files.go)):

1. **same-origin** — the Origin's host equals the request `Host`. This is what
   makes both public hosts work with no configuration, behind any proxy.
2. the **loopback** set (`localhost` / `127.0.0.1` / `[::1]` on the bound port),
3. the desktop **wails** origins,
4. the configured **`PublicURL`** — for a proxy that rewrites `Host` so (1)
   cannot match.

`sameOrigin` also refuses a plaintext `http://` Origin when `X-Forwarded-Proto`
proves the request arrived over TLS. The check is one-sided on purpose: an
`https` Origin is accepted whatever scheme is reported, because a
TLS-terminating proxy that omits the header would otherwise make the server
refuse its own SPA.

### Two things that do NOT protect a state-changing endpoint

Both were true here and both were load-bearing in the original defect:

- **The CORS preflight.** A `POST` with `Content-Type: text/plain` is a CORS
  *simple request* — never preflighted. The JSON decoders never inspect
  `Content-Type`, so such a request is decoded normally. Only `PUT`/`DELETE`/
  `PATCH` are preflighted, which is why the hole was POST-only.
- **Not returning `Access-Control-Allow-Origin`.** That stops an attacker
  *reading* the response. The request still executes; CSRF is about the side
  effect.

### `ITERION_REQUIRE_ORIGIN=0`

Disables the gate, for a rollback without a redeploy. It is also what the sweep
test toggles to prove the 403s come from the gate and not from something else.

## Session cookies carry the `__Host-` prefix

A host-only cookie is not one a sibling cannot **write**. Any
`*.social.gouv.fr` host can set `iterion_auth` with `Domain=social.gouv.fr`;
the browser sends both and the server reads whichever comes first — enough to
pin a victim onto an attacker's session ("cookie tossing").

So the cookies are `__Host-iterion_auth` and `__Host-iterion_refresh`. A
browser accepts a `__Host-` cookie only with `Secure`, `Path=/` and **no
`Domain`** — terms no other host can satisfy on our behalf.

- **Reads are not symmetric with writes** (`sessionCookie`,
  [pkg/server/auth_sessions.go](../pkg/server/auth_sessions.go)), and both
  asymmetries were paid for:
  - Where the prefix IS written, a bare **access** cookie is not a credential
    at all. Merely *preferring* the prefixed one is not enough: the real access
    cookie expires in 15 minutes while a tossed one is attacker-controlled and
    can carry a year, so every tab idle past the access TTL would fall back
    onto the attacker's session — and logout cannot help, since a host-only
    deletion cannot clear a `Domain`-scoped cookie.
  - Where the prefix is NOT written, the prefixed name is ignored **entirely**
    rather than preferred. Both switches that decide this are a single env var,
    i.e. what an operator reaches for to roll the change back; preferring it
    would leave the browser holding a `__Host-` cookie the rolled-back build can
    neither overwrite nor delete, and a fresh login would be served the previous
    user's session.
  - The **refresh** cookie does keep the legacy fallback — it is
    server-verified, single-use and rotating, and without it the deploy signs
    out every existing browser. A legacy browser pays one 401, which the SPA
    answers with a silent refresh, and comes back migrated.
- **The prefix is withheld** when `CookieSecure` is false (a plaintext local
  studio) or `CookieDomain` is set. Those terms are not a preference: a browser
  *discards* a `__Host-` cookie that breaks them, so emitting one there would
  mean nobody can log in.
- **The refresh cookie is `Path=/`**, not `/api/auth` — `__Host-` requires it.
  Path scoping was never a boundary (the cookie is `HttpOnly`, and any
  same-origin page can address any path); unforgeability by a sibling is.
- **Logout expires both spellings**, because mid-migration a browser can hold
  the legacy cookie and the prefixed one.

**Migration:** the current build reads both names and writes only the prefixed
one. Drop the bare-name read once the longest refresh TTL has elapsed since the
deploy.

## Response headers and the CSP

`securityHeaders` ([pkg/server/middleware_security_headers.go](../pkg/server/middleware_security_headers.go))
is wired **outermost**, so the headers ride the responses the stack
short-circuits too (a 401 from auth, a 403 from the origin gate, a recovered
panic). It sets `nosniff`, `Referrer-Policy`, `X-Frame-Options`,
`Permissions-Policy`, and an **enforced** CSP on non-`/api/` responses.

The policy is measured against the running SPA, not assumed:

- **`script-src 'self'`** holds with zero violations. It is the directive that
  carries the weight — the boundary between an injected string and executing
  code — and it is also what keeps the editor self-hosted (a CDN fetch is now
  refused rather than merely discouraged).
- **`style-src` needs `'unsafe-inline'`.** Without it the e2e run counts 234
  `style-src-attr` and 2 `style-src-elem` violations: the emotion-based
  CSS-in-JS under `@lobehub/ui` injects `<style>` at runtime and the component
  libraries set `style=""` attributes. Removing it needs a nonce pipeline
  through those libraries, not a policy edit. The residual exposure is CSS
  injection, categorically below script execution.

A handler that needs a different policy overrides it with its own `Set` — the
run-preview endpoint replaces it with a `sandbox` policy for attacker-influenced
run output.

**HSTS is deliberately not emitted by the app.** It is a transport promise only
the component terminating TLS can honestly make; the ingress already sends
`max-age=31536000; includeSubDomains; preload`. Emitting it from a plaintext
local `iterion studio` would pin the operator's own loopback to HTTPS.

`ITERION_SECURITY_HEADERS=0` disables the set.

### No third-party CDN in the SPA — ever

The Monaco editor used to be fetched at runtime from `cdn.jsdelivr.net`
(`@monaco-editor/react`'s default when no `loader.config()` is called). That put
third-party executable code in the surface that edits LLM keys, OAuth forfaits
and forge tokens, and told the CDN who was using a `gouv.fr` deployment.

It is now bundled ([studio/src/lib/monaco.ts](../studio/src/lib/monaco.ts)) —
**import `Editor`/`DiffEditor` from there, never from `@monaco-editor/react`.**
The same rule already governs the fonts (`@fontsource-variable/*`, self-hosted
so the desktop app and sandboxed runs work offline). `script-src 'self'` is now
the enforcement.

## Verifying it

**The gate, against a live deployment** — an invalid body, so nothing mutates.
Expect `403` on all three; before the fix the first two answered `400`, meaning
the request had reached body parsing with a foreign `Origin`:

```bash
for H in iterion.cloud iterion.fabrique.social.gouv.fr; do
  for P in /api/me/secrets /api/me/api-keys /api/v1/bots; do
    curl -s -o /dev/null -w "$H$P %{http_code}\n" -X POST "https://$H$P" \
      -H "Authorization: Bearer $TOKEN" \
      -H "Origin: https://evil.fabrique.social.gouv.fr" \
      -H "Content-Type: text/plain" --data '{"bogus":'
  done
done
```

**The gate, in CI** — `TestEveryStateChangingAPIRouteRefusesForeignOrigin`
([pkg/server/origin_gate_sweep_test.go](../pkg/server/origin_gate_sweep_test.go))
reads the **live routing table** and requires a 403 from all 174 state-changing
routes, so a route added tomorrow is covered with no edit. It names the
endpoints it exists for (so it cannot silently shrink) and is falsified by its
own kill switch (so a 403 arriving for some other reason cannot masquerade as
the gate working).

**The CSP and the editor, against the real browser** —
`devbox run -- task test:e2e:ui -- --grep "security-headers"`
(Playwright, [studio/e2e/specs/security-headers.spec.ts](../studio/e2e/specs/security-headers.spec.ts)):
boots the SPA and mounts Monaco under the policy, asserting zero CSP
violations, zero requests to any CDN, and that the editor actually rendered —
so a blank page cannot pass. One-time browser install:
`devbox run -- task test:e2e:ui:install`.

**Headers on a deployment:**

```bash
curl -sD - https://iterion.cloud/ -o /dev/null |
  grep -i "content-security-policy\|x-frame-options\|nosniff\|referrer-policy\|strict-transport"
```

## When you add an endpoint or a header

- A new state-changing `/api` route needs **nothing** — the gate covers it, and
  the sweep test will assert it. If you find yourself wanting an exemption,
  that is the thing to justify.
- A route that must accept a genuine cross-origin browser caller needs its
  origin in the allowlist (`PublicURL`), not a bypass.
- A surface that serves markup sets **its own** CSP, deliberately, next to the
  handler — as the run preview does.
