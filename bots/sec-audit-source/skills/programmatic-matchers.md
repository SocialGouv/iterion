---
name: programmatic-matchers
description: |
  How to ship custom programmatic matchers in this bundle. Mirrors
  deepsec's `MatcherPlugin` contract scaled to iterion's
  bundle-attachment model. Load when authoring a new matcher,
  troubleshooting why a matcher's output isn't reaching triage, or
  when an org-specific vulnerability shape exceeds what semgrep
  YAML can express.
---

# Programmatic matchers — `attachments/matchers/`

A matcher is a self-contained executable that emits structured
findings. The bundle's `run_custom_matchers` tool node enumerates
every executable file under `attachments/matchers/`, invokes each
with the standardised stdin/stdout JSON protocol, aggregates the
results, and hands them to `triage` as one more scanner source.

This is for the cases scanner-driven matchers (semgrep YAML, gosec,
bandit) can't express cleanly:
- multi-file state (e.g. "auth helper defined in `auth.go` is never
  imported by handlers in `api/*.go`")
- non-textual analysis (parse `package.json` against a registry
  policy file)
- per-file scoring or ranking (matcher reads a file twice with
  different lenses)

## Protocol

**Discovery**: any file under `attachments/matchers/` with the
executable bit set, or matching `*.py` / `*.js` / `*.sh` /
`*.matcher`. Non-executable files are skipped. Files starting with
`_` or `.` are skipped (reserved for helpers + dotfiles).

**Invocation**: each matcher is run as `<matcher>` with these
environment variables and stdin JSON:

```sh
WORKSPACE_DIR=<abs path>  MATCHER_NAME=<basename> ./<matcher>
< <stdin>
```

Stdin:
```json
{
  "workspace_dir":    "/abs/path/to/workspace",
  "tech":             {"langs": ["js","go"], "frameworks": ["nextjs"]},
  "files":            ["pkg/server/proxy.go", "app/api/auth/route.ts"],
  "matcher_name":     "no-external-redirect",
  "matcher_version":  "1.0",
  "config":           {}
}
```

`files[]` is the list of files in scope for this run (workspace
walk excluding `.git`, `node_modules`, `.iterion`, `.sec-audit`,
`vendor`, etc.).
A matcher can read any file via its own logic — `files[]` is a
performance hint, not a hard restriction.

**Stdout**: a single JSON object with this shape:

```json
{
  "matches": [
    {
      "rule_id":        "no-external-redirect",
      "file":           "pkg/server/proxy.go",
      "line_range":     [142, 158],
      "snippet":        "if redirect := r.URL.Query().Get(\"u\"); ...",
      "severity":       "high",
      "finding_type":   "redirect",
      "message":        "Open redirect: URL parameter from query string flows into http.Redirect without allowlist validation.",
      "metadata":       {"http_method": "GET"}
    }
  ],
  "errors": [],
  "matcher_name":    "no-external-redirect",
  "matcher_version": "1.0",
  "duration_ms":     142
}
```

**Exit code**: `0` on success (`matches[]` may be empty). Non-zero
causes the matcher's output to be discarded and an entry added to
the `run_custom_matchers` tool node's `errors[]` envelope; other
matchers continue.

## Field semantics

| Field | Required | Notes |
|---|---|---|
| `rule_id` | yes | Stable identifier; becomes the `matcher` field on triage candidates as `custom:<matcher_name>:<rule_id>` |
| `file` | yes | Relative to `workspace_dir` |
| `line_range` | yes | Inclusive `[start, end]` |
| `snippet` | recommended | 5-15 lines; truncated automatically beyond |
| `severity` | yes | `low` / `medium` / `high` / `critical` — same scale as the rest of the bot |
| `finding_type` | yes | One of `[[finding-taxonomy]]`; pick `other` rather than invent |
| `message` | yes | Human-readable rationale; surfaces verbatim in the kanban issue |
| `metadata` | optional | Free-form additional data |

## Language ergonomics

The protocol is stdin JSON + stdout JSON + exit code. Any language
works.

**Python skeleton** (`attachments/matchers/example-secrets.py`,
shipped as the default example):

```python
#!/usr/bin/env python3
import json, sys, os, re

req = json.load(sys.stdin)
ws = req["workspace_dir"]

KEY_RE = re.compile(r'(?i)(api[_-]?key|secret|token|password)\s*[:=]\s*["\']([^"\']{16,})')

matches = []
for rel in req.get("files", []):
    abs_path = os.path.join(ws, rel)
    try:
        with open(abs_path, "r", encoding="utf-8", errors="ignore") as f:
            for i, line in enumerate(f, start=1):
                m = KEY_RE.search(line)
                if m:
                    matches.append({
                        "rule_id": "literal-secret-in-config",
                        "file": rel,
                        "line_range": [i, i],
                        "snippet": line.rstrip(),
                        "severity": "high",
                        "finding_type": "secrets",
                        "message": f"Literal secret '{m.group(1)}' = '...' detected in source.",
                    })
    except (OSError, IOError):
        continue

json.dump({"matches": matches, "errors": [], "matcher_name": "example-secrets", "matcher_version": "1.0"}, sys.stdout)
```

**Node.js skeleton**:
```js
#!/usr/bin/env node
const chunks = [];
process.stdin.on('data', c => chunks.push(c));
process.stdin.on('end', () => {
  const req = JSON.parse(Buffer.concat(chunks).toString('utf-8'));
  const matches = [];
  // your logic here
  process.stdout.write(JSON.stringify({matches, errors: [], matcher_name: 'foo', matcher_version: '1.0'}));
});
```

**Shell** is supported but discouraged for non-trivial matchers
(JSON serialisation in shell is fragile). Use Python or Node.

## What programmatic matchers gain over semgrep YAML

semgrep covers a huge surface (~600 community rules + custom YAML).
You only need a programmatic matcher when semgrep can't express
your shape:

| Shape | Best tool |
|---|---|
| Single-file regex / AST pattern | semgrep YAML |
| Multi-pass: file A defines, file B uses | programmatic |
| State across files: count handlers, ratio, distribution | programmatic |
| Parse a registry policy file + cross-check against deps | programmatic |
| Inverse / "X is missing": handler without auth middleware | semgrep `pattern-not-inside` (usually) |
| Per-file scoring (entropy, complexity) | programmatic |
| Talk to a sandboxed sibling service (LSP, type-checker) | programmatic |

Don't reach for a programmatic matcher when semgrep can do it —
semgrep rules are easier to share, easier to test, and benefit from
the community registry.

## Performance

Matchers run sequentially in the V1 `run_custom_matchers` tool
node. For ~5 matchers each taking < 5s on a 10k-file repo, this is
~25s overhead. If a matcher is slow, split it into smaller ones or
push the work into a sibling tool node that runs in parallel via
fan_out.

## Output integration

The `run_custom_matchers` tool node emits a `scanner_output` with
`scanner: "custom"`, `subscanners: ["matcher-name-1", "matcher-name-2", ...]`,
and one JSON path per matcher under `json_paths`. `triage` reads
the matcher outputs as it would semgrep / gosec / bandit, mapping
each match's `finding_type` directly without LLM disambiguation
(since the matcher already declared the taxonomy).

## See also

- `[[finding-taxonomy]]` — required for `finding_type` field.
- `[[vuln-matchers-guide.md]]` — the strategy doc explaining the
  scanner-vs-LLM boundary.
- `[[sec-audit-source]]` — the orchestrating playbook.
