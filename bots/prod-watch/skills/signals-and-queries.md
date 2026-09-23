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
  `to − max_window_minutes` (default 60). A first tick uses
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
  Nothing is skipped — the next tick reopens at the frontier — but "no
  finding" proves nothing for a partial tick. A group of lines sharing one
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
  walk passes it: a line below it is history, not news, whichever tick
  reads it and whichever query returned it (a line the sweep and a
  template query both return is one record; a template counts its live
  lines apart, ranks by them, and renders their own first-seen, sample and
  streams — never the order of the queries decides); a lane that did not
  observe everything this tick — a failed query, a truncated walk, a
  declared gap, an empty window — concludes nothing about incidents it
  did not see (no "not observed any more"): template incidents are judged
  by the template queries and a full template list (a sweep running
  behind yields no template and blocks nothing), leak incidents by every
  query; an incident unseen for `forget_after_days` is forgotten whatever
  the lane observed (retention, not a conclusion); the run goes on with
  its other
  lanes (decide refuses a tick only when EVERY configured lane failed) and
  the lane's health is not refreshed while a query fails, so a query dark
  for good surfaces as a silent source after `source_stale_hours`, with
  its error; a 401/403 hard-fails immediately (a credential problem is
  actionable now).

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
| `bearer` | `Bearer <token>` | `[REDACTED:bearer]` |
| `cloud_or_forge_token` | AWS `AKIA…`, GitLab `glpat-`/`glrt-`, Grafana `glsa_`, Sentry `sntry…`, Anthropic `sk-ant-`, OpenAI `sk-…`, Slack `xox…`, GitHub `ghp_`/`github_pat_` | `[REDACTED:cloud_or_forge_token]` |
| `secret_kv` | `password=…`, `token: …`, `api_key=…` (the value only) | key kept, value replaced |
| `nir` | 13 digits + 2-digit key, **key validated** (Corsica 2A/2B handled) | `[REDACTED:nir]` |
| `iban` | country code + check digits + BBAN, **mod-97 validated** | `[REDACTED:iban]` |
| `card` | 15–19 digits, **Luhn-validated**, the issuer prefix a card network's (Visa, Mastercard, Amex, JCB, Discover, Diners, UnionPay, Maestro), and either grouped like a card (4-4-4-4 with a 1–3 digit tail for 17–19 digits, or the Amex 4-6-5, under one repeated separator of any kind) or preceded by a card word within 40 chars | `[REDACTED:card]` |
| `email` | RFC-lite address | `[REDACTED:email]` |
| `phone_fr` | French national or `+33` number | `[REDACTED:phone_fr]` |

Lines are NFKC-normalised first, so full-width digits are seen. The
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
design: digits joined by letters (`4111x1111x1111x1111`) match no class. This slice reports every class at severity `high`; the
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
  `templates_cut` says when it was cut (coverage is partial then).
- **leak** — per class: `count`, `distinct` (hashes of the values, never
  the values), `sources` (query + container/pod + first/last), one
  `sample_masked` (`jo***@***` style, or `nir:***12`).
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
- `error` — the API failed (counted as a lane error; the lane's incidents
  keep their clocks).

API warnings ride along in the result. Use `increase()`/`rate()` with an
explicit range for counters; the examples in `argus-config.md` assume
kube-state-metrics and ingress-nginx metrics, which are **not** present
on every cluster — validate them on yours.

## Health probes

`GET` on each configured URL, `ok` when the status matches
`expect_status`; latency in ms; the body is never stored. Redirects are
followed hop by hop through the address guard (public hosts only, unless
`allow_private_sources`). A failing probe is a `critical` incident by
default: it is the one signal that needs no observability stack.

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
quote. Notes (`:warning:`) announce an overflow, a silent source, or a
partial-coverage tick — once per change of coverage or of the lane
errors' kind, with the errors quoted, so a query dark for good says why.
