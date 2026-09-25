---
name: signals-and-queries
description: prod-watch (Argus) signal lanes — the Loki window and cursor contract, what the redaction scan derives (templates, leak classes, coverage), how Prometheus probes are typed, the health probes, and how to read a tick's outputs and messages.
---

# Signals and queries

## Loki — the window contract

Each configured query is fetched over a **frozen window** `[from, to)`:

- `to = now − ingest_lag_seconds` (default 120 s): Loki ingestion is not
  instantaneous; a line that arrives after the cursor moved past its
  timestamp would be lost for good.
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

The only node that opens `loki_raw.jsonl`. For every line, in this order
(secrets first, then validated identifiers, then contact data):

| class | how it is recognised | what replaces it |
|---|---|---|
| `private_key` | PEM private-key block | `[REDACTED:private_key]` |
| `jwt` | three base64url segments starting `eyJ` | `[REDACTED:jwt]` |
| `bearer` | `Bearer <token>`; `Authorization: Basic|Token|ApiKey|Bot|SSWS <credentials>` (16 characters at least — a scheme word followed by a short word is prose: `authorization: token authentication failed`); `Basic <credentials>` elsewhere when it decodes to a printable `user:password` (unpadded and `latin-1` credentials included; `basic` followed by an ordinary word is prose) | `[REDACTED:bearer]` |
| `cloud_or_forge_token` | AWS `AKIA…`, GitLab `glpat-`/`glrt-`, Grafana `glsa_`, Sentry `sntry…`, Anthropic `sk-ant-`, OpenAI `sk-…`, Slack `xox…`, GitHub `ghp_`/`github_pat_`, Stripe `sk_live_`/`rk_live_` (and `_test_`), Google `AIza…`, Hugging Face `hf_`, npm `npm_`, SendGrid `SG.…` | `[REDACTED:cloud_or_forge_token]` |
| `secret_kv` | a secret word starting a name part — snake, kebab, dotted, quoted or bracketed — or a camelCase part (`dbPassword`, `DBPassword`, `dbPass`), with only the suffixes a secret name takes (`_KEY`, `_VALUE`, a number, an environment: `SECRET_KEY=…`, `PASSWORD_2_PROD=…`, `TOKEN_PROD=…`), then `=`, `:` or `=>` (`"api_key": "…"`, `[password] => …`); a quoted value runs to its closing quote (an escaped `\"` stays inside — a JSON password with a quote is still redacted; a quote never closed runs to the end of the line, whatever its length); a flag `--password …`, `--db-pass …`; SQL `IDENTIFIED BY '…'` / `WITH PASSWORD '…'`; a URL's credentials `scheme://user:…@` (the password from 4 characters — shorter DSN passwords are a documented bound). Not a secret: a logger's own mask or a placeholder (`[Redacted]`, `********`, `${DB_PASSWORD}`), a word of 12 letters or fewer (a status or a usage text: `expired`, `requires`), a path **shape** (two slashes, or a system prefix: `/run/secrets/db` — a single-slash token like `/+AbCdEf123456` is base64, and is redacted), a count under a `…_token` key (`"prompt_token": 1234`), an API page token (`NextToken=`, `page_token=`); `total_tokens=…`, `PASSWORD_FILE=…` are not secret keys | key kept, value replaced |
| `nir` | 13 digits + 2-digit key, **key validated** (Corsica 2A/2B handled) | `[REDACTED:nir]` |
| `iban` | country code + check digits + BBAN, **the length that country's IBAN has, mod-97 validated** | `[REDACTED:iban]` |
| `card` | 15–19 digits, **Luhn-validated**, the issuer prefix a card network's (Visa, Mastercard, Amex, JCB, Discover, Diners, UnionPay, Maestro), and either grouped like a card (4-4-4-4 with a 1–3 digit tail for 17–19 digits, or the Amex 4-6-5, under one repeated separator of any kind) or preceded by a card word within 40 chars | `[REDACTED:card]` |
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
number under a compound `…_token` key (a count). This slice reports every
class at severity `high`; the
policy slice adds per-class `critical` with keyword context and the
circuit-breaker.

What leaves the node (`signals.json`, scratch; counts on stdout):

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
/ `NOT OBSERVED ANY MORE` — wording from `labels`), the app/environment,
the incident title, the detail line, severity, first-seen date and the
occurrence count, and — for a log template — the redacted sample as a
quote. Notes (`:warning:`) announce an overflow, a silent source (its
last error quoted), or a partial-coverage tick (a Loki query truncated,
gapped or failed, a Prometheus probe failed, a cut template list) — each
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
