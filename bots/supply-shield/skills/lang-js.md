---
name: lang-js
description: |
  npm / yarn / pnpm heuristic reference for sec-audit-deps. Covers
  the install-hook signal extraction, `npm audit` vuln DB lookup,
  and node_modules walk strategy. Loaded when tech.ecosystems ∋ npm.
---

# lang-js — npm / yarn / pnpm

Activated by `run_js_heuristics` when `enumerate_deps` finds any of:
`package.json` at workspace root, `package-lock.json`, `yarn.lock`,
`pnpm-lock.yaml`, or a populated `node_modules/`.

## Enumeration

Source of installed packages:
1. **Preferred**: walk `node_modules/**/package.json` for installed
   resolved versions.
2. **Fallback** (no node_modules): parse lockfile for resolved
   `name@version+integrity` triples.

Checksum: from lockfile `integrity` field (sha512 base64) — convert
to `sha256:<hex>` for cache consistency. If absent, hash the
package tarball.

## Heuristic logic (signals emitted)

Run per installed package:

### `install-hook`
- Read `package.json:scripts`. Emit if any of `preinstall`,
  `install`, `postinstall`, `prepare` are set.
- Evidence: `package.json:scripts.<hook>=<command>`.
- Score: 15 base + 5 per additional hook (cap at 30).

### `eval-on-startup`
- For files in main / module / exports / bin entry points, grep
  for `eval(`, `new Function(`, `vm.runInNewContext(`.
- Skip strings inside comments (rough check: line starts with
  `*` or `//`).
- Evidence: `<file>:<line>:<snippet>`.
- Score: 25.

### `network-on-import`
- Grep entry-point files for `require('https?')`, `require('http')`,
  `require('net')`, `import 'undici'`, `globalThis.fetch(`.
- Heuristic: if found AT MODULE TOP-LEVEL (not inside a function),
  emit. Detect "top-level" by indentation 0 + no surrounding
  `function`/`=>` within 5 lines above.
- Score: 20.

### `obfuscated-string`
- Compute Shannon entropy of each string literal > 60 chars in
  entry-point files. Threshold: entropy > 4.5 bits/char.
- Also flag `String.fromCharCode(...)` sequences > 5 elements.
- Evidence: `<file>:<line>` with entropy value.
- Score: 10.

### `binary-payload`
- `find <pkg-dir> -size +50k -type f \( -name '*.bin' -o -name '*.exe' -o -name '*.so' -o -path '*/bin/*' \)`
- Score: 15.

### `typosquat-shape`
- Edit distance ≤ 2 from packages in the top-1000 npm list
  (bundled at `attachments/typosquat-list.txt`).
- Skip when the maintainer matches the original package's
  registered maintainer (read from `npm view` cache or skip if
  no network).
- Score: 20.

### `version-rug-pull`
- Compare package's "maintainer" field in the local
  `node_modules/.../package.json` (locked at install time) vs the
  current registry maintainer (`npm view <name> maintainers`). If
  different, emit.
- This catches the npm "ownership transfer attack" pattern.
- Score: 35.

### `removed-from-registry`
- `npm view <name>@<version> deprecated` returns non-empty OR
  `unpublishVersion: true`.
- Score: 15.

### `vuln-db-known`
- `npm audit --json` once at workspace root. Parse advisories.
- For each `(name, version)` with CVSS ≥ 7.0, emit with
  `cve_id`, `cvss`, `advisory_url` in evidence.
- Score: 20 (CVSS 7-8) / 35 (CVSS 9-10).

## Tool node skeleton

```bash
# Pseudocode for run_js_heuristics tool node
mkdir -p {{vars.scan_dir}}/heuristics
npm audit --json > {{vars.scan_dir}}/heuristics/npm-audit.json || true

python3 <<'PY' > {{vars.scan_dir}}/heuristics/js.json
import os, json, hashlib, math, subprocess, re
# 1. Walk node_modules/**/package.json
# 2. For each pkg, run signal checks
# 3. Cross-reference npm-audit.json for vuln-db-known
# 4. Emit {packages: [...], errors: []}
PY
```

## Scope of "entry-point files"

A file is an entry point if it's referenced by:
- `package.json:main` (legacy CJS)
- `package.json:module` (ESM)
- `package.json:exports.*.default` (modern)
- `package.json:bin.*`
- `package.json:scripts.{pre,post}install,prepare`

These are the files that run when `require()`, `import`, or
`npm install` happens. Deep dependency code is NOT an entry point
for the current package.

## Out of scope

- React component XSS, server SSRF, etc. — those are
  `sec-audit-source` territory.
- Lockfile linting (e.g. `package-lock.json` integrity mismatch).
  Add later via a dedicated signal.

## js-x-ray AST malware analysis (supply-shield)

The high-value npm MALWARE signal in supply-shield is **js-x-ray AST
analysis** (`jsxray.json`), the @nodesecure analyzer the
no-package-malware project relied on. It is produced by the always-on
`run_jsxray` tool node (it runs on a bare checkout, no `npm install`),
and the reviewer maps its warning kinds to the canonical signal ids —
see `[[js-xray]]`. The `npm audit` heuristic below is the CVE companion
(it needs an installed tree).

## See also

- `[[js-xray]]` — the js-x-ray runner + warning-kind → signal mapping.
- `[[malware-signals]]` — canonical signal catalogue.
- `[[package-cache]]` — how the verdict is persisted.
- `[[supply-shield]]` — the orchestrating playbook.

## Heuristic SCA scanner (machine-readable — consumed by run_eco_heuristics)

Deterministic. `run_eco_heuristics` (a tool node, no LLM) runs `cmd` with
`$SCAN_DIR` in the env and cwd = workspace, then parses `output`. To adjust
JS/npm SCA, edit this block — no DSL change.

<!-- iterion:heuristics
[ {"id":"npm-audit","output":"npm-audit.json","cmd":"[ -f package.json ] && command -v npm >/dev/null 2>&1 && npm audit --json > $SCAN_DIR/npm-audit.json.tmp 2>/dev/null; [ -s $SCAN_DIR/npm-audit.json.tmp ] && mv $SCAN_DIR/npm-audit.json.tmp $SCAN_DIR/npm-audit.json; true"} ]
-->
