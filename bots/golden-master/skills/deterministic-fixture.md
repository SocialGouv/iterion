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

## A check's authority must be the property it claims, never a correlated hint

The single most productive defect class encountered building this bot, by a
wide margin. Eight distinct instances in one day, in eight different files, all
the same mistake: a check establishes something *near* what it claims, and
reports success on the strength of the resemblance.

| claimed | actually tested | how it failed |
|---|---|---|
| the database is up | a pidfile exists and `kill -0` succeeds | a wedged or reused pid exits 0; the app then fails telling you to start the database you just started |
| the artifact is current | source mtimes are older than the artifact | a toolchain change rebuilds nothing, and `git archive` restores commit times, so new files look old |
| the gate passed | the runner exited 0 | the runner printed a red report and exited 0 anyway |
| the held-out set was detected | `detected == total` | `0 == 0` holds when the set is absent |
| the net is complete | the references are committed | the harness that replays them was gitignored |
| the corpus is N wide | there are N entries | two entries shared one byte-identical reference |

The shape is always the same, and so is the consequence: **the failure mode is
a false green, never a false red.** A check that under-claims annoys someone; a
check that over-claims is trusted precisely when it is wrong.

Two rules follow:

1. **Test the property, not its neighbour.** "Does the server answer?" not
   "does a process exist?". "Is this the artifact the current inputs produce?"
   not "is it newer than the sources?". Where a direct test exists, a proxy is
   never worth its convenience — and one usually exists, one call away.
2. **Recover from stale state; never report it as success.** Leftovers are
   normal — runs get killed, machines reboot. Finding one is information, and
   the honest responses are to clean it up or to fail loudly. Exiting 0 because
   something that looks right is present is the one response that guarantees the
   next check inherits a lie.

The test that finds these: *if the thing I am claiming were false right now,
would this check say so?* Then make it false and watch. Every entry in the table
above survived review and died in under a minute against that question.

## Never bake the port

Environments that can host two checkouts at once **derive** their ports, so
that a second working copy neither refuses to start nor — far worse — captures
the first one's application and records a net describing a different tree. Such
an environment publishes the effective URL to a file when it comes up.

A recorded `base_url` is then valid **only on the machine and path that
recorded it**. Move the repository, hand it to a colleague, run it in CI, and
the net cannot reach the application at all. It looks complete: references,
mutants, runner, everything present — and it times out waiting on a port
nothing is listening on.

So prefer `base_url_file`, resolved at replay time:

| | |
|---|---|
| `base_url_file` | path to a file the environment writes on startup — **use this whenever the port is derived** |
| `base_url` | a genuinely fixed endpoint |

`base_url_file` wins when both are present. This is the same discipline as
committing the harness: an artefact that only works where it was born is a
record of one run, not a net.

## The lifecycle contract

`config.json` declares how the harness drives the application:

```json
{
  "base_url_file": "../.state/app/base-url.txt",
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
