---
name: triage
description: |
  Adversarial triage of raw scanner output: N-vote majority
  verification, two-pass dedup (deterministic + semantic), exclusion
  rules, severity re-rank from preconditions. Wires into Seki's
  revalidate phase via `[[disprove-voting]]` and consumes the FP
  memory at `.sec-audit/fp-known.yaml` ([[fp-memory]]).
attribution: |
  Adapted from Anthropic's `defending-code-reference-harness`
  (`/triage` reference implementation). Harness JSON checkpoint
  emission dropped; verdicts flow through Seki's existing
  `merge_verdicts` + `report_card` pipeline. The "disprove"
  framing for voters and the FP-memory hook are iterion-native.
---

# triage — verify, dedupe, rerank, route

Four jobs in one pass:

1. **Verify.** Each candidate is re-derived from source by N
   independent verifiers running the "disprove" protocol
   (`[[disprove-voting]]`). Majority decides.
2. **Deduplicate.** Two passes: deterministic (cheap) then semantic
   (one LLM agent).
3. **Re-rank.** Severity is recomputed from preconditions and
   access level, NOT from the scanner's claimed CVSS.
4. **Route.** Each survivor gets `owner_hint` from CODEOWNERS, git
   log, or module fallback.

Output is the verdict stream consumed by `merge_verdicts` and
`report_card` (see `[[sec-audit-source]]` phase 3.5–5). No JSON
file is written by this skill — verdicts move via the workflow's
data plane.

## Inputs

- The candidate list from `triage` phase 3 (already taxonomy-mapped
  per `[[finding-taxonomy]]`).
- `.sec-audit/fp-known.yaml` ([[fp-memory]]) — read BEFORE
  the LLM ever sees a candidate. Matching candidates get
  `status: known_fp` and skip revalidate.
- `.sec-audit/context.md` ([[threat-model]]) if present. The
  threat model defines the trust boundary and stated threats —
  preconditions and severity get evaluated against it.

## Phase 1 — Deterministic dedup (inline, no subagent)

Cluster candidates where all of:

- same `file` (after path normalization), AND
- same `finding_type` (per taxonomy, case-insensitive), AND
- `line` numbers within 10 lines.

Both-missing matches; one-side-missing does NOT. Within each
cluster, the canonical is the candidate with the fewest
`missing_fields`; ties break to lowest id. Every other member gets
`verdict: duplicate`, `duplicate_of: <canonical>`, and is removed
from the working set. Record absorbed ids on the canonical as
`absorbed: [...]`.

## Phase 2 — Semantic dedup (one LLM pass, conditional)

If >1 cluster survives, spawn ONE subagent with this brief:

```
You are deduplicating security findings before expensive
verification. Two findings are DUPLICATES if fixing one would also
fix the other. Two findings are DISTINCT if they have genuinely
independent root causes, even if they share a category or file.

Treat as DUPLICATE:
- Same root cause described with different wording or by different
  scanners.
- A shared vulnerable helper reported once per call site.
- A missing global protection (auth check, output encoding) reported
  once per endpoint that lacks it.
- A cause ("missing input validation on `name`") and its
  consequence ("SQL injection via `name`") in the same code path.

Treat as DISTINCT:
- Different finding_types in the same region.
- Same file, same finding_type, but different tainted vars reaching
  different sinks.
- Same helper, two independent bugs inside it.
- Two endpoints missing the same check (per-endpoint fix).

Respond with ONLY lines of the form:
  GROUP: <canonical_id> <- <dup_id>, <dup_id>, ...

CANDIDATES:
{one line per surviving candidate: "id | file:line | finding_type | title"}
```

