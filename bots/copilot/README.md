# copilot — Copi, the iterion assistant

ONE user-facing agent in a standing chat loop. Copi's **subject** is iterion
itself — the `.bot` DSL, the `Cxxx` diagnostics, run/resume semantics,
backends, bundles and convergence doctrine — but it advises about **whatever
workspace it is pointed at**, so that knowledge travels in this bundle's
[`skills/`](skills/), never in a `docs/` path of the tree under discussion.

## Shape (Terra → Sol + judge → Terra)

```
seed → turn_state → copi (Terra) → validate_scope_guard → terra_route
 ▲                                                            │
 │                       should_validate ──→ validate_draft ──┤
 │                                                            ▼
 │                       should_deliver / else ──────────→  gate ── is_close ──▶ done
 │                                                            │ (else)
 │                                                            ▼
 └─ conversation_loop(1000) ─ normalize_chat_turn ← extract_authoring_context ← chat ← compose

private lane — terra_plan_execution_cycle(1000):
terra_route ─ start_reflection / clarify_reflection ─▶ reflection_state → reflect (Sol) → judge
turn_state ◀── implementation_handoff ◀── judge_route ◀────────────────────────────────────┘
```

- **seed** (entry, compute) — fills the uniform per-turn input from the vars on
  turn 1; the loop edge fills the same contract from the chat answer and Copi's
  previous output afterwards. ONE shape for entry and loop alike.
- **copi** (agent) — *Terra*, the sole user-facing entry and executor. `claw` +
  `${ITERION_COPILOT_ENTRY_MODEL:-openai/gpt-5.6-terra}` at
  `${ITERION_COPILOT_EFFORT:-medium}`, `session: persist` on slot
  `assistant_conversation`, `interaction: human` so it can ask mid-turn,
  `tool_max_steps: 40`, tools `[read_file, glob, workspace_grep,
  diagnostic_shell, skill]`, capability `runs.read`. It answers simple turns
  directly and asks Sol for a plan on complex ones.
- **validate_scope_guard** + **terra_route** (compute) — apply the session's
  scope exclusions identically to every effect-bearing output, then route the
  turn: `scope_repair` (`terra_scope_repair(1)`), `start_reflection` /
  `clarify_reflection` into the private lane, `execution_retry`
  (`terra_actionless_execution_retry(3)`), `should_validate`, or delivery.
- **reflect** (agent) — *Sol*, the private persisted planner: `claw` +
  `${ITERION_COPILOT_REFLECTION_MODEL:-openai/gpt-5.6-sol}` at
  `${ITERION_COPILOT_REFLECTION_EFFORT:-high}`, slot `assistant_reflection`,
  `tool_max_steps: 12`. It emits no host action and never becomes a second
  visible assistant voice.
- **judge** (judge) — challenges Sol's plan: `claude_code` +
  `${ITERION_COPILOT_REVIEWER_MODEL:-claude-opus-5}` at
  `${ITERION_COPILOT_REVIEWER_EFFORT:-medium}`, `session: fresh`,
  `tool_max_steps: 24`, tools `[read_file, glob]`. Fallbacks in order: `kimi`
  (`kimi-code/k3`), `grok` (`grok-4.6`), then `reviewer_unavailable` with
  `action: skip` — with no reviewer reachable, `judge_route` sees
  `outputs.judge._skipped` and the debate is skipped rather than the
  conversation killed.
- **implementation_handoff** (compute) — returns the debated plan to Terra; a
  blocker found during execution re-enters the same private lane, bounded by
  `terra_plan_execution_cycle(1000)`.
- **gate** (compute) — projects `is_close` and passes the reply through, after
  appending the validator's verdict (below).
- **chat** (human) — `interaction: human_or_host`, and no `prompt`: Copi's
  reply IS the node's runtime-resolved `instructions:`. The pause is
  budget-free and can last days — this is the standby home base, and also
  where a studio host event (an executed-action receipt) re-enters the run.
  The manifest replays the last 8 messages / 12000 estimated tokens of
  `conversation_history` into it.
- Exit: `gate -> done when is_close`, i.e. an explicit operator close.
  `chat -> done` is the clean finish when `conversation_loop(1000)` is
  exhausted or the remaining budget cannot fund a further turn.

Both GPT agents carry a `claude` fallback to
`${ITERION_COPILOT_FALLBACK_MODEL:-anthropic/claude-opus-5}` on `usage_window`,
`unavailable` or `transient_exhausted` — still on `claw`, so the persisted
conversation, the declared tools and the permission gate survive the switch.
Anthropic credentials must be configured for Claw.

Planning and debate are **private**: the manifest marks every node `silent`
except `copi` (banner `Copi is thinking`) and `validate_draft` (banner
`Validating Copi's draft`). The dock shows Terra's final answer, never a plan
or a judge critique.

## Verify, don't assert

`validate_draft` is a deterministic tool node, not an agent tool call: it
writes the `.bot` Copi just drafted into the run's artifact-files directory,
runs `iterion validate` on it (120 s cap, report clipped at 4000 chars) and
removes it. `gate` then appends the verdict to the reply — ``✅ `iterion
validate` passed on this draft.``, ``⚠️ `iterion validate` did NOT pass on this
draft:`` with the report, or ``⚠️ This draft was NOT verified:`` when the check
could not run at all (no `python3` or no `iterion` on PATH, no files
directory, timeout). A draft is never presented as working on the agent's word
alone, and a bare host missing the helper degrades to an honest not-verified
verdict instead of killing a days-long conversation.

## Run

```sh
devbox run -- iterion run bots/copilot/main.bot \
  --var initial_message="que veut dire C128 ?"
```

