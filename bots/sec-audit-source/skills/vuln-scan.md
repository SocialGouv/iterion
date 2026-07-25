---
name: vuln-scan
description: |
  Static parallel review of a source tree by focus area. Reads
  `.sec-audit/context.md` (threat model) when present and
  delegates the scanner set to the per-language `lang-*` skills.
  Runs deterministic scanners + a per-focus-area review pass, then
  hands candidates to triage. Also usable as a single-file
  "re-attack lens" during remediation.
attribution: |
  Adapted from Anthropic's `defending-code-reference-harness`
  (`/vuln-scan` reference implementation). The harness's
  hand-rolled scanner list is replaced by Seki's per-language
  scanner registry (the `iterion:scanners` data blocks in
  `lang-*.md`), and the validation step is wired to Seki's
  `scan_health` smoke gate. C/C++/ASAN-specific guidance dropped in
  favor of Go/TS focus areas.
---

# vuln-scan — static review by focus area

Static vulnerability review of a source tree. Produces normalized
candidates that `triage.md` ingests. **Does not execute code.** No
build, no run, no network.

## The scanner set lives in `lang-*.md`

This skill does NOT enumerate scanners. The authoritative scanner
list is the `iterion:scanners` machine-readable data block at the
bottom of each language skill:

- `[[lang-generic]]` — always-on (gitleaks, trivy, semgrep auto).
- `[[lang-go]]` — semgrep `p/golang`, gosec.
- `[[lang-js]]` — semgrep `p/javascript,p/typescript,p/nodejsscan`.
- `[[lang-python]]` — semgrep `p/python,p/django,p/flask`, bandit.

To add or change a scanner, edit the data block in the matching
`lang-*.md` — no DSL change. Do not duplicate the list here.

The `run_lang_scanners` tool node iterates each entry and runs `cmd`
with `$SCAN_DIR` and `$WORKSPACE_DIR` in the environment, cwd =
workspace. Raw JSON lands under `{{vars.scan_dir}}/<output>` per
spec.

## Validation — Seki's `scan_health` smoke gate

After scanners run, `scan_health` (a tool node, no LLM) reads each
expected `output` path declared in the active `iterion:scanners`
blocks and verifies:

1. The file exists.
2. The file is parseable JSON (not truncated, not a stderr blob).
3. The file has the scanner's expected top-level shape (e.g.
   semgrep emits `{"results": [...]}`).
4. If the scanner exited non-zero, the workflow records the error
   but still surfaces partial output when present (`|| true` in the
   `cmd`).

`scan_health` failing means coverage is genuinely missing — that's a
workflow failure, not a quiet skip. The retriable classifier in
iterion's runtime retries transient errors automatically.

## Per-focus-area review pass

Scanners alone miss anything they don't have a rule for. After
deterministic scans land, fan out **one focus-area subagent per
area** identified by:

1. The threat model's section 3 (entry points & trust boundaries)
   from `.sec-audit/context.md` if present, OR
2. A quick recon over the source tree if `context.md` is absent or
   stale (read `README`, route registrations, package manifests;
   propose 3–10 focus areas).

Each subagent gets a brief scoped to its area and reads source +
the scanner outputs that overlap its scope. Cap at 10 concurrent
subagents. On tiny targets (<15 source files), fall through to a
single sequential pass.

### Review brief (per focus-area subagent)

