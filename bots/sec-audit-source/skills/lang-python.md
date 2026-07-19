---
name: lang-python
description: |
  Python scanner reference for sec-audit-source. Covers semgrep
  with the python/django/flask/fastapi rule packs and bandit, plus
  framework-specific threat hints. Loaded when tech.langs ∋ python.
---

# lang-python — Python scanners

Activated by the `run_python_scanners` branch when `detect_tech`
reports `python` in `tech.langs`. Project layout signals: any of
`pyproject.toml`, `setup.py`, `requirements*.txt`, `Pipfile`, or
`*.py` files at workspace root.

## Primary scanner — semgrep

```bash
semgrep \
  --config=p/python \
  --config=p/django \
  --config=p/flask \
  --config=p/owasp-top-ten \
  --json \
  --output={{vars.scan_dir}}/py-semgrep.json \
  --error \
  --metrics=off \
  --quiet \
  --exclude='**/migrations/**' \
  --exclude='**/__pycache__' \
  --exclude='**/venv' \
  --exclude='**/.venv' \
  --exclude='**/test_*.py' \
  --exclude='**/*_test.py' \
  {{vars.workspace_dir}}
```

FastAPI users: add `--config=p/fastapi` when `detect_tech.frameworks
∋ fastapi`. Triage wires it dynamically by reading `frameworks` from
the upstream node.

## Secondary scanner — bandit

```bash
bandit \
  --recursive \
  --format=json \
  --output={{vars.scan_dir}}/bandit.json \
  --severity-level=low \
  --confidence-level=low \
  --skip B101 \
  --exclude='*/test_*.py,*/tests/*,*/.venv/*,*/venv/*,*/migrations/*' \
  --quiet \
  {{vars.workspace_dir}}
```

We skip B101 (assert_used) because it produces too much noise in
codebases that use asserts for control flow. Re-enable per-project
in `attachments/bandit.yaml` if desired.

bandit rule → `finding_type` mapping (high-signal subset):

| bandit | finding_type | severity floor |
|---|---|---|
| B102 (exec_used) | `injection` | high |
| B103 (set_bad_file_permissions) | `config` | medium |
| B104 (hardcoded_bind_all_interfaces) | `config` | medium |
| B106-B107 (hardcoded passwords) | `secrets` | critical |
| B201 (flask_debug_true) | `config` | high |
| B202 (tarfile_unsafe_members) | `path-trav` | high |
| B301-B307 (pickle/marshal/shelve/jsonpickle) | `deserialization` | high |
| B311 (random module for crypto) | `crypto` | medium |
| B324 (insecure hashing) | `crypto` | medium |
| B501 (request without TLS verify) | `crypto` | high |
| B502-B504 (SSL/TLS protocols) | `crypto` | high |
| B601-B609 (shell injection family) | `injection` | high |
| B701 (jinja autoescape false) | `xss` | high |

## Framework-specific threat hints

### Django
- `views` returning `HttpResponse(request.GET[...])` without
  escaping → `xss`.
- `ORM.raw(sql_string)` or `.extra(where=[...])` with user input
  → `injection`.
- `MIDDLEWARE` missing `CsrfViewMiddleware` → `config` high.
- `DEBUG = True` in `settings.py` at the workspace root (not in
  `settings_dev.py`) → `config` critical.
- `SECRET_KEY = "..."` hardcoded → `secrets` critical.
- Views without `@login_required` / `@permission_required` on
  state-changing actions → `auth`.

### FastAPI
- Endpoints without `dependencies=[Depends(verify_token)]` or
  equivalent → `auth`.
- `Request.url` or `request.headers.get('host')` used in redirect
  URLs without allowlist → `redirect`.
- `response_model` omitted on read endpoints leaks internal
  fields → `other` medium (info disclosure).

### Flask
- `app.config['DEBUG'] = True` → `config` high.
- `render_template_string(user_input)` → `injection` (SSTI) high.
- `send_file(request.args.get('path'))` → `path-trav` high.
- Routes without `@login_required` decorator → `auth`.

## setup.py / pyproject.toml hygiene

Examined by `triage` (not a scanner tool):
- `setup.py setup(cmdclass=...)` with custom install commands that
  invoke `subprocess.run` / `urllib.request.urlopen` → flag with
  `dependency-install-hook` (real audit happens in sec-audit-deps;
  surface here as an own-repo signal).
- `pyproject.toml` `[tool.setuptools]` with `cmdclass` entries that
  fetch network resources at install time.

## Excluded patterns

Don't flag:
- Test files (`test_*.py`, `*_test.py`, `tests/`)
- Generated migrations (`migrations/`)
- Virtual environments (`venv/`, `.venv/`, `env/`)
- `__pycache__/`

## Output

```json
{
  "scanner": "python",
  "subscanners": ["semgrep", "bandit"],
  "json_paths": {
    "semgrep": ".../py-semgrep.json",
    "bandit":  ".../bandit.json"
  },
  "finding_count": 31
}
```

## See also

- `[[lang-generic]]` — always-on layer.
- `[[finding-taxonomy]]` — required mapping.

## Scanners (machine-readable — consumed by run_lang_scanners + scan_health)

Deterministic scanner specs for this language. `run_lang_scanners` (a tool
node, no LLM) runs each `cmd` with `$SCAN_DIR` and `$WORKSPACE_DIR` in the
environment and cwd = workspace; `scan_health` reads `output` to verify
coverage. To add/adjust Python scanning, edit this block — no DSL change.

<!-- iterion:scanners
[
  {"id":"semgrep-py","output":"py-semgrep.json","cmd":"semgrep --config=p/python --config=p/django --config=p/flask --config=p/owasp-top-ten --json --output=$SCAN_DIR/py-semgrep.json --metrics=off --quiet --exclude='.iterion' --exclude='.sec-audit' --exclude='.works' --exclude='**/migrations/**' --exclude='**/__pycache__' --exclude='**/venv' --exclude='**/.venv' --exclude='**/test_*.py' --exclude='**/*_test.py' $WORKSPACE_DIR || true"},
  {"id":"bandit","output":"bandit.json","cmd":"bandit --recursive --format=json --output=$SCAN_DIR/bandit.json --severity-level=low --confidence-level=low --skip B101 --exclude='*/test_*.py,*/tests/*,*/.venv/*,*/venv/*,*/migrations/*,*/.iterion/*,*/.sec-audit/*,*/.works/*' --quiet $WORKSPACE_DIR || true"}
]
-->
