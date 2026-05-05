# Privacy Filter

Iterion ships two complementary built-in tools that detect and redact
personally identifiable information (PII) using the HuggingFace
[`openai/privacy-filter`](https://huggingface.co/openai/privacy-filter)
token-classification model. They run locally via a Python sidecar — no
data leaves the host.

- **`privacy_filter`** — detect or redact 8 PII categories: `account_number`,
  `address`, `email`, `person`, `phone`, `url`, `date`, `secret`.
- **`privacy_unfilter`** — restore original values from placeholders for
  workflows that need a redact-then-restore stage (e.g. customer support
  drafting).

Both are regular tool registrations, so they work as `tool` nodes in
.iter workflows and as tools attached to Agent nodes. They obey the
recipe `tool_policy` allowlist like any other tool.

## Setup

```bash
devbox run -- task privacy:install   # creates a venv with transformers + torch CPU
```

Override the interpreter via `IT_PRIVACY_BIN` if you have your own.

## Tool reference

### `privacy_filter`

**Input:**

| field | type | default | notes |
|---|---|---|---|
| `text` | string | required | text to scan |
| `mode` | `redact` \| `detect` | `redact` | redact replaces with placeholders; detect returns spans |
| `categories` | string[] | all 8 | scope detection to a subset |
| `min_score` | number | 0.5 | confidence floor |
| `placeholder_format` | string | `[PII_{token}]` | `{token}` is 8-hex stable per (run, value, category) |
| `aggregation` | `simple` \| `first` \| `max` \| `average` | `simple` | HF pipeline strategy |

**Output (redact):** `redacted` text + `placeholders[]` + `category_counts` +
`has_<category>` booleans for routing.

**Output (detect):** `spans[]` (no raw values — `value_hash` only) +
`category_counts` + `has_<category>` booleans.

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
  in `events.jsonl` and `artifacts/`. The model still receives the real
  text — only the persisted side is sanitized.
- `privacy_unfilter`'s **output** (raw PII) is replaced symmetrically.

Events carry `input_redacted: true` / `output_redacted: true` so the
RunView and report consumers know what they are looking at.

The vault file itself is the only place on disk where raw PII lives. It
is gitignored (under `.iterion/`) and 0600. Audit it manually if needed.

## Typical workflows

### 1. Triage tickets without exposing PII to the LLM

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

The LLM only ever sees `[PII_xxx]` tokens. The triage output (category,
severity, summary) typically contains no PII either, so the entire run
trace is publishable / auditable as-is.

### 2. Web-fetch summarization with sanitization

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

### 3. Anti-secret commit gate

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
conditions — no intermediate node needed.

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

The agent is structurally prevented from seeing names, emails, or phone
numbers. The vault re-injects them at the end. Useful in regulated
contexts (GDPR, HIPAA-adjacent) where the LLM provider is not in scope
to process identifiable data.

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
    documents, recommended actions.

workflow main:
  start -> scan -> report -> done
```

No mutation of source data — produces an inventory. Combine with
`/schedule` for recurring compliance reporting.

## Cross-cutting design

Each typical workflow uses `privacy_filter` in one of three roles:

| Role | Position in graph | Mode | Categories |
|---|---|---|---|
| Pre-LLM hygiene | Before each Agent | `redact` | `person, email, phone, address` |
| Output guard-rail | After Agent / before commit | `detect` | `secret` |
| Audit / inventory | Over a corpus, read-only | `detect` | all |

This is a single tool parameterized by its input — not three different
node types. The DSL stays minimal.

## Limitations

- Latency: ~10–30s cold start per Run (model load), then sub-second per
  request on CPU.
- English-first: detection quality drops on non-English text. Consider
  fine-tuning if a different language is dominant.
- Not a compliance guarantee: `privacy_filter` is one layer of a
  privacy-by-design strategy, not a substitute. Audit the vault and your
  workflow graph. Uncategorized identifiers (employee IDs, internal
  ticket numbers) are not detected.
- Placeholder preservation by LLMs: instruct prompts explicitly to keep
  `[PII_xxx]` verbatim. Validate against your specific model.
- 128K context cap: inputs larger than the model's context return
  `input_too_long`. Pre-chunk if needed.

## Configuration

| Env var | Default | Purpose |
|---|---|---|
| `IT_PRIVACY_BIN` | unset | Override Python interpreter |
| `IT_PRIVACY_VENV` | unset | Pick a specific venv |
| `IT_PRIVACY_REVISION` | baked default | Pin model revision |
| `XDG_CACHE_HOME` | `~/.cache` | Where the venv and embedded sidecar.py land |

## See also

- [docs/workflow_authoring_pitfalls.md](workflow_authoring_pitfalls.md)
  — Goodhart's law in workflow design. PII filtering is complementary
  to anti-façade scanning: filter removes data, scanner removes lies.
- `pkg/backend/tool/privacy/` — implementation (when shipped).
