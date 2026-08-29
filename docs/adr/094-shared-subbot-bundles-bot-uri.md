# ADR-094: Shared subbot bundles — the `bot://` dependency URI, a unified subbot source resolver, and pinned local resolution

- **Status**: Accepted
- **Date**: 2026-08-29
- **Authors**: Victor (arbitration), Claude (code grounding)
- **Relates to**: [ADR-084](084-subbot-reattach-across-restarts.md) (subbot re-attach)
- **Code (today, the seams this ADR unifies or extends)**:
  [pkg/cli/run.go](../../pkg/cli/run.go) (`subbotRunnerForCLI`),
  [pkg/dispatcher/subbot.go](../../pkg/dispatcher/subbot.go) (`subbotRunnerForDispatch`),
  [pkg/runview/subbot.go](../../pkg/runview/subbot.go) (`subbotRunnerFor`),
  [pkg/runview/compile.go](../../pkg/runview/compile.go) (`CompileWorkflowWithHash`, `CompileBundleWorkflow`),
  [pkg/runtime/special_node.go](../../pkg/runtime/special_node.go) (`runSubbotNode`),
  [pkg/bundle/manifest.go](../../pkg/bundle/manifest.go) (`decodeManifest`),
  [pkg/bundle/tar.go](../../pkg/bundle/tar.go) (`collectContentHash`),
  [pkg/botinstall/install.go](../../pkg/botinstall/install.go) (install destination),
  [pkg/botregistry/registry.go](../../pkg/botregistry/registry.go) (`ResolveBotPath`)

## Context

Two sibling projects (Town, Tabarria) run different main bots but share the
same hierarchical-planning subbots — `hierarchy-feature-author`,
`hierarchy-epic-author`, `hierarchy-semantic-review` — plus the
`town_hierarchical_planner.py` script they drive. Today each project owns a
**copy**. The copies drift silently: a fix landed in one is invisible to the
other, and nothing in either repo records which version of the shared logic a
given run executed.

A `subbot` node names its child with `source:`, a plain filesystem path
resolved relative to the parent `.bot`. That resolution is **copy-pasted in
three places** — `filepath.Join(parentDir, source)` in the CLI, the dispatcher
and the studio runner. There is no URI scheme anywhere in the codebase, no way
to say "this child comes from a versioned artifact shared between projects",
and no record on the run of what a child's source actually was.

iterion already carries four distribution mechanisms for bot artifacts:
`pkg/bundle` (`.botz`), `pkg/botinstall` (install a bundle from a git URL into
`<workdir>/.botz/<name>`), `pkg/marketplace` (a registry whose entries carry
`kind: bot | plugin`), and `pkg/botsource` (team-authored multi-file bundles in
cloud). It also carries a plugin system whose `contributes:` kinds extend the
**runtime** — rewriters, MCP servers, hooks, and the markdown mirrored into
`.claude/`.

The question this ADR answers: how does a workflow reference a subbot it does
not own, reproducibly, without either project silently editing a shared copy?

## Decision

### 1. Distribution is a **bot bundle**, addressed by `bot://` — not a plugin

```
subbot feature_author:
  source: "bot://shared-planner/hierarchy-feature-author"
```

A shared planner ships workflows, schemas, prompts and scripts — that is the
content of a `.botz`, and it reuses `botinstall` / `marketplace` /
`botregistry` / `botsource` as they stand. A **plugin** stays what it is: the
vehicle for *runtime extensions* (rewriters, MCP servers, hooks, skills). One
repository may publish both, but the two contracts stay distinct.

Rejected alternative: `contributes: workflows` on `plugin.yaml`. It would turn
the runtime-extension system into a second bot-distribution system, duplicating
four existing mechanisms.

### 2. A unified subbot source resolver comes **first**, with no behaviour change

```go
type SubbotSourceResolver interface {
    Resolve(ctx context.Context, parentSource, requestedSource string) (ResolvedSubbotSource, error)
}
```

`ResolvedSubbotSource` carries the canonical identity, the resolved local path,
the kind (`file` | `bot`), the dependency name + version when applicable, the
bundle content hash when applicable, and the origin.

