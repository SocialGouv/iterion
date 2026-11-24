# ADR-151 — board.* behind the action seam: a native action transport, one capability surface for every node kind

- Status: accepted (ARB-105 = A découpée, 2026-10-29; authorization rule ARB #78=A, operator answer 2026-11-02)
- Date: 2026-11-02
- Issue: SocialGouv/iterion#2834 (part one), epic #2343
- Relates to: [098-connector-catalog](098-connector-catalog.md) (the recipe
  this is the second use of), ADR-044 (the ladder an action refuses), #1801
  (the declared-list grant semantics), #2465 (which board), #2632 (the
  sandbox transport this removes from the requirement set), #2388 (the bot
  the second lot closes)

## Context

The `action:` recipe of a `tool` node was HTTP end to end:
`ConnectorResolver.ResolveAction` hands back `(package, operation,
credential, baseURL)` and `pkg/connector/exec` issues the request. The
native kanban board's twelve operations run **in process** over a
`native.BoardStore`, behind the one capability-gated
`boardops.Call`/`CallWithEnv` that two transports already share — the
`__mcp-board` stdio child and the `/api/v1/mcp/board` handler. So a
deterministic board read had no path: an agent could read the board through
an MCP tool with a model in the loop, and a tool node could not read it at
all. `bots/rgaa-audit`'s publish step is the standing cost: its plan is
filed and "verified" by a judge's self-report (#2388).

Three questions were doctrine, not implementation, and both arbitrations
are now rendered:

1. **A credential-free connector in the compiler.** `connection:` was
   mandatory on every `action:` (C261), and `validActionID` is a shape
   check, so the compiler could not know a connector needs no credential.
2. **Who authorizes a mutating board action on a tool node** — fleet
   question #78, answered **A** by the operator (2026-11-02): *the tool
   node declares `capabilities:` like an LLM node, under the same grant
   rules — one authorization surface, never a second regime.*
3. **Where the `native.BoardStore` comes from inside the engine.** #2465
   had already settled *which* board: always the instance's native store —
   the fs store locally, the tenant's Mongo board on the cloud — never a
   forge's.

## Decision

### 1. The seam is a registry; registration is the whole of it (G110)

`pkg/backend/model` gains an `ActionTransport` seam: a registered transport
**owns** one connector id (`Connector()`), publishes the facade package it
serves (`Package()`), and executes its operations in process (`Call`).
`pkg/connector/exec` over HTTP stays the default transport for every
connector no registration claims. The executor's dispatch reads the
registry and nothing else — the next in-process connector adds a
registration, never an engine `if`.

The executor holds the transport to the SAME honesty the HTTP path carries:
the operation must come from the transport's own facade, marked
`deterministic` at an attachable maturity (`checkOperationUsable`, the same
guard), with arguments rendered and coerced by the same code, under the
same tool-policy gate, `timeout:` and started/finish hooks. What a native
action deliberately does **not** inherit is the retry ladder: its guards
(`Retryable`, idempotency keys, ambiguous effects) are HTTP-answer
semantics, and re-issuing a store refusal in a sleep loop is invented
behaviour. `retry:` is inert on a native action; [C335](../references/diagnostics.md)
says so at compile time. Nor does it inherit the `params` sink's
materialisation: that sink serves a vendor call, and the board is
iterion's own store, which files the card and shows it to every reader —
a native action's arguments keep a secret's placeholder, the form an
agent's board MCP call files ([secrets](../secrets.md#sinks--where-a-declared-secret-may-be-materialised)).
One engine-level classification IS added: a refused grant is
`CAPABILITY_DENIED`, typed by interface (`runtime.CapabilityDenial`, the
`AmbiguousEffect` discipline — the engine never learns what the transport
is), deterministic in `pkg/retrypolicy`'s table and terminal in the recovery
recipes, because the same IR meets the same grant on every attempt and a
retry buys the same no.

### 2. One authorization surface: `capabilities:` on a tool node (ARB #78=A)

