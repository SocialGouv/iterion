# Examples

Runnable `dsl: 2` workflows — each file validates: `iterion validate examples/<file>`.

The one deliberate exception is `cursors/cursors.bot`: a copy-paste snippet of cursor declarations with no workflow, so `iterion validate` reports it INVALID **by design** (its header documents the contract: copy the cursor blocks, not the `dsl:` line).
