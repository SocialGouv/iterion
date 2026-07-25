# ADR-079 — Cloud-portable plugin & library skills

Status: **accepted** (2026-07-20).

## Context

Skills reach a run through one mount point — `<workspace>/.claude/skills/` —
fed by three sources, all mirrored at run start (and on every resume) by
`(*Engine).runPersistWorkspace` in [pkg/runtime/engine_run.go](../../pkg/runtime/engine_run.go):

| Source | Resolved from | Mirror |
|---|---|---|
| **bundle** skills | the opened `.botz` / bundle dir (in memory) | `mirrorBundleSkills` |
| **plugin** contributions (skills/commands/agents) | `<iterion-home>/plugins/` + embedded builtins | `mirrorPluginContributions` |
| **library** skills (DSL `skills:`) | `<iterion-home>/skills/` | `mirrorLibrarySkills` |

Two of those three resolve from the **local filesystem**. That is correct for a
local CLI run or an in-process studio, where the operator's iterion home is the
same filesystem the engine runs on.

It is **wrong on cloud**. A cloud run is executed by a separate **runner pod**
([pkg/runner/loop.go](../../pkg/runner/loop.go) builds the same `runtime.Engine`),
and that pod's iterion home is **ephemeral and empty**: the Helm runner
deployment mounts only a Go-cache PVC and a `/run/iterion` emptyDir, the runner
image bakes only the bot catalog (`ITERION_BOTS_PATH=/opt/iterion/bots`), and
nothing syncs the server's `plugins/` or `skills/` dirs to it. The queue
`RunMessage` carried no skill payload either.

The consequence was a **silent** one, which is what makes it serious:

- An operator installs and enables an org-private plugin on the cloud studio.
  Its files land in the *server* pod's home. A run then executes on a *runner*
  pod, `plugin.Load()` there returns only the compiled-in builtins, and
  `mirrorPluginContributions` mirrors nothing. Mirroring is best-effort by
  design (a broken plugin must never fail a run), so **no error surfaces**.
- Same for a DSL `skills:` reference: `wf.Skills` travels inside the IR as a
  bare *name*, and the content was resolved from a library the pod does not
  have. The reference is soft, so it degrades to a warning.

A run that quietly loses its skill still reports success while doing the wrong
thing — the façade failure mode [workflow_authoring_pitfalls.md](../workflow_authoring_pitfalls.md)
is written against. The concrete trigger for this ADR: an org-private
**deploy-target** skill (the platform playbook a bot's deploy phase loads) could
never have worked on the prod cloud instance, and would have looked like it did.

Only bundle skills were cloud-portable, because the bundle is resolved from the
runner image's baked catalog rather than from an iterion home.

## Decision

**Resolve plugin/library skills where they exist — the launching instance — and
ship the resolved content to the runner on the queue message.**

1. **`runtime.Contributions`** ([pkg/runtime/contributions.go](../../pkg/runtime/contributions.go))
   is a pre-resolved payload: plugin markdown files (`Kind` = `skills` |
   `commands` | `agents`) plus library skills (name + frontmatter description +
   body). Handed to the engine via `WithContributions`.
   - `nil` → resolve from the local filesystem (the local path, **unchanged**).
   - **non-nil → authoritative**: mirror exactly this set, never consult the
     local registry/store. An empty payload legitimately means "nothing enabled
     on the launching instance" — it must *not* fall back, or a pod would
     silently re-enter the dead local lookup.
2. **Injected mirroring reuses the same collision policy.** Both injected paths
   stage content through a temp file and go through `reconcileSkillFile`, so the
   4-branch policy (copy / no-op / refresh / shadow) and the precedence
   **bundle > plugin > library > hand-authored** are identical whichever way the
   files arrived. Library skills are still written in the directory form
   `<name>/SKILL.md` — the only shape claude_code's Skill tool discovers.
3. **`queue.Contributions`** is the wire mirror on `RunMessage`, following the
   existing `queue.BudgetOverrides` ↔ `ir.BudgetOverrides` split so the schema
   package stays dependency-free and the publisher never imports the engine.
4. **The publisher resolves it** (`resolveContributions`,
   [pkg/server/cloudpublisher/contributions.go](../../pkg/server/cloudpublisher/contributions.go))
   at **launch and resume** — resume too, because the engine re-mirrors on every
   resume, so a resumed run must carry what a fresh launch would.
5. **`SchemaVersion` 4 → 5.** Adding an optional field is wire-compatible, so
   this bump is deliberate: a stale runner must **reject the message loudly**
   rather than execute the workflow without the skills it was launched with.
   Same rationale as the v=4 (Budget) bump. Cost: during a rolling upgrade the
   server is upgraded first and runs stall until runners follow — accepted,
   because the alternative is the silent façade above.

### Size

The payload is inline on the queue message, capped at **256 KiB**
(`maxContributionsBytes`); NATS' default max payload is 1 MiB and the compiled
IR shares the envelope. Exceeding the cap is an **explicit launch error**, never
a silent truncation — a run that quietly drops its deploy skill is precisely the
failure this ADR removes. (The IR already has an out-of-band `IRRef` fallback —
ADR-075 — which is the natural escape hatch if a real payload ever outgrows the
cap.)

## Consequences

- An operator-installed, **org-private** plugin's skill now reaches cloud
  runner-pod runs. This is what makes an attachable, swappable *deploy-target*
  plugin possible on a cloud instance, and it generalises: every plugin
  contribution kind (skills, commands, agents) and every DSL `skills:` library
  reference is now cloud-portable.
- Local runs are **untouched** — `nil` payload keeps the existing local
  resolution path verbatim.
- The runner no longer depends on its (empty) iterion home for skills. Baking
  plugins into the runner image or mounting a shared volume — the two
  alternatives considered — are both unnecessary, and neither would have
  respected *which* instance/tenant enabled what.
- Plugin **enablement remains global per instance**. This ADR makes an enabled
  plugin's skills portable; it does **not** introduce per-team or per-bot plugin
  scoping. A future per-bot deploy-target binding can build on this channel.
- Hooks (`mergePluginHooks`) are **not** carried yet: they merge into
  `.claude/settings.json` rather than mirroring markdown, so they need their own
  merge semantics on the wire. Out of scope here, and called out so the gap is
  not mistaken for coverage.

## Alternatives rejected

- **Bake `~/.iterion/plugins` into the runner image.** Freezes the plugin set at
  image-build time; an operator enabling a plugin in the studio would still not
  affect runs until a new image ships.
- **Mount a shared volume of the server's iterion home.** Couples runner pods to
  server-pod state, breaks the "pod is ephemeral, state lives in Mongo+S3" cloud
  invariant, and still ignores which tenant enabled what.
- **Leave it and document that plugins are local-only.** Rejected: the failure
  is silent, and the studio actively offers plugin enable/disable in cloud mode,
  so the UI would be promising something the runtime does not deliver.