A `tool` node declares `capabilities:` exactly like an `agent` or `judge`:
no line inherits the workflow's list, a declared `capabilities: []` grants
nothing (#1801's nilness rule holds on tool nodes too — parser, AST, IR,
unparser and the JSON transport all carry it). The same compile-time checks
(C080/C081) judge the entries.

The grant is read at one place — the executor's `nodeCapabilities`, the
same reading for every node kind — and the native transport hands that
effective list to `boardops.CallWithEnv`, whose per-tool `caps.Has` check
is the gate every board transport already shares. The seam adds **no**
second filter — a transport that re-gated beside boardops would be the
second regime #78=A refuses. Consequences, both deliberate:

- The **act ladder is the capability vocabulary** (`pkg/boardcaps`): read
  verbs need `board.read`; each mutating verb needs its own capability —
  `board.create`, `board.label`, `board.move`, `board.close`,
  `board.comment`, `board.assign` — and a bot opting into a write declares
  it by name, on the node, in text a review reads. That is the
  "check-only by default, write by explicit opt-in" rule: nothing is
  granted by default beyond what the workflow already declares, and no
  mutating verb answers to a read grant.
- The list is the tool node's grant for every recipe that consults one: a
  registry recipe naming an iterion host tool (`command:
  mcp.iterion_board.create_issue`) is held to it exactly like an agent's
  `tools:`, and C328 judges it against the same list. A `capabilities:`
  list with no reader — a `command:`/`script:` recipe, an HTTP action whose
  authorization is the connection's credential, or a registry recipe naming
  another tool — warns ([C332](../references/diagnostics.md)) — an inert
  grant reads as configured, which is how a node ends up looking authorized
  for something nothing enforces.
- The compiler also names the MISSING grant: a native board action whose
  capability is declared neither on the node nor on the workflow warns at
  compile time ([C334](../references/diagnostics.md), the C328 posture —
  a warning with the remedy) what the runtime would refuse as
  `CAPABILITY_DENIED`. The compile-time half reads
  `boardcaps.ActionCapability`, held to the facade's bindings and the native
  registry by the matrix test.

### 3. A native connector holds no credential — `connection:` is optional for it alone

