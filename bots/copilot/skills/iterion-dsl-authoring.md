---
name: iterion-dsl-authoring
description: Writing and fixing a .bot workflow — node types, edge syntax, references, budget, and the syntax traps that compile clean but fail at runtime (comment marker inside prompts, multi-line lists, permission glob semantics, silent underscore keys). Load before writing or editing any DSL.
---

# Authoring a `.bot`

Always finish with `iterion validate <file>`. It parses, compiles and
validates without any API call — exit 0 or it isn't done.

## Shape of a file

Top-level blocks, in any order:

```
vars:              # typed inputs with defaults
prompt <name>:     # indented text body
schema <name>:     # typed fields
cursor <name>:     # calibration dials (optional)
mcp_server <name>: # external MCP server (optional)
<node declarations>
workflow <name>:   # entry, controls, edges
```

## Node types

| Type | What it is |
|---|---|
| `agent` | LLM node with tools, structured I/O, a backend |
| `judge` | LLM node producing a verdict, usually no tools |
| `router` | `fan_out_all`, `fan_out_each`, `condition`, `round_robin`, `llm` |
| `human` | pause/resume; the operator answers |
| `tool` | direct shell command, no LLM |
| `compute` | deterministic expression, no LLM and no shell |
| `subbot` | runs another `.bot` as a nested child run |
| `emit` / `wait` | in-run event pair; `wait` needs a `timeout:` |
| `await_answers` | blocks until async questions are answered; needs `timeout:` |
| `done` / `fail` | terminals |

## Edges

```
src -> dst                          # default
src -> dst when <field>             # boolean field from src's output
src -> dst when not <field>
src -> dst else                     # fires only if no sibling `when` matched
src -> dst as loop_name(5)          # bounded loop
src -> dst with { f: "{{ref}}" }    # data mapping
```

References: `{{input.field}}`, `{{vars.name}}`, `{{outputs.node}}`,
`{{outputs.node.field}}`, `{{artifacts.name}}`.

Nodes with several incoming branches declare `await: wait_all` or
`await: best_effort` — convergence is a property of the *downstream*
node, there is no join declaration.

## The traps

These compile clean (or fail with a misleading message) and cost real
sessions. Check every one before saying a workflow is correct.

### 1. `##` is the COMMENT marker — never inside a prompt body

```
prompt bad:
  ## Mode: {{input.mode}}      <-- eaten as a comment, indentation breaks
  {{input.mode}}
```

This produces a cascade of `E001/E002` errors that point at the *next*
lines, plus phantom `C016 unreachable` on unrelated nodes. Worse, if it
somehow parsed, that line would never reach the model. Use plain text or
a rule of dashes for headings inside prompts.

### 2. Lists are INLINE arrays, never multi-line YAML

```
tools: ["read_file", "grep"]                 # correct
allow: ["Read(**)", "Bash(git log:*)"]       # correct

tools:                                       # WRONG — E002 + E012 cascade
  - read_file
```

The lexer is indentation-sensitive; a list does not continue onto the
next line.

### 3. `tools:` behaves differently per backend

- **claude_code**: `tools:` is a **no-op**. Under the always-on
  `--permission-mode bypassPermissions` the agent keeps the full native
  toolset. Declaring a short list advertises a restriction that does not
  exist. The real gate is `permission:`.
- **claw**: `tools:` **does** restrict — and an **empty** `tools:` list
  means the node gets **zero** tools, including board capabilities and
  MCP tools. The opposite of claude_code. If a claw node needs board or
  MCP tools, `tools:` must be non-empty.
- Some tools are injected **outside** `tools:` anyway: `structured_output`,
  `ask_user`, `todo_write`, and the `memory_*` family when a `memory:`
  block is present. Listing `memory_read` in `tools:` fails to resolve
  and kills the node at runtime.

### 4. Permission patterns match ABSOLUTE paths, and `*` has no path meaning