The subagent sees only `id | file:line | finding_type | title`
(enough to cluster, not enough to leak one scanner's reasoning into
another's verification). Parse `GROUP:` lines; absorb dups into
canonicals; drop them from the working set.

## Phase 3 — N-voter disprove (cross-link `[[disprove-voting]]`)

For each surviving candidate, spawn N independent verifiers
(default N=3, configurable via workflow var `votes_per_finding`).
Each voter:

- Sees ONLY the verifier prompt + the single candidate under
  review. **Never** sees other voters' reasoning (shared context
  propagates blind spots — that's the whole point).
- Is prompted to **disprove** the candidate. Default stance:
  scanner is WRONG. The voter must re-derive the claim from source.
- Uses `git log` / `git blame` for `fixed_recently` and
  `introduced_recently` signals.
- Emits one verdict block per candidate:
  `{vote: confirm | dismiss | uncertain, confidence, rationale
   citing file:line, git_signal, dismissed_by_guard,
   exploit_step_anchor}`.

Read-only. No edits, no execution, no network.

Detailed protocol, prompt, and output schema in
`[[disprove-voting]]`. The majority is computed deterministically
**downstream** (in Seki's `merge_verdicts` node, not by the
voters).

### Exclusion rules — applied during disprove

A voter MAY mark a candidate `dismiss` with a cited exclusion rule
when it matches any of:

1. Volumetric DoS / rate-limiting (infra-layer). ReDoS,
   algorithmic complexity, and unbounded recursion DRIVEN BY
   UNTRUSTED INPUT are NOT excluded.
2. Test files, dead code, fixtures, examples (`*_test.go`,
   `*.test.ts`, `tests/`, `__tests__/`, `testdata/`).
3. Behavior that is the intended design (compression middleware,
   backward-compat weak algorithm offered alongside strong one).
4. Memory-safety in memory-safe lang outside `unsafe` / FFI.
5. SSRF where attacker controls only path, not host or protocol.
6. User input flowing into an AI/LLM prompt (prompt injection is
   not a code vulnerability in the target).
7. Path traversal in object storage (S3/GCS) where `../` does not
   escape a trust boundary.
8. Trusted inputs as the attack vector (env vars, CLI flags set by
   the operator) UNLESS the trust boundary in
   `.sec-audit/context.md` marks them untrusted.
9. Client-side code flagged for a server-side finding_type.
10. Outdated dep versions (sec-audit-deps owns those).
11. Weak random used for non-security purposes (jitter, shuffle,
    dev-only fallback).
12. Low-impact nuisance (log spoof, CSRF on logout, self-XSS,
    tabnabbing, open redirect, regex injection) — **unless** the
    threat model promotes them.
13. Missing hardening / best-practice gap with no concrete exploit.
14. XSS in framework with default auto-escape (React, Vue, Angular,
    Jinja2 autoescape=on) UNLESS the sink is a raw-HTML escape
    hatch (`dangerouslySetInnerHTML`, `v-html`, `bypassSecurityTrustHtml`).
15. Unguessable identifiers (UUIDv4, 128-bit+ tokens) flagged as
    "predictable".
16. Race / TOCTOU that is theoretical only — no realistic window
    or no security-relevant state change between check and use.

Voters cite the rule number when invoking it. Rules also feed
`fp-known.yaml` append decisions (see `[[fp-memory]]`).

## Phase 4 — Severity re-rank (confirmed only)

For each candidate the majority confirmed, spawn one ranking
subagent. Recompute severity from preconditions and access level,
**independent** of the scanner's claimed CVSS. Verification and
severity are separate judgments — "this is real" must NOT inflate
into "this is critical".

### Ranking output

```
PRECONDITIONS:
- <one per line: required auth state, config, prior request, race
  window, attacker position>
ACCESS_LEVEL: <unauthenticated_remote | authenticated | local | physical>
SEVERITY: <critical | high | medium | low>
THREAT_MATCH: <T-id from context.md, or none>
VERIFY_VERDICT: <exploitable | mitigated | needs_manual_test>
RANK_RATIONALE: <2-4 sentences>
```

### Severity table

| Preconditions | Access required | Severity |
|---|---|---|
| 0 | Unauthenticated remote | high |
| 1–2 | Authenticated | medium |
| 3+ | Local-only / no demo path | low |

Evaluate each column independently and take the LOWER result.
Threat-match bumps severity by ONE step at most (never two —
caps re-inflation). Auth bypass and IDOR are bumped one step
when the endpoint is publicly reachable AND lets a request reach
another tenant's resource (per `[[finding-taxonomy]]`).

## Phase 5 — Route

For each confirmed finding, attach `owner_hint`, stopping at the
first hit:

1. **CODEOWNERS / OWNERS.** Grep workspace for `CODEOWNERS`,
   `OWNERS`, `.github/CODEOWNERS`, `docs/CODEOWNERS`. Match the
   finding's `file` against patterns; last match wins. Hint:
   `"CODEOWNERS: <pattern> -> <owner(s)>"`.
2. **git log.** If workspace is a git checkout, run
   `git -C <workspace> log --format='%an' -n 50 -- "<file>" |
    sort | uniq -c | sort -rn | head -3`.
   Hint: `"top committer: <name> (<n>/<total> recent commits); no
   CODEOWNERS entry"`.
3. **Module fallback.** Hint:
   `"component: <top-level dir>/; no CODEOWNERS or git history"`.

Set `owner_hint: null` on non-confirmed findings.

## Hand-off

Each confirmed candidate flows into `merge_verdicts` and onto the
kanban via `report_card` (see `[[iterion-board]]`) with labels:

- `severity:<level>` per the recomputed value.
- `type:<finding-type>` per `[[finding-taxonomy]]`.
- `source:sec-audit-source`.
- `scanner:<id>` — primary scanner that flagged it.
- `triage-uncertain` when no majority emerged AND noise tolerance
  is `recall`.

`dismiss` candidates with a strong, control-citing rationale are
appended to `fp-known.yaml` (see `[[fp-memory]]` append rules).

## Constraints

- **Never execute target code.** Read-only.
- **Never reach the network.** No CVE-DB lookups, no upstream
  commits.
- **Always set `subagent_type`** when spawning verifiers; never
  fork. A fork inherits the orchestrator's context and defeats
  voter independence.
- **All voter Task calls for one candidate in ONE message** so
  they run concurrently. Shard at ~40 parallel tasks if needed.

## See also

- `[[disprove-voting]]` — the per-voter protocol and output schema.
- `[[fp-memory]]` — `.sec-audit/fp-known.yaml` read/write.
- `[[finding-taxonomy]]` — the twelve categories.
- `[[threat-model]]` — `.sec-audit/context.md` consumer.
- `[[sec-audit-source]]` — six-phase orchestration this skill plugs
  into.
- `HARNESS-ATTRIBUTION.md` — upstream credit.
