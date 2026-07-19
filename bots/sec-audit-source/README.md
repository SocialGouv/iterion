# sec-audit-source

Universal source-code security auditor — a bundled iterion workflow
that runs SAST + secret scanners + filesystem scanners on the current
repo, triages the noise with an LLM, suppresses curated false
positives, revalidates with a two-phase judge, and emits one kanban
issue per real finding.

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
| Generic | gitleaks, trivy fs, semgrep `--config=auto` | Secrets, misconfigs, language-agnostic SAST |
| JS/TS | semgrep `--config=p/javascript`, semgrep `--config=p/typescript` | Express/Fastify/Next.js/NestJS handler hygiene |
| Go | semgrep `--config=p/golang`, gosec | gosec G-rules, Gin/Echo/Fiber handler hygiene |
| Python | semgrep `--config=p/python`, bandit | Django/FastAPI/Flask handler hygiene |

Per-language coverage is documented in `skills/lang-*.md`. Add a
language by adding one skill + one router branch — see the *Adding a
language* section at the bottom of this README.

## Quick start

```bash
# 0. Make sure the scanners are on PATH
#    (devbox.json already pulls gitleaks, trivy, semgrep, gosec,
#    bandit for the dev shell; if you run this bot outside devbox,
#    `iterion sandbox doctor` flags missing tools.)

# 1. Run on the current repo (claw + openai/gpt-5.5 by default).
devbox run -- iterion run bots/sec-audit-source/main.bot \
  --var workspace_dir=$(pwd) \
  --var severity_threshold=medium

# 2. Watch the live console:
#    open http://localhost:7777 → click the run → live findings on the board.

# 3. Findings land on the iterion kanban as issues:
#    - state:        ready
#    - labels:       severity:{low|medium|high|critical}, type:<finding-type>, source:sec-audit-source
#    - assignee:     unset (a follow-up remediation bot can pick them up)
#    - body:         file:line anchor, exploit hypothesis, reproduction recipe, fix sketch
```

## Cross-run memory — two stores

The bot keeps two kinds of memory between runs:

| Memory | Location | Purpose |
|---|---|---|
| **Curated FP list** | `.sec-audit/fp-known.yaml` in the scanned repo (committable) | Suppress findings the operator has reviewed and judged false positives. Human-editable. |
| **Per-file analysis records** | `.sec-audit/files/<sha1(path)>.json` in the scanned repo (typically committable) | Append-only history of every file analysis. Lets re-runs skip the expensive `revalidate` phase on files that haven't changed since the previous run at the same scanner version + within TTL. |

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
`status: known_fp` (not surfaced). The `revalidate` judge can ALSO
invalidate stale entries: when an FP-marked line range no longer
contains the validating allowlist call (e.g. someone removed it), the
judge flips the entry to `status: stale` and re-promotes the finding.

If you want to silence a finding the bot keeps surfacing, edit this
file by hand. If you want the bot to learn from a manual triage,
mention the suppression rationale in the run console's free-text
human turn (`fp_curation` node, optional) — the bot will append the
entry for you.

## Pipeline

```
detect_tech (claw, readonly)
  └─→ run_scanners (router fan_out_all)
        ├─→ run_generic_scanners   (tool: gitleaks + trivy + semgrep auto)
        ├─→ run_js_scanners        (tool: semgrep js/ts profile)        — if tech.langs ∋ js
        ├─→ run_go_scanners        (tool: semgrep golang + gosec)       — if tech.langs ∋ go
        └─→ run_python_scanners    (tool: semgrep python + bandit)      — if tech.langs ∋ python
  └─→ scan_join (compute, await: best_effort) ← converges scanner outputs
  └─→ triage (claw, readonly) ← reads scanner JSONs + fp-known.yaml
  └─→ filter_cached_files (tool) ← Cap1: skip revalidate on unchanged files
  └─→ revalidate (claw judge, two-phase) ← on fresh candidates only
  └─→ merge_verdicts (compute) ← fresh + cached verdicts unified
  └─→ report_card (claude_code, board.create + board.label) → kanban + findings.md
  └─→ update_file_records (tool) ← append one history entry per analysed file
  └─→ done
```

## Adding a language

1. **Drop a skill**: `skills/lang-<langid>.md` describing scanners
   to invoke, manifest files to parse, framework-specific threat
   hints (mirror the existing `lang-go.md` shape).

2. **Add a tool node**: a `tool run_<langid>_scanners:` node that
   emits a JSON file under `{{vars.scan_dir}}/<langid>.json`.

3. **Wire the router**: add a `with { ... } when tech.has_<langid>`
   branch under the `run_scanners` router.

4. **Add the framework taxonomy** to `skills/lang-<langid>.md`
   under `## Framework-specific signals`.

No DSL primitive changes, no new capability. Pure composition.

## See also

- [sec-audit-deps](../sec-audit-deps/) — sibling bundle for supply-chain malware.
- [skills/iterion-board.md](skills/iterion-board.md) — the board capabilities reference.
- [docs/security-bots.md](../../docs/security-bots.md) — shared threat model + ops guide.