The compiler knows the native ids through `ir.NativeConnectorIDs` — the
registry's compile-time half, fed from the same vocabulary leaf
(`boardcaps.ConnectorID`) so reading it links no dispatcher into a binary
that compiles a `.bot` (#2857). For those ids, and only those, C261 does
not fire; a `connection:` line written anyway warns ([C333](../references/diagnostics.md))
because the alias authenticates nothing. Every HTTP connector keeps the
requirement — a connector call with no credential is not a call iterion
can make. The id reservation runs the other way too: `spec.Validate`
refuses a CATALOG package named `board`, which the registry would hijack
where the transport is wired and refuse where it is not. The three board transports then agree on authorization without
any of them holding a secret: the stdio child gates on its env grant, the
HTTP handler on the minted per-run token, the action transport on the
node's `capabilities:` — all three ending in the same `caps.Has`.

### 4. The facade publishes what filing and verifying need — reads first, then the writes

The first lot shipped the read verbs; the second (this ticket's other half,
with #2388) adds the mutating ones, each gated by the capability the native
tool already names. The bot's `capabilities:` declaration IS the opt-in; a
node granted nothing reads nothing (A15's check-only stays the default).

`boardops` publishes its facade as a `spec.Package` built **in code** —
the operations this binary implements are not a file on disk, which would
be a second source of truth free to drift from the registry (the
two-loaders defect ADR-098's round three closed). The table is the
resource/verb matrix of the offer:

| Resource | Verb | Action id | Effect | Grant | Status |
|---|---|---|---|---|---|
| issue | list | `board.issue.list` | read | `board.read` | **exposed** |
| issue | get | `board.issue.get` | read | `board.read` | **exposed** |
| issue | create | `board.issue.create` | create | `board.create` | **exposed** (second lot) |
| issue | label | `board.issue.label` | update | `board.label` | **exposed** (second lot; the RELATIVE `add_labels`, never an absolute rewrite) |
| issue | transition | `board.issue.transition` | update | `board.move` | **exposed** (second lot) |

The genericity owes a **two-way test matrix** and has one
(`facade_matrix_test.go`): every exposed action id maps to a native tool of
`boardops.allTools`, and every native tool is either exposed or carries its
exemption **with its reason** (`nativeExemptions`) — a catalog with
invisible holes being the other way to be wrong. The read half of
`list_labels` is exempted as out of the filing/verifying scope; the nine
mutating tools the facade does not serve (close, comment, assign, the
absolute set_labels, …) stay exempted, each naming its capability.

The mutating verbs say what a failure means for their effect, because the
engine's generic recovery re-runs a failed node: every facade operation
validates all of its arguments before its one store mutation, so a refusal
raised there has no effect and keeps its ordinary recovery. The store
mutation itself is NOT atomic with its error — the fs store writes the card
then appends its event, the Mongo store inserts the card then allocates and
inserts the event, and a failure after the commit still answers an error —
so **a failed store write is undecided**: `boardops` types it
`UndecidedWriteError`, which implements `runtime.AmbiguousEffect`
structurally, and the recovery dispatcher fails the node terminally instead
of filing the card a second time (the guarantee `exec.Error` gives an HTTP
mutation). The classification sits on the store every operation runs
against, so the stdio and HTTP board transports carry it too, and a store
method an operation starts calling must be classified read or write before
it compiles. The facade adds no idempotency machinery on top; the matrix
test pins both halves against a store that fails before and after it
writes.

Admission (`runview.admitConnectorActions`) learned the seam's shape: a
native action with no registered transport is a **wiring** refusal (the
surface did not hand the store over — no package install fixes it), and a
native action whose verb the facade does not expose is refused with the
exposed set listed — and warned at compile time
([C336](../references/diagnostics.md)), since the verb set is the binary's
own. Both verdicts were computable before the first node
ran, so both are due at admission; `ITERION_CONNECTOR_ADMISSION=off` keeps
its old meaning as the way out.

### 5. The store comes from the surface, never from the node

The transport executes where the engine runs, so each plane wires the store
it owns: `runview.BuildExecutor` registers the fs store it already opens
for the claw board tools (`<storeDir>/dispatcher`) as the default for every
local surface — CLI, studio, dispatcher, subbot, resume — through ONE call
(`runview.NativeBoardTransport`, the single-wiring lesson of ADR-098's
round six), and the cloud runner registers the tenant's Mongo board through
`Config.BoardFor`, the same resolver the sandboxed host-MCP transport pins
tokens to. An explicit registration beats the fs default; a runner that
cannot resolve the tenant's board **refuses the run** rather than read a
pod-local board that is not the instance's (#2465's whole point).

Because the engine executes the call, a sandboxed node needs no
`BoardHTTPEndpoint`/`BoardRunToken` for it — the second half of #2632 stops
being a requirement for every workflow that can express its board work as
actions. (The MCP transports stay what they are for agent nodes; C082 keeps
its meaning there.)

## Consequences

- A deterministic read of the board costs no model call and no token:
  `publish_verify`'s board read (#2388) becomes a tool node, in the second
  lot, together with the mutating verbs the filing half needs
  (`board.issue.create/label/transition`) — each gated by its capability,
  the opt-in #78=A makes the whole of the rule.
- A new in-process connector is a registration plus a facade: the engine
  learns nothing per connector.
- The DSL surface grew in one place (`capabilities:` on tool nodes) with
  the grammar, the property registry, the unparser and the JSON transport
  moved together — the round-trip is pinned by the parser/registry
  agreement tests.

## Alternatives rejected

- **An HTTP connector package describing `/api/v1/mcp/board`** (ARB-105's
  option B): zero engine change, but the engine would call itself over
  HTTP, a per-run token would become a stored credential, and every plane
  would owe the `BoardHTTPEndpoint` plumbing — the hole #2632 files —
  forever.
- **A separate grant regime for deterministic writes** (#78's option B — a
  dedicated `board_write: true`): a second authorization surface to teach,
  validate and keep honest beside the first, to express what
  `capabilities: [board.create]` already says.
- **No mutating action ever** (#78's option C): determinism stops at
  read-only and the self-report in #2388 stands. Refused by the same answer
  that unblocked this lot.