Or from the studio: **/copi** (manifest `triggers:` are `copi` and `copilot`),
which hosts the conversation in the assistant dock. When dispatched from a
board card, the manifest maps the issue title + body into `initial_message`.

## Inputs

| Var | Type | Default | Purpose |
|-----|------|---------|---------|
| `initial_message` | string | `""` | seed for turn 1; empty → Copi opens the conversation itself |
| `mode` | string | `info` | posture: `info` (explain and orient), `design` (draft a workflow, compiled before the reply is shown), `debug` (diagnose a run from its real events) |
| `workspace_dir` | string | `${PROJECT_DIR}` | the workspace Copi reads and advises about |
| `scope_notes` | string | `""` | standing constraints for the session |

`launch.primary` renders `initial_message` and `mode` top-level in the studio
form; `scope_notes` is `hidden` plumbing, still settable via `--var` /
`bot_args`. `mode` is carried on the loop edge, so "passe en debug" switches
posture mid-conversation — the launch preset is frozen on the run and no
resume surface can change it.

## Environment

| Var | Default | Applies to |
|-----|---------|------------|
| `ITERION_COPILOT_ENTRY_MODEL` | `openai/gpt-5.6-terra` | `copi` (Terra) |
| `ITERION_COPILOT_EFFORT` | `medium` | `copi` reasoning effort |
| `ITERION_COPILOT_REFLECTION_MODEL` | `openai/gpt-5.6-sol` | `reflect` (Sol) |
| `ITERION_COPILOT_REFLECTION_EFFORT` | `high` | `reflect` reasoning effort |
| `ITERION_COPILOT_REVIEWER_MODEL` | `claude-opus-5` | `judge` |
| `ITERION_COPILOT_REVIEWER_EFFORT` | `medium` | `judge` reasoning effort |
| `ITERION_COPILOT_FALLBACK_MODEL` | `anthropic/claude-opus-5` | the `claude` fallback of both GPT agents |

## Guardrails

`worktree: none`, `sandbox: none`, `permission: deny` — **Copi writes
nothing.**

- **allow**: `Read(**)`, `Glob`, `workspace_grep`, `ToolSearch`, `TodoWrite`,
  `Skill`, and the single exact literal `diagnostic_shell(iterion --help)`.
- **ask**: `diagnostic_shell` — every other diagnostic command is presented to
  the studio for approval and resume. There is **no broad auto-allowed
  shell**, and that is load-bearing: an allow rule compiles to an *unanchored*
  prefix regexp, so one allow-listed prefix would grant arbitrary trailing
  commands. The allowance is anchored to that one complete command, so
  `iterion -h`, `iterion help`, a path to another executable and every
  whitespace- or shell-composed variant still ask.
- **deny**: `Grep`, `Task`, `Write`, `Edit`, `NotebookEdit`, `WebFetch`, plus
  an explicit credential-path list — `Read(**.env)`, `Read(**.env.*)`,
  `Read(**.ssh/*)`, `Read(**.aws/*)`, `Read(**.gnupg/*)`,
  `Read(**.docker/config.json)`, `Read(**.claude/.credentials.json)`,
  `Read(**.codex/auth.json)`, `Read(**.iterion/secrets.json)`,
  `Read(**.iterion/secrets.key)`, `Read(**cli-auth.json)`, `Read(**.netrc)`,
  `Read(**.git-credentials)`, `Read(*id_rsa*)`, `Read(*id_ed25519*)`,
  `Read(*.pem)`, `Read(*.key)`, `Read(*.p12)`.

On `claw` the declared `tools:` and this gate work together; on `claude_code`
the tools list is inert, so the gate remains the compatibility boundary for
both backends.

`worktree: none` is deliberate: Copi has no write or commit tool, so a
worktree would only produce phantom storage branches and aim `workspace_dir`
at a tree the operator is not looking at. `sandbox: none` is equally
deliberate (the C128 warning is assumed) — freshness IS the product, a chat
about "why did my run fail" must read the operator's LIVE tree, a container
start on every resume would dominate per-turn latency, and the security
boundary here is the permission gate, not filesystem isolation.

Run evidence arrives through the host-owned, capability-gated `runs.read`
tools, never through guessed store paths. For the active bot Copi may return
bounded exact replacements for companion files that bot declares in its
`authoring.editable_files`; the studio previews, hash-checks and saves them
under the operator's action policy (`editor: {context: true, proposals:
true}`). Copi itself still has no write tool and never chooses a path.

## Budget

| | |
|---|---|
| `max_parallel_branches` | `1` |
| `max_duration` | `12h` |
| `max_cost_usd` | `20` |

These are **session** caps, cumulative over the whole life of the run: budget
accounting is re-seeded from the checkpoint on every resume and only the pause
gap is excluded from the duration dimension. `max_iterations` and `max_tokens`
are deliberately unset — cost is the meaningful guard, and two redundant
ceilings only make a session die on the wrong one.

A **debug** conversation trades that guard for operator-supervised
persistence: the manifest's `chat.budget.unlimited_workflow_when`
(`launch_var: mode`, `state_node: compose`, `state_field: mode`, `equals:
[debug]`) lifts the workflow budget at launch, or on the first resume after
Copi emits `compose.mode = debug`. Platform and credential-pool ceilings still
apply.

## Skills bundled with this bot

All under [`skills/`](skills/), loaded by both `copi` and `reflect`:
`copi-conversation`, `iterion-concepts`, `iterion-dsl-authoring`,
`iterion-bot-architecture`, `iterion-run-debug`.

## See also

- [`docs/assistant-dock.md`](../../docs/assistant-dock.md) — the studio dock
  that hosts the conversation.
- [`docs/bot-runs/copilot.md`](../../docs/bot-runs/copilot.md) — dogfood
  history, newest run first.