The matcher anchors `^…$` and turns both `*` and `**` into `.*` with no
segment semantics. Read/Write/Edit patterns are matched against the
**absolute** path the agent emits.

```
deny: ["Read(.env*)"]      # INERT — never matches /home/u/repo/.env
deny: ["Edit(pkg/**)"]     # INERT — never matches /home/u/repo/pkg/x.go
deny: ["Read(*.env*)"]     # correct — the token wrapped in stars
```

An over-broad deny is safe; an under-broad deny is invisible. Precedence
is **deny > ask > allow**, so a broad `allow: ["Read(**)"]` bounded by a
precise deny list is a valid shape.

`Grep` and `Glob` are **not** path-scopable: the matcher sees the
*pattern*, which the model supplies. The only reliable control is
denying the bare tool.

Mode matters: `deny` blocks headlessly, `ask` **pauses the run** for
human approval. In a conversational bot, `ask` looks like a freeze —
use `deny`.

### 5. `permission:` without a mode does nothing

Declaring `allow:`/`deny:` lists while leaving the mode at its default
(`off`) produces a bot that *looks* protected and isn't. Diagnostic
`C111` flags exactly this.

### 6. Budget is CUMULATIVE over the run's whole life

On every resume, the engine re-seeds tokens, cost and iterations from
the checkpoint. Only the *pause gap* is excluded, and only from the
duration dimension.

So for a looping/conversational bot, `max_iterations` and `max_cost_usd`
are **session** caps, not per-turn caps. Sizing them like a one-shot
bot's kills the run after a handful of turns — with a `BUDGET_EXCEEDED`
that looks unrelated to the real cause.

### 7. Underscore-prefixed keys are exempt from reference diagnostics

`_session_id`, `_session_fingerprint`, `_reasoning_effort` and friends
are runtime-injected, so the compiler does **not** check them against a
schema. A typo compiles clean and silently disables the feature — the
classic symptom is a conversational bot that "forgets" everything
between turns because `_session_id` was misspelled on the loop edge.

Treat the loop edge's mapping as load-bearing and change it only
alongside its test.

### 8. A declared `mcp_server` is not active until a node selects it

Declaring the block is half the job; the node (or workflow) must also
carry `mcp: servers: [<name>]`. Without it the agent gets **zero** MCP
tools and the failure is completely silent. On claw, an unreachable MCP
server is worse: it fails the whole node.

### 9. `worktree` and `sandbox` defaults

`sandbox` defaults to `auto` — opting out with `sandbox: none` raises
`C128`, which is a warning you are expected to justify in a comment.
`worktree: auto` creates a fresh git worktree; a bot that never commits
should use `worktree: none` or it will only produce empty storage
branches.

## Convergence

Any improvement/review loop must reach an asymptote — settle and stop,
not oscillate. The shipped pattern is: one capable agent + a
deterministic verify gate (a real exit code, never an LLM judgment) + a
machine-checkable termination flag + a single bounded
`continuation_loop(N)`. Judge the working tree (`git diff HEAD`), and
make untracked files visible (`git add -N .`) before diffing, or a
change that only *adds* files is invisible.

## Diagnostic families

`C0xx`–`C2xx` are compile diagnostics. Common ones:

| Code | Meaning |
|---|---|
| C016 | node unreachable from entry (often a *cascade* from a parse error) |
| C030 | node uses the deprecated `codex` backend |
| C031/C032 | reference to a field absent from a schema |
| C080/C081 | unknown / malformed `capabilities:` entry |
| C083–C086 | cursor declaration problems |
| C089 | `reasoning_effort: ultracode` on a model that can't hold it |
| C102 | invalid `compress:` value |
| C110–C112 | permission mode / rule-list problems |
| C128 | `sandbox: none` opt-out (warning) |

When a validate run produces a dozen errors, fix the **first** one and
re-run: parse errors cascade, and the later messages usually name
innocent nodes.
