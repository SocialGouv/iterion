---
name: signals-and-queries
description: prod-watch (Argus) signal lanes — the Loki window and cursor contract, the Sentry lists and what posts when, what the redaction scan derives (templates, leak classes, coverage), how Prometheus probes are typed, the health probes, and how to read a tick's outputs and messages.
---

# Signals and queries

## Loki — the window contract

Each configured query is fetched over a **frozen window** `[from, to)`:

- `to = now − ingest_lag_seconds` (default 120 s): Loki ingestion is not
  instantaneous; a line that arrives after the cursor moved past its
  timestamp would be lost for good. Set `ingest_lag_seconds` to your
  worst-case ingestion delay: `overlap_seconds` buys back only the tail of
  ONE window — a line even later than lag + overlap is lost with a `full`
  coverage note, and no later tick reads it.
- `from` = the frontier where the last walk stopped, or the high-water
  mark − `overlap_seconds` (default 60 s), whichever is earlier — never
  before the band's lower bound, and bounded below by
  `to − max_window_minutes` (default 60): a cursor older than that is a
  declared **gap** — the lines between it and the floor are never read,
  and the coverage note says so. A first tick uses
  `bootstrap_window_minutes` (default 10) instead; `0` is a valid window
  that reads nothing and sets the cursor at `to`. A cursor ahead of `to`
  (an ingest lag raised between two ticks) is an empty window this tick,
  not an error — the frontier stays where the last walk stopped, so an
  unread tail is not lost to a narrower overlap afterwards; more than a
  full window ahead is reported as an error.
- The walk is `direction=forward`, `page_size` lines per page (default
  1000), the next page resuming AT the last timestamp (inclusive, the
  re-read deduplicated), until the window is exhausted or `max_lines`
  (default 5000) new lines were written. Reaching the cap **truncates**
  the window: the frontier stops at the last line fetched, the tick
  reports `truncated: true` and the scan reports `coverage: partial`.
  Nothing is skipped — the next tick reopens at the frontier (a complete
  walk moves it to its window's end, or keeps it where an earlier walk
  already read further) — but "no finding"
  proves nothing for a partial tick. A group of lines sharing one
  nanosecond wider than the cap drains across ticks, `max_lines` a tick.
- Every line is written exactly once across ticks: the overlap re-reads
  the tail of the previous window on purpose (late ingestion) and the
  lines seen there travel in the cursor's band, as `[timestamp, hash]`
  pairs; the scan additionally deduplicates by `(timestamp, line)`.
- A query that failed keeps its high-water mark and moves its frontier to
  where the walk stopped (the window is retried from there next tick,
  whatever the overlap becomes) and is listed in `errors`; the lines it wrote before failing
  are scanned this tick and its band knows them (bounded like any other:
  the cut applies up to where the walk reached), so the retry does not
  write them again; a query whose first window was never read (a failed
  first tick), or only partly (a truncated one), retries that window from
  its bound — it does not slide with the clock — and the end of the first
  window is the query's HISTORY boundary, carried in its cursor until the
  walk passes it: a line read while the first window is still being read
  is history, not news, whichever tick reads it and whichever query
  returned it; once the walk passed the boundary it is dropped, and a late
  line stamped below it and read in the overlap afterwards is news (a
  line the sweep and a template query both return is one record; a
  template counts its live lines apart, ranks by them, and renders their
  own first-seen, sample and streams — never the order of the queries
  decides; a known incident seen only in history lines keeps its count,
  fields and quiet note, and its clock follows the newest of those lines);
  a lane that did not
  observe everything this tick — a failed query, a truncated walk, a
  declared gap, an empty window — concludes nothing about incidents it
  did not see (no "not observed any more"): template incidents are judged
  by the template queries and a full template list (a sweep running
  behind yields no template and blocks nothing), leak incidents by every
  query, and with no template query configured template incidents are
  never concluded on; an incident unseen for `forget_after_days` is
  forgotten whatever the lane observed (retention, not a conclusion — if
  its pattern comes back later, it is posted again as new); an incident
  every source of which left the config (its probe removed, every query
  that last counted its template or found its leak removed) is concluded
  nothing either — nothing looks at it any more — and retention forgets
  it; while one of them is configured, it is observed as usual. The run goes on with its
  other lanes (decide refuses a tick only when EVERY configured lane
  failed, naming the errors) and the lane's health is not refreshed while
  a query fails, so a query dark for good surfaces as a silent source
  after `source_stale_hours`, with its error. A Grafana the lanes cannot
  use — a token refused (401/403), blank or unbound, a host that does not
  resolve — is such a failure, on every query and probe of both Grafana
  lanes, named in the coverage note: the health probes still report
  while another lane answers.

