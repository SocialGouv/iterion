---
name: js-xray
description: |
  js-x-ray AST malware analysis for npm/ts/js packages — the
  @nodesecure analyzer the no-package-malware project relied on. How
  the scanner runs (the iterion:heuristics entry in lang-js.md), what
  jsxray.json contains, and how the LLM reviewer maps its warnings to
  malware-signals ids.
---

# js-xray — static AST malware analysis for npm

[js-x-ray](https://github.com/NodeSecure/js-x-ray) parses JavaScript /
TypeScript with an AST and flags the patterns that distinguish malware
from normal code: obfuscation, encoded payloads, dynamic requires,
suspicious globals, environment serialization, and short-identifier
minification used to hide intent. It is the analyzer the
no-package-malware registry gate used and the core of supply-shield's
npm malware coverage — far more precise than grep.

## How it runs

The `run_jsxray` tool node invokes **`iterion-jsxray`**, a self-contained
tool shipped in the `iterion-sandbox-sec` image (its own `node_modules` +
an ESM walker under `/opt/iterion-jsxray`, with a PATH shim — js-x-ray
15.x is ESM-only so it cannot be imported by a bare specifier from an
arbitrary cwd). The walker runs js-x-ray's `AstAnalyser().analyseFileSync`
over each package's **install scripts + entry points** and writes
`$SCAN_DIR/jsxray.json`. It runs on a bare checkout — no `npm install`
needed — walking `node_modules/<pkg>` (depth ≤ 2).

The scan targets, per package, only the high-signal files (the same
"entry-point" scope as `[[lang-js]]`):

- `package.json:scripts.{preinstall,install,postinstall,prepare}`
  target `.js`/`.cjs`/`.mjs` files,
- `main` / `module` / `bin.*` entry points.

Deep transitive source is out of scope — install-time + import-time
code is where npm malware executes.

## jsxray.json shape

A JSON array, one object per scanned file (js-x-ray 15.x shape):

```json
[
  {
    "package": "evil-pkg",
    "version": "2.1.0",
    "file": "node_modules/evil-pkg/postinstall.js",
    "warnings": [
      {"kind": "unsafe-import",  "value": "child_process", "location": [[12,0],[12,40]]},
      {"kind": "unsafe-command", "value": "curl ...",      "location": [[14,0],[14,60]]},
      {"kind": "unsafe-stmt",    "value": "eval",          "location": [[3,10],[3,40]]},
      {"kind": "encoded-literal","value": "aHR0cHM6...",   "location": [[3,18],[3,38]]}
    ],
    "flags": ["is-minified"]
  }
]
```

(If `iterion-jsxray` is unavailable the node writes nothing;
`coverage_gate` banners "js-x-ray AST analysis did not run for npm
changes" so the gap is never silently presented as clean.)

## Warning kinds → malware-signals ids

The LLM reviewer reads `jsxray.json` and folds each warning into the
package's signals using the canonical ids from `[[malware-signals]]`
(js-x-ray 15.x kinds):

| js-x-ray `kind`               | malware-signals id            | weight |
|-------------------------------|-------------------------------|--------|
| `unsafe-import` (child_process / vm) | `child-process-on-import` | 20 |
| `unsafe-import` (http/https/net/dns) | `network-on-import`       | 20 |
| `unsafe-command`              | `child-process-on-import`     | 20 |
| `unsafe-stmt` (eval / Function / vm) | `eval-on-startup`        | 25 |
| `encoded-literal`             | `base64-blob`                 | 20 |
| `obfuscated-code`             | `obfuscated-string`           | 10 |
| `suspicious-literal` / `shady-link` | `obfuscated-string` / network-exfil-shape | 10 |
| `unsafe-regex` / `short-identifiers` / `is-minified` flag alone | informational (no bump) | 5 |

`unsafe-regex`, `prototype-pollution`, and `is-minified` fire routinely on
legitimate popular libraries (typescript, minimatch, meriyah, …) — they
are NOT malware signals on their own. Treat them as noise unless they
co-occur with an install-script warning.

Two rules keep precision high:

1. A js-x-ray warning **on an install script** (`preinstall` …
   `postinstall`) is HIGH-confidence — install-time code that obfuscates,
   evals, or reaches the network/process is the textbook npm malware
   shape. Combine with the deterministic `install-hook` signal and bump
   per the reviewer protocol.
2. A warning **only on a normal entry point** of a popular, long-lived
   package (especially `unsafe-regex` / `is-minified`) is almost always a
   false positive; confirm by reading the file before raising risk, and
   only for packages in `pending[]` (the newly added deps).

## Deep-read fallback

When js-x-ray is inconclusive (e.g. it flags `is-minified` but nothing
else) and the package ships an install script, the reviewer reads that
script directly and judges intent — does it fetch-and-eval, read
`~/.npmrc` / `~/.ssh` / `process.env` secrets, or spawn a subprocess?
This is the no-package-malware GPT-tools method, done by the reviewer
agent with `read_file`.

## See also

- `[[lang-js]]` — the npm heuristics block that runs js-x-ray.
- `[[malware-signals]]` — the canonical signal ids.
- `[[supply-shield]]` — the orchestrating playbook.
