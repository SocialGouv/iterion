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

**Known limit — the link preview loses its picture, not its message.**
`studio/index.html` sets `og:image` to a RELATIVE path, because the same bundle
is served from iterion.cloud, from preprod and from every self-hosted
deployment, and no build-time value is right for all three. OpenGraph specifies
an absolute URL and LinkedIn in particular drops a relative one, so those
previews render text-only — `og:title` and `og:description` are text and
resolve regardless, which is what carries the definition. The remedy is to
inject an absolute `og:image` from `PublicURL` when the server serves the
index; the seam exists (`ServeInjectedIndex` already rewrites the head for
workspace panes), but `serveIndex` has no config today. The docs site, built
for one origin, uses an absolute URL and does not have the problem.

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
