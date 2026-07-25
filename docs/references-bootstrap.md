# References bootstrap — deepsec + harness clones

Seki's `--var enable_deepsec=true` and its `external-security-tools`
skill both reach into a local references directory that holds two
upstream projects:

- [`vercel-labs/deepsec`](https://github.com/vercel-labs/deepsec)
  — Apache-2.0. The optional scanner backend (`scanner-deepsec` skill).
- [`anthropics/defending-code-reference-harness`](https://github.com/anthropics/defending-code-reference-harness)
  — Anthropic reference implementation. The source for Seki's ported
  `threat-model`, `vuln-scan`, `triage`, `patch` skills (see
  [`HARNESS-ATTRIBUTION.md`](../bots/sec-audit-source/skills/HARNESS-ATTRIBUTION.md)).

This page covers how to clone them, where Seki looks for them, and
what `--var enable_deepsec=true` actually does with them.

## Default location

```
~/lab/ai/references/
├── deepsec/                                  (vercel-labs/deepsec, Apache-2.0)
├── defending-code-reference-harness/         (anthropics, reference impl)
├── COMPARISON.md                             (cross-tool gap analysis)
└── README.md                                 (what this dir is for)
```

Override via the `ITERION_REFERENCES_ROOT` env var when you want to
keep the clones elsewhere:

```bash
export ITERION_REFERENCES_ROOT=$HOME/work/refs
# Seki then resolves $ITERION_REFERENCES_ROOT/deepsec and
# $ITERION_REFERENCES_ROOT/defending-code-reference-harness
```

The `run_deepsec_scanner` tool node in `bots/sec-audit-source/main.bot`
reads `${ITERION_REFERENCES_ROOT:-$HOME/lab/ai/references}` directly;
the bot var `deepsec_root` defaults to
`"${HOME}/lab/ai/references/deepsec"` (override with
`--var deepsec_root=...`).

## Bootstrap

```bash
# 1. Create the references dir
mkdir -p ~/lab/ai/references
cd ~/lab/ai/references

# 2. Clone both
git clone https://github.com/vercel-labs/deepsec.git
git clone https://github.com/anthropics/defending-code-reference-harness.git

# 3. (Optional but recommended) pin to a known-good sha for reproducibility
git -C deepsec checkout e3c8f05a0a72e3466e82bdcdda86e7c65fc2ad3b
git -C defending-code-reference-harness checkout 9e0f6c6cd54fc3b8ce79708e8208d862634a2624
```

The pinned shas above are the ones the iterion COMPARISON.md was
written against. Bumping them is fine but treat it as a Seki-affecting
change: deepsec's CLI surface or the harness's slash-command shape can
shift between commits.

## Node 22 requirement (deepsec only)

deepsec is a pnpm/TypeScript monorepo that requires **Node ≥ 22**.
Earlier versions silently fail to install or run. Check:

```bash
node --version       # must report v22.x or higher
```

The `run_deepsec_scanner` tool node checks the Node major version
before calling the CLI and degrades gracefully (envelope carries
`errors[]`, run never fails) when it is missing or `<22`:

```
deepsec unavailable: node binary not found on PATH
deepsec unavailable: node <X> < 22 (deepsec requires Node 22+)
```

The harness has no Node requirement of its own; its slash-command
skills run inside the local `claude` CLI.

## What `--var enable_deepsec=true` does

When set, the workflow:

1. **Probes** for a deepsec binary (`command -v deepsec`). When
   absent, the tool node returns a graceful-degrade envelope citing
   the reference clone path so the operator knows where it would
   have looked.
2. **Initialises** a per-run deepsec workspace at
   `<scan_dir>/deepsec-workspace/` (idempotent — only runs `deepsec init`
   when `deepsec.config.ts` is absent).
3. **Scans** the target workspace with deepsec's ~110-matcher regex
   pack (`deepsec scan`).
4. **Processes** up to `--var deepsec_process_limit` files (default
   `50`) at concurrency `--var deepsec_concurrency` (default `4`).
   This caps the LLM cost — deepsec's per-file batches can run
   $0.05–$0.30 with Opus.
5. **Exports** the JSON to `--var deepsec_out`
   (default `<scan_dir>/deepsec.json`).
6. Triage reads that JSON via `read_file` and maps each finding to a
   Seki candidate per the field-mapping table in
   [`scanner-deepsec.md`](../bots/sec-audit-source/skills/scanner-deepsec.md).

Provider selection (no API key plumbed):

- If `AI_GATEWAY_API_KEY` or `DEEPSEC_API_KEY` is set in the run env,
  deepsec routes through the Vercel AI Gateway (its native preflight).
- Otherwise the tool node passes `--agent claude` so deepsec reuses
  the local `claude` CLI subscription. The sec sandbox mounts
  `${localEnv:HOME}/.claude` into the container at
  `/home/devbox/.claude` so the OAuth credentials are visible to the
  spawned CLI.

```bash
# Default behaviour
devbox run -- iterion run bots/sec-audit-source/main.bot \
  --var workspace_dir=$(pwd) \
  --var enable_deepsec=true

# Override the references root + raise the process limit
ITERION_REFERENCES_ROOT=$HOME/work/refs \
  devbox run -- iterion run bots/sec-audit-source/main.bot \
  --var workspace_dir=$(pwd) \
  --var enable_deepsec=true \
  --var deepsec_process_limit=200 \
  --var deepsec_concurrency=8
```

When deepsec is disabled (the default), the `deepsec_gate` routes
straight to `scan_join` and the workflow is byte-identical to a
pre-enhancement-4 run. Triage's `deepsec_scan` input resolves to an
empty object via `if(vars.enable_deepsec, …, '')` so the runtime
never references a node that did not run.

`scan_health` surfaces a missing `deepsec.json` to `report_card` so a
degraded deepsec run shows up in the coverage banner — it is NOT
counted toward `min_generic_scanners` (so a missing deepsec NEVER
hard-fails the run — only flags as degraded).

## On-demand harness invocation (`external-security-tools` skill)

The harness's pipeline is C/C++/ASAN driven — **not applicable** to
Go/TS repos. But its slash-command skills (`/threat-model`,
`/vuln-scan`, `/triage`, `/patch`) are language-agnostic and run via
the local `claude` CLI.

The [`external-security-tools`](../bots/sec-audit-source/skills/external-security-tools.md)
skill is the on-demand entry point — used as a cross-check oracle when
a Seki verdict is uncertain, or as a fallback for languages Seki's
scanners do not cover. Invocation shape (always `--auto` for headless):

```bash
cd <workspace>

# Bootstrap a threat model (writes THREAT_MODEL.md in the target dir)
claude --print "/threat-model bootstrap ."

# Run the harness's adversarial triage on Seki's own VULN-FINDINGS.json
claude --print "/triage VULN-FINDINGS.json --auto --repo ."

# Static-mode patch on the top 5
claude --print "/patch TRIAGE.json --repo . --top 5"
```

Cross-check outputs are copied to `.sec-audit/cross-check/<ts>/`
to keep them out of the workspace root.

Both tools have the same read-only constraint Seki enforces: harness
`/patch` writes diffs to `./PATCHES/` (never `git apply`); deepsec
writes only scanner JSON and FileRecord caches. Seki forwards their
output through its own normalisation layer — it does not blindly
trust either.

## Attribution + licenses

Both clones are governed by their upstream licenses. Read them in the
clones themselves:

```bash
less ~/lab/ai/references/deepsec/LICENSE                                # Apache-2.0
less ~/lab/ai/references/defending-code-reference-harness/LICENSE       # Apache-2.0 (reference impl)
less ~/lab/ai/references/deepsec/NOTICE                                 # NOTICE file
```

Seki's bundled ports of the harness skills (`threat-model`, `vuln-scan`,
`triage`, `patch`) carry an `attribution:` frontmatter field naming the
upstream skill and the iterion-side adaptation. See
[`HARNESS-ATTRIBUTION.md`](../bots/sec-audit-source/skills/HARNESS-ATTRIBUTION.md)
for the full mapping.

## Troubleshooting

| Symptom | Likely cause | Check |
|---|---|---|
| `deepsec unavailable: node binary not found on PATH` | Node 22 not in the sandbox image | `node --version` inside the sec sandbox; rebuild `iterion-sandbox-sec` |
| `deepsec unavailable: node <N> < 22 (deepsec requires Node 22+)` | Old Node | Upgrade Node ≥ 22 |
| `deepsec unavailable: deepsec CLI not on PATH (reference clone exists at <path>…)` | The clone is there but no global install | `npm install -g deepsec`, or rebuild the sec sandbox |
| `deepsec unavailable: neither global deepsec CLI on PATH nor reference clone at <path>` | Neither path resolves | Clone deepsec into `$ITERION_REFERENCES_ROOT/deepsec` (or set the env var) |
| `claude CLI not on PATH` when invoking the harness skills | `@anthropic-ai/claude-code` not installed | The sec sandbox's `post_create` hook auto-installs it; outside the sandbox, `npm install -g @anthropic-ai/claude-code` |

## See also

- [`security-bots.md`](security-bots.md) — Seki + Depsy overview, the 6 new capabilities, kanban labels
- [`security-patcher.md`](security-patcher.md) — Seki's remediation phase (ladder, modes, resume)
- [`skills/scanner-deepsec.md`](../bots/sec-audit-source/skills/scanner-deepsec.md) — deepsec field mapping into triage
- [`skills/external-security-tools.md`](../bots/sec-audit-source/skills/external-security-tools.md) — on-demand harness invocation patterns
- [`skills/HARNESS-ATTRIBUTION.md`](../bots/sec-audit-source/skills/HARNESS-ATTRIBUTION.md) — upstream credit for the ported skills
- [`~/lab/ai/references/README.md`](https://github.com/vercel-labs/deepsec) — the references dir's own README + the COMPARISON.md gap analysis
