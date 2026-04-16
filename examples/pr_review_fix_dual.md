# pr_review_fix_dual — Workflow Design & Lessons Learned

## What this workflow does

`pr_review_fix_dual.iter` reviews a pull request using two independent AI models (Claude + Codex), identifies production-breaking issues, fixes them automatically, and iterates until both models agree the code is safe. It was designed and refined through iterative live runs on a real 120-file PR adding security-sensitive features (cryptographic signing, disaster recovery, authentication flows).

The workflow is generic. Replace the build/lint command and review rules and it adapts to any codebase (Go, TypeScript, Python, Rust, etc.).

## Architecture

```
context_builder -> reviewer (claude) -> build_check -> verdict (claude)
     ^                                                      |
     |                        not approved -> claude_fixer --+
     |                                                      |
     |                                approved -> codex_reviewer -> codex_verdict
     |                                                                    |
     |                                                      approved -> done
     |                                                                    |
     |                        not approved -> codex_fixer ----------------+
     |                                                                    |
     +---------------------- build_check_post_fix <-----------------------+
```

11 nodes, 13 edges. Single fix_loop(20) shared across all fix paths. Each fixer inherits the session of the reviewer that found the blockers.

## Core principles

### Dual verdict: two models must agree

A single model review is unreliable. In testing, Claude alone approved a PR that had data races, missing error handling, and security bypasses. Codex found these issues but was too strict on others. The dual verdict requires **both** models to approve before the PR passes.

The key insight: **different models have different blind spots**. Claude tends to be permissive on security edge cases but thorough on code structure. Codex tends to be strict on security but generates false positives on patterns it doesn't fully understand. The dual verdict catches both failure modes.

### Each model fixes its own blockers

Early designs used a single fixer (Claude) for all blockers. When Codex found security issues, Claude's fixer would attempt to fix them but often missed the nuance — it hadn't read the code with the same perspective. The fix would appear correct but the same blocker would reappear in the next Codex review.

The solution: **two fixers, each inheriting the session of its reviewer**. When Claude's verdict rejects, `claude_fixer` inherits Claude's review session (full context of files read, issues found). When Codex's verdict rejects, `codex_fixer` inherits Codex's review session. Each fixer has the exact context of what's wrong and why.

This pattern reduced fix loop iterations from 5+ to typically 1-2 per model.

### Strict blocker classification with self-critique

The reviewer prompt has explicit, exhaustive rules for what constitutes a BLOCKER vs SUGGESTION. Without these rules, reviewers drift — classifying cosmetic issues as blockers (wasting fix iterations) or security bugs as suggestions (letting them through).

The classification is asymmetric by design:
- **BLOCKER** (err on the side of caution): data races, missing error handling, security bypasses, data loss, panics, unbounded resource growth
- **SUGGESTION** (never a blocker): style, naming, log levels, missing tests, deprecation

The cross-reviewer (Codex) additionally performs **self-critique** before classifying blockers. For each potential blocker, it asks: "Am I reading the code correctly?", "Is this reachable in production?", "Did I miss a guard nearby?" This was added after Codex generated 6 blockers in one run, half of which were false positives that wasted ~30 minutes of fix iterations.

### Verdict trusts the reviewer, doesn't second-guess

Early designs had the verdict judge re-evaluating blocker classifications. This caused two problems:
1. The verdict would reclassify real blockers as suggestions ("it's unlikely in practice")
2. The verdict would approve a PR where the reviewer found 3 blockers — silently overriding the review

The current design: the verdict **trusts** the reviewer's classification. It only reclassifies if it can demonstrate a factual error (e.g., the mutex the reviewer missed IS present at line N). The verdict's job is to confirm the review, detect stagnation, and produce the executive summary — not to second-guess blocker/suggestion classification.

### No silent approval on loop exhaustion

When the fix loop is exhausted with remaining blockers, the workflow **fails** instead of silently approving. A human must decide whether the remaining blockers are acceptable. This prevents the workflow from shipping code with known security issues just because the fix loop ran out of iterations.

The `verdict -> fail` and `codex_verdict -> fail` fallback edges ensure the workflow always terminates cleanly when loops are exhausted.

### Stagnation detection without auto-approval

The verdict compares blockers with the previous iteration. If the same blockers appear repeatedly, it notes this in the executive summary so the human operator can see the loop is stuck. But it does NOT auto-approve — the workflow fails and the human decides.

This is a deliberate departure from the `rust_to_go_port.iter` pattern, which auto-approved on stagnation. For code review, auto-approving stagnant blockers means shipping code with known issues. Better to fail and let the human evaluate.

### Deterministic build verification

Build/lint checks run as tool nodes (not LLM evaluations). This provides a deterministic gate: if the code doesn't compile or passes lint errors, the fixer broke something. This catches regressions that the LLM reviewer might miss.

The check runs both before the verdict (so the verdict has factual build data) and after each fix (so regressions are caught immediately).

### Fix summary feeds back to the reviewer

