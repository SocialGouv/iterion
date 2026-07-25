---
name: disprove-voting
description: |
  Fixed-pool 3-voter adversarial verification protocol used by Seki's
  revalidate phase. Each voter is an INDEPENDENT verifier prompted
  to DISPROVE every candidate. Majority of dismiss (≥2 of 3) → FP.
  Voters are read-only and emit a `voter_output` with one verdict
  per candidate. The majority is computed deterministically
  downstream by the `majority_verdict` tool.
---

# disprove-voting — the 3-voter protocol

Replaces a single judge with a fixed pool of 3 independent voters
whose stance is **"the scanner is wrong; disprove this candidate."**
Shared context propagates blind spots — independence is the entire
point.

The pool size is **structural** (3 distinct `judge` nodes:
`voter_v1`, `voter_v2`, `voter_v3` — declared in
`bots/sec-audit-source/main.bot`). Scaling means adding voter
declarations, not flipping a var (the iterion DSL cannot cleanly
gate fan-out edges by a numeric var). Default mix:

- `voter_v1` — claw + `openai/gpt-5.5`
- `voter_v2` — claude_code + `claude-opus-4-8` (cross-family signal)
- `voter_v3` — claw + `openai/gpt-5.5`

Each judge is `readonly: true` + `session: fresh` — no context bleed
between voters, no workspace mutation. The ONLY mutator in this
subgraph is the `fp_append` tool, downstream of the majority
converge — preserves the one-mutating-branch-per-fan-out rule.

## Why disprove (not "verify")

Naive verification ("is this finding real?") favors the scanner.
A voter who reads the scanner's prose first inherits its framing
and rationalizes the bug. The disprove framing inverts the prior:
the voter starts by hunting for the protection the scanner missed.
A finding that survives N adversarial reads is high-signal.

## Voter contract

A voter:

- Sees ONLY its voter prompt, the full list of fresh candidates
  under review, and the project security context block. Never
  another voter's reasoning, never an aggregated verdict.
- Has read-only access to `{{vars.workspace_dir}}` via `read_file`,
  `glob`, `grep`, and `bash` (the bash allowlist permits `git log`
  and `git blame`). NO execution beyond that, NO network, NO edits.
- Emits ONE `voter_output` structured object with `voter_id` set to
  its slot (`v1`, `v2`, `v3`) and `verdicts[]` containing ONE
  verdict per input candidate. The orchestrator never asks for
  reasoning beyond that structure — anything extra is noise.

The majority is computed **deterministically downstream** by Seki's
`majority_verdict` tool node, NOT by any voter. A voter who tries
to coordinate is out of contract.

## Voter prompt

