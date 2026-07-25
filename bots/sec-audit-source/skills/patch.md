---
name: patch
description: |
  Generate candidate fixes for verified findings, then climb a
  five-rung verification ladder (build, reproduce, regress,
  re-attack, advisory style). Go and TS/JS only. Crypto findings
  hard-stop; humans patch those. The reviewer agent runs isolated
  on `{file, line, category, diff}` only.
attribution: |
  Adapted from Anthropic's `defending-code-reference-harness`
  (`/patch` reference implementation). C/C++/ASAN ladder replaced
  by Go (`go build` + `go test`) and TS/JS (`npm|pnpm build|test`
  or `tsc --noEmit`); the crypto hard-stop, reviewer-isolation
  contract, and per-category re-attack oracles are iterion-native.
---

# patch — verified candidate fixes (Go, TS/JS)

Turns a confirmed finding into a candidate diff and a verdict.
Output is inert text under `./.sec-audit/patches/`. **The
skill never applies a diff.** A human applies it after review.

## Languages — Go and TS/JS only

This skill targets:

- **Go.** Module-rooted projects with `go.mod`.
- **TypeScript / JavaScript.** Node projects with `package.json`
  (npm, pnpm, or yarn). Browser code with a build step is fine;
  pure-frontend assets without one fall back to the static review
  pass only.

C / C++ / Rust unsafe / FFI memory-safety is out of scope for V1.
The reference harness's ASAN-driven re-attack is replaced by Seki's
per-category re-attack oracles (cross-link `[[reattack-oracles]]`).

## Crypto hard-stop

If `finding_type == "crypto"` OR the touched file path matches a
crypto primitive (paths containing `crypto`, `cipher`, `kdf`,
`signature`, `verify`, RNG, key handling), Seki MUST
NOT auto-patch. It:

1. Produces the candidate diff for visibility.
2. Marks `risk_flag: touches_crypto_primitive`.
3. Downgrades verdict to `uncertain` regardless of ladder result.
4. Emits the patch as a kanban issue with `label:human-review`
   and `state: review`.

Full rationale and escalation path: `[[crypto-handling]]`.

## Inputs

- A confirmed candidate from `[[triage]]` (verdict = `confirm`,
  not `dismiss`, not `uncertain`).
- The workspace at `{{vars.workspace_dir}}`.
- The threat model `.sec-audit/context.md` if present
  (anchors variant hunts).
- `[[reattack-oracles]]` for per-category re-attack recipes.

## Phase 1 — Patch author

One subagent per finding, parallel. Subagent reads source under
`{{vars.workspace_dir}}` (read-only — it emits the diff as text;
the orchestrator writes the diff file).

### Author brief

```
You are conducting authorized security research. Write a candidate
fix for ONE confirmed vulnerability.

Read-only access to {workspace_dir}. You may NOT build, run,
install, edit on disk, or reach the network. Emit the fix as a
unified diff in your final response; do NOT apply it.

FINDING:
  id:           {id}
  file:         {file}
  line:         {line}
  finding_type: {finding_type}     # from [[finding-taxonomy]]
  severity:     {severity}
  title:        {title}
  description:  {description}
  recommendation: {recommendation or "(none provided)"}

PROCEDURE:

1. READ THE CODE. Open {file}:{line} and the surrounding function.

2. ROOT CAUSE FIRST. Trace backward from the cited sink to where
   the bad value or missing check originates. The fix usually
   belongs there, not at the line the scanner flagged. Name the
   root-cause file:line.

3. VARIANT HUNT. Grep for sibling call sites with the same
   pattern. Your fix should cover all of them, or your rationale
   should say why not.

4. MINIMAL DIFF. Smallest change that fixes the root cause. No
   refactoring, no drive-by cleanup, no reformatting.

5. ADVERSARIAL SELF-CHECK. Re-read your diff as an attacker. Name
   one input variation that reaches the same bad state without
   tripping your change. If you can name one, your fix is at the
   wrong layer — go back to step 2.

6. REGRESSION TEST. As part of the diff, add ONE test that fails
   BEFORE your change and passes AFTER. Recipes per finding_type:
   Go -> [[reproducer-go]]; TS/JS -> [[reproducer-ts]]. If no test
   directory exists, omit and say so in <test_note>.

7. TOOLCHAIN (Tier-2, ADR-017). If the repo ships a `devbox.json`, run any
   build / test / e2e you use to check your work THROUGH it —
   `devbox run -- <cmd>` (e.g. `devbox run -- go test ./...`,
   `devbox run -- npm test`, `devbox run -- npx playwright test`) — so you
   use the project's pinned toolchain, matching how the build / regress
   rungs verify. Bare command when there is no devbox.json.

OUTPUT — your final response MUST contain exactly these tags:

<patch_diff>
--- a/path/to/file
+++ b/path/to/file
@@ ... @@
 context
-removed
+added
</patch_diff>
<rationale>mechanical: file:line of root cause, what the change
enforces</rationale>
<variants_checked>file:function pairs you grepped, and whether
each needed the fix</variants_checked>
<bypass_considered>the input variation tried in step 5 and why it
no longer reaches the bad state</bypass_considered>
<test_note>where the regression test landed, or why none was
added</test_note>

If the finding is not fixable as described, emit:
<patch_diff>NONE</patch_diff>
<rationale>why no patch is appropriate</rationale>
```