The persisted cursor is `cursors.loki.<query>` in `state.json`:
`covered_to_ns` (the high-water mark, never moving backwards),
`frontier_ns` (where the last walk stopped; below the mark after a
truncated walk), `band` (the lines of the overlap as `offset:hash` strings
from `band_base_ns`, about 4000 at most, cut only on a timestamp boundary)
and `overlap_from_ns` (the band's lower bound, which never descends: a cut
or a raised overlap must not reopen the window over lines the band no
longer knows), and `history_to_ns` while the first window is not passed.
A query absent from the config keeps its mark for `forget_after_days`
and the part of its band at or above min(frontier, mark) — exactly what a
re-added query re-reads.

## The redaction scan (`leak_scan`)

The only node that opens the raw handoff (`loki_raw-<run>.jsonl`; every
scratch handoff name carries the run id — the scratch is shared between
runs — and a tick harvests the handoffs older than a day). For every line,
in this order
(secrets first, then validated identifiers, then contact data):

| class | how it is recognised | what replaces it |
|---|---|---|
| `private_key` | PEM private-key block | `[REDACTED:private_key]` |
| `jwt` | three base64url segments starting `eyJ` | `[REDACTED:jwt]` |
| `bearer` | `Bearer <token>`; `Authorization: Basic|Token|ApiKey|Bot|SSWS <credentials>` (16 characters at least; a scheme word followed by a short word is prose: `authorization: token authentication failed`, and a lowercase snake_case status word too: `token authentication_required` — a mixed-case one is a credential); `Basic <credentials>` elsewhere when it decodes to a printable `user:password` with a non-empty password (unpadded and `latin-1` credentials included; `basic` followed by an ordinary word is prose) | `[REDACTED:bearer]` |
| `cloud_or_forge_token` | AWS `AKIA…`, GitLab `glpat-`/`glrt-`, Grafana `glsa_`, Sentry `sntry…`, Anthropic `sk-ant-`, OpenAI `sk-…`, Slack `xox…`, GitHub `ghp_`/`github_pat_`, Stripe `sk_live_`/`rk_live_` (and `_test_`), Google `AIza…`, Hugging Face `hf_`, npm `npm_`, SendGrid `SG.…` | `[REDACTED:cloud_or_forge_token]` |
| `secret_kv` | a secret word starting a name part — snake, kebab, dotted, quoted or bracketed — or a camelCase part (`dbPassword`, `DBPassword`, `dbPass`), with only the suffixes a secret name takes (`_KEY`, `_VALUE`, `PHRASE`, a number, an environment: `SECRET_KEY=…`, `PASSWORD_2_PROD=…`, `PASSPHRASE=…`, `TOKEN_PROD=…`), then `=`, `:` or `=>` (`"api_key": "…"`, `[password] => …`); a quoted value runs to its closing quote (an escaped `\"` stays inside — closed or never-closed alike, after a key, a flag, in SQL; a value ending in a single backslash is taken whole; a quote never closed runs to the end of the line unless the tail reads as code — `;)}]`, even glued — or as a sentence quoting the key: three spaces, so a three-word tail still pages, the deliberate trade); an unquoted value rides the plain tokens after it (`password: hunter2 sekrit7` redacts the whole tail; a token shaped like a name with a value — `user=42` — ends it, base64 padding `Zq7h9xAb3cZq==` does not, an empty `user=` rides); a flag value, quoted or not (`--password …`, `--db-pass …`, `--passphrase "…"` — one shell word: a multi-word secret after a flag must be quoted to ride whole); SQL `IDENTIFIED BY '…'` / `WITH PASSWORD '…'`; a URL's credentials `scheme://user:…@` (the password from 4 characters — shorter DSN passwords are a documented bound). Not a secret: a logger's own mask or a placeholder (`[Redacted]`, `********`, `${DB_PASSWORD}`, or a shout-underscore placeholder after a flag — `--pass CHANGE_ME`, quoted or not), a word of 12 letters or fewer (a status or a usage text: `expired`, `requires`), a lowercase snake_case value under a bare `*pass` key (`mountain_pass=closed_for_winter` is a status feed — under any other key, and quoted, in a DSN or after a flag, the same value is a credential and is redacted), a path **shape** (a relative prefix `./`, `../`, `~/`, a system prefix `/run/secrets/db`, or lowercase alpha segments `/data/redis/sessions` — and every head-based refusal holds only while the head is the whole value or the tail is plain lowercase words: a credential riding behind a path, a prefix, a word or a placeholder head is scanned; a base64 token of lowercase letters alone with slashes reads as a path — a documented miss), a count under a `…_token` key (`"prompt_token": 1234`), an API page token (`NextToken=`, `page_token=`); `total_tokens=…`, `PASSWORD_FILE=…` are not secret keys | key kept, value replaced |
| `nir` | 13 digits + 2-digit key, **key validated** (Corsica 2A/2B handled) | `[REDACTED:nir]` |
| `iban` | country code + check digits + BBAN, **the length that country's IBAN has, mod-97 validated** | `[REDACTED:iban]` |
| `card` | 15–19 digits, **Luhn-validated**, the issuer prefix a card network's (Visa, Mastercard, Amex, JCB, Discover, Diners, UnionPay, Maestro — at 15 digits only Amex prefixes pass), and either grouped like a card (4-4-4-4 with a 1–3 digit tail for 17–19 digits, or the Amex 4-6-5, under one repeated separator of any kind) or preceded by a card word within 40 chars | `[REDACTED:card]` |
| `email` | RFC-lite address | `[REDACTED:email]` |
| `phone_fr` | French national or `+33` number | `[REDACTED:phone_fr]` |

