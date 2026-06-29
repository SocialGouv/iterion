---
name: lang-generic
description: |
  Language-agnostic supply-chain heuristics for sec-audit-deps.
  Covers binary entropy, base64-blob detection, locale anomalies,
  and the typosquat distance check across all ecosystems. Always-on.
---

# lang-generic — universal heuristics

Runs alongside every per-ecosystem heuristic node. The signals
here apply regardless of language because they are properties of
the artifact (binary content, name shape) not the manifest.

## Signals emitted

### `binary-payload`
- For each installed package directory: `find <dir> -size +50k
  -type f \( -name '*.bin' -o -name '*.exe' -o -name '*.so' -o
  -name '*.dll' -o -path '*/bin/*' \)`.
- Per-ecosystem skills emit this too; in `[[lang-generic]]` we
  cover paths NOT under the standard module dirs (e.g. embedded
  binaries in a Python wheel's `data/` dir).
- Score: 15.

### `base64-blob`
- Grep installed code for `^[A-Za-z0-9+/=]{200,}$` (base64-shape
  strings ≥ 200 chars on a single line).
- Cross-check: is the surrounding code calling `base64_decode` /
  `atob` / `base64.b64decode` / `Buffer.from(_, 'base64')`? If
  yes, score 20; else 10 (could be legitimate embedded data).
- Score: 10 / 20 (per cross-check).

### `locale-anomaly`
- Package name contains Cyrillic, Greek, or other non-Latin
  scripts mixed with Latin (homoglyph attack):
  `[А-Яа-я]` for Cyrillic homoglyphs of Latin letters
  (`а`, `с`, `е`, `о`, `р`, `х`, …).
- Or: the package name is identical to a top-1000 package
  when normalised through Unicode NFKD (catches `r᎒quest` vs
  `request`).
- Score: 25.

### `typosquat-shape` (cross-ecosystem)
- Edit distance ≤ 2 from a top-1000 package in the SAME
  ecosystem (npm vs pypi vs gomod — don't cross-check between
  ecosystems, names overlap legitimately).
- See `[[lang-js]]`, `[[lang-py]]`, `[[lang-go]]` for ecosystem
  whitelist sources.
- Score: 20.

### `tarball-anomaly`
- For npm-style tarballs / Python wheels with their own structure:
  - Detect symlinks pointing outside the package root.
  - Detect file paths containing `../`.
  - Detect setuid/setgid bits.
- Score: 30 (these are unambiguous abuse vectors).

### `manifest-version-mismatch`
- Compare the version declared in `package.json` / `setup.py` /
  inside the artifact vs the version requested in the lockfile.
- A mismatch is rare and concerning.
- Score: 20.

## Tool node skeleton

```bash
mkdir -p {{vars.scan_dir}}/heuristics
python3 <<'PY' > {{vars.scan_dir}}/heuristics/generic.json
import os, json, re, hashlib, unicodedata
# 1. For each pending package (passed via env or stdin), locate its
#    installed-on-disk path.
# 2. Run binary-payload, base64-blob, locale-anomaly,
#    typosquat-shape, tarball-anomaly, manifest-version-mismatch.
# 3. Emit {"packages": [...], "errors": []}.
PY
```

## Deduplication with ecosystem-specific signals

`binary-payload` is emitted by both lang-* (per-ecosystem) and
lang-generic. The aggregator in `llm_review` dedupes by
`(ecosystem, name, version, signal.id, evidence_path)`.

`typosquat-shape` is split: lang-generic does the edit-distance
calculation; the ecosystem-specific lang skill provides the
whitelist of legitimate maintainers to skip false positives.

## See also

- `[[malware-signals]]`
- `[[lang-js]]`, `[[lang-py]]`, `[[lang-go]]`
- `[[package-cache]]`
- `[[supply-shield-cve]]`
