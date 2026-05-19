[← Documentation index](README.md)

# Security bots — `sec-audit-source` + `sec-audit-deps`

Iterion ships two complementary security audit bundles. They share a
threat-model vocabulary, kanban label conventions, and FP-memory
discipline, but address different layers of the application.

| Bundle | Layer audited | Inspired by |
|---|---|---|
| [`sec-audit-source`](../examples/sec-audit-source/) | Source code in the repo | [vercel-labs/deepsec](https://github.com/vercel-labs/deepsec) |
| [`sec-audit-deps`](../examples/sec-audit-deps/) | Installed third-party dependencies | [SocialGouv/no-package-malware](https://github.com/SocialGouv/no-package-malware) |

## When to run which

```
                       ┌─────────────────────────────────────────┐
                       │  An attacker can reach your application │
                       └────────────────────┬────────────────────┘
                                            │
        ┌───────────────────────────────────┴──────────────────────────────┐
        │                                                                  │
        ▼                                                                  ▼
┌────────────────────┐                                       ┌──────────────────────────┐
│  Vulnerability in  │                                       │  Malicious code shipped  │
│  YOUR source code  │                                       │  in your DEPENDENCIES    │
│  (SQLi, XSS, IDOR, │                                       │  (preinstall hooks,      │
│  hardcoded secret) │                                       │   eval on import,        │
│                    │                                       │   typosquats, …)         │
│   → sec-audit-     │                                       │   → sec-audit-deps        │
│     source         │                                       │                          │
└────────────────────┘                                       └──────────────────────────┘
```

Run both as part of pre-release hardening, or pick the one matching
the threat you're chasing.

## Architectural patterns shared by both

### Static signals → LLM with strict JSON schema

Both bots delegate **pattern matching to deterministic tools**
(scanners or heuristic extractors) and use the LLM only for
**normalisation + reasoning + emission**:

```
deterministic scanners ─┐
                         ├──► structured JSON signals ──► LLM agent ──► verdicts + kanban issues
heuristic extractors  ──┘
```

Rationale:
- Determinism: scanner output is identical on identical input.
- Coverage attribution: a missed vuln is traceable to a ruleset.
- Cost: scanners are free; LLM tokens are not.

### Two-phase judge for FP reduction

`sec-audit-source.revalidate` runs the deepsec-inspired pass-1
(promote/dismiss/uncertain) followed by pass-2 self-critique that
specifically hunts for façades and over-relied dismissals. See
the `revalidate_system` prompt and
[memory `feedback_judge_two_phase`](../README.md).

### Cross-run memory

Each bot retains state between runs so it doesn't repeat itself:

| Bot | Memory | Location | Visibility |
|---|---|---|---|
| `sec-audit-source` | False positives | `.iterion/security/fp-known.yaml` in the scanned repo | Committed; human-reviewable |
| `sec-audit-deps` | Per-package verdicts | `~/.iterion/security-cache/packages.jsonl` | Host-wide; auto-mounted in sandbox via `host_state: auto` |

The `sec-audit-source` FP memory is per-repo because false positives
are pattern-specific: `urlSafe(input)` may guard an SSRF in repo A
and miss in repo B. The `sec-audit-deps` package cache is host-wide
because a published `name@version+checksum` is universally
identifiable.

### Capability-gated board writes

The node that creates kanban issues declares the minimum capability
set it needs:

```
report_card / llm_review:
  capabilities: [board.read, board.create, board.label]
```

Everything else (`detect_tech`, scanner tools, `triage`,
`revalidate`, …) runs `readonly: true` without board capabilities.
A capability-denied attempt to write the board surfaces as a hard
runtime error.

### Per-language extensibility

Both bots structure their language coverage as **one skill +
one router branch per language**. Adding a language:

1. Add `skills/lang-<id>.md` describing scanners / heuristics for
   the language.
2. Add a `run_<id>_scanners` (or `_heuristics`) tool node in
   `main.bot`.
3. Wire the router with a `when has_<id>` condition.

V1 ships:
- `sec-audit-source`: JS/TS, Go, Python + always-on generic (gitleaks
  + trivy + semgrep auto).
- `sec-audit-deps`: npm/yarn/pnpm, pip/poetry/uv, go modules +
  always-on generic.

Roadmap candidates: PHP, Ruby, Rust, JVM (Maven/Gradle), .NET (NuGet).

## Kanban label conventions

| Label | Emitted by | Purpose |
|---|---|---|
| `severity:<low\|medium\|high\|critical>` | both | Bucketing on the board |
| `type:<finding-type>` | sec-audit-source | One of 12 from `[[finding-taxonomy]]` |
| `type:supply-chain-<signal-id>` | sec-audit-deps | Primary signal that triggered the issue |
| `scanner:<id>` | sec-audit-source | Primary scanner (`semgrep`, `gosec`, `bandit`, `gitleaks`, `trivy`) |
| `ecosystem:<id>` | sec-audit-deps | `npm`, `pypi`, `gomod` |
| `source:sec-audit-source` / `source:sec-audit-deps` | both | Lets a remediation bot filter to security findings |
| `triage-uncertain` | sec-audit-source | Revalidate judge couldn't decide; human review needed |

A future remediation bot can list issues via
`mcp__iterion_board__list_issues` with `labels: ["source:sec-audit-source"]`
to scope itself to security findings.

## Comparison with deepsec

[deepsec](https://github.com/vercel-labs/deepsec) and `sec-audit-source`
target similar problems with different ergonomics. Trade-offs:

| Property | deepsec | `sec-audit-source` |
|---|---|---|
| Distribution | Vercel AI Gateway + Vercel Sandbox; npx install | Self-hosted; bundled with iterion |
| Distributed execution | Yes (`--sandboxes N --concurrency M`) | V1: single-process; roadmap: cloud queue fan-out via iterion's cloud mode |
| Matcher expressiveness | Programmatic TS plugins (full filtering / multi-pattern / per-file state) | Scanner-based (semgrep / gosec / bandit / gitleaks / trivy) + custom semgrep YAML rules |
| Per-file append-only records | Yes (atomic locking, distributed-ready) | V1: scan-time only; roadmap: file records + per-file resume |
| Operator-visible FP suppression | `revalidate` verdicts in workspace | Committed `.iterion/security/fp-known.yaml` |
| Integration with project boards / dispatcher | None built-in | Native: each finding is a kanban issue, routable to a remediation bot |
| Budget headroom | Tuned for $1000s/scan large monorepos | Tuned for $25/scan repos; cloud mode scales further |

The roadmap in this bundle's plan file aims to close the
distributed-execution gap (Cap. 3 — cloud queue) and the
programmatic-matchers gap (Cap. 2) over upcoming versions.

## Comparison with no-package-malware

[no-package-malware](https://github.com/SocialGouv/no-package-malware)
solves a different problem: it's a **Verdaccio gateway** that
intercepts npm installs at the registry boundary. `sec-audit-deps`
runs **after** the install, on the installed tree.

| Property | no-package-malware | `sec-audit-deps` |
|---|---|---|
| Position | In front of `npm install` (registry proxy) | Post-install audit |
| Enforcement | Fail-closed: blocks `npm install` of risky versions | Advisory: surfaces issues, doesn't block install |
| Ecosystems | npm only | npm + pip + go modules (extensible) |
| State | MongoDB + Redis + Verdaccio + workers | Bundle-only (one JSONL file) |
| Org / token / budget model | Yes (per-org Verdaccio tokens) | None; runs on whoever invoked `iterion run` |

Use both if you can: the gateway prevents known-bad versions from
ever landing on disk; the audit-bundle catches what slipped through
+ provides a board-integrated remediation surface.

## See also

- [`examples/sec-audit-source/README.md`](../examples/sec-audit-source/README.md)
- [`examples/sec-audit-deps/README.md`](../examples/sec-audit-deps/README.md)
- [`docs/bundles.md`](bundles.md) — bundle layout + runtime resolution
- [`docs/native-tracker.md`](native-tracker.md) — the kanban board
  where findings land
- [`docs/workflow_authoring_pitfalls.md`](workflow_authoring_pitfalls.md) —
  Goodhart's law in workflow design (façade resistance), required
  reading if you author `.iter` files