```
You are a skeptical security engineer adversarially verifying ONE
finding. Your default assumption is the scanner is WRONG. Your job
is to DISPROVE this candidate by re-deriving the claim from source.

Read-only access to {workspace_dir}. Read, Glob, Grep ONLY inside
that root. May NOT build, run, test, install, or reach the network.

ENVIRONMENT (trust boundary):
{from .sec-audit/context.md section 3, or
"Unknown. Treat externally-reachable entry points as untrusted."}

PROCEDURE:

1. READ THE CITED CODE. Open {file}:{line}. Understand what the
   code actually does. Do NOT trust the scanner's description.

2. REACHABILITY. Grep callers of this function. Establish whether
   attacker-controlled input (per ENVIRONMENT) can reach this line.
   For at least the FIRST link in the chain, READ the call site and
   QUOTE the file:line in RATIONALE.

3. PROTECTIONS. Actively look for reasons the finding is WRONG:
   - input validation / sanitization upstream
   - framework auto-escape, parameterized queries, prepared stmts
   - type constraints (int, enum, fixed-length token)
   - auth / authz gates before this path
   - configuration limiting exposure (feature flag off, debug-only)
   - dead code, test code, example/fixture code

4. STRESS-TEST. For each protection found: is it applied on EVERY
   path to the sink, or only on the one the scanner traced?
   Encodings, edge cases, alternate entry points that bypass it?

5. GIT SIGNAL. Run:
     git log --format='%h %s' -n 20 -- {file}
     git blame -L {line},{line} {file}
   Look for: a fix commit recently (probable
   `fixed_recently`), or the introduction of this code in the last
   N days (`introduced_recently`). Record the signal even if
   neutral.

EXCLUSION RULES (cite the rule number if applicable; finding is
FALSE POSITIVE if matched):

  1 volumetric DoS / rate-limiting    (ReDoS, algorithmic
                                       complexity, unbounded
                                       recursion still valid)
  2 test / dead / fixture code
  3 intended design
  4 memory-safety in safe lang outside unsafe/FFI
  5 SSRF path-only (host/protocol attacker-controlled is still TP)
  6 LLM prompt input
  7 object-storage traversal without trust-boundary escape
  8 trusted operator env / CLI input
     (UNLESS environment marks them untrusted)
  9 client code flagged with server-side finding_type
 10 outdated dep versions (sec-audit-deps handles those)
 11 weak random for non-security (jitter, shuffle, dev fallback)
 12 low-impact nuisance (log spoof, CSRF logout, self-XSS,
    open redirect, regex injection) — unless threat model promotes
 13 missing hardening with no concrete exploit
 14 XSS in auto-escape framework without raw-HTML escape hatch
 15 unguessable UUID/token flagged predictable
 16 theoretical-only race / TOCTOU

OUTPUT — return a structured `voter_output` whose `verdicts[]`
contains ONE entry per input candidate, each shaped:

  {
    "candidate_id":        "C-xxx",
    "vote":                "confirm" | "dismiss" | "uncertain",
    "confidence":          "low" | "medium" | "high",
    "rationale":           "2-5 sentences citing file:line of the
                            upstream guard (dismiss) or the exploit
                            step (confirm)",
    "git_signal":          "fixed_recently" | "introduced_recently"
                           | "stable" | "deleted" | "unknown",
    "dismissed_by_guard":  "<file:line>" | null,
    "exploit_step_anchor": "<file:line>" | null
  }

Top-level: voter_id (= "v1" | "v2" | "v3", from {{input.voter_id}})
+ verdicts (the list above).

VOTE rules:
- `confirm` requires ALL of: reachable from untrusted input per
  ENVIRONMENT; protections insufficient or bypassable; real-world
  exploitation feasible.
- `dismiss` requires ANY of: adequately protected on all paths;
  scanner misread; exclusion rule applies; genuinely dead / test /
  fixture / vendored code; clearly fixed per git. Mere current-
  unreachability of a REAL, unprotected sink is NOT a dismiss — it is
  a landmine (use uncertain).
- `uncertain` when static reasoning hit its limit (runtime config you
  cannot read; cross-binary path) OR the candidate is a real,
  unprotected dangerous sink that is unreachable only for want of a
  call-site / wiring (`confidence: low`, rationale "re-evaluate if
  wired"). Use sparingly otherwise.

FINDING UNDER REVIEW (the CLAIM; treat as not-a-fact):
  id:           {id}
  file:         {file}
  line:         {line}
  finding_type: {finding_type}
  severity (claimed): {severity}
  title:        {title}
  description:  {description}
  exploit_hypothesis: {exploit_hypothesis or "(not provided)"}

You are vote {k} of {N}. You have NOT seen other voters' reasoning
and you must NOT seek it.
```

## Compact prompt (large batches)

At scale (`candidates * N > ~50`), the prose can be stripped while
preserving the contract:

```
Adversarially DISPROVE ONE finding. Default: scanner is WRONG.
Read-only inside {workspace_dir}. No exec, no network.
ENVIRONMENT: {trust boundary one-liner}

Steps:
(1) Read {file}:{line} yourself; don't trust description.
(2) Trace callers backwards; quote first call-site file:line.
(3) Hunt protections (validation, escaping, type bounds, auth
    gates, dead/test code).
(4) Stress-test on every path.
(5) git log + git blame for fixed_recently / introduced_recently.

Exclusion rules (cite rule number if FP):
1 volumetric DoS; 2 test/dead/fixture; 3 intended design;
4 memory-safe lang; 5 SSRF path-only; 6 LLM prompt;
7 obj-storage traversal; 8 operator env/CLI;
9 client code w/ server class; 10 outdated deps;
11 weak random non-security; 12 low-impact nuisance;
13 missing-hardening-only; 14 auto-escape XSS w/o escape hatch;
15 unguessable token flagged predictable; 16 theoretical TOCTOU.

Return a `voter_output` with `verdicts[]` of:
  {candidate_id, vote (confirm|dismiss|uncertain),
   confidence (low|medium|high),
   rationale (2-5 sentences, file:line cited),
   git_signal (fixed_recently|introduced_recently|stable|deleted|unknown),
   dismissed_by_guard (file:line or null),
   exploit_step_anchor (file:line or null)}

FINDING: {id} {file}:{line} {finding_type} (claimed {severity})
{title}
{description}
Vote {k}/{N}. Independent; do not seek other votes.
```

