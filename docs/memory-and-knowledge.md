# Memory and knowledge spaces

**Audience.** Bot authors who write to memory (`memory_read` /
`memory_write` / `memory_list`), org admins enforcing a quota on
shared knowledge, and operators wiring multi-tenant isolation.

Memory is how iterion agents carry knowledge across runs — what they
learned, where they left off — persisted as a per-org, quota-governed
tree of markdown documents with tenant-isolated visibility scopes. The session-continuity skill
([bots/whats-next/skills/session-continuity.md](../bots/whats-next/skills/session-continuity.md))
is the canonical consumer.

## Visibilities and what scopes them

A space is addressed by a `SpaceRef`
([pkg/knowledge/scope.go:SpaceRef](../pkg/knowledge/scope.go)). The
`Visibility` is the primary sharing axis; the other fields qualify it.
Validation is enforced everywhere the ref crosses an untrusted boundary
(REST handler, FS adapter, Mongo store) so a stray `?project=` can't
escape the tree.

| Visibility | Shared across | Required qualifiers | Default sub-cap |
|---|---|---|---|
| `private` | one run | — | 64 MiB |
| `bot` | every run of one bot in one project | `ProjectID` + `BotID` | 256 MiB |
| `project` | every bot in one project (the cross-bot inbox) | `ProjectID` | 256 MiB |
| `cross_project` | every project in one org | — | 512 MiB |
| `user` | one user across projects | `UserID` | 128 MiB |
| `org` | every bot / run / project in one org | — | 1 GiB |
| `global` | the whole iterion instance (read-only catalogue) | — | 0 (not writable through org path) |

A space's identity is `v1:<visibility>:<tenant>:<project>:<bot>:<user>:<name>`
— deterministic; equal refs always produce equal ids
([pkg/knowledge/scope.go:SpaceRef.ID](../pkg/knowledge/scope.go)).

## Quotas

Two levels, both enforced at write:

- **Per-org aggregate**: `DefaultOrgAggregateQuota = 1 GiB`. Override
  per-org via `PATCH /api/admin/orgs/{id}` with
  `memory_quota_bytes` — the handler propagates the change into the
  enforced counter via the cloud Mongo memory store's `SetTenantQuota`
  capability
  ([pkg/server/admin_orgs_routes.go:tenantMemoryQuotaSetter](../pkg/server/admin_orgs_routes.go)),
  so the field on `Team` alone is not enough.
- **Per-visibility sub-caps**: the per-space defaults from the table
  above. Override via env at process start
  (`ITERION_MEMORY_QUOTA_ORG_TOTAL`, …).

`DefaultMaxDocumentSize` caps any one markdown document at 2 MiB
([pkg/knowledge/quota.go](../pkg/knowledge/quota.go)).

`GET /api/orgs/{id}/usage` surfaces the org's `memory_used_bytes`
against `effective_memory_quota_bytes` for the org member; the per-space
write CAS is what actually blocks an over-budget write.

## REST surface

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/memory/usage` | member | `{used_bytes, quota_bytes}` for one space |
| `GET` | `/api/memory/docs` | member | List documents (optional `?dir=`) |
| `GET` | `/api/memory/doc` | member | Read (`?path=`) |
| `PUT` | `/api/memory/doc` | member (super-admin for `visibility=global`) | Write |
| `DELETE` | `/api/memory/doc` | member (super-admin for global) | Delete |
| `GET` | `/api/memory/export` | member | Tarball export of the space |
| `POST` | `/api/memory/import` | member (super-admin for global; optional `?strategy=`) | Tarball import |

Query params resolve the space:

| Param | Required when | Meaning |
|---|---|---|
| `name` | always | space name (single segment, no `/`, no `..`) |
| `visibility` | optional (default `project`) | one of the values above |
| `bot` | `visibility=bot` | bot id |
| `project` | `visibility ∈ {bot, project}` | encoded project key (`store.EncodeWorkDirKey` of the workspace root) |

Tenant + user are taken from the identity on the request — never a
query param — so a member can't read another org's memory by editing
the URL ([pkg/server/memory_routes.go:memoryRef](../pkg/server/memory_routes.go)).
Cross-tenant isolation is the contract the cloud (Mongo) adapter
fail-closes on.

`visibility=global` is **instance-wide** (no tenant scoping); a write
or import there requires **super-admin** in cloud mode — otherwise any
authenticated member could pollute or wipe another org's shared
catalogue. Local single-tenant mode (no identity store) treats every
write as allowed.

Doc path safety: the path is relative, no `..` segments, no NUL byte,
no absolute prefix. The same `ValidateDocPath` guard runs at the REST
boundary and inside the FS adapter so the rule holds everywhere
([pkg/knowledge/scope.go:ValidateDocPath](../pkg/knowledge/scope.go)).

## Export / import

`GET /api/memory/export` streams a gzipped tarball of the space; the
client gets `Content-Disposition: attachment; filename="memory-export.tar.gz"`.

`POST /api/memory/import` decompresses and writes back; the
`?strategy=` query param picks a merge strategy from
`knowledge.ImportStrategy`. Use the export → import pair to migrate a
space between orgs (or environments).

## CLI (`iterion memory`)

The local (desktop/CLI) filesystem store under `~/.iterion/` is managed
with `iterion memory export|import|du`. A space is addressed by
`--visibility` (`bot|project|cross_project|user|org|global`, **default
`bot`** — note the REST default is `project`) + `--name`, with `--project`
(defaults to the current directory) for `bot`/`project` spaces and `--bot`
(required when `--visibility=bot`).

```bash
iterion memory du --visibility project              # usage vs quota for a space
iterion memory export --bot whats-next --out mem.tar.gz
iterion memory import --in mem.tar.gz --strategy skip   # skip|overwrite|rename
```

`export` writes a `.tar.gz` (stdout when `--out` is omitted); `import`
reads one (stdin when `--in` is omitted) and merges under `--strategy`
(default `skip`).

## How bots use memory

The canonical consumer is the **session-continuity** skill shipped in
the `whats-next` bundle
([bots/whats-next/skills/session-continuity.md](../bots/whats-next/skills/session-continuity.md)).
It exposes three tools that every catalog bot can use:

- `memory_read` — read a document from the configured space.
- `memory_write` — write or overwrite a document.
- `memory_list` — list documents under an optional dir prefix.

The skill ships per-bundle (not per-instance) so each bot's authored
scope is part of the bundle it lives in — see
[bundles.md → Resource resolution](bundles.md#resource-resolution-at-run-time)
for the workspace-mirror mechanism.

## Tenant isolation

Multi-tenant safety lives on three boundaries:

1. **REST**: the handler stamps the tenant from the JWT/PAT identity
   onto the `SpaceRef`; query params can only override `project`,
   `name`, `bot`, `visibility`. Cross-tenant reads are not expressible.
2. **Cloud Mongo adapter**: every document carries the full
   `SpaceRef.ID()` (which includes the tenant); queries are tenant-stamped
   from the request ctx, and the adapter fail-closes when the ctx
   carries no tenant.
3. **Validate path traversal**: `SpaceRef.Validate` rejects `..`, `/`,
   and `\` in every qualifier and the document path
   ([pkg/knowledge/scope.go](../pkg/knowledge/scope.go)).

`visibility=global` is the only space that **deliberately** crosses the
tenant boundary; writes there are gated on `IsSuperAdmin` and produce
an audit entry through the admin path.
