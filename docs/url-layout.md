# URL layout — what answers at the root, and what lives under `/studio`

A deployment serves two things from one origin: the **product home** and the
**studio**. The root belongs to the home; the studio has its own base path.

```
https://iterion.cloud/                    the product home — always, signed in or not
https://iterion.cloud/studio              the studio
https://iterion.cloud/studio/runs/<id>    a run
https://iterion.cloud/studio/board        the board
https://iterion.cloud/studio/admin/users  …and every other studio page
```

In a deployment that is **not** in cloud mode — `iterion studio` on a laptop,
the desktop app, a self-hosted server — there is no product home, so `/`
redirects into the studio and an operator never sees the difference.

## What stays at the root, and why

These addresses are printed in mail, minted by the CLI or handed out as links.
Nobody can reissue them, so they do not move:

| Path | Who wrote the address down |
|---|---|
| `/login` | the product home's own sign-in button |
| `/auth/reset`, `/auth/forgot-password`, `/auth/password/change` | password mail |
| `/invitations/accept` | invitation mail |
| `/cli-auth` | `iterion remote login` opens it |
| `/config/<id>#<token>` | a config-share link; the token rides in the fragment |
| `/marketplace` | public, browsable without an account. A visitor who already has a session is carried on to `/studio/marketplace` — the same catalogue, inside the studio shell. Both addresses answer; neither 404s |
| `/api/…` | **every integration**: inbound forge webhooks, OAuth and OIDC callbacks, the REST API, the MCP server, the remote CLI |
| `/brand/…`, `/healthz`, `/readyz` | public assets and probes |

**The link preview's image is absolutised by the server.** `studio/index.html`
ships `og:image` as a RELATIVE path, because the same bundle is served from
iterion.cloud, from preprod and from every self-hosted deployment, and no
build-time value is right for all three. OpenGraph specifies an absolute URL
and LinkedIn in particular drops a relative one, so the preview would render
text-only.

The server is the first place that knows its own origin, so `serveIndex`
rewrites `og:image` and `twitter:image` into `PublicURL` + the path when one is
configured (`absolutiseSocialImages`, [`pkg/server/spa.go`](../pkg/server/spa.go)).
A no-op when it is unset — every local `iterion studio` and desktop run, where
nothing crawls and the bytes are served exactly as built. A protocol-relative
`//host/path` is left alone: it is already absolute.

The docs site does not go through this at all: it is built for one origin and
carries an absolute URL in its VitePress config.

**No integration was affected by the move.** Anything a third party calls lives
under `/api/`, which the studio base does not touch.

## Pre-move links still resolve

A URL published before the move — a run link in old mail, a bookmark, a target
URL on a forge commit status, a run cited as evidence in these docs — gets a
`302` to its current address, with the query intact. The fragment needs no
mechanism: a browser never sends one, and it re-attaches its own to a
`Location` that carries none.

A path that does not survive cleaning gets a `404` instead of a redirect.
`/runs/%2e%2e/%2e%2e/x` would otherwise be answered with a `302` whose target,
once a browser resolves it, sits outside `/studio` — with the product vouching
for it.

The list of redirected segments is **frozen** in
[`pkg/server/studio_legacy_redirect.go`](../pkg/server/studio_legacy_redirect.go):
it records what the URL space looked like at one past moment. A route added
after the move never had a root-level spelling, so it does not belong there;
removing an entry breaks a link that is already out in the world.

The merge gate reads back what it wrote — it decides whether a commit status
belongs to a given run by comparing its target URL — so it recognises **both**
spellings. A pull request opened before the move is still attributable.
See `gateRunTarget` in
[`pkg/server/forge_gate_reconcile.go`](../pkg/server/forge_gate_reconcile.go).

## Changing the prefix

It is one constant, declared twice because nothing executes both sides:

- Go — `deeplink.StudioBase` in [`pkg/deeplink`](../pkg/deeplink/deeplink.go),
  used by every server-built link (notification mail, web push, ops alerts,
  forge commit statuses, post-OAuth redirects).
