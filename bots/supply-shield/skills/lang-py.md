---
name: lang-py
description: |
  pip / poetry / uv heuristic reference for sec-audit-deps. Covers
  setup.py / pyproject.toml install hooks, pip-audit vuln DB
  lookup, and installed dist-info walk. Loaded when tech.ecosystems
  ∋ pypi.
---

# lang-py — Python ecosystems (pip, poetry, uv)

Activated when `enumerate_deps` finds any of: `pyproject.toml`,
`setup.py`, `requirements*.txt`, `Pipfile`, `poetry.lock`,
`uv.lock`, or a populated `.venv/` / `venv/`.

## Enumeration

Source order:
1. `.venv/lib/python*/site-packages/<pkg>-<ver>.dist-info/METADATA`
   when virtualenv is populated.
2. Lockfile (`poetry.lock` / `uv.lock` / `requirements.lock`).
3. As a last resort: parse the requirement spec strings from
   `requirements.txt` (versions may be ranges — record range, mark
   as "unresolved" in the cache).

Checksum: from lockfile `hashes:` field or `[[package.files]]`
section. Convert any non-sha256 to sha256 by re-hashing the wheel.

## Heuristic logic (signals emitted)

Run per installed package:

### `install-hook`
Two flavours, since Python doesn't have npm's lifecycle scripts:

a) **setup.py custom cmdclass**:
   - Read `setup.py` (if shipped). Look for
     `cmdclass={...: <SomethingCustom>}`, `setup(..., scripts=...)`,
     or any imperative code (not just `setup()` call) at module top
     level.
   - Evidence: `setup.py:<line>`.
   - Score: 15.

b) **pyproject.toml entry points / scripts**:
   - `[tool.poetry.scripts]` or `[project.scripts]` entries.
   - `[build-system].build-backend` referencing a custom backend
     (i.e. not setuptools / poetry-core / hatchling / pdm / flit).
   - Evidence: `pyproject.toml:<line>`.
   - Score: 10.

### `eval-on-startup`
- For files in `__init__.py` and module-level Python files (those
  reachable by `import <pkg>`), grep for `eval(`, `exec(`,
  `compile(...)`, `__import__('os').system(`.
- Evidence: `<file>:<line>`.
- Score: 25.

### `network-on-import`
- For `__init__.py` and modules imported at top level, grep for
  `urllib.request.`, `requests.get(`, `requests.post(`,
  `httpx.`, `socket.`, `urllib3.`.
- Heuristic: if found at module top-level (indentation 0,
  outside `if __name__ == "__main__":`), emit.
- Score: 20.

### `obfuscated-string`
- Shannon entropy > 4.5 bits/char on string literals > 60 chars
  in `__init__.py` and `setup.py`.
- Also flag `base64.b64decode(...).decode(...)` at top level →
  emit BOTH `obfuscated-string` AND `base64-blob`.
- Score: 10 (just obfuscation) / 20 (with base64 decode).

### `child-process-on-import`
- Grep entry-point files for `subprocess.run(`, `subprocess.Popen(`,
  `os.system(`, `os.exec*(`, `os.popen(`.
- Skip when inside `if __name__ == "__main__":`.
- Score: 20.

### `binary-payload`
- `find <dist-info-dir> -size +50k -type f -name '*.so' -o -name '*.pyd'`
  for compiled extension modules.
- Some legitimate packages ship .so (numpy, scipy). Whitelist
  by name: if `name` ∈ top-1000-py-list with > 1y registry
  history, downgrade score from 15 to 5.

### `typosquat-shape`
- Edit distance ≤ 2 from top-1000 PyPI packages. Same logic as
  lang-js but against `attachments/typosquat-list-pypi.txt`.
- Score: 20.

### `version-rug-pull`
- Compare uploader of the installed wheel (from PyPI metadata
  cached locally or fetched via `pip show -v`) with the
  package's primary maintainer on PyPI today.
- Score: 35.

### `vuln-db-known`
- `pip-audit --format=json --requirement requirements.txt` once.
- Cross-reference advisories with installed versions.
- Score: 20 / 35 by CVSS.

## Tool node skeleton

```bash
mkdir -p {{vars.scan_dir}}/heuristics
pip-audit --format=json --output={{vars.scan_dir}}/heuristics/pip-audit.json || true

python3 <<'PY' > {{vars.scan_dir}}/heuristics/py.json
import os, json, ast, math, glob
# 1. Walk .venv/lib/*/site-packages/*.dist-info/METADATA
# 2. For each pkg, locate __init__.py + setup.py
# 3. Run signal checks (AST parse for top-level network calls
#    is more reliable than grep — uses ast.walk)
# 4. Cross-reference pip-audit.json
# 5. Emit {packages: [...], errors: []}
PY
```

## AST-based detection (preferred over grep)

For Python, AST parsing catches what grep misses. Example for
`network-on-import`:

```python
import ast
def has_network_on_import(path):
    tree = ast.parse(open(path).read())
    for node in tree.body:                         # top-level only
        for sub in ast.walk(node):
            if isinstance(sub, ast.Call):
                func = getattr(sub.func, 'id', None) or \
                       getattr(getattr(sub.func, 'attr', None), '__str__', lambda: '')()
                if func in {'urlopen', 'get', 'post', 'request'}:
                    return True, ast.get_source_segment(open(path).read(), node)
    return False, None
```

This is precise (no false positives from comments / strings) but
adds ~50 ms per file. For workspaces > 10k Python files we may
need to AST-parse only files matching `[__init__.py, setup.py]`
and grep the rest.

## Scope of "entry-point files"

- `__init__.py` of installed packages
- `setup.py`
- `pyproject.toml` `[build-system]` backend module's source (if
  custom)
- Any file referenced by `entry_points` / `[project.scripts]`

## See also

- `[[malware-signals]]`
- `[[package-cache]]`
- `[[supply-shield]]`

## Heuristic SCA scanner (machine-readable — consumed by run_eco_heuristics)

Deterministic. `run_eco_heuristics` (a tool node, no LLM) runs `cmd` with
`$SCAN_DIR` in the env and cwd = workspace, then parses `output`. To adjust
Python SCA, edit this block — no DSL change.

<!-- iterion:heuristics
[ {"id":"pip-audit","output":"pip-audit.json","cmd":"{ [ -f pyproject.toml ] || [ -f requirements.txt ] || [ -f setup.py ]; } && command -v pip-audit >/dev/null 2>&1 && pip-audit --format=json --output=$SCAN_DIR/pip-audit.json.tmp 2>/dev/null && mv $SCAN_DIR/pip-audit.json.tmp $SCAN_DIR/pip-audit.json || true"} ]
-->
