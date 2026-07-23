---
name: vuln-matchers-guide
description: |
  How sec-audit-source uses external scanners as matchers, and how
  to add custom matchers for project-specific vulnerability shapes.
  Adapted from vercel-labs/deepsec's writing-matchers.md but
  reframed for the iterion `tool` + `agent` boundary.
---

# Vulnerability matchers — strategy and customisation

This bot does NOT implement matchers in Go. Matchers are external
scanners (semgrep, gosec, bandit, gitleaks, trivy) invoked from
`tool` nodes, plus optional **project-specific semgrep rules** the
operator can ship as bundle attachments.

## The matcher boundary

```
   ┌─ scanners (deterministic) ─────────────────────────┐
   │  semgrep, gosec, bandit, gitleaks, trivy           │
   │  produce raw findings in scanner-native JSON       │
   └─────────────┬──────────────────────────────────────┘
                 │
                 ▼
   ┌─ triage agent (claw, readonly) ────────────────────┐
   │  reads scanner JSONs, [[finding-taxonomy]], and    │
   │  [[fp-memory]]; emits normalised candidates[]      │
   └─────────────┬──────────────────────────────────────┘
                 │
                 ▼
   ┌─ revalidate judge (claw, two-phase) ───────────────┐
   │  reads candidate + file context (±50 lines);       │
   │  confirms / dismisses / flags uncertain            │
   └────────────────────────────────────────────────────┘
```

**The LLM never invents matchers.** It normalises raw scanner
output, applies taxonomy, and reasons about exploitability. The
scanners do the pattern matching.

This boundary is deliberate. Three reasons:

1. **Determinism.** Scanners with the same rules + same code emit
   identical output; the LLM only varies in its reasoning step.
2. **Coverage attribution.** When a vuln is missed, you know which
   ruleset to extend, not "the LLM didn't notice".
3. **Cost.** Scanners are free; LLM tokens are not.

## Built-in matcher sets

| Layer | Scanner | Default ruleset |
|---|---|---|
| Generic | semgrep | `--config=p/default` (semgrep's curated cross-language pack) |
| Generic | gitleaks | builtin gitleaks rules |
| Generic | trivy fs | builtin `--scanners=vuln,misconfig,secret` |
| JS/TS | semgrep | `--config=p/javascript,p/typescript,p/nodejsscan` |
| Go | semgrep | `--config=p/golang` |
| Go | gosec | `-include=G101,G102,...,G505` (all G-rules) |
| Python | semgrep | `--config=p/python,p/django,p/flask,p/fastapi` |
| Python | bandit | `-r` recursive, default plugins |

## Adding project-specific matchers (semgrep rules)

Drop a YAML file under `attachments/semgrep/`:

```yaml
# attachments/semgrep/my-org-rules.yaml
rules:
  - id: my-org.route-no-auth
    pattern-either:
      - pattern: |
          export $METHOD = async ($REQ, $RES) => { ... }
      - pattern: |
          export default async function ($REQ, $RES) { ... }
    pattern-not-inside: |
      withAuth($X)
    message: HTTP handler exported without withAuth() wrapper
    languages: [typescript, javascript]
    severity: WARNING
    metadata:
      finding_type: auth        # picked up by triage normalisation
```

Register it in `manifest.yaml`:

```yaml
attachments:
  my_org_rules: attachments/semgrep/my-org-rules.yaml
```

Then reference it in the relevant scanner tool node command:

```iter
tool run_js_scanners:
  command: |
    semgrep \
      --config=p/javascript \
      --config=p/typescript \
      --config={{attachments.my_org_rules}} \
      --json --output={{vars.scan_dir}}/js.json \
      {{vars.workspace_dir}}
```

The `triage` node picks up `metadata.finding_type` from the
semgrep output and maps the candidate directly without LLM
disambiguation.

## When custom matchers help

You want a custom matcher when:

- A vulnerability class is **specific to your codebase** (e.g.
  every public handler MUST call `withAuth()`; the matcher is
  "exported handler without `withAuth` wrapper"),
- The scanner registry doesn't cover a framework you use
  (proprietary SDK, internal RPC), or
- The default ruleset is too noisy (write a stricter pattern with
  more `pattern-not` exclusions).

You don't need a custom matcher for generic CWE categories — the
built-in rule packs already cover SQL injection, XSS, SSRF, weak
crypto, etc. across all supported languages.

## Tiering (informational)

deepsec uses three tiers (`precise` / `normal` / `noisy`) to
modulate scanner sensitivity. We don't expose this knob in V1 — the
bundled scanner configs are tuned for `precise` defaults, and the
`triage` agent's job is to drop low-signal findings before they
reach the board. If a future version surfaces too many low-signal
findings, the lever is in `triage` (raise the `severity` floor) or
in scanner config (drop a noisy rule pack), not in matcher
authoring.

## See also

- `[[finding-taxonomy]]` — finding_type enum
- `[[fp-memory]]` — how to suppress noisy matchers permanently
- `[[lang-js]]`, `[[lang-go]]`, `[[lang-python]]`, `[[lang-generic]]`
  — language-specific scanner invocations