Scope — workdir, bots paths, and later the tenant — is injected at
**construction**, not passed per call, matching how the current runners close
over `parentPath` / `storeDir`. A `tenantID` parameter is deliberately absent
from v1: no resolution site has a tenant today, and a parameter that is always
empty lies about the contract.

The first implementation only reproduces `filepath.Join(parentDir, source)`.
It replaces the three duplicated sites and ships alone, observably inert.

### 3. The bundle manifest stays `schema_version: 1`; the new fields are optional

`decodeManifest` **hard-rejects** any `schema_version` other than
`CurrentManifestSchema` (=1):

> `manifest schema_version 2 not supported by this iterion build (expected 1)`

A bump would make the shared bundle unloadable by every binary not yet
rebuilt — including the studios running the consumer projects and the baked
catalog images. The manifest already absorbed `produces` / `consumes` /
`invocations` / `retry` / `repo` as optional fields without a bump, and the
parser is `yaml.UnmarshalStrict`: an older binary reading a manifest that
carries `dependencies:` already fails loudly, **naming the offending field** —
a better diagnostic than a version number. If a version gate is ever needed,
`Compat` exists for it.

Consequence on deployment order: **rebuild/restart iterion first, install the
shared bundle second.**

Shared bundle:

```yaml
# shared-planner/manifest.yaml
name: shared-planner
schema_version: 1
enabled: false
exports:
  workflows:
    - id: hierarchy-feature-author
      path: main.bot
```

Consumer bundle:

```yaml
schema_version: 1
dependencies:
  workflows:
    - name: shared-planner
```

### 4. The consumer must be a bundle; a bare `.bot` using `bot://` fails explicitly

The dependency is declared in the consumer's `manifest.yaml`. A loose `.bot`
has nowhere to declare it, so `bot://` from a bare `.bot` is a resolution
error with an explicit message, never an implicit lookup.

### 5. The pin lives in `bots.lock` at the **project root**, and is **materialised**

```yaml
# bots.lock
version: 1
dependencies:
  shared-planner:
    source: https://git.example/shared-planner.git
    ref: v1.0.0
    bundle_sha256: "..."
```

The manifest declares the **need**; the lockfile fixes its **resolution**. No
SHA enters the DSL: a `.bot` feeds the resume hash (`CompileWorkflowWithHash`),
so a pin inside it would invalidate every in-flight run's resume for a change
that does not touch the bot's logic — and two bots of the same project should
share one pin.

The lockfile is **not** under `.iterion/`: that directory is the gitignored run
store, and a file meant to be committed and diffed does not belong in it.

A lockfile without materialisation is decorative. `botinstall` installs into
`<workdir>/.botz/<name>`, which `.gitignore` excludes (`*.botz`), so a fresh
clone of a consumer project has **no** shared bundle on disk. Therefore:

- **`iterion bots sync`** (or equivalent) restores every `bots.lock`
  dependency into `.botz/` and verifies each against its `bundle_sha256`;
- **`iterion bots update <name>`** fetches one dependency, recomputes its
  content hash, atomically advances the lock, and rematerialises that bundle;
- the resolver **never trusts whatever happens to sit in `.botz/`**: it
  verifies the installed bundle's content hash against the lock and refuses on
  divergence. "It is already there" is not a resolution.

### 6. `enabled: false` removes the bundle from the catalog and from automatic routing — nothing more

`pkg/bundle`'s loader requires a `main.bot` at the bundle root, so
`shared-planner` ships one — the pilot's single export *is* `main.bot`. Without
further precaution the bundle would then appear in `iterion bots list`, the
studio catalog and Nexie's generated bot catalog as a routable bot.

`enabled: false` is the right instrument, and the existing semantics are
already the ones we want — but they are narrower than "not launchable":

- `List` / `ListWithSchema` **keep** disabled bots (`Enabled=false`) so admin
  surfaces can show them;
- `catalog.go` filters to enabled bots only — Nexie never routes to it;
- `invocation_dispatch.go` skips disabled entries — no command/webhook
  invocation reaches it;
- `ResolveBotPath` **ignores `Enabled` entirely**, and **no launch path gates
  on it**: an explicit launch of a disabled bot still works.

