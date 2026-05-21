# Prompt-engineering cursors

Cursors are **framing dials, not gates.** They adjust the tone and
emphasis of an LLM node's system prompt — ambition, depth, rigor,
escalation threshold — without forking the underlying `prompt:`
block. Goodhart-resistance still lives in the judge architecture and
the scanner/coverage gates documented in
[workflow_authoring_pitfalls.md](workflow_authoring_pitfalls.md);
cursors are calibration, not enforcement.

Read that warning twice. The most common cursor misuse is "lower
`rigor` so the failing judge passes." That is a façade. Fix the issue
or adjust the gate.

## What a cursor looks like

A cursor declares a name plus either an **enum** of named values or a
**numeric band map** over the unit interval `[0, 1]`. Each value or
band carries a prompt fragment that gets appended to the agent's
system prompt under a `## Calibration` section when the cursor is
activated.

```
cursor ambition:
  description: "How aggressively to expand scope beyond the stated request."
  values:
    cautious: "Stick strictly to the stated request; do not propose extensions."
    balanced: "Address the request; surface one obvious adjacent improvement."
    ambitious: "Proactively identify 2-3 adjacent improvements; rank by leverage."

cursor depth:
  description: "Investigation thoroughness."
  bands:
    "0.0..0.33": "Skim the surface; cite primary sources only."
    "0.34..0.66": "Examine main code paths and one layer of dependencies."
    "0.67..1.0":  "Trace all call sites and edge cases."
```

A cursor declares **either** `values:` (enum) **or** `bands:`
(numeric) — not both. The IR compiler rejects the both-form with
`C085`.

## Activating cursors on a node

```
agent reviewer:
  model: "anthropic/claude-sonnet-4-6"
  system: review_system
  user: review_user
  cursors:
    enabled: true
    ambition: ambitious      # enum lookup
    depth: 0.7               # numeric → matches the 0.67..1.0 band
    rigor: ${ITERION_RIGOR}  # env-substitution allowed
```

When `enabled: false` (or the block is absent), no `## Calibration`
section is emitted — cache-stable, zero cost.

The runtime resolves each setting like so:

1. Expand `${VAR}` against the process env (same rules as
   `reasoning_effort`).
2. If the value parses as a float, take the **numeric** path.
   Otherwise take the **enum** path.
3. **Numeric path**: clamp to `[0, 1]`. If the cursor has `bands:`,
   return the prompt of the first band whose `[lo, hi]` contains the
   value. If it has `values:` only, snap to the enum position by
   `floor(v * len)`.
4. **Enum path**: look up the value by name against the cursor's
   `values:` list. Misses fail at compile time (`C084`).

Resolved fragments are sorted alphabetically by cursor name and
appended under a `## Calibration` section as
`**<CursorName>:** <fragment>` lines. Sorting is deterministic so
the resulting prompt is prompt-cache-friendly across runs.

## Diagnostics

| Code | Severity | Meaning |
|------|----------|---------|
| `C083` | warning | Agent/judge references a cursor name that is not declared at workflow scope. |
| `C084` | error | Cursor value is invalid: not in the enum, outside `[0, 1]`, or no matching band. Skipped for `${VAR}` values (runtime handles substitution). |
| `C085` | error | Cursor declaration is malformed: neither `values:` nor `bands:`, both forms set, bands overlap, or a range falls outside `[0, 1]`. |
| `C086` | error | Duplicate cursor name within a single workflow source. |

## Risks and guardrails

### Goodhart on numerics

Operators will treat `depth: 0.9` as "more = better" and ratchet.
Resist this. Band fragments must stay **qualitative**: describe what
the model should *do* (`"Trace all call sites"`), not what it should
*output* (`"Produce at least 5 references"`). Quantitative outputs
belong to deterministic scanners, not to framing prompts.

### Prompt bloat

Four enabled cursors add roughly 4 × 60 tokens to every node call.
The soft cap is **3 cursors per node**. Beyond that, the calibration
section starts to dominate the base prompt — either consolidate
cursors or accept the cost intentionally.

### Cache-toggling

Flipping `enabled` or changing any setting value invalidates the
prompt-cache for that node. The runtime sorts cursor fragments
alphabetically so identical activations across runs share their
cache entry, but cross-run drift of values still costs cache hits.

### False precision in hybrid mode

When a numeric cursor (`bands:`) is invoked with values like `0.51`,
the difference between `0.51` and `0.49` is a band boundary, not a
gradient. Bands are coarse zones. For workflows reviewed by humans,
prefer enum cursor names (`depth: deep`) over numeric values — they
travel better through code review and diff.

### Cross-node non-transitivity

Cursors on a judge do **not** propagate to the agent it judges.
Each node sees only the cursors set on its own declaration. A
"demanding reviewer" judge does not by itself make the upstream
implementer agent more demanding — you'd activate the matching cursor
on the implementer too.

### The façade temptation

Setting `rigor: loose` to make a stuck judge pass is a misuse. The
judge is signaling something. Investigate, fix the issue, or adjust
the gate. Calibration is a framing dial; it is not authorization to
ignore validation.

## Reference catalogue

Four reference cursors live at
[examples/cursors/cursors.iter](../examples/cursors/cursors.iter):

- **`ambition`** — how aggressively to expand scope beyond the stated request
- **`depth`** — investigation thoroughness, 0.0 (skim) → 1.0 (exhaustive)
- **`rigor`** — verification standard, 0.0 (accept) → 1.0 (cite every claim)
- **`autonomy`** — escalation threshold (`gated` → `autonomous`)

These are copy-paste-ready snippets, not a runnable workflow.
[examples/cursors/sample.iter](../examples/cursors/sample.iter) is
a minimal validatable workflow that exercises two of them.

## Authoring guidance

- Lead each fragment with an imperative verb (`Trace`, `Surface`,
  `Stick`). Avoid hedge words and "consider X" phrasing — the LLM
  reads "consider" as "ignore."
- Cursor names are lowercase, snake_case if multi-word. They become
  capitalized labels in the `## Calibration` section.
- Numeric cursors use string-keyed ranges (`"0.0..0.33"`). The lo
  and hi are inclusive on both ends; bands cannot overlap.
- Cursor declarations are per-file. There is no shared catalogue
  imported across workflows. If you find yourself copying the same
  cursor into many `.bot` files, that is a sign the workflow should
  share a bundle, not that cursors should grow an import system.
