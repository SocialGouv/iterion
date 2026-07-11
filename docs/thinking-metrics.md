# Thinking (reasoning) metrics

Iterion surfaces two per-node extended-thinking metrics for LLM nodes:

- **`thinking_ms`** — wall-clock time spent in thinking blocks (milliseconds).
- **`thinking_tokens`** — count of thinking tokens, always shown with a `~`
  because it is an **approximation** (see below).

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

## Why tokens are approximate

The Anthropic Messages API bills thinking inside `output_tokens` with **no
separate breakdown**, so there is no exact thinking-token count to read. Iterion
re-encodes the thinking text with a real BPE tokenizer (`o200k_base`, vendored
and offline) in [pkg/backend/thinktokens](../pkg/backend/thinktokens/thinktokens.go).
`o200k_base` is OpenAI's encoding, not Anthropic's (whose tokenizer is not
public), so the figure is a comparable-across-backends estimate — never claimed
to be exact. It falls back to a chars/4 heuristic if the codec fails to load.

## How time is measured

- **claw** (in-process): **exact**. The streaming aggregator
  ([generation.go](../pkg/backend/model/generation.go)) measures each thinking
  block from its `content_block_start` to `content_block_stop`. This relies on
  claw-code-go surfacing `thinking_delta` / `signature_delta` in its SSE parser.
- **claude_code** (subprocess): **best-effort**. The Claude Code SDK delivers
  assembled `ThinkingBlock`s (not deltas), so there is no intra-block timing.
  Iterion attributes the wall-clock gap since the previous stream item to a
  thinking-bearing assistant message — a proxy, not an exact measurement.

## Model-dependent redaction — claude_code thinking content

Whether the thinking **content** is visible depends on the model, not on
iterion (verified against `claude` CLI 2.1.195 by inspecting the raw
`stream-json` frames):

- **claude-sonnet-4-6** — the CLI streams `ThinkingBlock`s with the full
  reasoning text; iterion folds it as the 🧠 LogBlock and counts tokens.
- **claude-opus-4-8** — the provider redacts thinking client-side: the CLI
  streams the block with **empty** text and only the encrypted `signature`
  (even the session transcript and `--include-partial-messages` deltas carry
  empty text with `estimated_tokens` only). There is no content to display
  anywhere. Iterion still detects the signed-but-empty block and logs
  `🧠 thinking: Nms (content withheld by provider)`, accumulating the timing
  metric; the token metric stays 0 (nothing to re-encode).

The claw path is unaffected by this: it parses the raw SSE, so whenever the
API returns thinking deltas (Anthropic models via API key, OpenAI reasoning
summaries via the Responses API) the content is captured.