This ADR formalises that reading: **`enabled: false` means "out of the catalog
and out of automatic routing"; it does not forbid an explicit launch, and it
must never forbid resolving an explicitly declared dependency.** If either full
invisibility or a launch prohibition is required later, it takes a real
`kind: library` or `catalog_visibility: hidden` — not a redefinition of this
flag.

One correction to inherit: `bot://` resolution must be an **exact** name match.
`ResolveBotPath` falls back to normalized matching (kebab/snake tolerance) for
ticket-assignee ergonomics; for a pinned dependency that tolerance is a
liability — it could resolve a lock to a differently-named bundle.

### 7. Reuse `pkg/bundle`'s existing content hash — do not write a second one

`collectContentHash` already computes the stable, format-independent hash of a
bundle directory: the sorted sequence of (relative path, file bytes), so a
packed archive and an extracted tree agree. It is the logical tree hash this
ADR needs; it is merely unexported.

Decision: **expose it for directories** and record it as `bundle_sha256`. No
second hashing algorithm enters the codebase.

Hashing applies only to `bot://` dependencies. Local relative subbot paths keep
exactly today's `CompileWorkflowWithHash` behaviour.

### 8. Subbot execution becomes **bundle-aware**

`runtime.WithBundle` is wired at seven sites — and **not one of them is a
subbot child**. All three runners compile a child with
`CompileWorkflowWithHash` and attach no bundle, so a child that lives inside a
bundle silently loses its prompts, skills, attachments, bundle metadata, the
read-only bundle mount in the sandbox, and its own `devbox.json` provisioning
(which the sandbox devbox path requires that mount for).

A `bot://` child is *by construction* a bundle export, so it must compile
through `CompileBundleWorkflow` with `runtime.WithBundle` attached. This is a
prerequisite of the feature, not a nicety.

Note the latent gap this exposes: a **local** subbot pointing inside a bundle
directory is lossy today for the same reason. Fixing that is desirable but is a
deliberate behaviour change — it belongs in the `bot://` lot with its own
tests, never in the inert unification lot (§2).

### 9. Local only in v1 — cloud is explicitly out of scope

`pkg/runner` **never wires `WithSubbotRunner`**. A cloud run reaching a
`subbot` node therefore already dies, *at the node*, on
`pkg/runtime/special_node.go`:

> `subbot %q: no SubbotRunner is wired`

Transporting bundles to a pod that cannot execute a subbot buys nothing. v1
declines cloud `bot://`, and states honestly what that means: today's failure
is a **late, node-time** error. A refusal at launch would be a **new
preflight** (reject a queued run whose IR carries a `bot://` subbot) — small,
worth doing for the diagnostic, but it is added behaviour, not existing
behaviour.

The consumer projects run local studios, so this is off the pilot's critical
path. The cloud sequence, when it comes:

1. wire `WithSubbotRunner` in `pkg/runner`;
2. port child re-attach (ADR-084) to the runner;
3. make child IR available to the pod;
4. add a content-addressed `BotArtifactRef` — modelled on `IRRef`
   (`pkg/queue/types.go`, `S3Client.PutIRBlob/GetIRBlob`, the
   `store.IRBlobStore` seam), with the difference that `IRRef` is keyed per
   run while a bundle artifact is content-addressed and shared, hence its own
   retention policy;
5. mount the bundle's resources read-only;
6. define retention and tenant isolation.

`queue.Contributions` is **not** the channel: it carries markdown
(`skills|commands|agents`) resolved server-side by
`cloudpublisher.resolveContributionsFor` and shipped inline under a 256 KiB
cap. `pkg/pluginsource` is not it either — its git fetch happens on the
launching server, and only markdown reaches the wire.

### 10. The run records the resolved dependency; resume verifies it

Persisted on the run: dependency name, workflow export, bundle version,
`bundle_sha256`, resolved path.

On resume: same identity and same hash → normal resume. Absent or divergent →
**synchronous, explicit refusal**. `--force` accepts the current resolution and
emits an explicit audit event (the `resume --force` precedent).

v1 does **not** promise resume after the bundle is deleted: that would require
materialising the closure into the run store, a cost this ADR declines until a
real need appears. Recording detects the divergence; `bots sync` restores the
artifact; neither reproduces a deleted upstream.

### 11. Studio: read-only from the consumer project, **after** the editor work lands

