---
name: package-cache
description: |
  Cross-run host-wide package analysis cache, located at
  ~/.iterion/security-cache/packages.jsonl. Read by load_package_cache
  + filter_cached, appended by update_package_cache.
---

# Package cache — `~/.iterion/security-cache/packages.jsonl`

Append-only JSONL. One line per `(ecosystem, name, version, checksum)`
tuple analysed by sec-audit-deps. Universal across repos on the host
(a published package version is the same artifact everywhere).

## Location

```
~/.iterion/security-cache/packages.jsonl
```

The parent directory is created on first write. The file is
auto-mounted into sandboxes when `host_state: auto` is in effect
(the default), so sandboxed runs share the cache transparently.

Pass `--sandbox-host-state=none` to opt out, e.g. on multi-tenant
cloud runners where operator state must not leak between users.

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
  "scanner_version": "supply-shield@0.1.0",
  "ttl_days":        30
}
```

## The `kind` discriminator (malware vs CVE)

The same shared file holds two independent verdict axes, distinguished
by `kind`:

- `kind: "malware"` — written/read by supply-shield (Shieldy) and
  sec-audit-deps (Depsy). Durable: a published artifact's malware verdict
  does not change unless the artifact does (the checksum guards that).
- `kind: "cve"` — written/read by supply-shield-cve (Vulny). These carry
  an extra `advisory_db_date` and a SHORT ttl, because a clean-today
  version can gain a CVE tomorrow as advisories land — see that bot's
  package-cache skill.

A reader consults only lines matching its own `kind` (legacy lines with
no `kind` field are treated as `malware` for backward compatibility).

## Cache key

`ecosystem:name:version:checksum` (within a `kind`) — the checksum is
part of the key because npm has experienced cases where a `name@version`
was republished with different content (rare, attack vector). If the
checksum differs, the cached verdict does NOT apply (supply-shield
escalates a checksum change to a `checksum-mismatch` finding).

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

### Force a rescan of a package
```bash
# Remove all lines for a specific package@version
grep -v '"name":"<name>","version":"<ver>"' ~/.iterion/security-cache/packages.jsonl > /tmp/p.jsonl
mv /tmp/p.jsonl ~/.iterion/security-cache/packages.jsonl
```

### Clear the cache entirely
```bash
rm ~/.iterion/security-cache/packages.jsonl
```

### Audit cache size
```bash
wc -l ~/.iterion/security-cache/packages.jsonl
# 100k lines ≈ 50 MB. Compaction (keep latest line per key) is a
# manual step for now:
sort -u ~/.iterion/security-cache/packages.jsonl > ...
# (but the dedup needs to be per-key, not global; a future compact
# tool node will handle this; for V1 the file grows append-only)
```

## Why host-wide and not per-repo

A published package version is identical across repos. Caching
per-repo would multiply tokens by the number of repos on the host.
Host-wide gives the most value with the least state.

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
- `[[supply-shield-cve]]` — the playbook that orchestrates this cache.
