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
- `from = previous covered_to − overlap_seconds` (default 60 s), bounded
  below by `to − max_window_minutes` (default 60). A first tick uses
  `bootstrap_window_minutes` (default 10) instead.
- The walk is `direction=forward`, `page_size` lines per page (default
  1000), the next page starting at the last timestamp + 1 ns, until the
  window is exhausted or `max_lines` (default 5000) is reached. Reaching
  the cap **truncates** the window: the cursor stops at the last line
  fetched, the tick reports `truncated: true` and the scan reports
  `coverage: partial`. Nothing is skipped — the next tick resumes from
  the cursor — but "no finding" proves nothing for a partial tick.
- Overlap and page boundaries can hand the same line twice; the scan
  deduplicates by `(timestamp, line)`.
- A query that failed keeps its previous cursor (the window is retried
  next tick) and is listed in `errors`; a run where EVERY query failed
  hard-fails; a 401/403 hard-fails immediately (a credential problem is
  actionable now).

The persisted cursor is `cursors.loki.<query>.covered_to_ns` in
`state.json` — the upper bound actually covered, never `now`.

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
| `card` | 15–19 digits, **Luhn-validated**, and either grouped like a card (4-4-4-4 with a 1–3 digit tail for 17–19 digits, or the Amex 4-6-5, under spaces, dots or hyphens) or preceded by a card word within 40 chars | `[REDACTED:card]` |
| `email` | RFC-lite address | `[REDACTED:email]` |
| `phone_fr` | French national or `+33` number | `[REDACTED:phone_fr]` |

Lines are NFKC-normalised first, so full-width digits are seen. The
detection is **heuristic**: validators narrow the false positives (a
timestamp is not an IBAN; a digit run is a card only with a card's own
grouping or a card word next to it, since Luhn alone is a coin flip on
trace ids, epochs, decimals and lists of counters), they do not make it a
proof. Runs of blanks are folded to one space first, so tabs between a
card's groups do not hide it. A bare run of 12+ digits that is not a card is
still masked as `<num>` in every sample. This slice reports every class at severity `high`; the
policy slice adds per-class `critical` with keyword context and the
circuit-breaker.

What leaves the node (`signals.json`, scratch; counts on stdout):

- **templates** — for every query except `leak_sweep`: the redacted line
  with UUIDs → `<uuid>`, hex runs → `<hex>`, quoted strings → `"..."`,
  numbers → `#`, whitespace collapsed; fingerprint = sha1 of that
  template (12 chars); `count`, `first_ts`/`last_ts`, one redacted
  `sample` (≤ 300 chars), the `container=`/`pod=` streams (≤ 6).
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
partial-coverage tick (once per change).