Lines are NFKC-normalised first, so full-width digits are seen, and every
class of names, tokens and phone numbers is bounded in ASCII: a value glued
to a letter or a digit of another script, an accented letter, punctuation or
an emoji is still found (the NIR, card and belt read any digit NFKC leaves);
an IBAN followed by a word is found too (the prefix of its country's
length), and two IBANs in a row both. The
detection is **heuristic**: validators narrow the false positives (a
timestamp is not an IBAN; a digit run is a card only with a card's own
grouping or a card word next to it, since Luhn alone is a coin flip on
trace ids, epochs, decimals and lists of counters), they do not make it a
proof. Runs of blanks are folded to one space first, so tabs between a
card's groups do not hide it. A run of 12+ digits, bare or under
non-alphanumeric separators (mixed or not — a timestamp glued to other
numbers by separators is masked with them), that no class claimed is
masked as `<num>` before the contact classes run (a PAN in pairs is not a
phone number) and therefore in every sample. Outside that boundary, by
design: digits joined by letters (`4111x1111x1111x1111`) match no class,
a token glued to ASCII letters, digits or `_` (`keyAKIA…`, `x_AKIA…`)
reads as part of an identifier and goes unfound, as does a secret word
glued to a letter before it (`xpassword=…`); `x_password=…` and an email
glued so are still found. A password that is one plain word of 12 letters
or fewer is not reported (it reads as a status or a usage text), nor a
number under a compound `…_token` key (a count), nor a short all-digit
value (`passcode: 998877`) — it reads as a counter or an id, and the
digit-key vocabulary is a deliberate trade. This slice reports every
class at severity `high`; the
policy slice adds per-class `critical` with keyword context and the
circuit-breaker.

What leaves the node (`signals-<run>.json`, scratch; counts on stdout):

- **templates** — for every query except `leak_sweep`: the redacted line
  with UUIDs → `<uuid>`, hex runs → `<hex>`, quoted strings → `"..."`,
  numbers → `#`, whitespace collapsed; fingerprint = sha1 of that
  template (12 chars); `count`, `first_ts`/`last_ts`, one redacted
  `sample` (≤ 300 chars), the `container=`/`pod=` streams (≤ 6), and the
  same for the LIVE lines alone (`count_live`, `first_ts_live`,
  `sample_live`, `streams_live`, `query_live`) — what decide posts on.
  The list keeps the 200 templates with the most live lines;
  `templates_cut` says when it was cut (coverage is partial then),
  `templates_total` how many there were, and `cut_last_ts` the newest
  line of each cut template (a cut drops the templates with the fewest
  live lines, history-only ones first; each still moves a known
  incident's clock, so a cut concludes nothing against it — its count,
  reminders and quiet note wait for a tick where it is kept).
