# Privacy Filter

Iterion ships two complementary built-in tools that detect and redact
personally identifiable information (PII) — entirely in Go, with no
external dependencies. They run as part of the iterion binary itself;
no model to download, no Python, no setup.

- **`privacy_filter`** — detect or redact 5 PII categories:
  `account_number`, `email`, `phone`, `url`, `secret`.
- **`privacy_unfilter`** — restore original values from placeholders for
  workflows that need a redact-then-restore stage (e.g. customer support
  drafting).

Both are regular tool registrations, so they work as `tool` nodes in
`.iter` workflows and as tools attached to Agent nodes. They obey the
recipe `tool_policy` allowlist like any other tool.

## Detection backend

The Go-native detector combines:

- **Regular expressions** for well-formed identifiers (RFC 5322 emails,
  E.164 phones, URLs, IBANs, credit cards).
- **Structural validation** for numbers (mod-97 for IBAN, Luhn for
  credit cards) — invalid candidates are rejected to avoid false
  positives.
- **Curated secret patterns** (~25 rules, inspired by the
  [gitleaks](https://github.com/gitleaks/gitleaks) ruleset): AWS keys,
  GitHub tokens, Slack tokens, Stripe keys, Google API keys, PEM/SSH
  private keys, JWTs, npm/pypi tokens, etc.
- **Shannon entropy** as a secondary filter on `bearer_token`,
  `password = "..."`, and generic high-entropy candidates — `password
  = "changeme"` (entropy ~2.5) is *not* flagged; `password = "Xj9$mK2#nQ8@vL5"`
  (entropy ~3.8) is.

Detection runs in microseconds, deterministic across machines and
versions. No cold start.

### Categories deferred to v2

`person`, `address`, and `date` are context-dependent and cannot be
detected reliably with regex alone. They are planned for v2 via an
optional Go ONNX sidecar (still no Python). For now, these categories
are not in the schema. Workflows that need person-name redaction
should either pre-process inputs or wait for the v2 release.

## Tool reference

### `privacy_filter`

**Input:**

| field | type | default | notes |
|---|---|---|---|
| `text` | string | required | text to scan |
| `mode` | `redact` \| `detect` | `redact` | redact replaces with placeholders; detect returns spans |
| `categories` | string[] | all 5 | scope detection to a subset |
| `min_score` | number | 0.5 | confidence floor (per rule) |
| `placeholder_format` | string | `[PII_{token}]` | `{token}` is 8-hex stable per (run, value, category); `{category}` substitutes the upper-case category name |

**Output (redact):** `redacted` text + `placeholders[]` (each with
`token`, `category`, `score`, `rule`) + `category_counts` +
`has_<category>` booleans for routing + `engine` + `elapsed_ms`.

**Output (detect):** `spans[]` (each with `category`, `score`, `start`,
`end`, `rule`, `value_hash` — *no raw value*) + `category_counts` +
`has_<category>` booleans.

The `rule` field identifies the matching rule (e.g. `aws_access_key`,
`github_pat`, `rfc5322`). Useful for tuning false positives.

### `privacy_unfilter`

**Input:** `text` (string) + `missing_policy` (`leave` \| `error` \|
`remove`).

**Output:** `text` (restored) + `substituted[]` + `missing[]`.

The vault holding the placeholder → value mapping lives at
`<store-dir>/runs/<run_id>/pii_vault.json` with mode `0600`. It is
created on the first redact call of the run and persisted across
resumes. The tool reads it via the run context — no `vault_path`
parameter needed.

## Persistence guarantees

Iterion specifically strips PII from the persisted event stream:

- `privacy_filter`'s **input** (raw PII) is replaced with a placeholder
  in `events.jsonl` and `artifacts/`. The detector still receives the
  real text — only the persisted side is sanitized.
- `privacy_unfilter`'s **output** (raw PII) is replaced symmetrically.

The vault file itself is the only place on disk where raw PII lives. It
is gitignored (under `.iterion/`) and `0600`. Audit it manually if
needed.

## Typical workflows

### 1. Anti-secret commit gate (flagship)

```iter
tool generate_diff:
  command: "git diff --cached"
  output: diff_text

tool scan_secrets:
  command: "privacy_filter"
  input: diff_text
  output: scan_result
  # mode: detect, categories: [secret], min_score: 0.8

router gate:
  mode: condition
  input: scan_result

judge review:
  model: "anthropic/claude-sonnet-4-6"
  input: diff_text
  output: review_verdict

workflow main:
  start -> generate_diff -> scan_secrets -> gate
  gate -> review when not has_secret
  gate -> fail_node when has_secret
  review -> done
```

A material safety net against agents committing API keys or tokens.
`has_secret` is exposed directly on the output for use in router
conditions — no intermediate node needed. The detector covers the same
ground as `gitleaks` (~25 patterns: AWS, GitHub, Slack, Stripe, JWT,
PEM, etc.) plus generic high-entropy detection.

### 2. Triage tickets without exposing PII to the LLM

```iter
schema ticket_in:
  raw: string
schema redacted:
  text: string
schema triage:
  category: string
  severity: string
  summary: string

tool sanitize:
  command: "privacy_filter"
  input: ticket_in
  output: redacted

agent classify:
  model: "anthropic/claude-sonnet-4-6"
  input: redacted
  output: triage
  prompt: triage_prompt

workflow main:
  start -> sanitize -> classify -> done
```

The LLM only ever sees `[PII_xxx]` tokens for emails and phone numbers.
The triage output (category, severity, summary) typically contains no
PII either, so the entire run trace is publishable / auditable as-is.

### 3. Web-fetch summarization with sanitization

```iter
tool fetch_doc:
  command: "web_fetch"
  input: url_in
  output: page_raw

tool sanitize:
  command: "privacy_filter"
  input: page_raw
  output: page_clean

agent summarize:
  model: "openai/gpt-5.4-mini"
  input: page_clean
  output: summary

workflow main:
  start -> fetch_doc -> sanitize -> summarize -> done
```

Useful for scheduled scraping workflows where the input domain is
unpredictable. Prevents accidental retention of contact details from
public pages in your `events.jsonl`.

### 4. Two-stage customer support (redact → draft → unredact)

```iter
schema email_in:
  body: string

tool redact_email:
  command: "privacy_filter"
  input: email_in
  output: clean_email

agent draft_reply:
  model: "anthropic/claude-sonnet-4-6"
  input: clean_email
  output: draft
  prompt: |
    Draft a reply. The text contains tokens like [PII_xxx] —
    PRESERVE THEM VERBATIM. Do not rewrite, translate, or remove them.

tool restore_pii:
  command: "privacy_unfilter"
  input: draft
  output: final_reply

human approve:
  mode: pause_until_answers
  input: final_reply
  output: approval

workflow main:
  start -> redact_email -> draft_reply -> restore_pii -> approve -> done
```

The agent is structurally prevented from seeing emails, phone numbers,
URLs, account numbers, or secrets. The vault re-injects them at the
end. Useful in regulated contexts where the LLM provider is not in
scope to process identifiable data.

The prompt explicitly instructs the model to preserve the placeholder
tokens. Validate empirically — modern LLMs typically respect this, but
the placeholder format is overridable if needed.

### 5. Anonymized dataset / example export

```iter
tool list_artifacts:
  command: "ls .iterion/runs/$RUN_ID/artifacts/*.json"
  output: file_list

router fan:
  mode: fan_out_all
  input: file_list

tool redact_artifact:
  command: "privacy_filter"
  output: clean_artifact

join collect:
  strategy: wait_all

workflow main:
  start -> list_artifacts -> fan
  fan -> redact_artifact -> collect -> done
```

Convert real-data run traces into shareable demo material. Combine with
`iterion report` to produce a public-safe markdown of a run.

### 6. Sanitization after a Human node

```iter
human collect_context:
  mode: pause_until_answers
  output: raw_context

tool sanitize:
  command: "privacy_filter"
  input: raw_context
  output: clean_context

agent investigate:
  input: clean_context
  output: report

workflow main:
  start -> collect_context -> sanitize -> investigate -> done
```

Belt-and-braces: the raw answer lives only in `interactions/<id>.json`
(file-system local) and never reaches the LLM context or downstream
artifacts.

### 7. Pure detect-mode audit / compliance scan

```iter
tool scan:
  command: "privacy_filter"
  input: doc_in
  output: scan_report
  # mode: detect

agent report:
  input: scan_report
  output: human_report
  prompt: |
    Produce a markdown report listing: spans per category, highest-risk
    documents, recommended actions. Use the `rule` field to identify
    which detection rule matched.

workflow main:
  start -> scan -> report -> done
```

No mutation of source data — produces an inventory. Combine with
`/schedule` for recurring compliance reporting.

## Cross-cutting design

Each typical workflow uses `privacy_filter` in one of three roles:

| Role | Position in graph | Mode | Categories |
|---|---|---|---|
| Output guard-rail | After Agent / before commit | `detect` | `secret` |
| Pre-LLM hygiene | Before each Agent | `redact` | `email, phone, account_number` |
| Audit / inventory | Over a corpus, read-only | `detect` | all 5 |

This is a single tool parameterized by its input — not three different
node types. The DSL stays minimal.

## Limitations

- **Categories are regex/heuristic-detectable in v1.** Person names,
  free-form addresses, and dates in prose are out of scope until the
  v2 ONNX sidecar lands.
- **English bias for entropy thresholds.** Secret detection works on
  any text since it matches structural patterns, but the entropy
  heuristic is calibrated against ASCII strings; non-ASCII text may
  see slightly different false-positive rates.
- **Not a compliance guarantee.** `privacy_filter` is one layer of a
  privacy-by-design strategy, not a substitute. Audit the vault and
  your workflow graph. Custom identifiers (employee IDs, internal
  ticket numbers, project codes) are not detected unless they match a
  built-in rule — add a regex pre-processor if needed.
- **Placeholder preservation by LLMs.** Instruct prompts explicitly to
  keep `[PII_xxx]` verbatim. Validate against your specific model.
- **Large inputs.** No hard limit, but very large texts (>10 MB) will
  be slow due to regex scanning. Pre-chunk if needed.
- **Adversarial inputs.** Go's `regexp` package uses RE2 — no
  catastrophic backtracking is possible. Inputs designed to wedge the
  detector cannot, by construction.

## Configuration

No environment variables needed in v1. The detector is built into the
binary. The vault path is derived from the run's store directory.

## See also

- [docs/workflow_authoring_pitfalls.md](workflow_authoring_pitfalls.md)
  — Goodhart's law in workflow design. PII filtering is complementary
  to anti-façade scanning: filter removes data, scanner removes lies.
- `pkg/backend/tool/privacy/` — implementation (when shipped).
- [gitleaks](https://github.com/gitleaks/gitleaks) — the project the
  secret-detection ruleset is inspired from.
