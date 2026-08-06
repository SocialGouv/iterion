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

A document path is relative, stays inside the space (no `..`, no absolute
form), and must be **canonical** — `MEMORY.md`, not `./MEMORY.md`;
`topics/deploy.md`, not `topics//deploy.md`. One document, one spelling: the
two adapters used to disagree about it (the filesystem one normalised on the
way in, the cloud one stored the key verbatim), which let one document exist
under several keys and a filesystem round trip lose track of its own file. The
error names the canonical form, so a caller — or a model — can correct itself.

The skill ships per-bundle (not per-instance) so each bot's authored
scope is part of the bundle it lives in — see
[bundles.md → Resource resolution](bundles.md#resource-resolution-at-run-time)
for the workspace-mirror mechanism.

## `auto_memory:` — the backends' own MEMORY.md

The `memory:` block above is iterion's memory: explicit tools, an authored
scope, an autoload set. Alongside it, **an agent already keeps a MEMORY.md if you let it** — Claude Code has
auto-memory of its own (`~/.claude/projects/<cwd>/memory/`), and claw and pi
maintain one from a prompt section plus their ordinary file tools.
`auto_memory:` is the switch for that mechanism.

```
workflow main:
  auto_memory: off          # workflow default

agent implement:
  backend: "claude_code"
  auto_memory: on           # node override

agent review:
  backend: "claw"
  auto_memory: on           # the SAME MEMORY.md as `implement`
```

**Off by default.** A run is hermetic unless it asks otherwise. Left alone,
Claude Code's own default is *on*, which means every claude_code node of every
bot run would read and write the operator's personal memory for that working
directory — a side effect nobody opted into, and cross-run contamination
between unrelated bots.

**Precedence** (first set wins): `--auto-memory` / studio Launch → node
`auto_memory:` → workflow `auto_memory:` → `ITERION_AUTO_MEMORY` → off.
The run-level override travels the whole way — including onto the cloud queue
(`RunMessage.auto_memory`, schema v6) and into a detached runner subprocess —
so an operator's `off` is not quietly replaced by a bot's `on` further down.
It is NOT persisted on the run, though: `iterion resume --auto-memory` has to
re-state it.

Diagnostics: **C131** (invalid value) and **C132** (`on` on a backend that
ignores it — `claude_code`, `claw` and `pi` consume it; kimi, grok and the
legacy codex do not). C132 fires per node on an explicit node-level `on`, and
once for the workflow when a workflow-level `on` is inert because *nothing* in
the graph can honour it.

### One directory, both backends, persisted

Each backend's native store keys on the working directory, which defeats the
purpose twice over: a `worktree: auto` bot gets a fresh, empty memory every
run, and a claude_code node and a claw node of the same bot never see each
other's notes.

So iterion owns the location instead. It resolves ONE space — visibility
`bot`, reserved name `auto-memory`, keyed on the run's **repository root** —
materialises it into a directory before the node runs, points every supported
backend at that absolute path, and folds the agent's edits back into the space
afterwards, including when the node failed.

**Which bot the space belongs to** is resolved by one rule, shared by every
surface that starts a run (CLI, studio in-process, a detached subprocess, the
cloud runner, a resume, a subbot child, the dispatcher): the bot id persisted
on the run when there is one, else the bundle directory name for a
`<id>/main.bot` path, else nothing — and the executor then falls back to the
workflow's own name. A standalone `.bot` deliberately takes that last branch:
it has no identity beyond its workflow name, and deriving one from wherever the
file happens to sit would move its memory when the file moves.

The rule is shared because the alternative was not: while each surface resolved
its own, a bundle whose id and workflow name differ — `whats-next` against
`whats_next`, the usual shape — wrote its notes to one space at launch and read
an empty other one on resume. Nothing failed; the memory simply split in two.
A test over the construction sites (`TestEveryExecutorConstructionDecidesTheBotIdentity`)
keeps a new surface from quietly reintroducing that.

For a bundle, the three names must agree — `manifest.yaml`'s `name`, the
`workflow NAME:`, and the bundle directory. **C230** enforces it for any bundle
carrying per-bot memory, which now includes `auto_memory: on`, because the rule
above resolves whichever of the three the launching surface happens to know.
Two bundles that pick the SAME name still share one space; that is the same
property `memory: visibility: bot` has always had, and the space is at least
scoped per project, so the collision needs two identically-named bundles in one repo.

How the backend is told splits in two, and only in two:

| Backend | Told by |
|---|---|
| `claude_code` | `--settings autoMemoryDirectory` — it has auto-memory of its own, so it only needs pointing |
| `claw`, `pi` | a rendered `# Auto memory` system-prompt section — they have no such concept, so the section IS the mechanism, and their ordinary file tools do the rest |

The section is claw's own renderer, reused rather than re-authored, so a
wording improvement upstream reaches every backend that needs it.

Because the space goes through the same `knowledge.MemoryStore` as everything
else on this page, **cloud runs persist**: the Mongo store carries MEMORY.md
past the pod's ephemeral disk, and `iterion memory du|export` sees the space
like any other.

The sync-back deliberately runs on a context detached from the node's. A run
that ends early — an operator Cancel, a runner drain, a timeout — is exactly
when the agent's notes matter most, and the cloud store honours cancellation,
so syncing on the node's own context would discard them. It is bounded, and the bound
scales with how much there is to persist: cancelling a run does not return
until the in-flight node has finished, so a fixed budget would either lose
notes on a large memory or make Cancel feel stuck on a small one.

Three properties worth knowing:

- **Only Markdown is persisted.** The store indexes `.md`; anything else in
  the directory is reported and skipped rather than written somewhere nobody
  reads it.
- **Secret-shaped bodies are refused.** Memory is readable by every later run
  of the bot, so a document containing a literal credential token is rejected
  with a warning instead of stored.
- **A sync-back failure never fails the node.** Quota rejections, a skipped
  symlink, an unreadable file — each is warned about individually and the rest
  of the agent's notes still land.

**Known gaps:**

- **A copy-based sandbox cannot carry it — and that includes cloud runs.** The
  kubernetes driver populates the pod with a COPY of the workspace and offers no
  per-file read-back, so the agent's notes would stay in the pod until teardown
  — long after the sync that should have persisted them. Such a run refuses
  auto-memory with a visible warning and proceeds without it, rather than
  running a half cycle whose only symptom is a memory that is always empty.
  Docker (bind mount) and unsandboxed runs are unaffected.

  A cloud run is NOT automatically exempt, whatever the sandbox section of
  CLAUDE.md still says about the runner pinning `ITERION_SANDBOX_OVERRIDE=none`.
  Measured on the production instance (2026-08-05): the runner's config carries
  `ITERION_SANDBOX_DEFAULT=auto`, `ITERION_SANDBOX_HOST_STATE=none` and an EMPTY
  `ITERION_SANDBOX_OVERRIDE` — so a bot that does not opt out gets the
  kubernetes sandbox and its auto-memory is refused. A cloud bot that wants
  MEMORY.md declares `sandbox: none` (proven end to end there: one run wrote a
  note, a second run on a different pod read it back through the store), or
  waits for a read-back seam on the copy-based drivers.
- `iterion fork` / `rewind` do not carry `--auto-memory`; the bot's DSL
  survives, the run-level override does not. (`run` and `resume` both carry it;
  `resume` needs it RE-STATED, since run-level overrides are not persisted on
  the run.)
- A fork re-hydrates from the space as it is TODAY, not as it was at the fork
  point — the store is the source of truth, and it has moved on since.
- The studio's Memory panel does not list `bot`- or `project`-visibility spaces:
  `/api/…/memory` takes the project as a raw path while the engine keys on the
  encoded workdir key, and `SpaceRef.Validate` rejects a raw path outright.
  `iterion memory` encodes it and works. This predates auto-memory and affects
  every such space.
- A subbot's child executor is built without the parent's `MemoryStore`, so on a
  cloud-mode studio running subbots in-process the child's memory falls back to
  the local filesystem. Also predates auto-memory — the `memory:` block has the
  same behaviour.
- A credential with no published shape (a bare password, an internal token) is
  not recognised and will be persisted. The guard matches structures the
  provider documents, which is what keeps it from refusing an agent's ordinary
  prose; it is not a general secret scanner.

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
