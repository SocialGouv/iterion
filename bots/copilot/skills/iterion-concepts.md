---
name: iterion-concepts
description: The iterion mental model — what a bot, a run, a bundle, a skill and the catalog are; how the compilation pipeline and the runtime fit together; backends, sandbox, the board, triggers and schedules. Load in info posture to answer "what is X" and "how does X work" without guessing.
---

# The iterion mental model

Iterion is a workflow-orchestration engine with its own DSL. A runnable
workflow is a `.bot` file; a packaged one is a `.botz` bundle.

## The pipeline

```
.bot source
  → lexer (indentation-sensitive)
  → parser → AST
  → compile → IR (nodes, edges, schemas, prompts, budget)
  → diagnostics (Cxxx)
  → runtime: execution with events, budget and persistence
```

`iterion validate <file>` runs everything up to the diagnostics — no API
call, no credential, no network. It is the cheapest possible check and
the only acceptable proof that a workflow is syntactically sound.

## Bot, run, bundle, catalog

- A **bot** is a workflow: `main.bot` plus, in bundle form, a
  `manifest.yaml` and optional `skills/`, `prompts/`, `presets/`,
  `attachments/`.
- A **run** is one execution of a bot. It has an id, a status, an event
  stream, versioned node outputs, and a checkpoint. Everything about a
  run is inspectable after the fact.
- The **catalog** is the set of bots discoverable in a workspace —
  conventionally `<workdir>/bots`, `<workdir>/examples`,
  `<workdir>/.botz`. `iterion bots list` is the authoritative answer to
  "what can I run here"; the answer differs per workspace.
- A **manifest** declares metadata the engine and the studio read
  without executing the bot: display name, description, `when_to_use`,
  triggers, capabilities, launch hints, dispatch vars, produces/consumes.

## Skills

A skill is a `SKILL.md`-style markdown file with `name` and
`description` frontmatter plus an imperative body. Skills ship **inside
the bundle they support** and are mirrored into the workspace's
`.claude/skills/` at run start and on every resume, so all backends see
the same directory.

Progressive disclosure is the point: the system prompt only carries each
skill's name and description (about one line each); the agent loads a
body on demand. That is what lets a bot carry a lot of knowledge for a
few hundred tokens.

There is also a standalone **skill library** (`iterion skill …`), global
and per-project, referenced from a workflow's `skills:` field.

## Backends

| Backend | What it is |
|---|---|
| `claude_code` | the Claude Code CLI — richest native toolset, own session transcript |
| `claw` | in-process multi-provider API client; iterion owns the message list |
| `pi` | the pi agent CLI, ~36 providers, provider-computed cost |
| `kimi`, `grok` | vendor CLIs through the generic CLI-agent seam |
| `codex` | **deprecated and frozen** — emits `C030`, do not adopt |

Two behavioural differences worth knowing by heart:

- **System prompt composition.** For `claude_code`, a node's `system:`
  text is *appended* to the CLI's native prompt (never replaces it), so
  the native agentic posture survives. For `claw`, there is no native
  prompt, so iterion prepends an authored baseline before the node's
  text.
- **Tool restriction.** On `claude_code`, `tools:` is a no-op; on
  `claw`, it is a real allowlist — and an empty one yields *zero* tools.

When neither the node nor the workflow names a backend, the runtime
probes the host for credentials and picks by preference order.

## Sandbox

Per-run container isolation is **on by default** (`sandbox: auto`): it
reads a devcontainer config if present, else falls back to a published
image, and degrades visibly when the host cannot sandbox. `sandbox:
none` opts out and raises `C128`. Network egress is unrestricted unless
a policy is declared.

Trade-off to explain when asked: the sandbox protects the host, but it
also *freezes* the workspace view at container start and adds a
container start to every resume — which is why interactive/conversational
flows often opt out deliberately.

## Persistence

```
<store-dir>/runs/<run_id>/
  run.json            # status, inputs, checkpoint  ← the source of truth
  events.jsonl        # timestamped events           ← observational only
  artifacts/<node>/<v>.json
  interactions/<id>.json
  report.md
```

The store dir is anchored on the working directory. Two runs launched
from different directories can land in different stores — which is the
usual reason the studio "cannot find" a run.

## The board and the dispatcher

Iterion ships a native kanban board (`iterion issue …`, the `/board`
view) plus adapters for GitHub Issues and Forgejo. The **dispatcher**
(`iterion dispatch <config.yaml>`) polls a tracker and launches one run
per eligible ticket, with retry, stall detection and per-state
concurrency.

Agent and judge nodes reach the board by declaring `capabilities:`
(`board.read`, `board.create`, `board.move`, …); the runtime opens the
matching tools transparently, by a different transport per backend but
through one shared implementation.

## Triggers, schedules, webhooks

- **`iterion schedule`** wires recurring bots through the host crontab —
  no resident daemon.
- **Inbound webhooks** launch a bot per external forge event, behind
  per-org tokens, rate limits and quotas.
- **The trigger spine** is the unifying layer: one canonical event
  envelope, an internal bus, and subscriptions binding
  *(event filter) → (bot launch)*. Board transitions, run completions,
  schedule ticks and forge events all ride it.

## Studio

`iterion studio` serves the visual editor, the run console, the board,
the pipelines view, the bot gallery and a launch modal. The run console
renders a run's conversation from its event stream, and its chat panel
*steers* a live run — it queues a message into the running agent rather
than starting a new conversation.

## Two doctrines worth quoting

**Catalog bots are repo- and stack-agnostic.** A bot in the catalog must
run against *any* repository, in any language. Layout- or
ecosystem-specific knowledge lives in its skills, not in its DSL. Adding
support for a new language should mean adding a skill, not editing a
workflow.

**Improvement loops must converge to an asymptote.** A review/improve
loop settles into a stable approved state and stops; it does not
oscillate. The shipped mechanism is one capable agent plus a
deterministic verify gate (a real exit code) plus a machine-checkable
termination flag, closing a single bounded loop.

## Honest boundaries

Iterion is large and moves. When asked about a flag, an endpoint or a
field you are not certain of, check rather than assert:

```
iterion <command> --help
iterion bots list
iterion models
iterion version
```

"I'm not sure, here is how to check" is a good answer. An invented flag
is not.
