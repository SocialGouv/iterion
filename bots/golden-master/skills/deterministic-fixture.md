---
name: deterministic-fixture
description: Freeze the application's world so captures are reproducible AND the surface is actually exercised — seeded data, credentials, clock, locale, outbound calls.
---

# Freezing the world

Two failures live here, and the second is the one that quietly ruins golden masters.

**Failure one — the capture drifts.** Two runs of an unchanged application disagree. Everything
downstream becomes noise.

**Failure two — the capture is vacuous.** The runs agree perfectly, because every page is empty.
Nothing can ever differ, so nothing can ever be detected. The net is stable, reproducible, and
worthless.

A worked example, from a real modernisation delivery. The seeded offers all carried a publication
window of September to November 2018. The public search filters on that window, so from 2019
onward it returned nothing. Four references in the delivered catalogue captured `"content": []`
at around 400 bytes each — including one whose entire purpose was to cover sorting and
pagination. A broken `WHERE` clause, an inverted sort, a collapsed join: all of them still return
`[]`, and all of them pass. The back office, which does not filter on that window, was covered
properly. The vacuous lane was the product's main function.

**Before recording anything, look at what the references actually contain.** A reference of a few
hundred bytes where you expected a listing is the signal. Ask of each entry: *what could change
in this application that this reference would fail to show?*

## What to freeze, and how

**Data that exercises the surface.** Prefer removing the time dependency to postponing it. Widen
a publication window to an interval that never expires rather than shifting it relative to today
— shifting means the same failure returns next year, on someone else's watch. Where a state
machine gates visibility, seed entities in each state.

**Credentials.** Seeded accounts frequently carry a password hash nobody recorded. Rewrite the
hash to a known value; that is fixture setup, not a code change. Match the hash **variant** the
application's security stack accepts — an older stack may reject a `$2b$` bcrypt hash that a
modern tool emits by default, while accepting `$2a$`. Verify by actually logging in, not by
reading the encoder's documentation.

**Locale and timezone.** Force them, do not default them. An operator's shell in a national
locale changes sort order, number formatting and date rendering. `LC_ALL=C.UTF-8` gives
byte-order collation while keeping UTF-8 — plain `C` will break on any non-ASCII filename the
build touches.

**Database collation.** Match what the application ran on in production, and do not "improve" it.
Collation determines `ORDER BY` for text, which is exactly what shifts when the database engine
changes. If the reference was recorded under a different collation from the one the migration
lands on, the drift is invisible — and that drift is the whole point of capturing it.

**Outbound calls.** Anything reaching a third party must be stubbed or pointed at a local sink,
otherwise the reference encodes someone else's uptime. A local SMTP sink with a read API is
enough for mail flows.

**What NOT to freeze.** Do not neutralise volatile values in the *data* when the canonicaliser
should be handling them in the *capture*. If logging in updates a last-login timestamp that
appears in a listing, that is a canonicalisation problem ([[canonicalization]]). Solving it in the
fixture removes a genuine test of the canonicaliser.

## The lifecycle contract

`config.json` declares how the harness drives the application:

```json
{
  "base_url": "http://127.0.0.1:8080",
  "up":   "…bring database and application up, then apply the fixture…",
  "down": "…stop both…",
  "ready_path": "/login",
  "personas": [
    {"name": "anon"},
    {"name": "admin", "login": {"path": "/login", "method": "POST",
                                "fields": {"email": "…", "password": "…"},
                                "csrf_field": "_csrf"}}
  ]
}
```

- **`up` must be idempotent and must include the fixture.** It runs again on every restart, so a
  non-idempotent fixture corrupts the state mid-run.
- **Careful: `up` must not overwrite what a mutant changes.** If the fixture rewrites a column and
  a mutant mutates that same column, a restart silently reverts the mutation and the mutant reads
  as undetected. Either keep those fields disjoint, or mark such mutants `needs_restart: false`.
- **`csrf_field` is optional on purpose.** A legacy baseline may have no CSRF protection at all
  while its modernised target does. Requiring a token would make the harness unusable on the very
  baseline it must capture.

## No containers

The runtime forbids mounting a container socket, so `docker compose`, Testcontainers and any
sibling-container trick are unavailable. Run services **natively**: a database binary started on
a socket in a scratch directory, seeded by the application's own migrations. This is faster than
containers anyway, and it produces something the CI can reuse without privileged mode.

Keep all mutable state — data directory, build caches, logs — **outside the worktree**. The
worktree is recreated on every run; a cache inside it is lost or, worse, half-lost.
