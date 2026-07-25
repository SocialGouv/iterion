---
name: package-cache
description: |
  Package analysis cache at {{vars.cache_path}} (default: the engine's
  out-of-tree run scratch dir). Read by load_package_cache
  + filter_cached, appended by update_package_cache. Point cache_path
  at ~/.iterion/security-cache/packages.jsonl for host-wide reuse.
---

# Package cache — `{{vars.cache_path}}`

Append-only JSONL. One line per `(ecosystem, name, version, checksum)`
tuple analysed by sec-audit-deps. A published package version is the
same artifact everywhere, so cached verdicts are portable across repos.

## Location

```
{{vars.cache_path}}
# default: ${PROJECT_SCRATCH_DIR}/sec-audit-deps/cache/packages.jsonl
```

The parent directory is created on first write. The default lives in
the engine's out-of-tree scratch dir — always writable, including in
sandboxes whose image pins a non-host user — at the cost of being
per-run (ephemeral) under a sandbox.

For true cross-run, cross-repo reuse, opt into the host-wide store:

```
--var cache_path=$HOME/.iterion/security-cache/packages.jsonl
```

That file is auto-mounted into sandboxes when `host_state: auto` is in
effect (the default). Pass `--sandbox-host-state=none` to opt out,
e.g. on multi-tenant cloud runners where operator state must not leak
between users (and expect EACCES on images pinning a non-host user —
the reason the scratch default exists).

## Line schema

```json
{
  "kind":            "malware",
  "ecosystem":       "npm",
  "name":            "left-pad",
  "version":         "1.3.0",
  "checksum":        "sha256:abc123...",
  "scanned_at":      "2026-05-19T10:00:00Z",
  "risk_score":      25,
  "risk_level":      "LOW",
  "summary":         "Install hook runs setup.js; no network calls.",
  "flags":           [{"type": "install-hook", "severity": "low", "description": "..."}],
  "files_audited":   ["node_modules/left-pad/setup.js"],
  "scanner_version": "sec-audit-deps@0.1.0",
  "ttl_days":        30
}
```

`kind` discriminates verdict axes that share this host-wide file:
`malware` (this bot + supply-shield/Shieldy) vs `cve`
(supply-shield-cve/Vulny, which adds `advisory_db_date` + a short ttl).
Each reader consults only its own `kind`; a legacy line with no `kind`
is treated as `malware`.

## Cache key

`ecosystem:name:version:checksum` (within a `kind`) — the checksum is
part of the key because npm has experienced cases where a `name@version`
was republished with different content (rare, attack vector). If the
checksum differs, the cached verdict does NOT apply.

## Lookup rules (filter_cached)

A `(ecosystem, name, version, checksum)` is **cache-hit** when:

1. A line exists with matching key.
2. The line's `scanner_version` is ≥ the current bot's version
   (lexical compare on the `vMAJOR.MINOR.PATCH` part is enough;
   updates to the bot are expected to re-bucket findings).
3. `now - scanned_at < ttl_days * 24h`. Default TTL: 30 days.

Otherwise it's a **cache-miss** and goes into `pending[]` for
phase 4.

## Append rules (update_package_cache)

The `update_package_cache` tool node appends one JSONL line per
package analysed in the current run. Write order:

1. Compose JSON line (no embedded newlines; pretty-printing OFF).
2. Append atomically: `printf '%s\n' "$line" >> packages.jsonl`.
   POSIX guarantees `>>` to a file is atomic for writes shorter
   than PIPE_BUF (typically 4096 bytes) on local filesystems.

If a package was already in the cache (because we re-scanned it
due to TTL or scanner version bump), the older line stays but is
shadowed by the newer line (which comes later in the file). The
index built by `load_package_cache` keeps only the most recent
line per key.

## Operator workflows

All examples below use `CACHE=<your cache_path>` (the host-wide
`~/.iterion/security-cache/packages.jsonl` when opted in).

### Force a rescan of a package
```bash
# Remove all lines for a specific package@version
grep -v '"name":"<name>","version":"<ver>"' "$CACHE" > /tmp/p.jsonl
mv /tmp/p.jsonl "$CACHE"
```

### Clear the cache entirely
```bash
rm "$CACHE"
```

### Audit cache size
```bash
wc -l "$CACHE"
# 100k lines ≈ 50 MB. Compaction (keep latest line per key) is a
# manual step for now:
sort -u "$CACHE" > ...
# (but the dedup needs to be per-key, not global; a future compact
# tool node will handle this; for V1 the file grows append-only)
```

## Why the host-wide override exists

A published package version is identical across repos. Caching
per-repo (or per-run, the scratch default) multiplies tokens by the
number of repos on the host; the host-wide override gives the most
value with the least state, when the sandbox setup can write it.

## Why JSONL and not SQLite

Three reasons:
- Operator readable + line-editable.
- Append-only writes don't require locks for safe concurrency
  (POSIX appends are atomic up to PIPE_BUF).
- No new runtime dep (sqlite vs `printf`).

Trade-off: lookup is O(n) on file load. At 100k lines this is
~100 ms — negligible compared to the LLM and scanner costs the
cache saves. If the cache grows past 1M entries we'll revisit.

## See also

- `[[malware-signals]]` — the signal catalogue persisted in `flags`.
- `[[sec-audit-deps]]` — the playbook that orchestrates this cache.
