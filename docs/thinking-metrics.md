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

## Known limitation — claude_code thinking blocks

The `claude` CLI does not always emit `thinking` content blocks in its
`stream-json` output (observed: `claude-opus-4-8` at `reasoning_effort: high`
returned a final answer with usage but **no thinking block**, so iterion
recorded zero thinking). When no `ThinkingBlock` arrives, claude_code thinking
metrics stay at zero even though the model did reason internally.

This is CLI/stream-shape behaviour, independent of iterion's extraction code
(which populates the moment a `ThinkingBlock` is present — verified on the claw
path, where iterion parses the raw SSE). A guaranteed live end-to-end check of
the **claw** path needs Anthropic API access (`ANTHROPIC_API_KEY`); revisit the
claude_code emission behaviour and a live claw validation when such a key is
available.
