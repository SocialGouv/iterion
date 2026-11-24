# Live agents — OpenMausBot, mapped onto iterion

What an always-on agent product does that a parked iterion run does not.
The mechanisms, the gap, and what is not pursued.

**Audience.** Anyone about to propose that iterion run agents that never
finish — an always-on assistant, a bot that wakes on events, a crew that works
for weeks — or to answer "how does iterion compare with a personal-agent
product". Read this before writing the proposal, not after: most of what such
a product does, a parked run already does, and the gap is narrower and more
specific than "a workflow ends, an agent does not".

This page follows the shape of
[context-retrieval-state-of-the-art.md](context-retrieval-state-of-the-art.md):
what the outside work validates, what iterion imports, what it deliberately
does differently, and what it does not pursue — with the date and the
condition that would reopen the question. The work it opens is tracked under
the epic [#3133, standing crews](https://github.com/SocialGouv/iterion/issues/3133).

---

## What was read, and how

- **OpenMausBot** (`milind-soni/OpenMausBot`, Apache-2.0 outside its
  `enterprise/` folder): an open-source personal-agent application — "your
  own team of AI bots, in a chat app", in its README's words. Read at commit
  [`6f83379`](https://github.com/milind-soni/OpenMausBot/tree/6f83379a80a83bdc283d4d57f9d07d8150b5bf59)
  of 2026-11-10. Every OpenMausBot path below is relative to that tree, and
  true of that commit only — the project ships almost daily.
- **iterion** at `e25073e3e`, the same day.
- **A reading, never an execution.** Nothing from that tree was built or run,
  no bot was started, no latency was timed. Each claim about OpenMausBot was
  graded *code* (a file and a line), *doc* (documented as shipped) or *plan*
  (a design file with no matching code); where its documentation and its code
  disagree, the code is what is quoted here. What could not be established is
  listed under [Not verified](#not-verified).

---

## What "live" means there

Not an immortal process, and not an immortal context:

- **The engine process is disposable.** One CLI stays warm per conversation,
  is closed after an idle window — ten minutes by default, 150 seconds on a
  machine of 1.5 GB or less, and on its cloud 30 seconds for a conversation
  nobody has in front of them (`server/drivers/session-idle.ts:10-14`,
  `:91-105`) — and the next turn relaunches it on the session id stored for
  that conversation. An opt-in cache keep-warm holds a Claude process for
  hours instead — three by default — on a subscription sign-in, once the
  conversation carries 50,000 tokens of context (`server/cache-keepwarm.ts`).
- **A triggered run works in a thread of its own.** A scheduled or webhook run
  opens a new thread on the bot unless it was told to join one
  (`server/routines.ts:1129-1146`). It reports into the chat the routine
  chose, else the bot's main chat; a run started by an outside event reports
  into a chat of its own (`server/automation-destinations.ts:1-3`).
- **A question can wait without a process.** A turn that ends on a question
  settles as `needs_input`, and the park is armed again after a restart
  (`server/runtime/runtime.ts:84-96`). A question asked mid-turn through the
  engine's own question tool is the other case: it keeps the turn, and its
  process, waiting (`server/drivers/claude.ts:523`).

What persists is four things, and "live" is their sum:

| | In OpenMausBot | Where |
|---|---|---|
| A durable identity | A bot record that owns N threads, each with the provider session id of its engine | `server/store.ts`, `shared/wire.ts` |
| A memory in files | `MEMORY.md` injected into every prompt, a capture pass after a quiet turn, keyword recall, search across the bot's threads | `docs/memory.md`, `server/drivers/agents-catalog.ts:537-606` |
| One router of wake-ups | A person's message, a clock, a webhook, an app event, a teammate's result: each becomes a turn for that bot | `server/admission.ts`, `server/routines.ts`, `server/runtime/` |
| A host of its own | One process per workspace; on the hosted cloud, one machine per customer — always on, or put to sleep when idle and woken for the next request or scheduled run | `docs/cloud-pro.md`, `server/cloud-power.ts` |

The fourth is the one a tenant needs to itself, and the router runs inside
it: a sleeping cloud machine is woken for a scheduled run.

---

## The mapping

| Mechanism | OpenMausBot | iterion | Gap |
|---|---|---|---|
| A durable thread, its session resumed | Session id per thread, CLI relaunched on it | A run parked on its chat node ([ADR-060](../adr/060-conversational-single-agent-bots.md)); what the next turn finds depends on the backend and the session mode (finding 1) | Fidelity, by backend |
| A question that waits for hours | A turn that ends on a question parks as `needs_input`, no process held | `paused_waiting_human`; `ask_user_async` to keep working meanwhile, on `claude_code`, an unsandboxed `claw` and `pi` over RPC ([ADR-081](../adr/081-async-human-interaction.md)) | None |
| A message while a turn runs | Steered into the turn, or a persisted queue (`server/admission.ts`, `server/steer-queue.ts`) | A queued message drained at each tool boundary; the run console's Steering panel | None |
| Woken by a clock or a webhook | Schedules, webhooks, app events, set by the bot itself (`server/drivers/agents-catalog.ts:609-663`) | The trigger spine ([ADR-046](../adr/046-event-driven-runs-trigger-spine.md)); `overlap: keepalive` ([scheduling](../scheduling.md#always-on-agents--overlap-keepalive)) | A subscription can only launch a fresh run or promote a card |
| An event delivered to the conversation that exists | A teammate's result resumes the sender; a `wait_until` follow-up runs in the chat it was asked in, outside rooms and its cloud (`server/wait-host.ts`) | The chat node's second door, `interaction: human_or_host` ([assistant dock](../assistant-dock.md)); a card watch queues a message into the watching run | The door is wired for the assistant only; a card's transition is by itself at most a queued message, never a wake-up |
| The bot programs itself | `trigger`, `trigger_info`, `delete_trigger`, `wait_until` tools | `watch.subscribe` on board cards, on `claw`; `cron_*` tools whose entries nothing fires | Missing for clocks, webhooks and waits |
| Memory across conversations | On by default; background capture; recall | A scoped, quota-governed store with a cloud twin ([memory and knowledge](../memory-and-knowledge.md)); a per-bot `auto_memory` space, off by default; `memory_*` tools on `claw` only | Opt-in, with no capture pass and no search of past conversations |
| An identity above the run | The bot record | A bot id that keys its memory space, its schedules and its configuration; the conversation itself is one run, bounded by `conversation_loop(1000)`, a 12-hour active budget and a 20-dollar cost cap in [`bots/copilot/main.bot`](../../bots/copilot/main.bot) | No address, no successor |
| A room of several bots, with a router | Rooms, a classifier for un-mentioned messages, `coordinate_bots` (`server/drivers/agents-catalog.ts:226`) | Sub-bots inside a graph, the board, the assistant's `run.launch` + `run.watch` | No shared conversation |
| A session kept warm between two turns | The CLI is kept ten minutes by default; hours for a long Claude conversation on a subscription, with the opt-in cache keep-warm | Every process of a node ends with the node ([ADR-127](../adr/127-an-agent-nodes-processes-end-with-the-node.md)) | A cold resume per turn; its cost is not measured |

---

## Five findings that decide the design

### 1. A parked run already is the thread

A chat-loop bot parks on a human node in `paused_waiting_human`: its budget is
suspended, it holds no pod, and it holds no credential slot
(`HoldsCredentialSlot` in [pkg/store/lifecycle.go](../../pkg/store/lifecycle.go)).
OpenMausBot keeps the same facts in SQLite and JSON files on one machine.
Nothing here calls for a new execution model.

What the next turn finds is where the fidelity differs, and it depends on the
backend and on the node's session mode:

- `claw` under `session: persist` — the shape of
  [`bots/copilot/main.bot`](../../bots/copilot/main.bot) — reloads a compacted
  message envelope checkpointed with the run
  ([backends](../backends.md#a-best-effort-session-that-no-longer-loads)):
  the conversation survives a new pod in compacted form, its most recent
  messages preserved.
- A CLI backend under `session: persist` (`claude_code`, `pi`, `codex`) packs
  its session files into a store that has a cloud twin and unpacks them on
  whichever pod takes the resume
  ([ADR-089](../adr/089-session-persist.md)).
- `inherit_if_available` — the shape of `bots/whats-next/main.bot` — is best
  effort by contract: in cloud the CLI's session files need not outlive a
  resume — a sandbox container is replaced, a subscription login's config
  directory is removed after each delivery — and the turn then runs fresh.

### 2. The two halves of the router exist, and they meet in one place only

The trigger spine normalises six sources — forge, schedule, board, run,
manual, custom ([pkg/trigger/event.go](../../pkg/trigger/event.go)) — onto two
execution modes, `direct` and `board`
([pkg/bundle/manifest.go](../../pkg/bundle/manifest.go)): launch a fresh run,
or promote a card.

Separately, a chat node has two doors. `interaction: human_or_host` lets a
host-attested event resume the pause the operator's message would have
resumed. [`bots/copilot/main.bot`](../../bots/copilot/main.bot) is the only
bundle that declares it, and the only writers are the assistant's own
machinery: run watches, card watches, missions, action receipts, workspace
hand-offs ([pkg/server/assistant_host_event.go](../../pkg/server/assistant_host_event.go)).

The two meet through that machinery and nowhere else: the assistant's run
watch subscribes to the spine's run events and wakes the assistant by the host
door ([pkg/server/assistant_run_watch.go](../../pkg/server/assistant_run_watch.go)).
A subscription itself has no such effect. So a clock or a webhook cannot reach
a bot that is already parked: it can only start another run of it. The join
was foreseen — `paused_waiting_event` is named as deferred in
[ADR-051](../adr/051-in-bot-event-driven-primitives.md) and
[ADR-081](../adr/081-async-human-interaction.md) — and it exists in no Go
file.

### 3. The conversation is one run, so it ends with the run

A bot already has an identity that outlives a run: its bot id keys a per-bot
memory space ([memory and knowledge](../memory-and-knowledge.md)), its
schedules, its configuration overrides and the cards assigned to it. The
conversation does not outlive a run: it is one, and its loop bound and budget
are its lifetime. iterion's own
answer for always-on work is already the opposite of
an immortal run: `overlap: keepalive` keeps a bot alive "as a stream of fresh,
individually-budgeted runs". OpenMausBot does the same thing in its own terms —
a fresh thread per trigger, under an identity that stays. What iterion lacks
is not longer runs but the address above them, and the hand-over from one run
to its successor.

### 4. A bot can barely program itself, and does not remember by default

The capabilities a bot can be granted are `board.*`, `watch.subscribe` and
`watch.unsubscribe`, `runs.read` and `connectors` (`KnownCapabilities` in
[pkg/dsl/ir/validate_capabilities.go](../../pkg/dsl/ir/validate_capabilities.go)).
One of them is a wake-up a bot arms for itself, and it marks how far that
goes today: `watch.subscribe` adds a board card to the run's watched set, and
each transition of that card is queued into the run as a message
([pkg/server/watch_coordinator.go](../../pkg/server/watch_coordinator.go)).
It is wired for `claw` only, its coordinator runs only where the native board
store is wired — which a cloud server does not do — and a parked run reads
the message at its next resume: it is not woken by it. The assistant is the
exception here too: a card it watches arms a run watch on the run that card
produces, in cloud as well, and a failed or cancelled run does wake it. The
`claw` loop also
registers four `cron_*` tools; they write to an in-memory registry that
nothing reads back, so an entry never fires.

The assistant's closed action catalogue — the `Closed catalogue` block of
[`bots/copilot/main.bot`](../../bots/copilot/main.bot) — launches, watches,
resumes and rewinds runs, and names no schedule or trigger. Memory is
deliberately off: a run is hermetic unless it asks otherwise, `auto_memory`
defaults to `off` and reaches `claude_code`, `claw` and `pi`, and the
`memory_read` / `memory_write` / `memory_list` tools reach `claw` only.

OpenMausBot sets a trigger inside the turn that was asked for it, and writes
memory without being told.

### 5. The two hosting models are opposite bets

In OpenMausBot a bot is live because its machine is. One server process owns
one data directory, state is SQLite and JSON files on a local volume, and the
hosted cloud is one machine per customer (`docs/cloud-pro.md`,
`deploy/fly/fly.toml`): always on, or, on a plan that sleeps, suspended when
idle and woken for the next request or scheduled run (`server/cloud-power.ts`;
its documentation reports under a second to wake from a suspend, about twenty
from a stop). The control plane that provisions and wakes those machines is
not in the repository. What lives in memory dies with the process: a pending
permission card (refused after 15 minutes in any case,
`server/drivers/claude.ts`), and a goal run, which is capped at 13 turns
(`server/group-goal-run.ts`, `server/host/one-on-one-goals.ts`).

In iterion a parked bot costs nothing and any replica resumes it. The price is
that every turn is a cold resume — queue, pod, a sandbox where the bot runs in
one, the session reloaded. The shipped assistant already pays to shorten it:
`bots/copilot/main.bot` declares `sandbox: none` on purpose, and one of the
reasons it gives is that a container start on every resume would dominate the
latency of a turn. What a
turn costs has not been measured, and it is the one number that decides
whether anything must stay warm.

---

## What iterion imports

The gap, as the work under [#3133](https://github.com/SocialGouv/iterion/issues/3133):

- **A third trigger effect: deliver the event to the bot that is parked.** The
  spine's event reaches the chat node's host door instead of launching a run;
  a busy bot receives it through the queue an operator's message — and a card
  watch — already uses. This is the lot that changes what the product is.
- **A bot manages its own triggers** — a capability beside the existing ones,
  with `watch.*` as its precedent, and the matching ids in the assistant's
  action catalogue, under the same operator policy as every other action. It
  replaces the `cron_*` tools that fire nothing. A bounded, code-only wait
  that wakes the bot that armed it belongs with it.
- **A standing bot is an address over rotating runs.** A read model over its
  runs, its subscriptions and its memory space — never a second source of
  truth ([philosophy](../philosophy.md), principle 5). When a run's bound or
  budget is spent, a successor takes the address, and memory carries the
  hand-over.
- **Memory a standing bot keeps without being told.** The `memory_*` tools on
  `claude_code` and `pi`, memory on for this class of bot, a capture pass
  after a quiet turn, and recall across the bot's own past conversations.
- **A chat channel as a conversation surface.** A thread in the team's chat
  tool is a conversation with a bot, on the pattern of
  [forge conversations](../forge-conversations.md) — with the session resumed
  per thread instead of the transcript replayed at each reply. That page
  chose the stateless form on purpose (no run held open per merge request, no
  reply-to-run map); a chat thread reverses the choice, and the design owes
  the argument.
- **A crew: a bot hands work to a teammate and is woken with the result.**
  What `run.launch` and `run.watch` give the assistant, for any standing bot,
  bounded in depth and budget.
- **Measure before keeping anything warm.** The wall clock of one turn on a
  parked run in cloud, by phase. A warm hold is designed only if that number
  asks for it.

## What iterion deliberately does differently

- **Nothing stays running in order to be live.** A parked bot is a row and a
  blob. Restart is normal, and ownership is elected, not assumed.
- **An approval and a long mission are durable where OpenMausBot's live in
  memory.** With `permission: ask`, a tool call that needs approval pauses the
  run and survives a restart ([permissions](../permissions.md)); a long
  mission is a budgeted, resumable loop.
- **A sandbox by default.** OpenMausBot's box is off outside its cloud unless
  `OMB_BOT_BOX=1` (`server/bot-box/detect.ts`), its CLIs run with the user's
  own privileges (`SECURITY.md`), and new bots start at full access — except
  on a hosted company workspace, where they start at Ask
  (`docs/approval-levels.md`). iterion's default posture is a sandbox per
  run, and opting out is explicit
  ([ADR-082](../adr/082-sandbox-by-default.md)); in cloud it is a deployment
  setting. The two meet awkwardly in a conversation: the shipped assistant
  opts out and relies on its permission gate, and a sandboxed `claw` node can
  neither pause on an approval nor ask a question asynchronously.
- **One deployment serves every tenant.** OpenMausBot is one instance per
  tenant: a machine on its cloud, an operating-system user under
  `openmausbot fleet`. The spend cap it ships is one monthly limit for the
  whole workspace, under licence, not per bot (`enterprise/README.md`). Its
  consumer cloud has no model gateway, a free trial's credit aside
  (`docs/cloud-pro.md`); a hosted company workspace calls a metered one that
  is not in the repository (`server/hosted-models.ts`).
- **Self-programming goes through policy.** OpenMausBot applies a bot's new
  trigger at once and offers an undo. An action an iterion assistant proposes
  is asked by default.

## What is not pursued, and what would reopen it

Stated on 2026-11-10, as dated positions rather than omissions, so that the
next session reopens them on evidence instead of rediscovering the question.

- **Voice, control of a desktop, native mobile apps.** They are the surface of
  a personal-agent application. iterion reaches people through the studio,
  the forge, the board, web push and one-way alert webhooks. *Reopens on:* a
  named deployment whose
  users cannot reach their bots through any of those, nor through a chat
  channel.
- **A machine per tenant, kept up or put to sleep.** It contradicts "every
  replica is disposable". *Reopens on:* nothing short of a measured turn
  latency that a bounded warm hold cannot bring to what a chat surface needs.
- **Rooms built into the studio.** A team already has a chat tool; the channel
  adapter above makes its threads the room. *Reopens on:* a second adapter
  showing that thread semantics differ too much to share one model.
- **The breadth of an app aggregator** (its catalogue of connected apps, and
  their events). It stays on the connector catalog's own track
  ([ADR-098](../adr/098-connector-catalog.md)) and is not duplicated here.
- **Managing OpenMausBot instead of emulating it.** It looks cheap:
  OpenMausBot accepts third-party MCP servers
  (`docs/custom-mcp-servers.md`) and iterion ships one
  ([mcp-server](../mcp-server.md)); in the other direction OpenMausBot has a
  headless CLI and a per-bot API. Worth a bench, not a target: it would put a
  single-tenant server, unsandboxed by default, behind a multi-tenant
  platform. *Reopens on:* the bench, which was not run.

## Not verified

- **The latency of one conversation turn on a parked run in cloud.** Not
  timed. It decides the warm hold.
- **MCP interoperability, in either direction.** Inferred from both projects'
  documentation, never tried.
- **OpenMausBot at run time.** It was read, not executed. `server/index.ts`
  is more than 15 000 lines and was read by targeted search. The hosted
  control plane is not in the repository, so plan limits and team invitations
  cannot be checked from the code.
- **Its trajectory.** A durable goal record, an autonomous fixer behind its
  watchdog and single sign-on are written down as plans
  (`docs/plans/2026-09-02-goal-mode-v2.md`, `docs/sentinel.md`,
  `enterprise/FEATURES`). They may ship; this page describes one commit.

---

## Sources

OpenMausBot at the commit above: `README.md`, `LICENSING.md`, `SECURITY.md`,
`enterprise/README.md`, `docs/runtime-contract.md`, `docs/memory.md`,
`docs/approval-levels.md`, `docs/self-hosting.md`, `docs/cloud-pro.md`,
`docs/team-cloud.md`, and the files cited inline. iterion:
[ADR-046](../adr/046-event-driven-runs-trigger-spine.md),
[ADR-051](../adr/051-in-bot-event-driven-primitives.md),
[ADR-060](../adr/060-conversational-single-agent-bots.md),
[ADR-081](../adr/081-async-human-interaction.md),
[ADR-089](../adr/089-session-persist.md),
[ADR-091](../adr/091-ubiquitous-assistant-chat-dock.md),
[ADR-127](../adr/127-an-agent-nodes-processes-end-with-the-node.md),
[scheduling](../scheduling.md), [assistant dock](../assistant-dock.md),
[memory and knowledge](../memory-and-knowledge.md).

Star counts, plan prices and marketing claims about either project carry no
weight on this page. The one figure taken from OpenMausBot's documentation
rather than its code — the time a sleeping machine takes to wake — is marked
as its own.