- **leak** — per class: `count`, `distinct` (hashes of the values, never
  the values), `sources` (query + container/pod + first/last, the 8
  busiest), `queries` (every query that returned one of its lines — the
  incident's sources), one `sample_masked` (`jo***@***` style, `nir:***12`;
  a secret shows only its length: `*** (9 chars)`).
- **coverage** — `full`, or `partial` when any query was truncated,
  failed, or had a gap (its previous cursor fell out of the max window).

## Prometheus probes

An instant query at `now`. The samples of the answer (a vector or a
scalar) are folded by `agg` (default `max`) and compared with `op` to
`threshold`. The result is **typed**:

- `healthy` — a value, not breaching;
- `breached` — a value breaching the threshold → an incident at the
  probe's severity;
- `no_data` — the query matched no series or only NaN: **not** healthy —
  the metric may not exist on this cluster (posted as a `medium`
  incident so a mis-wired preset is noticed);
- `error` — the API failed (a lane error, in the coverage note). A lane
  error quotes only the lane's own message or the transport's, an HTTP
  error as its code and the standard phrase (a reason phrase is the
  server's text): an answer that does not parse — a value that is text, an
  error text that may quote label values — is named by its type, its text
  withheld. The tick
  is still reported when another probe answered; while any probe errors,
  nothing is concluded about the lane's incidents (no "not observed any
  more") and its health is not refreshed. Every probe failing is the lane
  failing: decide refuses the tick only when every configured lane
  failed, naming the errors.

API warnings are counted in the result (`warnings`), their text withheld
(it may quote label values). Use `increase()`/`rate()` with an
explicit range for counters; the examples in `argus-config.md` assume
kube-state-metrics and ingress-nginx metrics, which are **not** present
on every cluster — validate them on yours.

## Sentry — three lists, facts the server holds

Off unless `config.sentry` is set (see `skills/argus-config.md`). Each tick
reads, through the org-scoped API (`/api/0/organizations/<org>/issues/`,
`project=<id>`, the configured `environment`, `statsPeriod=90d` for the
two lists (a list returns only issues with an event inside its window:
90 days, which Sentry clamps to its retention, reaches as far back as it
keeps events — an SDK's offline cache delivering a first event late still
makes a new issue) and `14d` for the by-id read (its
count is the issue's whole life anyway), `collapse=lifetime`,
`collapse=filtered` and an empty `groupStatsPeriod`,
100 per page — never `collapse=stats`: Sentry then drops an issue's seen
stats (`count`, `userCount`, `firstSeen`, `lastSeen`), the only way the
lane sees an event; an answer carrying none of them at all is a lane
error, never read as "no event"):

- **new** — `is:unresolved firstSeen:>=<cursor − overlap>`, sorted new.
  With an environment, Sentry filters on the processing time of the
  issue's first event THERE: a late event or a client clock cannot hide a
  new issue. An issue returned here that the lane does not track is NEW.
  The payload's `firstSeen` is never read — its meaning changes with the
  API path. (This holds on Sentry's Postgres search path, the one measured
  on self-hosted 24.11; an organization whose issue search runs on the
  group-attributes executor filters `firstSeen` project-wide instead: an
  issue already seen in another environment is then not NEW when it first
  reaches the watched one — only its regressions and escalations post.)
- **transitions** — `is:unresolved substatus:[regressed,escalating]`.
  Resolved and archived issues never appear in an unresolved list, so a
  regression arrives as an issue the lane may never have tracked; it is
  dated by its own activity (`set_regression` / `set_escalating`), at
  most `max_transition_checks` lookups a tick: the ones never checked
  first (one the cap leaves is a loss — partial coverage), then the
  re-checks — of dated ones, and of ones a check left undated (an issue
  unresolved through the API reads REGRESSED with a `set_unresolved`
  only) — least recently checked first (in state generations, never by
  a runner's clock: runners taking turns may disagree on the time): a
  deferred re-check keeps its
  date and waits its turn, so neither busy regressed issues nor
  undatable ones ever starve a new one. Every dated one is re-checked in turn, whether or not an event
  moved in the watched environment: substatus and activities are
  project-wide, so a second regression can come from another environment.
  A transition dated before the arming (minus the overlap) or before the
  catch-up floor (now − `max_catchup_hours`) is recorded as history: a
  lane off for days, or a lowered level floor admitting issues it never
  knew, posts no old regression as news. The floor spares a transition
  the armed lane watched in the list from within `max_catchup_hours` of
  it with its date still unknown (undated, or a re-check deferred or
  failed — a closure the lane never read included); the watch ends when
  the issue leaves a list read whole. Dated however late, it posts, and
  every transition alert says when it happened (`dated …`). One dated
  after the arming that the floor still makes history is named in the
  coverage note, after the lane's losses — carried until a note says it;
  past 100 names the oldest are counted, not named. The level floor does
  not apply to an issue the lane already knows.
- **tracked** — the alerted (or pending) issues by id — open, being
  reprocessed, archived (Sentry reopens an archived issue as ongoing) or
  resolved (unresolved by hand — the issue page's button, a bulk action —
  an issue is ongoing, in neither list) — their current status and last
  event. The open ones go first, then the resolved ones (best effort),
  the least recently read first in each (in state generations, like the
  re-checks) — an issue read in a list this
  tick counts as read, so a fresh one joins the back: over `max_tracked`
  they take turns, none left out for ever, and a coverage note of its own
  says the cut once the open ones take every read while others wait —
  whatever else the lane says (the resolved ones that wait are counted;
  `max_tracked` 0 turns the reads off). An issue asked
  and absent from the answer (deleted, merged) was read too, nothing
  more: it keeps its turn until retention forgets it.

An explicit `query` always: without one Sentry applies its default
(`is:unresolved issue.priority:[high, medium]`) and low-priority issues
vanish. The environment is checked first (an unknown one answers an
empty list — indistinguishable from "no issue"). Only the `cursor=` value
of the `Link` header is followed, never its URL; a short page with
`results="true"` is not the end, and a page with no `Link` header (a
proxy stripping it) is an error, never the end: the list is not read
whole and the cursor stays. The cursor's `since` is Sentry's own
`Date` (the runner's clock, read before the first request, when the
header is missing or runs ahead of it by more than `overlap_minutes` —
the walk says `clock: local`). Every stamp Sentry writes is bounded so:
an issue's `lastSeen` or an activity's date later than the runner's
clock plus `overlap_minutes` is not taken, and the walk counts it among
its partial reasons — a server or a front whose clock runs ahead never
mutes a path in silence. `deadline_secs`
is a wall clock over each exchange: a server or proxy trickling bytes
into the headers, a chunk-size line or the body cannot outlast it — nor
can a name lookup (every node resolves a host once, in a worker thread
it stops waiting for at its timeout, and connects to the addresses it
checked) —, nor
can one address of several the host resolves to hanging on connect (the
walk stops; it does not move on to the next address). The cursor
advances whenever the new-issue list was read whole (an activity lookup
failing holds nothing). A cursor older than `max_catchup_hours` opens at that
floor and the gap is declared — only a cursor itself below the floor:
the overlap below it was read already (`overlap_minutes` must stay below
`max_catchup_hours` × 60).

**What posts, in this order.** A bootstrap (no cursor, or a changed lane
identity) posts nothing and records every issue read as backlog — a
transition it read but could not date (the check cap) is dated at the
arming, history like the others. It arms once the new-issue list was
read whole (an activity lookup failing does not hold it); until then
nothing posts from the lane, a coverage note says `sentry: NOT ARMED`
and its health is not stamped (a silent-source note follows). The
identity is stamped on the state from the lane's first tick, so a switch
during a bootstrap that never armed drops what it recorded too.
Then:
a transition dated after the arming (minus the overlap) and newer than
the recorded one posts `REGRESSED IN SENTRY` / `ESCALATING IN SENTRY` —
tracked or not, backlog or not, with or without a new event this tick
(the substatus is project-wide, the counts environment-scoped); an
untracked issue of the new list posts NEW; an issue whose last word in
the channel was its closing note posts `OPEN AGAIN IN SENTRY` the first
tick it is read open — by id, or in an unresolved list —, event or not
(Sentry reopens an issue archived for a while as ongoing; an operator
unresolving a resolved one puts it in the transition list through the
API's single-issue call, undated, and leaves it ongoing — read by id —
from the issue page or in bulk); an alerted
issue whose last event moved is a sighting — `ESCALATED` when its
level-mapped severity rose above the one the channel last heard, `STILL
OPEN` once per `renotify_hours` (none of them while an alert of it is
pending); an alerted issue Sentry reports closed gets one note
naming the status (`RESOLVED IN SENTRY`, `ARCHIVED IN SENTRY`, `DELETED
OR MERGED IN SENTRY` — after any pending alert of it went out) and, while
it stays closed, its events are no news (Sentry keeps ingesting an
archived issue's: no reminder, no escalation, the note is not owed
again); one idle for `quiet_after_hours` (read by id this tick) one
`NOT OBSERVED ANY MORE` — that note is the episode's last word: a closing
note or a reopening by hand after it is not announced. Any posted alert restarts the idle clock. An
alert the per-run cap cuts stays PENDING and is re-emitted every tick
until posted (a new issue, a dated transition or an escalation does not
recur by itself — an escalation stays pending at the severity it
reached, dropped if a lowered `max_severity` brings it back to what the
channel already heard), ahead of the tick's fresh alerts, oldest first —
and the kinds holding one start their rank's turns; a cut closing note is re-emitted
from the recorded status, and a posted transition owes its closing note
again. The same holds for a Sentry leak class, a new log template and a
log leak class, while their lane is on (off, the pending record waits,
and retention may forget it, like a pending Sentry issue's). The Sentry
lane and the log templates — data anyone can mint incidents in (a
public DSN, log lines carrying user input) — post at most
`--var max_alerts_per_lane` (5) of their alerts one by one per tick, the
most severe first (pending ones first inside a rank); the others are
NAMED in notes of their kind (a state at one severity), which the
per-run cap never cuts, each within the message budget
(`max_message_chars` less 1000, 3000 at most; `decide` refuses a budget
under 1500) — and past `max_message_chars` in parts, each naming its own
members, never a name cut. A fact — a new issue or template, a transition, a
reopening, an escalation: what the tick read, bounded by its caps, and
that will not recur by itself — is named in full, in as many notes as its
names need; what is derived again from the state every tick — a
reminder, an idle note, a closing note — takes one note of its kind a
tick, the members it has no room for counted and named by the next
ticks' notes, the longest owed first (a steady supply of fresher ones
never holds one back). The alert log takes one line a message (a note's, the
names it said). Each member is then followed like any issue, its
follow-ups folding the same way. A flood neither drowns the channel nor
holds the per-run cap — against another lane, or against its own lane's
other alerts, named the tick they come. Anything
else read is tracked silently: a backlog issue
never posts on mere recurrence, and an issue merely re-read for
`forget_after_days` leaves the tracked set — not while it is still in
the regressed/escalating list (forgotten, its transition would be
re-dated and posted again).

**What a message shows.** The scrubbed title, the level, the events
(the lists count the last 90 days, or the retention if shorter; an issue
read only by id shows its count over its whole life, in every environment —
the detail says which), the users (not next to a whole-life count: their
scope differs), the culprit, and ONE clickable link built
from the configured base URL, org and the digit id — never the API's
`permalink`, rendered only when it is exactly that shape. Severity comes
from the level (`severity` map), capped by `max_severity` — a lowered cap
applies to the severities the lane already recorded too. A severity never
goes down on its own otherwise; an escalation is measured against the
severity the channel last heard, so one reached while the issue was
closed is said when it is read open again.

**Redaction.** Titles, culprits and metadata go to the scratch handoff
only; `leak_scan` scrubs every field (bounded first, cut to display size
after the scrub), and counts a class found in an issue's text once per
sighting of that issue — a `sentry_leak:<class>` incident with a masked
sample, never a value. The classes are written for text an attacker
controls — no repeated group with overlapping alternatives, an email
local part matched from where its run starts, the NFKC fold's output
cut too (a character can fold to eighteen) — so a crafted field or log
line cannot stall the scan. No event body is read in this slice: the user
block, the request, frame locals and breadcrumbs stay in Sentry — the
leak classes cover issue text only.

**Limits, said.** Level and substatus are project-wide Sentry facts (an
event from another environment moves them); the "last event" of a
sighting is an event time (tolerance = `overlap_minutes`); a public DSN
lets anyone create issues — a flood pushes the lists into their caps
(partial coverage, named) and its issues into one note of a kind a tick
(inside one rank each alert kind — a Sentry issue, a Sentry leak, a log
template, a log leak, a probe — takes its turn under the cap, so a
flood never holds it against another). Issue text anyone can write never pings nor links: every value
a message quotes — title, culprit, any lane's field, a sample — renders
as inline code, where Mattermost parses neither mentions nor links —
flattened to one line first, U+2424 included (Mattermost's markdown
reads that symbol as a line break, which would end the span), control
characters dropped (Mattermost cannot store a NUL), an empty value
rendered as nothing (an empty span would pair with the next value's
backtick). Escaping a value alone is not enough: the autolinker takes a
host after a hyphen, a word character or a parenthesis, whatever
precedes it. The label's own words render as written, and none of their
markdown can wrap a value: emphasis, code, link and LaTeX characters are
escaped, `&`, `<`, `>` and `|` are entities (the server rewrites `<url|text>`
first), `$` becomes its full-width form (inline LaTeX ignores a
backslash), every dot is escaped (the autolinker reads a host after any
letter, a `-` or a `.`, and `www.` through a zero-width space; a push
notification shows the backslash) and a zero-width space goes before a
colon after a letter or digit unless a space follows — every scheme's
colon, a server's custom URL schemes included, and an emoji code's own
(its name could be a scheme: a code before a space still renders, one
glued to what follows is text). A label is one line (a blank line would
open a block: a table splits a value's span at `|`), and a value that
renders as nothing joins the words around it. Parentheses and plain
colons stay as written. A value is never scanned for placeholders,
and a message over `max_message_chars` is cut on a line boundary (a
line ending at the limit is kept).

## Health probes

`GET` on each configured URL, `ok` when the status matches
`expect_status`; latency in ms; the body is never stored. Redirects are
followed hop by hop through the address guard (public hosts only, unless
`allow_private_sources`). A failing probe is a `critical` incident by
default: it is the one signal that needs no observability stack. Its error
is the HTTP status, the transport's message or the exception's type — a
malformed status line is the server's text, withheld.

## Reading a tick

`iterion report --run-id <id>`:

- `decide.summary` — `alerts: N to post (M overflow), K observed, J open
  incident(s), coverage full|partial[, BOOTSTRAP][ -- lane errors: …]`.
- `notify` — `delivered`, `targets`, `consume`, `summary` (failures are
  spelled out per sink).
- `commit_state` — `committed` (pushed) and what was logged.

Messages carry a marker (`PRODUCTION ALERT` / `ESCALATED` / `STILL OPEN`
/ `NOT OBSERVED ANY MORE`, and for Sentry `REGRESSED IN SENTRY` /
`ESCALATING IN SENTRY` / `RESOLVED IN SENTRY` — wording from `labels`), the app/environment,
the incident title, the detail line, severity, first-seen date and the
occurrence count, and — for a log template — the redacted sample as a
quote; a Sentry alert its scrubbed title and link. Notes (`:warning:`)
announce an overflow, a silent source (its last error quoted), or a
partial-coverage tick (a Loki query truncated, gapped or failed, a
Prometheus probe failed, a cut template list, a Sentry walk partial or
failed) — each
COMPONENT of the partiality once, then again only after `renotify_hours`,
whatever the combination (a query's gap, a lane's error kind — queries or
probes failing in turn with the same error are one kind, the same error on
two lanes two —, a truncation, a cut), with its reasons quoted: the gaps first — they lose lines —
then the lane errors, the truncated queries, a cut template list. The
note holds 600 characters: what is new first, then what was said already,
then the other queries of an error kind; a component counts as said only
when it is in the text — what the budget left out is said the next tick.
A stamp dated in the future (a clock running ahead) counts as not said,
for the note, a reminder and a silent-source warning alike. So a query
dark for good, a lane flapping, or a flood that outruns `max_lines` until
the cursor falls out of the max window, says why once, instead of every
tick or never.