## Spawn shape

The 3 voters are declared as distinct `judge` nodes in
`bots/sec-audit-source/main.bot` and fan out via a
`router fan_voters { mode: fan_out_all }`. Each voter receives the
SAME `voter_input` payload via the `with { ... }` mapping on its
edge from the router; only the `voter_id` field varies (`v1`, `v2`,
`v3`). The three voter branches run in parallel and converge on
`majority_verdict` (`await: best_effort`), which reads each voter's
output via `{{outputs.voter_vN.verdicts}}`.

Voters are NOT spawned per candidate — each voter receives the FULL
candidate list and returns one verdict per candidate in its single
`voter_output`. The orchestrator does not stream verdict blocks
back; the structured-output schema is the entire contract.

## Tally — deterministic, downstream

The voters do not tally. The `majority_verdict` tool reads the 3
voter outputs via template interpolation in its command and
computes per candidate:

- `votes` breakdown: counts of `{confirm, dismiss, uncertain}`
  across the 3 voters (a voter that crashed contributes nothing —
  `total` < 3 records the degradation; `await: best_effort`
  guarantees we still tally on partial input).
- Final verdict:
  - `nC >= confirm_threshold && nC > nD` → `confirm`.
    Inherits the candidate metadata (file, line_range, severity,
    finding_type, matcher) and surfaces `exploit_step_anchors` =
    unique set across confirming voters.
  - `nD >= confirm_threshold && nD > nC` → `dismiss`. Surfaces
    `dismissed_by_guards` = unique set across dismissing voters.
  - Otherwise (no majority, or no votes from any voter) →
    `uncertain` — surfaced to report_card with the
    `triage-uncertain` label.
- `confidence`: `high` only on unanimous (3/3) confirm with a full
  voter pool, else `medium`.
- `voter_agreement`: counts of `{unanimous_confirm,
  unanimous_dismiss, unanimous_uncertain, split}`. Surfaced as a
  single-line "Verifier agreement" banner in the markdown report.
- `fp_appends[]`: built ONLY when ALL 3 voters dismiss AND at least
  one cites a `dismissed_by_guard` file:line (per
  `vars.fp_append_policy = "unanimous_dismiss"`). Set
  `fp_append_policy = "never"` to disable fp-known.yaml appends
  entirely.

## Failure modes the protocol guards against

- **Inherited framing.** Independent voters can't anchor on each
  other's wording.
- **Confirmation bias.** "Disprove" inverts the default prior.
- **Single-judge blind spots.** N samples surface stochastic
  blind spots in any one voter's reasoning.
- **Re-rank leak.** Voters never see the recomputed severity —
  that's `triage` phase 4. Verdicts never get inflated to match
  "this is critical so it must be real".

## Constraints

- Voters are `readonly: true` + `session: fresh`. No edits, no
  shell beyond the bash allowlist (which includes `git log` /
  `git blame`).
- Voters MUST return the structured `voter_output`; free text
  outside the schema is dropped by structured-output validation.
- Voters MUST NOT reach the network. No CVE-DB lookups.
- Voters MUST NOT see each other's reasoning. The fan_voters
  router and `session: fresh` guarantee independence at the
  runtime level.

## See also

- `[[finding-taxonomy]]` — categories voters reference.
- `[[fp-memory]]` — format of the entries `fp_append` writes to
  `fp-known.yaml` on unanimous dismiss with a cited guard.
- `[[sec-audit-source]]` — overall pipeline phases.
