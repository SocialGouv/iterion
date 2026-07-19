---
name: external-security-tools
description: |
  When and how to invoke external security tools from Seki:
  vercel-labs/deepsec (heavy-duty matchers, per-file FileRecord
  cache) and Anthropic's defending-code-reference-harness
  (threat-model / triage / patch reference). Headless invocation
  patterns, clone locations, and graceful-degrade rules when
  binaries are absent.
---

# external-security-tools — when to reach outside Seki

Seki is self-contained for the common path (`detect_tech` →
scanners → triage → revalidate → board). Two external toolkits
extend that envelope for specific cases. Both are optional;
Seki must degrade gracefully when they're absent.

## Tools

### deepsec (vercel-labs)

Heavy-duty matcher engine with a per-file FileRecord cache and a
high-precision pattern library. Stronger than Seki's bundled
matchers on a small set of curated categories; slower; depends on
Node 22+.

- Upstream: https://github.com/vercel-labs/deepsec
- Local clone: `~/lab/ai/references/deepsec/` (override via
  `ITERION_REFERENCES_ROOT`).
- Node 22+ required. Earlier versions silently fail.

### defending-code-reference-harness (Anthropic)

The reference implementation Seki's ported skills (`threat-model`,
`vuln-scan`, `triage`, `patch`) are adapted from. Useful as a
ground-truth reference when Seki misbehaves, and as a runnable
oracle for the harness's own targets (C/C++/ASAN — outside Seki's
Go/TS scope, but useful for ad-hoc reviews).

- Upstream: https://github.com/anthropics/defending-code-reference-harness
- Local clone: `~/lab/ai/references/defending-code-reference-harness/`
  (override via `ITERION_REFERENCES_ROOT`).

## When to reach for them

| Situation | Tool | Why |
|---|---|---|
| Hot category needs precision boost (auth-bypass, IDOR, custom RPC) | deepsec | Deeper matcher library than the bundled semgrep packs |
| Per-file cache too coarse with Seki's FileRecord | deepsec | deepsec's locking + record format is the design Seki's `[[file-records]]` is scaled down from |
| Cross-check a Seki verdict you're unsure of | harness | Run `/triage` on the same finding for a second opinion |
| Re-baseline the threat model against the original framework | harness | Run `/threat-model bootstrap` on the same target |
| C / C++ / Rust unsafe code in scope | harness | Seki's `[[patch]]` is Go/TS only; harness covers ASAN-driven patching |
| Auditing a target that isn't in `tech.langs` Seki supports | harness | Harness's focus-area subagents are language-agnostic |

If none of the above applies, stay inside Seki — adding a
sub-tool inflates run time and complicates failure modes.

## Clone location

Both tools live under `~/lab/ai/references/` by default. Override
with the env var:

```bash
export ITERION_REFERENCES_ROOT=~/path/to/your/refs
```

Seki resolves them as:

```
$ITERION_REFERENCES_ROOT/deepsec
$ITERION_REFERENCES_ROOT/defending-code-reference-harness
```

If a directory is absent, Seki logs a one-line WARN and continues
without it (see "Graceful degrade" below).

## Headless invocation

### deepsec — claude-code provider fallback

deepsec normally drives a hosted LLM. For local subscription-based
use (no API key), the bundled fallback drives the local `claude`
CLI:

```bash
DEEPSEC_PROVIDER=claude-code \
  node $ITERION_REFERENCES_ROOT/deepsec/cli.mjs \
  scan \
  --target {{vars.workspace_dir}} \
  --output {{vars.scan_dir}}/deepsec.json \
  --record-dir {{vars.workspace_dir}}/.sec-audit/deepsec-records
```

Requirements:
- `node --version` reports 22.x or higher.
- `claude` is on `$PATH` and authenticated.
- `claude` is the model-side authentication, not Anthropic's
  hosted API. No `ANTHROPIC_API_KEY` needed.

deepsec writes JSON in its native schema; Seki normalizes it into
the triage candidate shape via the same path the lang-specific
scanners use. Map deepsec categories to `[[finding-taxonomy]]`
during triage.

### harness — `claude --print` invocation

The harness's slash-command skills are invoked via the local
`claude` CLI in print mode:

```bash
cd {{vars.workspace_dir}}

# threat model (writes THREAT_MODEL.md in the target dir)
claude --print "/threat-model bootstrap ."

# vulnerability scan (writes VULN-FINDINGS.json)
claude --print "/vuln-scan ."

# triage of pipeline output or VULN-FINDINGS.json
claude --print "/triage VULN-FINDINGS.json --auto --repo ."

# patch (static mode, top 5)
claude --print "/patch TRIAGE.json --repo . --top 5"
```

The harness's `.claude/skills/` directory ships with the upstream
clone; nothing to install. Outputs land where the upstream
contract says:

- `<target>/THREAT_MODEL.md`
- `<cwd>/VULN-FINDINGS.json`, `<cwd>/VULN-FINDINGS.md`
- `<cwd>/TRIAGE.json`, `<cwd>/TRIAGE.md`
- `<cwd>/PATCHES/bug_NN/{patch.diff, patch_result.json}`

When using the harness as a cross-check, Seki copies the
`TRIAGE.json` into `.sec-audit/cross-check/<ts>/` rather
than letting it litter the workspace root.

## Graceful degrade

Seki MUST handle each external tool being absent. The bot logs a
WARN once per run, then continues:

```
deepsec: not found at $ITERION_REFERENCES_ROOT/deepsec (skipping)
harness: not found at $ITERION_REFERENCES_ROOT/defending-code-reference-harness (skipping)
node 22+ not on PATH (deepsec disabled)
claude CLI not on PATH (deepsec --provider claude-code disabled; harness skill invocation disabled)
```

The degrade rules:

- A missing external tool MUST NOT fail the workflow.
- The kanban report should note "deepsec not available" in the
  finding's `scanner_rationale` when a category that would have
  benefited from it was scanned with only the bundled scanners —
  so a human reading the report knows precision was lower.
- `[[sec-audit-source]]` phase 2's branch list is fixed; deepsec
  is an OPTIONAL sub-branch inside the per-language `tool` nodes,
  not a top-level branch.

## Determinism considerations

- deepsec's `--record-dir` adds a non-deterministic warm/cold
  cache layer. Put it under `.sec-audit/deepsec-records/`
  (gitignore-able) so it sits beside the file-record store in the
  repo's `.sec-audit/` dotdir instead of littering the source tree.
- The harness's `/triage` interview is interactive by default —
  always pass `--auto` from a headless Seki invocation.
- `claude --print` flushes once; capture stdout and stderr
  separately so the harness's progress prints don't pollute the
  parsed output.

## Security boundary

Both external tools have the SAME read-only constraint Seki
enforces: no `git apply`, no `patch`, no edits in
`{{vars.workspace_dir}}`. The harness's `/patch` writes diffs
under `./PATCHES/` (no apply). deepsec writes scanner JSON and
record files only. Seki forwards these outputs through its own
normalization layer; it does not blindly trust them.

## See also

- `[[sec-audit-source]]` — phase 2 scanner branches that may
  optionally invoke deepsec.
- `[[vuln-scan]]`, `[[triage]]`, `[[patch]]` — Seki's ports of
  the harness skills.
- `[[file-records]]` — Seki's per-file cache, scaled down from
  deepsec's FileRecord design.
- `HARNESS-ATTRIBUTION.md` — credit for the ported reference
  implementation.
