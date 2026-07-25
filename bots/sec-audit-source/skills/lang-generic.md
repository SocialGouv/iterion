---
name: lang-generic
description: |
  Language-agnostic scanner reference for sec-audit-source. Covers
  gitleaks (secrets), trivy fs (filesystem misconfig + vuln DB),
  and semgrep `--config=p/default` (multi-language SAST). Always-on:
  every run executes these scanners regardless of detected stack.
---

# lang-generic — always-on scanners

These run on every workspace regardless of `tech.langs`. They cover
the categories that aren't language-specific: secrets, container/IaC
misconfig, and broad-spectrum SAST.

## gitleaks — secrets in source

```bash
gitleaks detect \
  --source={{vars.workspace_dir}} \
  --report-format=json \
  --report-path={{vars.scan_dir}}/gitleaks.json \
  --redact \
  --exit-code=0           # do NOT fail the workflow on findings; the
                          # triage agent decides severity
```

What it catches:
- AWS / GCP / Azure / Stripe / GitHub / Slack tokens
- Generic high-entropy strings near secret-suggestive names
- Private keys in any format

`finding_type` mapping: every gitleaks hit is `secrets`. Severity:
- `critical` if the rule id matches `aws-access-token`,
  `gcp-service-account`, `stripe-secret-key`, `private-key`
- `high` otherwise
- `low` if the file path is under a `*test*`, `*example*`,
  `*fixture*`, `*spec*` directory (likely test artefact)

## trivy fs — filesystem misconfig + vuln DB

```bash
trivy fs \
  --format=json \
  --output={{vars.scan_dir}}/trivy.json \
  --scanners=vuln,misconfig,secret \
  --severity=LOW,MEDIUM,HIGH,CRITICAL \
  --quiet \
  {{vars.workspace_dir}}
```

Categories trivy emits:
| trivy category | → `finding_type` |
|---|---|
| `vuln-config` (Dockerfile, k8s YAML, Terraform) | `config` |
| `vuln-os` / `vuln-lib` | `other` (these belong to sec-audit-deps logically; trivy fs may surface lockfile vulns — keep but tag with `redundant-with-deps-audit` label) |
| `secret` | `secrets` (trivy has weaker rules than gitleaks; deduplicate by file+line — if gitleaks already flagged it, drop) |

## semgrep — broad SAST (`p/default`)

```bash
semgrep \
  --config=p/default \
  --json \
  --output={{vars.scan_dir}}/semgrep-auto.json \
  --metrics=off \
  --quiet \
  {{vars.workspace_dir}}
```

`p/default` is Semgrep's curated cross-language pack — the
metrics-off-compatible floor. (`--config=auto` is rejected when
combined with `--metrics=off`, which is why the earlier auto+off
combo silently errored.) Overlaps with the per-language
`run_*_scanners` nodes — triage MUST deduplicate by
`(file, rule_id, line_range)`. The output file and subscanner id stay
`semgrep-auto` for backward compatibility.

If semgrep cannot fetch its rules in an offline sandbox run, the tool
node captures the error under `errors[]` and continues — a scanner
failure never fails the workflow (see Output format below).

## Output normalisation

The `triage` agent reads all three JSON files and emits candidates
with these scanner ids:
- `gitleaks` → primary scanner for `secrets`
- `trivy` → primary scanner for `config`
- `semgrep-auto` → fallback scanner; used only if no per-language
  semgrep already flagged the same `(file, rule, line)`

## Deduplication rules

A finding is considered duplicate if:
- same `file` + same `line_range` + same `finding_type`, AND
- both scanners produced it.

Keep the more specific scanner (per-language semgrep > semgrep auto;
gitleaks > trivy for secrets).

## Output format

The tool node writes a unified JSON envelope:

```json
{
  "scanner": "generic",
  "subscanners": ["gitleaks", "trivy", "semgrep-auto"],
  "json_paths": {
    "gitleaks": ".../gitleaks.json",
    "trivy":    ".../trivy.json",
    "semgrep":  ".../semgrep-auto.json"
  },
  "finding_count": 42,
  "errors": []
}
```

Errors during scanner execution are captured under `errors[]` but do
NOT fail the tool node unless all scanners failed.
