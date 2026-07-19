---
name: file-records
description: |
  Per-file append-only analysis records — `.sec-audit/files/`.
  Read by `filter_cached_files` (skip work) and written by
  `update_file_records` (capture). Load this skill when the run is
  scoped large enough that the LLM revalidate cost dominates, and
  when reasoning about cache invalidation, hash mismatches, or
  resume after an interrupted scan.
---

# File records — `.sec-audit/files/<sha1>.json`

Append-only JSON files, one per source file the bot has ever
analysed. Each file accumulates a history of analyses. Re-runs skip
the revalidate phase for files that:
- have NOT changed (content hash match), AND
- were analysed at an acceptable scanner version, AND
- were analysed within the configurable TTL (default 30 days).

The pattern is taken from deepsec's append-only FileRecord +
per-file locking, scaled down to single-process iterion-bundle
form. It lets a multi-hour scan resume cheaply after an
interruption, and shaves the most expensive phase (revalidate) on
re-runs where most files are unchanged.

## Location

```
<workspace_dir>/.sec-audit/files/<sha1(rel_path)[0:16]>.json
```

The filename is the first 16 hex chars of the SHA-1 of the file's
relative path. This:
- avoids filesystem issues with paths containing `/`, spaces,
  unicode, etc.,
- keeps filenames fixed-length and sortable,
- makes lookups deterministic.

The directory `.sec-audit/files/` is auto-created on first
write. It is committed (recommended) so the cache survives a fresh
clone — though developers may add it to `.gitignore` if they
prefer per-environment caching.

## Schema

```json
{
  "path": "pkg/server/proxy.go",
  "history": [
    {
      "content_hash":    "sha256:abc123...",
      "analysed_at":     "2026-05-19T11:00:00Z",
      "scanner_version": "sec-audit-source@0.1.0",
      "candidates": [
        {"id": "C-001", "finding_type": "ssrf", "line_range": [120,145], "matcher": "..."}
      ],
      "verdicts": [
        {"candidate_id": "C-001", "verdict": "confirm", "rationale": "..."}
      ],
      "issues_created": ["native:abc123"]
    }
  ]
}
```

`history[]` is append-only. Each run appends one entry. The
authoritative state for cache-hit decisions is `history[-1]` (the
most recent entry).

Tombstones: when a file is deleted from the workspace, its record
file is NOT deleted (history is precious data). The bot ignores
records whose `path` no longer exists in the workspace.

## Cache-hit rule (filter_cached_files)

Given a current run with scanner_version `S` and TTL `T` (days),
a candidate is **cache-hit** when all of:

1. The file at `candidate.file` exists and its current sha256 ==
   `history[-1].content_hash`.
2. `history[-1].scanner_version` ≥ `S` (lexical compare on the
   `vMAJOR.MINOR.PATCH` part; ties broken on full string).
3. `now - history[-1].analysed_at < T * 24h`.

Cache-hit candidates DO NOT go to revalidate. Their cached verdicts
from `history[-1].verdicts` are merged into the workflow's verdict
stream by `merge_verdicts` and surfaced to `report_card` so the
board sees them.

## Write rule (update_file_records)

After every successful `report_card`, for each file mentioned in
the run's candidates (whether cache-hit or fresh):

1. Open `.sec-audit/files/<sha1>.json` (create if absent).
2. Compute the file's current content_hash.
3. Append one entry to `history[]` with:
    - `content_hash`: current sha256
    - `analysed_at`: now (UTC ISO)
    - `scanner_version`: this bot's version
    - `candidates`: the candidates from triage that targeted this
      file
    - `verdicts`: the verdicts from revalidate (or surfaced cached)
      for those candidates
    - `issues_created`: the board issue ids from report_card

Always append, never edit. The history is the audit trail; old
entries are evidence of past analyses and useful when debugging a
regression.

## Operator workflows

### Force a rescan of a file
```bash
# Delete the file's record:
rm .sec-audit/files/<sha1>.json
# Or truncate history to one entry to keep the path tracked:
jq '.history = [.history[0]]' file.json > tmp && mv tmp file.json
```

### Force a rescan of everything
```bash
rm -rf .sec-audit/files/
```

### Audit cache size
```bash
ls .sec-audit/files/ | wc -l
du -sh .sec-audit/files/
```

### Inspect a file's history
```bash
PATH_HASH=$(printf '%s' 'pkg/server/proxy.go' | sha1sum | head -c 16)
jq '.history | length' .sec-audit/files/$PATH_HASH.json
jq '.history[-1].verdicts' .sec-audit/files/$PATH_HASH.json
```

## Relation to `fp-known.yaml`

`fp-known.yaml` ([[fp-memory]]) and FileRecords serve different
roles:

| Memory | Scope | Authoritative for |
|---|---|---|
| `fp-known.yaml` | Curated FP suppression | "this matcher on this line should never surface" |
| FileRecords | Cache of analyses | "this file's verdicts haven't changed since last run" |

A file with a fresh content_hash but matching `fp-known.yaml`
entries: the bot **still** loads FileRecords for cache decisions,
but `triage` independently applies `fp-known.yaml` suppression to
candidates before they reach the cached/fresh split. The two
memories don't conflict.

## Locking (future)

`history[-1].locked_by_run_id` is reserved for future distributed
execution (Cap. 3 in the roadmap). When the cloud queue lands, a
runner acquiring a file's record sets this field atomically; other
runners skip the file until the lock clears.

V1 ignores this field. Locking is a no-op in single-process runs.

## See also

- `[[sec-audit-source]]` — the orchestrating playbook (which
  references this skill at the cache phase).
- `[[fp-memory]]` — the curated FP companion memory.
- `[[finding-taxonomy]]` — the enum used in `candidates[].finding_type`.
