# Extensions, skills and bot sources — package invariants

One entry per package, verbatim from the engine map this tree replaces.
The generated [map-packages.md](../../references/map-packages.md) lists
every package with its purpose and interfaces; these leaves carry the
invariants. `iterion map find|impact|path` answer the exact questions.

- `pkg/plugin/` — The **plugin ecosystem**: declarative out-of-process extensions described by a `plugin.yaml` manifest with typed `contributes:` kinds (rewriters, mcp_servers, skills/commands/agents, hooks, lifecycle). Builtins embedded under `pkg/plugin/builtin/`; never injects Go code (static `CGO_ENABLED=0`), only wires manifests into existing seams. See [docs/plugins.md](../../plugins.md)

- `pkg/pluginsource/` — Team-scoped durable binding for private plugins: persists git repo + referenced secret id so cloud pods can fetch and cache skills; the checkout is a re-derivable cache, the credential referenced never inlined. A source is **verified at registration** (`Materialize`: clone + parse + read, refused with 422 otherwise) and **quarantined, never launch-fatal, when it breaks later** — skipped for that launch, flagged `degraded` with the reason on its record (both store twins) and in the server log, cleared by the next resolution that succeeds

- `pkg/skilllib/` — **skill library** (ADR-059): a standalone, operator-curated store of `SKILL.md` skills, global `~/.iterion/skills/` + per-project override, referenced from workflows by the DSL `skills:` field. Distinct from bundle/plugin skills (both artifact-coupled); the three share the run-time `.claude/skills/` mirror (bundle > plugin > library precedence). Ships the shared frontmatter parser reused by `runview`. See [docs/skills-library.md](../../skills-library.md)

- `pkg/knowledge/` — Backend-agnostic `MemoryStore` contract for iterion's shared memory; adapters implement filesystem (`pkg/memory`) and cloud (Mongo) storage. Memory documents are treated as untrusted data, never instructions (the operating-posture/secret-handling clauses always outrank them). See [docs/memory-and-knowledge.md](../../memory-and-knowledge.md)

- `pkg/memory/` — Filesystem `MemoryStore` adapter: the per-workspace tree at `~/.iterion/projects/<encoded-workdir>/memory/<scope>/`, indexed from Markdown frontmatter and space-quota'd

- `pkg/marketplace/` — Curated hosted registry over `pkg/botinstall` for bot **and** plugin entries (repo URL, moderation status, visibility scope); backs `iterion marketplace`

- `pkg/botinstall/` — Installs bot bundles from git URLs or local paths into a workspace; shared core for the CLI and studio bundle-install endpoints

- `pkg/botimport/` — Converts Claude-Code workflow scripts (`.js`) to draft `.bot` DSL via lossy parse-and-lower with a mapped/degraded/dropped import report (backs `iterion import`, see [docs/import.md](../../import.md))

- `pkg/botscaffold/` — Generates a new bot bundle (`main.bot` + `manifest.yaml` + layout) from a builder Spec; engine behind `iterion bots create` and the studio guided builder

- `pkg/botregistry/` — Discovers bots on disk (single `.bot` files + `.botz` bundle dirs); the shared layer behind `iterion bots list`, the studio `GET /api/v1/bots`, and the dispatcher's per-ticket bot-override resolution. Also generates Nexie's bot-catalog skill from manifests (`iterion bots regen-catalog`, [pkg/botregistry/catalog.go](../../../pkg/botregistry/catalog.go))

- `pkg/bundle/` — `.botz` bundle loader (workflow + skills + recipes packaged together)

- `pkg/bundlelint/` — Cross-checks a bundle's `manifest.yaml` against its compiled `main.bot` (var/secret mismatches the DSL compiler can't see), surfaced at `iterion validate` under a dedicated C2xx diagnostic family

- `pkg/authoringlock/` — The lock that serializes the writers of ONE stored bot file, extracted from `pkg/server` so writers outside the composition root can take it (ARB-95 / #2826, the bridge of #2689): the lock's identity (`<physical dir>/.iterion/authoring/<sha256 of the lower-cased NFC base name>.lock`), its exclusive `flock` take, and the CONTRACT of the control directory — 0700 directories, an ignore rule of exactly `*\n` in 0600, every entry an owned private regular file with an expected name. The contract is the reason the package exists: a writer that creates that directory with the wrong modes makes the server refuse EVERY save of that file. The lock is per file (two writers of two bots never wait on each other) and folds case, Unicode composition and symlink aliases of one name to one lock. `pkg/server` computes its own lock name through it, and an agreement test holds the two takes to being the same `flock`; the server keeps `checkStoredFile` (it carries HTTP statuses) and one extra gate the bridge does not run — the `git ls-files` refusal of a control directory Git tracks. Its takers outside the composition root: the catalogue refresh (`pkg/botregistry.refreshCatalogOwner`, which holds it from before it reads an owner's template until after its rename) and the CLI writers' chokepoint (`pkg/cli.replaceBotFile` — `fmt -w`, `fmt --to`, `fix`, `dsl migrate`, `import`), which replaces a stored bot file only over the bytes its writer READ: a save that landed since is `ErrBotFileChanged`, a save in flight is `ErrBotFileNotTaken`, and both leave the file whole

- `pkg/botsource/` — Team-authored bot bundles: the writable, tenant-scoped counterpart to the read-only catalog baked into a runner image (the plugin-side `pkg/pluginsource` analogue for bots). Stores the bundle CONTENT as a multi-file map (`main.bot` + `manifest.yaml` + `skills/`…) since it's authored in the studio editor, not fetched from git; Mongo in cloud, memory-backed for tests/local. Two-tier editability: baked catalog bots stay read-only; a team forks one or authors a new one. Backs the studio cloud bot editor + `/api/teams/{id}/bot-sources` (see [docs/cloud-rest-api.md](../../cloud-rest-api.md)) — and, under the reserved `platform:` sentinel tenant, the deployment-wide **platform bot overrides** (super-admin `/api/admin/bots` + `iterion remote admin bots push`): the DB-backed form of the baked catalog, resolved team → platform → baked at every launch surface via [pkg/server/bot_resolver.go](../../../pkg/server/bot_resolver.go) and materialized runner-side from the frozen, digest-checked snapshot the queue message's `bot_bundle` ref carries (see [docs/platform-bots.md](../../platform-bots.md))

- `pkg/sessionboard/` — Curation-bot platform rendering declarative semantic widgets (milestone progress, blockers, narrative note, small chart) alongside the run view; the agent emits a Spec against a fixed widget registry, never code

- `pkg/artifactlabels/` — Derives semantic labels (`plan`, `verdict`) for published artifacts from output shape; kept in sync with the studio's render-time `ArtifactDiff` detection

- `pkg/askusermcp/` — Shared MCP tool surface (`ask_user`, `ask_user_async`, `await_answers`) exposed over both stdio and HTTP transports for interactive workflows
