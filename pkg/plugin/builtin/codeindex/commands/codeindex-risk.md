---
description: Where the risk concentrates in this codebase — churn hotspots, complexity, hidden coupling and dead code.
argument-hint: "[git ref to measure churn since, e.g. v1.2.0]"
---

# Risk report

Find where this codebase is most likely to break, using the `codeindex` MCP
tools. The server is pinned to this workspace, so omit the `repo` argument.

If `$ARGUMENTS` names a git ref, pass it as `since` to every churn-based tool so
the report covers that window rather than all history.

1. `hotspots(since)` — churn × size. Where the work concentrates.
2. `complexity(risk: true)` — complexity × churn. Complicated code that also
   keeps changing.
3. `coupling(since)` — files that change together. High `strength` with no
   import edge means an undocumented dependency.
4. `dead_code()` — the `unreferenced` and `uncalled` tiers.
5. `check_rules(rules)` — if the repo has an architecture-rules config, validate
   it; otherwise at minimum run the `cycles` and `orphans` builtins.

Report, most actionable first:

- **Hotspots** — file, why it ranks, what it owns.
- **Risky complexity** — the overlap of complex and churning. This is where
  defects cluster; be concrete about which functions.
- **Hidden coupling** — pairs with high strength and no import relationship,
  with a guess at why, marked as a guess.
- **Structural violations** — cycles, orphans, forbidden edges.
- **Dead code candidates** — keep the two tiers separate and state plainly that
  neither is proof: reflection, dynamic dispatch, string-keyed lookup and
  external consumers are all invisible to static analysis. Recommend
  verification, not deletion.

Close with the three changes that would most reduce risk, and what each costs.
Rank by evidence you actually gathered — if the data does not support a ranking,
say so instead of inventing one.
