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
- Runtime requirement: the global `deepsec` CLI on `PATH`, running on
  Node 22+.
- Optional reference clone: `~/lab/ai/references/deepsec/` (override via
  `ITERION_REFERENCES_ROOT`, or `--var deepsec_root=...`). It is used only
  to report a missing CLI precisely; Seki never executes it as a fallback.

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

The harness reference lives under `~/lab/ai/references/` by default;
override the root with:

```bash
export ITERION_REFERENCES_ROOT=~/path/to/your/refs
```

Seki resolves the harness as
`$ITERION_REFERENCES_ROOT/defending-code-reference-harness`. For deepsec,
the same lookup (or `deepsec_root`) only locates an optional reference
checkout used in the unavailable message. The executable contract remains
`deepsec` on `PATH`: an existing clone without the global CLI does not run.
A missing harness directory logs a one-line WARN and continues (see
"Graceful degrade" below).

## Headless invocation

### deepsec — claude-code provider fallback

deepsec normally drives a hosted LLM. For local subscription-based
use (no API key), Seki runs the global CLI with its claude fallback. The
shape is:

```bash
DSW={{vars.scan_dir}}/deepsec-workspace
deepsec init "$DSW" {{vars.workspace_dir}} --force
(cd "$DSW" && deepsec scan)
(cd "$DSW" && deepsec process --concurrency {{vars.deepsec_concurrency}} \
  --limit {{vars.deepsec_process_limit}} --agent claude)
(cd "$DSW" && deepsec export --format json --out {{vars.deepsec_out}})
```

`run_deepsec_scanner` wraps those calls with its provider, timeout, retry,
coverage and per-run path guards. It derives the exported file as
`{{vars.scan_dir}}/deepsec-out-<run.id>/deepsec.json` and publishes it in
`json_paths.deepsec`, the only route by which triage, `scan_health`,
`bank_deepsec_findings` and `cap_findings` reach a deep-scan export. A
flat `{{vars.scan_dir}}/deepsec.json` is the pre-0.1.4 shared slot — the
scanner node removes it once a pass gets past its preflight, and no node
harvests it — so a hand-run export reaches triage only through
`json_paths`, never by sitting under `scan_dir`.

Requirements:
- `deepsec` is on `$PATH` (for example through the pinned sec image).
- `node --version` reports 22.x or higher.
- `claude` is on `$PATH` and authenticated for the fallback, or a gateway
  credential selects the hosted provider.
- `claude` is the model-side authentication, not Anthropic's hosted API.
  No `ANTHROPIC_API_KEY` is needed for that fallback.
- `deepsec_root` is NOT a runtime prerequisite: it only points at an
  optional reference checkout so a missing CLI can be diagnosed precisely.

deepsec 2.3.9 exports a bare findings array. Each finding carries a
`[SEVERITY]`-prefixed `title`, `description`, `severity` and `labels` at
the root. `labels` always begins with `security`, `project:<projectId>`,
`severity:<severity>`, `slug:<vulnSlug>` and `confidence:<confidence>`;
`owning-team:<slug>` entries appear only when the exporter supplied teams.
`filePath`, `lineNumbers`, `vulnSlug`, `confidence`, `runId`, `projectId`,
`severity`, `discoveredAt` and `owners` live under `metadata`; `owners` is
an OwnerSummary object whose arrays may be empty: `teams[]` objects
`{name, slug}`, `oncall[]` objects `{name, email, …}`, `managers[]`
objects with at least `{email}`, `contributors[]` objects
`{name, email, score, …}`, and `recentCommitters[]` objects
`{name, email, date}`. Root/owner assignee fields exist only as
exporter-derived data when oncall exists. `description` starts with
`**File:** \`<path>\` (lines n, n, …)`, `**Project:**`, a
`**Severity:** / **Confidence:** / **Slug:**` line, then an
exporter-supplied `## Owners` section only when owner data exists, before
`## Finding` and `## Recommendation`; there is no separate root
recommendation field and no Evidence section. Seki maps that shape to its
candidate schema and maps `metadata.vulnSlug` to `[[finding-taxonomy]]`;
see `[[scanner-deepsec]]`.

`cap_findings` recognizes the export by the inode identity of the path
published in `DEEPSEC_PATHS`, never by filename, realpath spelling, or
array shape. Deepsec findings keep their metadata and full long
descriptions through severity sorting and capping, both inline and on
disk; other scanners retain the standard trimming. Triage applies the
same mapping to either transport.

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

Seki MUST handle each external tool being absent. deepsec's scanner node
always exits 0 and reports the fact as DATA in its `scanner_output.errors[]`
(not as a WARN): `json_paths` stays empty, coverage is marked
`deepsec_unavailable`, and `scan_health`/`report_card` surface that state.
The harness path can still log a WARN when its reference clone is absent:

```
deepsec unavailable: deepsec CLI not on PATH (reference clone exists at <path>...)   # scanner_output.errors[]
deepsec unavailable: node <N> < 22 (deepsec requires Node 22+)                     # scanner_output.errors[]
harness: not found at $ITERION_REFERENCES_ROOT/defending-code-reference-harness   # WARN
```

The degrade rules:

- A missing external tool MUST NOT fail the workflow.
- The kanban report should note "deepsec not available" in the
  finding's `scanner_rationale` when a category that would have
  benefited from it was scanned with only the bundled scanners —
  so a human reading the report knows precision was lower.
- deepsec is an OPTIONAL, separately gated scanner branch alongside
  `run_generic_scanners`, not a sub-branch of the per-language tools;
  disabling or losing it leaves those language branches unchanged.

## Determinism considerations

- deepsec's shared data workspace (`scan_dir/deepsec-workspace`) adds a
  non-deterministic warm/cold cache layer by design; the per-run export and
  logs stay keyed on `run.id` beside it, so reused records never change
  attribution.
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
