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

All three mirror into a run's `<workspace>/.claude/skills/` at launch. Claude
Code discovers that directory through its native `--setting-sources project`
lookup, claw through the `skill` tool, and pi through the explicit
`--skill <workspace>/.claude/skills` argument in both RPC and print modes. See
[ADR-059](adr/059-skill-library.md).

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

## Adding a skill to a run you did not author

A bot's `skills:` list is its author's. To bring your OWN skill to a run —
a house authoring standard, a domain playbook — without editing someone
else's bundle:

```sh
iterion skill add bot-authoring --from ~/standards/bot-authoring.md
iterion run bots/copilot/main.bot --skill bot-authoring      # this run
export ITERION_SKILLS=bot-authoring                          # every run, this machine
```

Four properties, each deliberate:

- **Additive, never a filter.** The run's list is UNIONED with the
  workflow's; nothing an operator passes can remove a skill the bot's
  author declared. Flag and env are likewise unioned with each other — a
  machine-wide standard and a per-run addition are both things you asked
  for, so neither silently replaces the other.
- **Not posture-aware, on purpose.** It is tempting to bind skills to a
  bot's mode (design / debug / …). A node's mode is a *bias its own turn
  may flip*, so filtering the roster by it would lock the agent out of a
  skill exactly on the turn it discovers it needs one. Emphasis belongs in
  the bot's prompt; availability does not.
- **A name that resolves to nothing is a launch ERROR**, listing what the
  library does hold — unlike the workflow's own reference above, which
  stays soft. Nobody typed those; you typed this one, and dropping it with
  a log line is precisely the silent failure that makes a bot look dumber
  instead of misconfigured.
- **It survives resume.** Persisted as `extra_skills` on the run and
  re-applied on every resume. Load-bearing for a conversational bot: the
  studio dock drives one resume per message, so a launch-only list would
  be gone by your second reply.

The run says so on its own event stream — a `skills_injected` event naming
what was added and whether it came from the flag or the env. Without that,
a run carries knowledge its `.bot` never mentions and a bug report against
it is irreproducible.

**Cloud runs cannot carry these yet.** The launch surfaces that would set
them (studio Launch modal / HTTP API) are not wired, so the list is a
CLI-and-local-studio-resume feature today. When that surface lands, the
cloud half is one union: the run's extras join `collectWorkflowSkillRefs`
in `pkg/server/cloudpublisher/contributions.go`, whose payload already
ships each library skill's *content* to the pod (`queue.LibrarySkillFile`)
because a runner pod has no library on disk.

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