Navigation resolves `bot://` to the resolved file, opens it read-only, and
hands Copi the context (bundle, version, export, hash). Saving is refused with
a message naming where to edit (the shared bundle's own repository). "Fork into
workspace" stays a manual `cp` until the model is validated on both projects.

Sequencing constraint: the advanced editor surfaces this lot would build on
live in the `feat/assistant-epic` worktree, not in `main`. That branch is dirty
and behind. **This lot waits for its integration** rather than stacking onto
it.

### 12. `${BUNDLE_DIR}` is smaller than expected, and still deferred

Child `.bot` resources already resolve correctly for free: each runner recurses
with `childPath` as the new parent, so a shared subbot's own relative
references resolve against its own root. What does not follow automatically is
external commands — the shared planner still calls
`${VERTICAL_PIPELINE_DIR}/scripts/…`.

Once §8 lands, `WithBundle` already gives the child its read-only bundle mount
in the sandbox. What remains is **exposing the path through var expansion**,
alongside `${PROJECT_DIR}` (`pkg/runtime/engine_resolve.go`, sandbox remap
included).

The pilot still passes the root explicitly, to keep its scope small:

```
subbot feature_author:
  source: "bot://shared-planner/hierarchy-feature-author"
  with:
    pipeline_dir: "{{vars.pipeline_dir}}"
```

This is the hand-rolled version of the same mechanism, and it documents the
need before generalising it.

## Alternatives rejected

- **`contributes: workflows` on `plugin.yaml`** — see §1: a second bot
  distribution system layered on the runtime-extension system.
- **`plugins: {version, sha256}` block in the DSL** — see §5: couples the
  environment lock to the resume hash and to a per-bot file.
- **`schema_version: 2`** — see §3: a hard load failure on every binary not
  yet rebuilt, in exchange for a guard `UnmarshalStrict` already provides with
  a better message.
- **A new tree-hash implementation** — see §7: `collectContentHash` exists and
  is exactly the right function.
- **Content-addressed artifact store now** — the most expensive lot, for a
  capability (`subbot` in cloud) the engine does not have.
- **Status quo (copy-paste) or a git submodule** — the submodule gives sharing
  without an identity recorded on the run, without an export contract, without
  hash verification, and without a read-only guarantee in the consumer's
  editor.

## Consequences

**Positive.** One shared source of truth for the planner subbots; each
project's main bot stays its own. The resolved dependency is recorded on the
run, so "which planner version produced this?" has an answer. The unification
lot pays down a three-way copy-paste that would otherwise have to absorb
`bot://` three times. §8 closes a latent gap for every bundle-resident subbot,
not only shared ones. Nothing in the engine learns about a specific bot: the
resolver is generic, and the shared bundle is an artifact.

**Negative / accepted costs.** Deployment order becomes constrained (rebuild
before installing the shared bundle). The `bots sync` and `bots update`
commands and their verification paths are engine surface that must be
maintained. The shared bundle
carries a `main.bot` it does not conceptually need. Resume after bundle
deletion is not supported in v1. Cloud runs cannot use `bot://` — but they
cannot use subbots at all today.

**Risks and rollback.** The pilot kept each project's local copy until the
locked bundle passed in both consumers. Once the full hierarchy suite and its
script moved into the bundle, those dead copies were removed; rollback is now
a Git revert, not an alternate path that can silently drift back into use.

## Implementation sequence

1. This ADR.
2. A dedicated branch off current `main`.
3. Unify the three subbot resolvers — no functional change.
4. Optional manifest fields (`exports:` / `dependencies:`, schema 1), the
   `bots.lock` model, the shared hash exposed for directories, and
   `iterion bots sync`.
5. `bot://` with exact matching **and** bundle-aware child execution
   (`CompileBundleWorkflow` + `runtime.WithBundle`) in the three runners.
6. Record resolved dependencies on the run; verify the hash on resume, with
   `--force` and an audit event.
7. Pilot `hierarchy-feature-author` only, Town then Tabarria, same
   `bundle_sha256`.
8. `${BUNDLE_DIR}`, then share the scripts.
9. Studio read-only opening — after `feat/assistant-epic` is integrated.
10. Extend to the remaining shared subbots; cloud stays out of scope.