- TypeScript — `STUDIO_BASE` in `studio/src/lib/scope.ts`, handed to wouter's
  `<Router base>`.

`TestStudioBaseMatchesTheStudioConstant` fails if they disagree.

Two things do **not** need touching when the prefix changes, by design:

- **studio routes and navigations** — `<Route path="/runs">` and
  `navigate("/runs")` are relative to the router base. wouter appends a nested
  base to its parent's, which is also how a desktop workspace pane resolves to
  `/x/<id>/studio/…` with nothing pane-specific to maintain.
- **`/api` calls** — they were never under the studio base.

What *does* need care is code that reads `window.location` directly, since it
bypasses the router: use `studioBase()` for an absolute studio path, and
`rootRoute(path)` to address a root side-door from inside the studio (a bare
`~/login` would also strip a pane's `/x/<id>`).

## Inside a nested route — the `/admin` console

The super-admin console is mounted as `<Route path="/admin" nest>`, which gives
its subtree a router whose base is `…/studio/admin`. Three consequences, all
measured on the console itself:

- `useLocation()` reports a path **relative to /admin** (`/users`), and
  `navigate()` / `<Link href>` resolve against that base. A console path
  written in full (`/admin/users`) therefore lands on `/admin/admin/users`,
  which matches no route. Console paths live in
  `studio/src/views/admin/adminRoutes.ts`, relative, and `adminPath()` spells
  one for a link written **outside** the console.
- A link from inside the console to a page outside it (`/orgs/<id>`) needs
  `studioRoute(path)` (`studio/src/lib/scope.ts`): `~` drops every base, and
  `studioBase()` re-applies the one the path is written against. A bare
  `~/orgs/<id>` would strip a workspace pane's `/x/<id>`, like `rootRoute`'s
  case above.
- `useLocation()` reports the address as typed, and the route matcher accepts
  two spellings the table does not hold: the trailing slash a published address
  can carry (`/users/` — the server's own legacy redirect sends `/admin/users/`
  to `/studio/admin/users/`) and any case variant (`/USERS`), since wouter
  compiles its patterns with the `i` flag. Both render the screen, so anything
  comparing the location against the table goes through `consolePath()`, which
  folds exactly those two — one slash only, because `/users//` matches no
  route, and case because every table path is lowercase.

Two mechanisms keep this true, and they cover different halves:

- `source-discipline.test.ts` refuses an **absolute path literal** anywhere
  under `views/admin/`, whatever consumes it — an `href`, a `navigate()`, a
  const, a table of `{ value: "/admin/x" }`. The route tables are the licensed
  homes of a console path, and `studioRoute()`'s own argument is the way out of
  the nest; `adminPath()` is not, since inside the console it would apply the
  base twice. A first segment counts whether it is spelled out (`"/orgs/…"`) or
  interpolated (`` `/${orgId}` ``). The detector is `_adminNestLinks.ts`, run by
  the guard over the tree and by `adminNestLinks.test.ts` over hand-written
  bypass spellings — what that file proves refused is what the gate refuses. It
  has two limits, each with its own case in that file: a target assembled
  across LINES, and one whose leading slash is its own literal (`"/" + kind`),
  which is the separator a relative path is joined with everywhere else it
  appears. `AdminNav.test.tsx`, `UserDrawer.test.tsx` and `AuditTab.test.tsx` —
  rendering the real routers — are what pin the hrefs for that half.
- `consoleScreens.test.tsx` mounts every screen the table declares through the
  console's own routes, with the real pages: each offers the nav — the only way
  off the screen — and each honours the per-page gate, which is copy-pasted into
  nine files and so has to be held for all twelve at once.
- a shared component mounted inside the console carries its own targets, so it
  cannot assume the studio base: `views/teams/tabs/AuditTab.tsx` renders both
  on the team page and in the platform console, and its cross-link therefore
  uses `studioRoute()`, which resolves to the same address under the studio
  base and to the right one inside the nest.
