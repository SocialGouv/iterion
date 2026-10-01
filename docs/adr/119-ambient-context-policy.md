# ADR-119 — `ambient_context`: what an agent node inherits besides its prompt, declared and the same on every backend

- Status: proposed
- Date: 2026-10-01
- Deciders: jo (direction: neither interactive-session doctrine nor the
  operator's personal setup should reach a bot; full backend parity in one
  change), Claude (measurements, analysis)
- Relates to: issue #2062;
  [061-per-backend-system-prompt-composition-mode.md](061-per-backend-system-prompt-composition-mode.md)
  (amended: the `user` scope leaves claude_code's default);
  [085-pi-as-execution-backend.md](085-pi-as-execution-backend.md);
  [docs/merge-gate.md](../merge-gate.md) (the per-node `setting_sources:`
  follow-up this record answers)

## Context

Besides its `.bot` prompt, an agent node reads whatever its harness picks up
from the disk: a repository's `CLAUDE.md`, `.claude/rules/`, `AGENTS.md`, and
the operator's own agent home. Which of these reach a node is decided today by
each backend's defaults and by two process-wide environment variables. The bot
has no say. Measured on `main` @ `1c0779ab6` and `3558d029f`, Claude Code CLI
2.1.282 and codex-cli 0.156.1. Each measurement plants a marker in every
candidate file and searches the session transcript for it, with a deliberately
invalid API key, so no measurement cost anything:

| backend | what a node receives today | knob |
|---|---|---|
| `claude_code` | `--setting-sources user,project`: the repository's memory (`CLAUDE.md`, `.claude/rules/**`), its settings and skills, and the operator's user scope (memory, rules, settings, hooks, skills, commands). The structured-output pass passes no `--setting-sources` at all, so it loads every scope, `local` included. | `ITERION_CLAUDE_CODE_SETTING_SOURCES`, process-wide; its `none` omits the flag, which loads everything |
| `claw` | no instruction file at all; iterion uses claw-code-go's auto-memory section only | — |
| `pi` | the first of `AGENTS.override.md`, `AGENTS.md`, `CLAUDE.md` in every directory from the working directory up to `/`, plus `~/.pi/agent/AGENTS.md` | `ITERION_PI_NO_CONTEXT_FILES=1`, process-wide, all or nothing |
| `codex` | `AGENTS.md` from the repository root down to the working directory, plus `$CODEX_HOME/AGENTS.md` | — |
| `opencode`, `kimi`, `grok` | their own conventions; `opencode` has its Claude-compat layer pinned off | — |

Three measurements make the `user` / `project` split of `--setting-sources`
insufficient on its own:

- **Project memory walks up to `/`, not to the repository root.** With
  `--setting-sources project` and a workspace under `$HOME`, which is every
  developer laptop, the CLI still loads `~/.claude/CLAUDE.md` and
  `~/.claude/rules/**`: `~/.claude/` is the `.claude/` directory of an
  ancestor. Excluding `user` does not keep the operator out.
- **A run worktree nested in the repository** (`.iterion/worktrees/<run-id>`)
  also loads the primary checkout's `CLAUDE.md` through that walk, so a node
  pays for the repository's instructions twice.
- **`project` cannot be dropped.** The engine mirrors a bundle's skills and its
  plugins' commands, agents and hooks into `<workspace>/.claude/`, and the CLI
  only discovers them under the `project` scope
  (`pkg/runtime/plugin_skills.go`).

Consequences today:

1. **Parity.** The same node receives the repository's instructions on
   `claude_code` and none on `claw`. This goes against the pre-arbitrated
   parity addendum.
2. **Reproducibility.** A `claude_code` node on a laptop inherits the
   operator's personal setup: 27 rule files and 33.5 KB on the reporter's
   machine, several written for a human in the loop. It does or does not
   depending on the credential channel, which moves `CLAUDE_CONFIG_DIR` but
   not the walk. A cloud runner never does. One bot, three behaviours.
3. **Interactive doctrine reaches bots.** Anything a repository puts in
   `.claude/rules/` steers every `claude_code` node working in it. Guidance
   meant for interactive sessions has to be neutralised by prose ("bot runs are
   out of scope") that every node pays for in tokens.
4. **No control, no cost lever.** A review bot cannot drop the context
   injection that dominates its cost floor ([docs/merge-gate.md](../merge-gate.md)),
   or keep a pull request's own `CLAUDE.md` out of the reviewer's
   instructions.

## Decision drivers

- Philosophy #1: maximum power to the user. An author and an operator choose,
  per bot and per run, and an escape hatch stays.
- Philosophy #2, the Nth-variant test. Two backend-specific environment
  variables already exist, so the seam is missing.
- Philosophy #3: the same bot behaves the same on a laptop and in a cluster.
- The parity addendum: a capability wired for one backend is wired, or
  refused by type, on the others.
- Workspace safety, already stated for claw's workspace commands: "a
  workspace is a checkout of a repository the run does not control, and host
  state does not belong in a sandboxed or multi-tenant run"
  ([docs/backends.md](../backends.md)).

## Considered options

1. **Status quo plus scope clauses in prose.** Rejected. Every node pays the
   tokens, compliance depends on the model, an author has no control, and
   nothing stops the operator's setup from leaking.
2. **More backend-specific environment variables.** Rejected. They stay
   process-wide, never per bot. This is the very pattern the Nth-variant test
   flags.
3. **The engine resolves one file set and injects it into every backend,**
   with every native loader turned off. Rejected for `claude_code`: its
   native memory semantics are imports, rules made conditional by `paths:`,
   and nested `CLAUDE.md` files loaded on demand. Reproducing them would
   reopen the adaptivity gap ADR-061 closed. It is kept, bounded, for `pi`,
   whose native walk cannot be bounded (see below).
4. **A declared policy, translated by every backend into its own native
   mechanism.** Chosen.

## Decision

### The field

```
ambient_context: none | workspace | operator | all
```

- It can be set on a node (`agent:` / `judge:`) and on the workflow block.
- A run override (`--ambient-context` on `iterion run`/`resume`,
  `ambient_context` on the launch API) and `ITERION_AMBIENT_CONTEXT` complete
  it.
- Precedence follows `auto_memory`: run override > node > workflow >
  environment > default.
- **The default is `workspace`.**

The two origins:

- **workspace** is the repository the node works in: its instruction files,
  meaning the backend's native ones (`CLAUDE.md`, `.claude/rules/`,
  `AGENTS.md` …), from the working directory up to the **repository root**.
  That root is the git top level of the working directory; outside a
  repository it is the working directory itself. Directories above it are
  never workspace. The repository's settings, skills, commands and hooks are
  not governed by this field and keep loading: they carry the engine's own
  mirrored skills and plugin contributions. Whether a repository's settings
  are trusted is #1719's question.
- **operator** is what the host provides beyond the repository: the
  operator's agent home (`~/.claude` or `$CLAUDE_CONFIG_DIR`, `$CODEX_HOME`,
  pi's agent directory), with its instruction files and, where the backend
  ties them to the same switch, its personal settings, hooks, skills and
  commands. It also covers instruction files in directories above the
  repository root.

### Per-backend translation

| backend | `none` | `workspace` | `operator` | `all` |
|---|---|---|---|---|
| `claude_code` | `--setting-sources project` + `claudeMdExcludes: ["**"]` | `project` + `claudeMdExcludes` of every ancestor above the root (`<a>/CLAUDE.md`, `<a>/CLAUDE.local.md`, `<a>/.claude/**`) | `user,project` + `claudeMdExcludes: ["<root>/**"]` | `user,project` |
| `claw` | no project-instructions section | claw-code-go's project instructions with the walk stopped at the root, user memory off, `.claude/rules` on | user memory and the ancestors above the root | both |
| `pi` | `--no-context-files` | `--no-context-files` + the engine-resolved workspace files, appended to the system prompt with pi's own per-directory precedence | the same with the operator files (agent dir and ancestors) | both |
| `codex` | `project_doc_max_bytes=0` + a per-run `CODEX_HOME` without `AGENTS.md` | per-run `CODEX_HOME` without `AGENTS.md` | `project_doc_max_bytes=0` | unchanged |
| `opencode`, `kimi`, `grok` | not enforced: diagnostic C185 | | | |

- **`claude_code`.** `claudeMdExcludes` rides the flag settings layer, the
  same `--settings` object that pins the routing variables, so it outranks
  every settings file. Both spawns of a node follow its policy: the session,
  and the structured-output pass.
- **`claw`.** The claw-code-go change adds three prompt options:
  - a toggle for user memory;
  - a walk-up boundary;
  - `.claude/rules/**/*.md` without `paths:` frontmatter.

  A conditional rule only applies while the agent works on files that match
  its `paths:` globs. A static system prompt cannot express that, so claw
  skips conditional rules; this is the one recorded divergence.
- **`codex`.** `project_doc_max_bytes` is merged into the same configuration
  map as `web_search`. The per-run `CODEX_HOME` mirrors the operator's
  entries (`auth.json`, `config.toml` …) by symlink, minus `AGENTS.md` and
  `AGENTS.override.md`. Authentication and routing are unchanged; only the
  instructions are gone. Codex's project-doc walk already stops at the
  repository root.
- **`pi`.** pi's native walk goes up to `/` and cannot be bounded. So iterion
  always passes `--no-context-files` and supplies the files itself. They are
  resolved with pi's rule: the first existing file per directory among
  `AGENTS.override.md`, `AGENTS.md`, `AGENTS.MD`, `CLAUDE.md`, `CLAUDE.MD`,
  root-most first.
- **`opencode`, `kimi`, `grok`.** No mechanism is known. An explicit value
  raises the C185 warning, which names the gap. The default is documented
  as not enforced. Each backend gets a follow-up ticket.

### Diagnostics

- **C184**, an error: an invalid `ambient_context` value.
- **C185**, a warning: the declared value is not enforced on the node's
  backend. It fires on explicit values only, never on the default.

### Legacy variables

`ITERION_CLAUDE_CODE_SETTING_SOURCES` and `ITERION_PI_NO_CONTEXT_FILES=1` stay
as raw, backend-specific overrides for operators who need a combination the
policy does not name. When `ITERION_CLAUDE_CODE_SETTING_SOURCES` is set, it
replaces the policy's translation for `claude_code`, and the engine logs so
once. Its `none` now passes `--setting-sources ""`. Omitting the flag, as
today, loads every scope.

## Consequences

- A `claude_code` bot on a laptop stops inheriting the operator's personal
  setup by default: memory, rules, user settings, hooks, skills and commands.
  An operator who wants it back sets `ITERION_AMBIENT_CONTEXT=all`. A bot
  author who needs it declares `ambient_context: all`.
- Nested run worktrees stop paying twice for the repository's `CLAUDE.md`.
- `claw` nodes gain the repository's instructions by default, the parity
  cost. A bot that wants the old behaviour declares `none`, which is also the
  cost lever the merge gate asked for.
- ADR-061's "adaptivity parity" paragraph now reads `project` plus the
  ancestor exclusions, instead of `user,project`.
- The default cannot be enforced on `opencode`, `kimi` and `grok`. That gap is
  documented and ticketed; it does not fail silently on explicit values.

## Plan review

The cross-family plan review (codex) was not run, by the operator's decision
on 2026-10-01. The implementation follows this record as written. Each slice
gets a local adversarial round, and so does the whole diff before the push.
