# Skill library

The **skill library** is a curated, standalone collection of Claude-Code-style
`SKILL.md` skills that you author and edit independently of any bot, and
reference from any workflow by name. It is the general-purpose counterpart to
the two pre-existing, artifact-coupled skill sources:

| Source | Where it lives | Scope |
|--------|----------------|-------|
| **bundle** skills | `<bundle>/skills/*.md` | one bot |
| **plugin** skills | `~/.iterion/plugins/<name>/skills/` | a shared, enable/disable-able pack |
| **library** skills (this doc) | `~/.iterion/skills/` (+ per-project) | any workflow, referenced by name |

All three mirror into a run's `<workspace>/.claude/skills/` at launch, where
both `claude_code` (native `--setting-sources project` lookup) and the `claw`
`skill` tool discover them. See [ADR-059](adr/059-skill-library.md).

## Storage

Skills live under a machine-global directory with an optional per-project
override that shadows the global by name:

```
~/.iterion/skills/<name>/SKILL.md          # global (default)
<store-dir>/.iterion/skills/<name>/SKILL.md   # per-project override (--project)
```

A flat `<name>.md` form is also read (as produced by an imported pack); `add`
writes the directory form (which can carry auxiliary files) unless a flat file
already exists. A skill is plain markdown with optional YAML frontmatter:

```markdown
---
name: changelog-writer
description: Writes changelogs from a range of commits.
---

# Changelog writer

Imperative guidance the agent follows when this skill is loaded…
```

The library's canonical name for a skill is its **directory/file basename** —
that is the DSL reference and the mirror target. The frontmatter `description`
feeds the prompt hint; a frontmatter `name` does not override the on-disk name.

No sealing: a skill is public guidance text, not a secret.

## Referencing a skill in a workflow

Add a `skills:` list to an `agent`/`judge` node, or a workflow-level default:

```
agent draft:
  model: "anthropic/claude-sonnet-4-6"
  skills: ["changelog-writer", "semver-bump"]

workflow main:
  entry: draft
  skills: ["house-style"]          # default for every node
  draft -> done
```

Kebab-case names must be **quoted** (the lexer does not treat `-` as an
identifier character); a bare ident works for simple names.

At run start iterion resolves the union of the workflow default and every
node's list against the library, mirrors each **resolved** skill into
`.claude/skills/`, and injects a `## Skills` section into that node's system
prompt listing only the skills that node references (name + description). The
skill body is loaded on demand by the agent from `.claude/skills/` — it is not
inlined into the prompt.

An unknown reference is **soft**: the compiler emits no error for a
well-formed-but-absent name (compiles stay portable — CI without the library
passes), and the runtime logs a warning and skips it. A malformed name (path
separator, leading dot, empty) warns at compile time as **C199**.

### Precedence on name collision

`bundle > plugin > library > hand-authored`. The library mirrors **last**, so a
same-named bundle or plugin skill wins, and a file you placed by hand in
`.claude/skills/` (no `.iterion-managed` marker) shadows all three. Collisions
are logged, never silent.

### Cloud runs (ADR-079)

The library lives on the filesystem (`~/.iterion/skills`), which a **cloud
runner pod** does not have — its iterion home is ephemeral and empty. A DSL
`skills:` reference travels inside the IR as a bare *name*, so before
**ADR-079** the pod resolved that name against an empty store and mirrored
nothing, warning but not failing (the reference is soft by design).

Now the launching instance resolves each referenced skill's body + description
and ships them on the queue message (`RunMessage.Contributions`, schema v5);
the runner mirrors that payload through the same collision policy, so
precedence and the `## Skills` prompt hint are identical to a local run. No
authoring change is required — reference skills by name exactly as before.

### Scope

`add`/`rm` default to the **global** store; `--project` targets the per-project
override, which fully shadows the global of the same name (identical to the
local secret store's layered semantics).

## CLI

```sh
iterion skill add <name> --from <file>        # create/overwrite (global; stdin if no --from)
iterion skill add <name> --project            # per-project override
iterion skill list                            # both scopes, with descriptions
iterion skill show <name>                      # resolved path + full body
iterion skill rm <name> [--project]
iterion skill export <name> [<dir>]            # copy the markdown out
iterion skill import <git-url|path>            # install a public skill pack (see below)
```

## Importing third-party packs (the hybride model)

The library holds the skills **you** author. To install a **public pack** —
a bare `skills/` git repo — use `iterion skill import <git-url>`, which
delegates to the plugin install path: it synthesizes a skills-only
`plugin.yaml` and installs the pack under `~/.iterion/plugins/<name>/`,
disabled by default. Enable it with `iterion plugin enable <name>` to have its
skills mirror into runs. See [docs/plugins.md](plugins.md#public-skill-libraries-shipped).

So: **library** = your editable, per-skill store; **plugin pack** = a shared,
versioned, enable/disable-able unit. `iterion skill import` bridges the two.

## Studio

The studio surfaces a **Skills** view (nav → Extend, gated on
`server_info.skills_enabled`, local mode only) backed by
`/api/local/skills` — list / create / edit (markdown editor) / delete, with the
global/project scope selector.

## Implementation pointers

- Store: [pkg/skilllib/store.go](../pkg/skilllib/store.go) (layered global +
  project) + shared frontmatter parser [pkg/skilllib/frontmatter.go](../pkg/skilllib/frontmatter.go).
- DSL field: token/AST/parser/IR/validate mirror `capabilities:`; C199 in
  [pkg/dsl/ir/validate_skills.go](../pkg/dsl/ir/validate_skills.go).
- Runtime mirror + prompt hint: [pkg/runtime/library_skills.go](../pkg/runtime/library_skills.go)
  (reuses `reconcileSkillFile`); `## Skills` section in
  [pkg/backend/delegate/delegate.go](../pkg/backend/delegate/delegate.go)
  (`BuildSystemPrompt`).
- CLI: [cmd/iterion/skill.go](../cmd/iterion/skill.go) + [pkg/cli/skilllib.go](../pkg/cli/skilllib.go).
- Server: [pkg/server/local_skills_routes.go](../pkg/server/local_skills_routes.go).
- Studio: [studio/src/views/Skills.tsx](../studio/src/views/Skills.tsx) + [studio/src/api/skills.ts](../studio/src/api/skills.ts).
