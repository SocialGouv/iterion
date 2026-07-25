---
description: Blast radius of changing a symbol or file, from the codeindex index — callers, references and hidden change coupling.
argument-hint: "<symbol name or file path>"
---

# Impact of changing $ARGUMENTS

Determine what breaks if `$ARGUMENTS` changes, using the `codeindex` MCP tools.
The server is pinned to this workspace, so omit the `repo` argument.

If `$ARGUMENTS` is empty, ask what to analyze and stop.

**If it looks like a file path:**

1. `symbols_overview(file)` — what the file declares.
2. `find_references()` on each exported symbol — who depends on them.
3. `coupling()` — files that historically change together with this one.
4. `complexity(file)` — how risky the file itself is to touch.

**If it looks like a symbol:**

1. `find_symbol(namePath, includeBody: true)` — the declaration and its source.
   If several match, list the candidates and ask which one before continuing.
2. `find_references(name)` — the three tiers.
3. `callers(name)` — the precise call sites.
4. `coupling()` — what else tends to move with the defining file.

Report:

- **Definition** — where it lives, what it is.
- **Direct callers** — the line-precise `callSites`. These are the bindings the
  index actually resolved; treat them as the reliable tier.
- **Weaker references** — `referencingFiles` (identifier or doc mentions, and
  possibly homonyms). Label them as such; do not merge them into the tier above.
- **Hidden coupling** — high-`strength` pairs from `coupling()`: files that
  change together without any import edge between them.
- **Verdict** — is this a safe local change or a wide one, and what to verify by
  hand. Static analysis does not see reflection, dynamic dispatch or string-keyed
  lookups; say where that limit applies here rather than implying full coverage.