Parse the five tagged blocks. Write the diff to
`./.sec-audit/patches/<id>/patch.diff`. Tolerate stray ```
fences and HTML-escaped angle brackets; unescape before writing.

## Phase 2 — Verification ladder

Climb in order. **Stop on the first failure**, record the failed
tier, and emit the diff with `verdict: ladder_failed`.

| Tier | Question | Oracle | Go command | TS/JS command |
|---|---|---|---|---|
| 0 build | Patched tree compiles? | exit code | `go build ./...` | `npm run build` or `pnpm build` or `tsc --noEmit` |
| 1 reproduce | Original finding gone? | re-run the original scanner rule on the patched file; it must NOT fire | matcher-specific (per `iterion:scanners`) | matcher-specific |
| 2 regress | Existing behavior intact? | exit code | `go test ./...` | `npm test` or `pnpm test` |
| 3 re-attack | Root cause gone, or just this input? | fresh adversarial pass across the vuln CLASS | `[[reattack-oracles]]` per category | `[[reattack-oracles]]` per category |
| 4 style | Would a maintainer accept it? | advisory LLM judge 0–10 | n/a — never gates | n/a — never gates |

### Tier 0 — build

Apply the diff to a scratch worktree (NOT `{{vars.workspace_dir}}`
itself). Run the build command. Capture stderr/stdout; the build
artifact and the exit code are the oracle. Failure → record
`t0_builds: false`, attach the compiler error, stop.

### Tier 1 — reproduce

Re-run the original scanner rule (`matcher` field from triage) on
the patched file. The scanner MUST no longer fire on the original
finding's line range. If it does, the fix didn't address what the
scanner saw — `t1_reproduce_stops: false`, stop.

Use the `iterion:scanners` data block from the matching
`lang-*.md` to find the exact `cmd`. For Go that's `semgrep
--config=p/golang ...` or `gosec`; for TS/JS that's `semgrep
--config=p/javascript,...`.

### Tier 2 — regress

Run `go test ./...` (Go) or the project's test script (TS/JS).
Capture exit code. Failure → `t2_tests_pass: false`, attach the
failing test names, stop.

If no test suite exists at all, this tier is skipped (mark
`t2_tests_pass: skipped`).

### Tier 3 — re-attack

A FRESH adversarial pass that re-scans the whole vulnerability
**class** in the affected files, not just the line that was patched.
Recipes per finding_type live in `[[reattack-oracles]]`. Examples:

- `injection` → re-run injection matchers AND scan for sibling
  taint paths into the same sink.
- `ssrf` → re-run SSRF matchers AND check that the validator covers
  every outbound call, not just the patched one.
- `authz / idor` → re-run missing-filter matchers across all
  handlers on the same router.
- `path-trav` → re-run + check `filepath.Clean` / `path.normalize`
  is applied at every join site.

Re-attack failing means the fix is at the wrong layer. Mark
`re_attack_clean: false`, stop, attach the new finding.

### Tier 4 — style (advisory, NEVER gates)

LLM judge reads the diff and assigns 0–10 for "would a maintainer
merge this as-is". Recorded in `t4_style_score`. Never blocks the
verdict. Useful for human review prioritization.

## Phase 3 — Reviewer isolation

After the ladder, spawn ONE reviewer subagent per diff with the
strict isolation contract (cross-link `[[reviewer-isolation]]`):
the reviewer sees **only** `{file, line, category, diff}` plus
read-only source. It does NOT see the finding's `description`,
`recommendation`, `exploit_hypothesis`, or the author's
`rationale`.

Why: target source can carry injected instructions that survive
into a scanner's `description` field. The patch author has to see
that prose (it has to, to know what to fix); the reviewer doesn't,
so injected text cannot pass its own gate.

### Reviewer brief

```
You are reviewing a candidate security patch as a maintainer would.
Read-only access to {workspace_dir}. May NOT build, run, or apply.