```
You are conducting authorized static security review. Focus area:
**{focus_area}**. Other agents cover other areas; duplication is
wasted effort.

TARGET: {workspace_dir}
TRUST BOUNDARY: {from .sec-audit/context.md section 3, or
"untrusted input -> server process memory"}
THREATS IN SCOPE: {T-ids from section 4 mapped onto this focus area}

TASK: read source in your focus area and identify candidate
vulnerabilities. Static review — do NOT build, run, or probe. Reason
from the code.

REPORTING BAR: report anything with a plausible exploit path. Skip
style concerns and purely theoretical issues with no attack story.
If unsure, REPORT IT — triage does rigorous N-voter verification.

WHAT TO LOOK FOR (taxonomy from [[finding-taxonomy]]):
  injection, xss, ssrf, auth, authz/idor, crypto, secrets,
  deserialization, path-trav, redirect, config, other

DO NOT REPORT (common FPs):
  - test files, fixtures, build scripts, generated code
  - memory-safety in memory-safe lang outside unsafe/FFI
  - XSS in React/Vue without a raw-HTML escape hatch
  - env vars / CLI flags as the attack vector (operator-controlled)
  - missing hardening with no concrete exploit
  - outdated dependency versions (sec-audit-deps owns those)

For each finding, trace: entry point -> sink -> trigger.

OUTPUT — one block per finding:

<finding>
<id>F-{focus_idx:02d}-{n:02d}</id>
<file>{relative/path}</file>
<line>{n}</line>
<finding_type>{from [[finding-taxonomy]]}</finding_type>
<severity>{low|medium|high|critical}</severity>
<confidence>{0.0-1.0}</confidence>
<title>{one line}</title>
<description>{root cause, attacker control, trigger, data flow.
Cite file:line.}</description>
<exploit_hypothesis>{1-2 sentences — how a human attacks this}</exploit_hypothesis>
<recommendation>{specific fix}</recommendation>
</finding>
```

Collate findings, drop empties, deterministically dedupe on
`(file, finding_type, line ±10)` (semantic dedupe is `triage.md`'s
job — keep this pass cheap).

## Hand-off shape

Each `<finding>` becomes a triage candidate per `[[sec-audit-source]]`:

```json
{
  "id": "F-01-03",
  "finding_type": "ssrf",
  "severity": "high",
  "file": "pkg/server/proxy.go",
  "line_range": [120, 145],
  "matcher": "<scanner-rule-id or focus-area-llm>",
  "scanner": "<semgrep-go | gosec | gitleaks | focus-llm>",
  "snippet": "...",
  "scanner_rationale": "...",
  "exploit_hypothesis": "...",
  "status": "candidate"
}
```

The `triage` node (see `[[sec-audit-source]]` phase 3) then applies
`fp-known.yaml` ([[fp-memory]]) and the taxonomy mapping.

## Re-attack lens — single-file / single-rule mode

After a patch lands (see `[[patch]]`), this skill also runs in a
narrow re-attack mode: re-scan a single file with a single matcher
or focus area, verifying the original finding no longer fires AND
no sibling variant has cropped up. Invocation contract:

```
vuln-scan --reattack <file> --rule <rule-id>
vuln-scan --reattack <file> --focus <area>
```

Re-attack mode:
- Runs ONLY the matchers that produced the original finding plus a
  one-pass focus-area review (`recipes` per category live in
  `[[reattack-oracles.md]]`).
- Exits non-zero if the matcher still fires OR a new finding of
  the same `finding_type` appears in the same function.
- Writes nothing to the kanban; output is consumed by `[[patch]]`.

## Constraints

- **Never execute target code.** If asked to "reproduce" or "PoC",
  decline.
- **Never invent line numbers.** Every `file:line` must come from
  a Read or Grep result.
- **Never drop a finding silently.** Confidence calibration is
  triage's job; this skill's bar is "plausible exploit path".

## See also

- `[[sec-audit-source]]` — Seki's six-phase orchestration.
- `[[triage]]` — N-voter adversarial verification + dedup + rerank.
- `[[finding-taxonomy]]` — required mapping for normalized output.
- `[[lang-generic]]`, `[[lang-go]]`, `[[lang-js]]`, `[[lang-python]]`
  — scanner registries (machine-readable `iterion:scanners` blocks).
- `[[reattack-oracles]]` — per-category recipes used in `--reattack`.
- `HARNESS-ATTRIBUTION.md` — upstream credit.