When the fixer encounters a false positive ("B2 is a false positive because the mutex IS present at line 45"), this is captured in the fix summary and passed back to the reviewer in the next iteration. This prevents the reviewer from re-flagging the same false positive.

## Loop design

A single `fix_loop(20)` counter is shared across all fix edges. Each fix iteration consumes approximately 2-4 loop units (fixer → check → reviewer → check → verdict, and potentially → codex_reviewer → codex_verdict → codex_fixer → check). The bound of 20 allows for 5-7 full fix cycles before exhaustion.

Edge evaluation order matters at verdict nodes:
1. `verdict -> codex_reviewer when approved` — primary approved, proceed to cross-review
2. `verdict -> claude_fixer when not approved` — primary rejected, fix
3. `verdict -> fail` — loop exhausted, fail

The third edge is a terminal fallback. The runtime skips edges whose loop counter is exhausted, so when `fix_loop` hits 20, the conditional edges are skipped and the fallback catches the flow.

## Session continuity

Session modes are critical for fix quality:

- **context_builder**: `session: fresh` — standalone, runs once
- **reviewer**: `session: fresh` — clean slate each iteration (receives previous verdicts and fix summaries as data)
- **claude_fixer**: `session: inherit` from reviewer — has the reviewer's full context (files read, issues found)
- **codex_fixer**: `session: inherit` from codex_reviewer — has the cross-reviewer's context
- **verdicts**: `session: fresh` — stateless judges

Corollary: fixers MUST use the same backend as their reviewer. `claude_fixer` uses `claude_code`, `codex_fixer` uses `codex`. Switching backends breaks session inheritance (different KV caches).

## Adapting to other use cases

### Different languages

Replace the `build_check` tool command with your project's build/lint:
- Go: `go build ./... && go vet ./...`
- TypeScript: `tsc --noEmit && eslint .`
- Python: `python -m py_compile *.py && ruff check .`
- Rust: `cargo check && cargo clippy`

Update `review_rules` with language-specific conventions.

### Single-model variant

Remove `codex_reviewer`, `codex_verdict`, and `codex_fixer`. Change `verdict -> codex_reviewer when approved` to `verdict -> done when approved`. This gives a simpler review+fix loop with one model.

### Adding more reviewers

Fan out from the primary verdict to multiple cross-reviewers (security, performance, architecture). Each produces blockers/suggestions. Add a synthesis judge that merges verdicts from all reviewers. Each specialized reviewer gets its own fixer.

### Stricter quality

Reduce `fix_loop(20)` to `fix_loop(10)`. Add parallel specialized review nodes (security audit, performance review). Lower the self-critique threshold in the cross-reviewer prompt.

## Empirical data

### Convergence pattern (from live testing)

| Iteration | Claude | Codex | Action |
|-----------|--------|-------|--------|
| 1 | approved (0 blockers) | rejected (3 blockers) | codex_fixer corrects |
| 2 | approved (0 blockers) | rejected (2 blockers) | codex_fixer corrects |
| 3 | approved (0 blockers) | rejected (2 new blockers) | codex_fixer corrects |
| 4 | approved (0 blockers) | approved (0 blockers) | done |

Typical convergence: 2-4 iterations. Claude tends to approve early; Codex catches deeper issues but converges after its own fixes.

### Token usage per iteration

| Node | Tokens (typical) | Duration |
|------|-------------------|----------|
| context_builder | ~9,000 | ~2 min |
| reviewer (Claude) | ~8,000 | ~4 min |
| build_check | — | ~15 sec |
| verdict (Claude) | ~1,000 | ~30 sec |
| codex_reviewer | ~170,000 | ~10 min |
| codex_verdict | ~40,000 | ~2 min |
| claude_fixer | ~6,000 | ~3 min |
| codex_fixer | ~50,000 | ~10 min |

Codex is significantly more verbose than Claude (~20x tokens per review). Budget accordingly.

### Key findings from live runs

1. **Claude is permissive, Codex is strict** — Claude approved a PR with data races and missing error handling. Codex found them but also produced false positives. The dual verdict catches both failure modes.

2. **Session inherit matters** — When Claude tried to fix Codex's blockers without Codex's context, the same issues reappeared. Giving each model its own fixer with session inherit resolved this.

3. **Self-critique reduces false positives** — Without self-critique, Codex generated 6 blockers per iteration (3 false positives). With self-critique, it dropped to 2-3 (mostly real issues).

4. **Stagnation without auto-approval** — Auto-approving stagnant blockers (the `rust_to_go_port` pattern) is dangerous for code review. Better to fail and let the human decide.

5. **Build checks catch regressions** — Fixers occasionally introduce compilation errors. The deterministic build check after each fix catches these immediately, preventing cascading failures in subsequent review iterations.

6. **Fix summaries prevent re-flagging** — When the fixer notes "B2 is a false positive because...", the reviewer sees this in the next iteration and doesn't re-flag the same issue.