You have NOT seen the scanner's description or the author's
reasoning. Work only from location, category, and diff.

LOCATION: {file}:{line}
CATEGORY: {finding_type}
DIFF UNDER REVIEW:
<diff>
{diff_text — or "Read ./.sec-audit/patches/<id>/patch.diff"
if >50 lines}
</diff>

ANSWER FOUR QUESTIONS:

1. SCOPE. Does the diff touch only files/functions on the path
   between {file}:{line} and its callers? List out-of-scope hunks.

2. SUPPRESSION. Does the diff fix root cause, or suppress symptom
   (try/catch swallow, early-return on magic value, deleting the
   check that fired, lowering log level)?

3. NEW SURFACE. Does the diff add parsing, trust a new input
   field, weaken validation elsewhere, remove a security check?

4. STYLE. 0-10: would you merge this as-is?

End your response with EXACTLY:

  REVIEW: ACCEPT | REJECT
  STYLE_SCORE: <0-10>
  OUT_OF_SCOPE_HUNKS: <file:line list, or none>
  REASON: <2-4 sentences citing specific diff hunks and source
    lines>

ACCEPT requires: in-scope, root-cause fix, no new attack surface,
style >= 5. Otherwise REJECT.
```

## Phase 4 — Output

Per finding, write
`./.sec-audit/patches/<id>/patch_result.json`:

```json
{
  "id": "F-01-03",
  "finding_type": "ssrf",
  "file": "pkg/server/proxy.go",
  "line": 122,
  "severity": "high",
  "language": "go" | "ts" | "js",
  "ladder": {
    "t0_builds": true,
    "t1_reproduce_stops": true,
    "t2_tests_pass": true,
    "re_attack_clean": true,
    "t4_style_score": 7
  },
  "review": "ACCEPT" | "REJECT" | null,
  "rationale": "...",
  "variants_checked": "...",
  "bypass_considered": "...",
  "test_note": "...",
  "risk_flag": null | "touches_crypto_primitive",
  "verdict": "ladder_passed" | "ladder_failed" | "uncertain"
}
```

A patch is `ladder_passed` when build, reproduce, regress (or
skipped), and re-attack are all clean AND the reviewer ACCEPTED.
Style score is informational. Crypto findings ALWAYS emit
`verdict: uncertain` (cf. `[[crypto-handling]]`).

The diff lands at
`./.sec-audit/patches/<id>/patch.diff` for human review.

## Guard rails

- **Never applies diffs.** No `git apply`, no `patch`, no Edit
  against `{{vars.workspace_dir}}`. The skill writes ONLY under
  `./.sec-audit/patches/` and a scratch worktree the
  build/test tiers use.
- **Reviewer isolation is non-negotiable.** Passing the reviewer
  any of `description`, `recommendation`, `exploit_hypothesis`, or
  the author's `rationale` defeats the gate. See
  `[[reviewer-isolation]]`.
- **Crypto hard-stop.** See `[[crypto-handling]]`.
- **Always set `subagent_type`** when spawning the author and the
  reviewer; forks would leak finding prose into both.

## See also

- `[[crypto-handling]]` — hard-stop policy.
- `[[reviewer-isolation]]` — what the reviewer sees and why.
- `[[reattack-oracles]]` — per-category re-attack recipes.
- `[[reproducer-go]]` — Go regression test recipes per category.
- `[[reproducer-ts]]` — TS/JS regression test recipes per category.
- `[[finding-taxonomy]]` — the twelve categories.
- `[[triage]]` — produces the confirmed findings this skill patches.
- `[[sec-audit-source]]` — six-phase orchestration.
- `HARNESS-ATTRIBUTION.md` — upstream credit.
