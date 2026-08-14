---
name: surface-discovery
description: Enumerate an application's observable surface and choose a corpus with real width — how to find routes, which personas to open, and the entries teams systematically forget.
---

# Finding the surface

The corpus is the perimeter of the net. Anything outside it is unprotected, and a perimeter
chosen for convenience produces a completeness that is an artefact rather than a property.

Work from **both ends**, because each end misses what the other finds.

**From the code.** Route declarations, controller annotations, a router table, a URL
configuration, template names. This finds endpoints no link points at — exports, callbacks,
legacy paths still wired up.

**From the running application.** Crawl from the entry points with each persona's session. This
finds what the code reading missed: routes assembled at runtime, redirects, endpoints the front
end calls that no server-side route table names in one place.

Anything only one method finds deserves a second look. It is usually either dead or important.

## Width beats depth

Twenty-five well-chosen entries beat two hundred variations of the same page. Cover, at minimum:

- **one entry per route family**, not per route
- **every persona**, including anonymous — see below
- **list, detail, creation form, and error** for at least one central resource
- **pagination and sorting**, on a collection that actually has enough rows to paginate
- **each export format** the application produces
- **the refusal lane**: requests that must be denied. A net that only records successes cannot see
  an authorisation regression, which is the failure with the worst consequences
- **at least one 404 and one validation error**

## Personas

Open one session per distinct authorisation level, and always include an anonymous one. Add one
persona that is a CASE VARIANT of another (`"case_variant_of": "admin"`, same credentials with
the case changed): whether it logs in or is refused is a behaviour, and either way it must stay
the one the baseline had — identifier case-folding is exactly what drifts when the storage
engine or the security stack changes.

Do not assume every role in the schema has a seeded account — frequently several do not. When a
role has no account, say so explicitly in the corpus and cover the endpoint with the nearest role
that does. Recording "this role was not covered, and here is why" is an honest artefact; silently
dropping the endpoint is not.

## Traps

**Redirect following.** An HTTP client follows redirects by default. A refusal that answers
`302 → /login` will then record the login page as its reference — stable, plausible, and useless:
it captures the login page, not the refusal. Capture the redirect itself for authorisation
entries.

**Entries that look alike are usually the same entry.** Two references of identical size in a
corpus almost always mean two requests landed on the same rendered page. Check.

**Client-rendered pages.** When a page is a shell filled by JavaScript, the HTML capture holds an
empty template regardless of the data. The signal is in the API call the page makes — capture that
endpoint directly, and note in the corpus that the HTML entry is a shell.

**Requests with side effects.** A `GET` that mutates state exists in older applications and will
poison every reference captured after it. Find them: in legacy code, a state-changing `GET` is
common enough to expect rather than to hope against. Keep them out of the corpus, or place them
last and restart afterwards.

## Required probes — the classes teams systematically skip

Width has a floor, and the harness enforces it before the application even boots. Each probe
below is a regression class that shipped through a corpus hole on a real target: the net was
green, the mutants were green, and the defect lived on a surface no entry watched. An entry
claims a probe in its `probes` list; the harness counts it only when the mechanical shape
holds — a tag without the substance is a declaration, and the net does not grade declarations.

<!-- iterion:corpus-probes
[
  {"probe":"write_create","required":true,
   "catches":"a creation path broken by an upgrade — form binding, converter registration, validation chains — while every read and even the update lane stay green",
   "shape":"a `write` entry, method != GET, that INSERTS through the application's own form or API; `readback` shows the created resource"},
  {"probe":"error_then_corrected","required":true,
   "catches":"an error state that sticks to the re-rendered form and turns every later submission into a refusal — the journey real users make, invisible to any single-shot write",
   "shape":"a `write` entry with `steps` (>= 2): an invalid submission, then its correction; the reference captures the whole trail plus the readback"},
  {"probe":"case_pair","required":true,
   "catches":"case-folding and collation drift — a lookup or search that stops (or starts) matching when only case differs, typically after a database engine or collation change",
   "shape":"two entries whose paths differ only by case (same route, one query value re-cased)"},
  {"probe":"text_sort","required":true,
   "catches":"collation ORDER drift — a listing sorted on a textual key changes order between engines or locales while every row is still present, so subset and value mutants see nothing",
   "shape":"one entry whose query string orders a real listing on a TEXTUAL column; numeric-id sorts cannot carry this signal"},
  {"probe":"auth_case","required":true,
   "catches":"identifier case-sensitivity drift at login — accounts that stop authenticating because stored and typed case ceased to be folded the same way",
   "shape":"a persona declaring `case_variant_of` another persona, credentials equal ignoring case and different with it"}
]
-->

The probes are corpus WIDTH; the mutant archetypes ([[oracle-mutation]]) are comparator DEPTH.
The two compose: `write_create` gives the surface, `create_lost` proves the lane would notice
if creation silently stopped persisting.

## The perimeter is stated, not assumed

"From the code" above is not advice — it is a deliverable. The configuration must declare
`routes_probe`: a command written next to `state_probe` that prints the application's own
routing table, one route per line, `METHOD /path/{param}` or a bare path, `#` comments allowed.
Derive it from the routing declarations themselves (annotations, a router table, a URL conf),
not from a crawl: the probe exists to find what crawling misses.

The harness replays it at every gate and matches every route against the corpus — `{x}`, `:x`
and `*` match one segment, `**` a tail, and a trailing slash is NEVER folded: with-slash and
without-slash are different routes, and the difference has shipped real 404s. A route the
corpus does not touch fails the gate, unless `route-coverage.json` records its exclusion WITH a
written reason:

```json
{"exclusions": [
  {"route": "POST /internal/webhook",
   "reason": "fires only from the payment provider; covered by integration suite, not replayable here"}
]}
```

An exclusion without a reason is refused. The point is not that every route must be watched; it
is that every unwatched route is a decision someone wrote down, instead of a hole nobody chose.

## The corpus file

```json
{
  "entries": [
    {"id": "001", "persona": "anon", "method": "GET", "path": "/",
     "surface": "http", "note": "shell page; the data comes from 002"},
    {"id": "014", "persona": "anon", "method": "GET", "path": "/admin/users",
     "surface": "http", "note": "refusal lane: anonymous must not reach this"},
    {"id": "030", "persona": "manager", "method": "POST", "path": "/items/new",
     "surface": "write", "probes": ["write_create", "error_then_corrected"],
     "steps": [
       {"fields": {"name": "", "kind": "basic"},
        "note": "invalid on purpose: empty name must re-render the form"},
       {"fields": {"name": "Probe item", "kind": "basic"}}
     ],
     "readback": "/items?q=Probe+item",
     "note": "the journey: fail, correct, succeed; readback proves the INSERT"}
  ]
}
```

A `write` entry may carry `steps`: each step posts in the SAME session (method and path default
to the entry's), the reference captures every step's status and body plus the readback, and
`restore` runs once after the entry as usual. Ids are stable and never reused — a reference
file is named after its id, and renumbering silently re-points every mutant's `targets`. Keep a
`note` wherever the entry's purpose is not obvious from its path; the next reader is deciding
whether a diff matters, and the note is what lets them.
