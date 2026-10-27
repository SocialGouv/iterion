# Trimmed fork of github.com/tiktoken-go/tokenizer

First-party fork of upstream **v0.8.1**, wired through a `replace` directive
in the root `go.mod`. Only the `o200k_base` encoding is kept: iterion's
single consumer is `pkg/backend/thinktokens` (`Get(O200kBase)` + `Count`),
and the figure it produces is display-only.

Deleted relative to upstream: the cl100k/p50k/r50k/gpt2 encodings and their
vocab tables (~200k LOC of frozen BPE ranks), `ForModel`, the `Model`
constants and the model-prefix table. `ErrModelNotSupported` went with them;
`ErrEncodingNotSupported` stays.

**Re-apply on resync:** this is NOT an upstream patch — it is a deletion.
Bumping upstream re-imports the deleted encodings; re-trim, and let the
closure metric (`task deps:closure`, ADR-138) flag any regression.

The vocab tables are frozen BPE ranks: never regenerate them; resync only to
take upstream engine fixes.
