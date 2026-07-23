# sec-audit-source

Universal source-code security auditor — a bundled iterion workflow
that runs SAST + secret scanners + filesystem scanners on the current
repo, triages the noise with an LLM, suppresses curated false
positives, revalidates with a three-voter adversarial majority, and
emits one kanban issue per real finding. An opt-in remediation phase
(`--var remediate=true`) can then verify and land fixes on a temporary
branch; it is off by default, so the bot is a read-only auditor out of
the box.

Inspired by:
- [vercel-labs/deepsec](https://github.com/vercel-labs/deepsec) — the
  matchers → batched LLM review → revalidate FP-reduction pipeline.
- [SocialGouv/no-package-malware](https://github.com/SocialGouv/no-package-malware) —
  the static-signals → structured-LLM-prompt pattern (we use it for
  scanner output normalisation, not malware detection: that lives in
  the sibling [sec-audit-deps](../sec-audit-deps/) bundle).

## What it scans

| Layer | Scanners | Coverage |
|---|---|---|
| Generic (always-on) | gitleaks, trivy fs (`--scanners=vuln,misconfig,secret`), semgrep `--config=p/default` | Secrets, vulns, misconfigs, language-agnostic SAST |
| JS/TS | semgrep js/ts profile | Express/Fastify/Next.js/NestJS handler hygiene |
| Go | semgrep golang, gosec | gosec G-rules, Gin/Echo/Fiber handler hygiene |
| Python | semgrep python, bandit | Django/FastAPI/Flask handler hygiene |

The generic layer always runs; the per-language rows are dispatched by
the single `run_lang_scanners` tool, which reads the `iterion:scanners`
block from `skills/lang-<id>.md` for each language `detect_tech`
reported. Add a language by dropping one `skills/lang-<id>.md` — no DSL
edit — see the *Adding a language* section at the bottom of this README.

## Quick start

```bash
# 0. Make sure the scanners are on PATH
#    (devbox.json already pulls gitleaks, trivy, semgrep, gosec,
#    bandit for the dev shell; if you run this bot outside devbox,
#    `iterion sandbox doctor` flags missing tools.)

# 1. Run on the current repo. Only detect_tech defaults to
#    claw + openai/gpt-5.5 (cheap tech survey); triage, the three
#    revalidation voters, and report_card default to
#    claude_code + claude-opus-4-8.
devbox run -- iterion run bots/sec-audit-source/main.bot \
  --var workspace_dir=$(pwd) \
  --var severity_threshold=medium

# 2. Watch the live console:
#    open http://localhost:7777 → click the run → live findings on the board.

# 3. Findings land on the iterion kanban as issues:
#    - state:        ready
#    - labels:       severity:{low|medium|high|critical}, type:<finding-type>, source:sec-audit-source
#    - assignee:     unset (the opt-in remediation phase, or a follow-up bot, can pick them up)
#    - body:         file:line anchor, exploit hypothesis, reproduction recipe, fix sketch
```

## Cross-run memory — two stores

The bot keeps two kinds of memory between runs:

| Memory | Location | Purpose |
|---|---|---|
| **Curated FP list** | `.sec-audit/fp-known.yaml` in the scanned repo (committable) | Suppress findings the operator has reviewed and judged false positives. Human-editable. |
| **Per-file analysis records** | `.sec-audit/files/<sha1(path)>.json` in the scanned repo (typically committable) | Append-only history of every file analysis. Lets re-runs skip the expensive three-voter revalidation on files that haven't changed since the previous run at the same scanner version + within TTL (`filter_cached_files`). |

The records mechanism mirrors deepsec's append-only `FileRecord`
pattern, scoped to single-process iterion-bundle execution. See
[skills/file-records.md](skills/file-records.md) for the schema and
cache-hit rules. The TTL defaults to 30 days
(`--var records_ttl_days=N` to override) and a scanner_version bump
invalidates the cache (`--var scanner_version=…`).

## False-positive memory

Confirmed false positives are written to
`.sec-audit/fp-known.yaml` in the **scanned repo** (NOT in the
host store). The file is committable + human-reviewable, and is the
authoritative source of suppression rules.

Schema:

```yaml
known_false_positives:
  - id: fp-2026-001
    finding_type: ssrf
    file: "pkg/server/proxy.go"
    line_range: [120, 145]
    matcher: "outbound-request-with-userinput"
    rationale: |
      URL is validated against a static allowlist in
      pkg/policy/allowlist.go before any outbound call.
    confirmed_by: "@devthejo"
    confirmed_at: "2026-05-19"
    expires_at: null
```

The `triage` agent reads this file and tags matching candidates as
`status: known_fp` (not surfaced). New FP entries are only appended by
`fp_append` when `majority_verdict` emits them — under the default
`fp_append_policy: unanimous_dismiss`, that requires all three voters
to dismiss a candidate with at least one citing a `dismissed_by_guard`
file:line, the strictest safeguard against the bot suppressing real
signal on a single voter's error.

If you want to silence a finding the bot keeps surfacing, edit this
file by hand. The bot itself only appends entries automatically, via
`fp_append`, when the three voters unanimously dismiss a candidate
under the `unanimous_dismiss` policy above; set
`--var fp_append_policy=never` to disable those appends entirely.

## Pipeline

```
inventory (tool)                    ← deterministic bounded file/manifest list
  └─→ detect_tech (agent: claw + openai/gpt-5.5, readonly)
        emits an OPEN `langs: []` list (no per-language booleans)
  └─→ … project-context + diff-scope + shard-planning gates …
  └─→ run_generic_scanners (tool: gitleaks + trivy fs + semgrep --config=p/default) — ALWAYS on
  └─→ run_lang_scanners (tool: ONE skill-driven node; reads skills/lang-<id>.md
        `iterion:scanners` block per detected language)
  └─→ … optional deepsec gate …
  └─→ scan_join (compute, await: best_effort) ← converges scanner outputs
  └─→ scan_health (tool) ← anti-façade gate: hard-fail if the generic floor is missing
  └─→ cap_findings (tool) ← overflow guard (top N findings/file)
  └─→ triage (agent: claude_code + opus-4-8) ← reads scanner JSONs + fp-known.yaml
  └─→ filter_cached_files (tool) ← skip revalidation on unchanged files
  └─→ voter_v1 → voter_v2 → voter_v3 (3 judges: claude_code + opus-4-8, adversarial "disprove")
  └─→ majority_verdict (tool) ← tally confirm/dismiss/uncertain; ≥confirm_threshold (default 2/3) confirms
  └─→ fp_append (tool, only when majority emits fp_appends[]) → merge_with_cache
  └─→ merge_with_cache (compute) ← fresh + cached verdicts unified
  └─→ report_card (agent: claude_code + opus-4-8, board.read/create/label) → kanban + findings.md
  └─→ update_file_records (tool) ← append one history entry per analysed file
  └─→ remediate_gate (compute) ← when remediate=false (DEFAULT): done
                                  when remediate=true: opt-in remediation phase (below)
  └─→ done
```

Revalidation is a **three-voter adversarial majority**, not a single
two-phase judge: `voter_v1 → voter_v2 → voter_v3` each run the same
"the scanner is wrong — disprove it" protocol independently, and
`majority_verdict` confirms a finding when at least `confirm_threshold`
(default 2 of 3) vote to confirm.

The scanners run as **one sequential branch** (`run_generic_scanners ->
run_lang_scanners`), not a parallel router fan-out — this stays inside
the runtime's one-mutating-branch rule; the lang scanner no-ops on
absent languages.

### Opt-in remediation phase (`--var remediate=true`)

Off by default. When enabled, after `report_card` the workflow runs a
verification-ladder remediation phase: `remediation_plan` (split
confirmed findings into fixable vs hard-stopped) →
`prepare_branch` (temp git branch) → per-finding loop
(`select_finding → patch_author → build_rung → reproduce_rung →
regress_rung → reattack → reviewer_isolation → aggregate_verdict →
record_finding`) → `merge_fixes` / `abandon_fixes` (gated on the
`approve_fixes` human node under the default `apply_gated` mode) →
`remediation_report`. See the `remediate` / `remediation_mode` vars in
`main.bot` for the mode contract.

## Adding a language

Drop a single skill — **no DSL edit, no tool node, no router branch**:

1. **Drop a skill**: `skills/lang-<langid>.md` with an
   `<!-- iterion:scanners ... -->` data block listing the scanner
   commands to run, plus the manifest files to parse and
   framework-specific threat hints under `## Framework-specific
   signals` (mirror the existing `lang-go.md` shape).

That is the whole change. `detect_tech` reports the language in its
open `langs` list; the single `run_lang_scanners` tool reads that
skill's `iterion:scanners` block and runs the commands, and
`scan_health` reads the same block to verify per-language coverage.
No per-language boolean, no new node. Pure composition.

## See also

- [sec-audit-deps](../sec-audit-deps/) — sibling bundle for supply-chain malware.
- [skills/iterion-board.md](skills/iterion-board.md) — the board capabilities reference.
- [docs/security-bots.md](../../docs/security-bots.md) — shared threat model + ops guide.
