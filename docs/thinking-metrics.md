# Thinking (reasoning) metrics

Iterion surfaces two per-node extended-thinking metrics for LLM nodes:

- **`thinking_ms`** — wall-clock time spent in thinking blocks (milliseconds).
- **`thinking_tokens`** — count of thinking tokens, shown with a `~` because
  it may be an approximation depending on the backend (see below; on claw it
  is the provider's exact billed count whenever the API reports one).

They appear on four surfaces:

- **Leveled logs** — a `🧠` block per step / per node. When the thinking
  **content** was captured, the full reasoning text is logged as a foldable
  `LogBlock` (`[node#iter/claw] thinking step N (~T tok, Dms):` or
  `[node#iter/claude-code] thinking ~T tok, Dms:` followed by the
  `│ `-indented body) — the studio's run-log view renders it collapsed with
  an expand/unfold toggle, like tool I/O and assistant text. When only the
  counts are known, the metrics-only line remains
  (`[node#iter/claw] step N thinking: ~T tok, Dms`).
- **`events.jsonl`** — `thinking_ms` / `thinking_tokens` keys on
  `llm_step_finished` events, and `_thinking_ms` / `_thinking_tokens` stamped
  on the node output of `node_finished`. The thinking **text** is log-only
  by design: events.jsonl stays bounded to small payloads (big bodies live
  in sidecar blobs), and run.log is the surface that renders it.
- **Studio** — the run-view `NodeDetailPanel` header (`🧠 ~T tok · Ds`).
- **`iterion report`** — `Thinking Tokens` / `Thinking Time` rows in the
  metrics table.

## How tokens are counted

On the **claw** path, the preferred source is the provider's **exact billed
count**: Anthropic's `usage.output_tokens_details.thinking_tokens` (raw
internal reasoning, independent of `thinking.display`) and OpenAI's
`output_tokens_details.reasoning_tokens` both flow through claw-code-go's
`UsageDelta` into `Usage.ReasoningTokens`. This is authoritative — it counts
the model's full internal reasoning, which with summarized display is
systematically larger than the visible summary.

When the provider omits the breakdown (and always on the **claude_code**
path, whose stream-json usage carries no details), iterion falls back to
re-encoding the visible thinking text with a real BPE tokenizer
(`o200k_base`, vendored and offline) in
[pkg/backend/thinktokens](../pkg/backend/thinktokens/thinktokens.go).
`o200k_base` is OpenAI's encoding, not Anthropic's (whose tokenizer is not
public), so that figure is a comparable-across-backends estimate — and with
summarized display it counts the *summary*, not the underlying reasoning.
It falls back to a chars/4 heuristic if the codec fails to load.

## How time is measured

- **claw** (in-process): **exact**. The streaming aggregator
  ([generation.go](../pkg/backend/model/generation.go)) measures each thinking
  block from its `content_block_start` to `content_block_stop`. This relies on
  claw-code-go surfacing `thinking_delta` / `signature_delta` in its SSE parser.
- **claude_code** (subprocess): **best-effort**. The Claude Code SDK delivers
  assembled `ThinkingBlock`s (not deltas), so there is no intra-block timing.
  Iterion attributes the wall-clock gap since the previous stream item to a
  thinking-bearing assistant message — a proxy, not an exact measurement.

## Model-dependent redaction — the `thinking.display` parameter

Whether the thinking **content** is visible is governed by the Anthropic
API's `thinking.display` parameter, not by iterion
([adaptive-thinking docs](https://platform.claude.com/docs/en/build-with-claude/adaptive-thinking)):

- `display: "summarized"` returns a **summary** of the reasoning (produced
  by a separate summarizer model — the raw chain-of-thought is never
  returned on Claude 4 models; that policy is anti-distillation/misuse).
  This is the request-time default on Opus 4.6 / Sonnet 4.6 and earlier.
- `display: "omitted"` returns thinking blocks with an **empty** `thinking`
  field and only the encrypted `signature` (full reasoning, decryptable
  only by the API for multi-turn continuity). This is the default on
  **Opus 4.8 / 4.7 and Sonnet 5** — a silent change from Opus 4.6. Billing
  is identical either way (full thinking tokens); omitting only reduces
  time-to-first-text-token.

Consequences per backend (verified against `claude` CLI 2.1.195 by
inspecting raw `stream-json` frames):

- **claw** — requests `display: "summarized"` on every adaptive request
  (claw ≥ a27d632), so Anthropic models via API key show summarized
  thinking even on Opus 4.8. Override with
  `CLAW_ANTHROPIC_THINKING_DISPLAY=omitted|off`. OpenAI reasoning
  summaries flow via the Responses API (`reasoning.summary=auto`).
- **claude_code** — in `--print` (headless/SDK) mode the CLI defaults
  thinking display to omitted on Opus 4.8+ while its own interactive UI
  requests the summary (that's why the VS Code extension shows thinking
  live). The CLI has an undocumented `--thinking-display
  summarized|omitted` flag (present in 2.1.195; absent from `--help`),
  which iterion passes as `summarized` by default — so opus thinking
  summaries fold in run.log like sonnet's. Override with
  `ITERION_CLAUDE_CODE_THINKING_DISPLAY=omitted` (latency) or `off`
  (don't pass the flag — required for claude CLIs that predate it and
  reject unknown options). When the flag can't be applied and blocks
  arrive signed-but-empty, iterion still logs
  `🧠 thinking: Nms (content withheld by provider)` with the timing
  metric (tokens stay 0 — nothing to re-encode; even the session
  transcript and `--include-partial-messages` deltas are empty then).
