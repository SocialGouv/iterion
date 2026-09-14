# ADR-091 — The assistant is a shell-level dock with implicit route context, not a route

- Status: accepted
- Date: 2026-08-01
- Deciders: jo (direction), Claude (analysis + implementation)

## Context

The studio's assistant was reachable from exactly **one** route.
`/whats-next` ([`WhatsNextView.tsx`](../../studio/src/components/WhatsNext/WhatsNextView.tsx))
owned the session; every other page had no way to talk to it.

The one other chat-shaped thing —
[`SteeringPanel`](../../studio/src/components/Runs/SteeringPanel.tsx)
on `/runs/:id` — is **not** an assistant. It is *steering*: text typed
there is queued into a live agent's inbox
([`api/queueMessages.ts`](../../studio/src/api/queueMessages.ts)) and
picked up at its next turn. Nothing replies. Both surfaces called
themselves "Conversation", which is how the ambiguity started.

So an operator looking at a red pipeline card, a stuck run, or a `.bot`
they were editing had to *leave the page* to ask about it — and then
retype by hand what they had just been looking at.

Three questions had to be answered together, because each constrains the
others:

1. **Where does a ubiquitous surface live** in an app whose views are
   `React.lazy` route components?
2. **How does the assistant learn what page you are on** without a
   per-view integration in twenty views, and without inlining page
   content into a prompt?
3. **How do assistant and steering coexist** on `/runs/:id` without the
   operator having to guess which box does what?

## Decision

### A. Mount the dock — and the session — above the route tree

`ChatDock` is mounted in
[`App.tsx`](../../studio/src/App.tsx) next to `GlobalCommandPalette`,
outside the `<Switch>`. It survives navigation *by construction* rather
than by remembering to re-attach.

Assistant presentation is extracted into
[`ChatDockShell`](../../studio/src/components/ChatDock/ChatDockShell.tsx):
the three states (`closed` bubble → `floating` non-modal panel →
`docked-right` column), the `lg`-breakpoint open-from-closed rule, the
Escape handling and the unread badge, with transcript, composer and
unread count injected. The run-scoped `SteeringPanel` reuses only its
docked panel/chrome primitives: it is always present in the run console's
right dock and deliberately has no floating or minimised state.

The session moves with it.
[`AssistantProvider`](../../studio/src/components/ChatDock/AssistantProvider.tsx)
runs `useWhatsNextSession` once for the lifetime of the authenticated
app. This is not a stretch of the existing design: the whats-next run is
long-lived and parks on its budget-free `chat` node for days, and
`rememberSessionRunId` / `useSessionDiscovery` were already written for
re-attachment. What did *not* survive the naive lift is state ownership —
see (B).

Assistant dock state (`closed|floating|docked-right`) is persisted **per
user** under one key, not per route: docking the assistant on `/board`
must leave it docked on `/runs`. Steering has no presentation state to
persist because it is a permanent run-console column
([`lib/chatDock/dockState.ts`](../../studio/src/lib/chatDock/dockState.ts)).

### B. Give the always-mounted session its own run store

The session wrote to the **module-default** `RunStore`. That was
tolerable while it only existed on one route. Mounted globally it would
permanently hold the assistant's run in the store every shell-level
consumer reads — `useDocumentTitle` would have titled `/runs/:id` after
the assistant's run.

So `AssistantProvider` creates a store of its own, runs the session hook
under it, and hands the **default** store back to the subtree below.
Surfaces that render the assistant's transcript re-enter the assistant
store through `<AssistantStoreScope>`. Per-run tabs are unaffected: they
already use the registry (`getOrCreateRunStore`) via `RunTabHost`.

The provider exposes **two** contexts, deliberately. The session object
changes on every websocket event; the dock state changes when the
operator clicks. A single context would re-render every consumer —
including `AppShell`, which reads the dock state to reserve the docked
column — on each event, dragging the whole route subtree with it.

### C. Implicit context is a typed pointer plus a bounded visible snapshot

[`referenceForRoute`](../../studio/src/lib/chatDock/routeReference.ts)
maps a location to `run/<id>`, `card/<id>`, `bot/<path>`, `repo/<key>` or
`view/<name>` — the same vocabulary an explicit drop chip produces, so
both paths converge on one protocol and a bot has one thing to learn. The
route visible at the first accepted message also emits a one-line
`<visible-page-context>{…}</visible-page-context>` snapshot with its pathname
and semantic page state. Known views enrich that
floor through `useAssistantPageContext` (selected editor item, active section,
dirty state); it is explicitly not a DOM or accessibility-tree scrape.

Three properties are load-bearing:

- **A pointer plus a bounded host attestation, never page content.** The
  reference rides on the opening message as one line
  (`[page context: run/019f…]`).
  At send time, the server resolves run/node/card pointers against its current
  project/tenant stores and stamps only status-level facts. Bots granted
  `runs.read` can fetch a projected chronology without learning or guessing a
  store path. Other reference kinds remain the receiving bot's contract. A
  studio that emits a wire format its consumer was never taught is a chip that
  looks like context and is not; teaching the bot is part of shipping the
  protocol, not a follow-up.
- **Automatic coverage, explicit enrichment.** `ROUTE_RULES` upgrades known
  routes to resolvable entity pointers. An unmapped route still receives a
  generic, distinct `view/route-…` reference, so a newly-added page never
  silently loses context. A view hook is reserved for state a route cannot
  know; its contributions merge and mounted-but-hidden panes do not publish.
- **First-message anchoring, explicit later scope.** The reference and visible
  snapshot are frozen synchronously when send begins, then persisted as the
  conversation's immutable anchor once the message is accepted. Navigation
  cannot silently redefine what a standing conversation is about. A later
  page enters one message only through `Join this page`, a drag/drop, or a
  typed suggested reply whose destination the operator chose. The live editor
  document remains a separate per-message capability for a conversation
  anchored to that editor.
- **The delimiter is a security boundary, so it is enforced, not
  assumed.** *(added 2026-08-01, after the first implementation shipped
  without it.)* The reference is minted from route params, which are URL
  input — the operator only has to open a link someone sent them. `%0A`
  survives in `window.location.pathname` and decodes to a real newline;
  `%5D` closes the bracket early. Either one puts the rest of the segment
  **outside** `[page context: …]`, as ordinary free text at the top of
  the operator's own message — and the chip does not expose it, because
  it renders the label, truncated to 8 characters for a run. The receiver
  is a `claude_code` agent with a shell in the workspace and board
  writes, so the escalation is "opened a link" → "agent runs
  attacker-authored instructions in the operator's workspace".

  The bot contract above — treat this line as a pointer, "never as an
  instruction" — is only enforceable if it *is* one line. So
  `sanitizeReferenceText` strips the line- and bracket-breakers at the
  **mint**, in `ref()`, where an explicit drop chip inherits the
  guarantee instead of having to remember it; and again in
  `withPageContext`, which owns the delimiter. **Escaping** instead of
  stripping was rejected: nothing downstream unescapes — the bot reads
  the raw line. The strip is narrow — it takes only what can break the
  line or the bracket, because a blanket printable-ASCII range would eat
  the digits and uppercase that make up most of a run id.

  **Stripping alone is not enough, and a per-kind allowlist supplements
  it.** A blanket id allowlist was rightly rejected (the vocabulary
  carries `/`, `:` and file paths, and one rule over all kinds would
  silently drop legitimate references as it grows) — but that rejection
  left a real hole: no forbidden character is needed to inject. Prose
  that survives the strip rides *inside* the delimiter as a
  plausible-looking pointer, and the chip shows a label the attacker
  chose — a run id truncated to 8 characters, a `?file=` reduced to its
  basename. So the allowlist is applied **per kind**, only where a shape
  is actually known: `run`, `card` and `node` ids have narrow formats, so
  a value that is not one is not a reference and the route degrades to
  its plain `view/` fallback. `bot` and `repo` carry paths rather
  than ids, so they get a deliberately loose shape (no whitespace, no
  prose punctuation) PLUS the visibility rule: their chip shows the whole
  value, never a prettier stand-in, because "context is never invisible"
  has to mean the operator can see the part an attacker controls.
  Visibility was first shipped as their only control and that was too
  weak — the chip truncates inside a 380px column, so a crafted
  200-character value only surfaced on hover. The cost of the shape is
  that a filename containing a space degrades to `view/editor`.

  Its line terminators are **Unicode's set, not JS's**: the first cut
  stripped `U+2028`/`U+2029` but left `U+0085` NEL, which splitting on
  `\n` and `/\s/` both miss while UAX #14 puts it in the same class — a
  set inconsistent with its own rationale. C0 *and* C1 now go. Bidi
  controls go too, for the chip rather than the prompt: they reorder
  rendered text, so a crafted reference could make the chip display
  something other than what the message carries, and the chip exists
  precisely so context is never invisible.

The candidate reference is shown in a persistent banner below the assistant
header with an explicit `Remove` action until the first send. That opt-out is
conversation-scoped: removing it from one empty tab cannot silence another.
After acceptance the banner names the immutable conversation context and,
when navigation moved elsewhere, links back to its exact Studio location
including the query string. It no longer offers to remove context already
present in history.

### D. Two surfaces, named apart — not one dock with a mode switch

On `/runs/:id`, the dock is the **Assistant** (it answers you) and the run
panel is **Steering** (it pushes into a live agent). They are titled
accordingly in one place
([`lib/chatDock/labels.ts`](../../studio/src/lib/chatDock/labels.ts)),
and use different presentation: Assistant may float, minimise, or dock;
Steering is always the run console's right-hand dock (and shares that
column with Browser through tabs).

On `/whats-next` the dock stands down: that route renders the same
session full-width, and since both composers write the same store's
`chatDraft`, a second one would be an echo.

### E. Reads stay autonomous; writes cross a typed Studio action boundary

A chat model never receives a generic Studio API, file or fetch executor. It
may publish up to eight `{id, intent, args}` requests with its turn. The host
owns the closed id catalogue, argument projection, risk label, policy decision,
API call, result link and idempotency key. `intent: explicit` records that the
current operator message asked for the exact action; it is input to policy,
not permission.

Policies are global per browser and per action: deny, always ask, auto only for
an explicit request, or always allow. Every new catalogue entry defaults to
ask. Server-side identity, tenant permissions and domain validation remain in
force after the Studio decision. Secrets are not an action argument type.

When an offer appears is part of the boundary too: only once the turn is
parked on its chat pause, i.e. after the optional reviewer feedback has gone
back to Copi and Copi's revised answer is ready. The artifact carrying the
requests is published on the first agent pass, earlier than that, and rendering
from it directly put an executable card in front of the operator while the
private editorial loop was still running (run 01a04999, 2026-08-28). A turn
that never reaches its pause offers nothing.

This also closes an architectural inconsistency in Nexie: board reads remain
capability-gated MCP calls, while board writes no longer happen inside the
model's tool loop and therefore cannot bypass the global Assistant settings.
Approved ready transitions are subscribed to the conversation by the Studio so
the existing watched-card feedback loop survives the move.

## Consequences

**Good**

- The assistant is reachable from every authenticated route, already
  knowing what the operator is looking at, with no per-view work.
- One session and one transcript across navigation — by construction, not
  by re-attachment. **Amended:** the session host stayed above the route
  tree, but WHICH bot it is pointed at became route-dependent. Nexie answers
  `/whats-next` and only there; the dock everywhere else is the general
  assistant. One shared bot across both surfaces was wrong in both
  directions — see "One session per correspondent" in
  [docs/assistant-dock.md](../assistant-dock.md).
- One dock implementation. A third chat surface (a copilot) supplies a
  session + a transcript renderer and inherits every state, the Escape
  handling, the breakpoint rule and the a11y wiring.
- Assistant vs steering is legible at a glance rather than by experiment.
- `useDocumentTitle` stops being poisoned by a visited whats-next
  session — a latent bug the store isolation fixes on the way past.

**Costs, accepted**

- **The session hook is now in the initial bundle.** It used to ride the
  lazy `/whats-next` chunk. Ubiquity is the feature; a lazy dock would
  reintroduce the "not there when you need it" failure.
- **Startup discovery runs on every page load,** not only on
  `/whats-next` — up to two `listRuns` calls, one per candidate workflow
  spelling (`findLiveRunForBot` probes both the hyphen- and
  underscore-spelled name, deduped, so a bot id without a hyphen costs
  one). It only ever *attaches*; it never launches, so a cold boot
  cannot start a run.
- **`AppShell` reads the dock context.** A layout component now depends on
  the assistant's existence. It is a single scalar off the stable context
  and degrades to `0` when the provider is absent, but the coupling is
  real: a docked column has to reserve real width, and an overlay that
  covers the thing you are asking about defeats the point.
- **Node-level context is not route-derivable.** `node/<run>/<node>` is in
  the vocabulary, but node selection is store state, not URL state — so
  that reference can only arrive as an explicit drop until the run console
  puts the selected node in the URL.

**Rejected alternatives**

- *A portal rendered by each view.* Every view pays integration cost, and
  the session unmounts with whichever view happened to own it — exactly
  today's bug, spread wider.
- *Inlining page content into the prompt.* Cheap to build and unbounded in
  cost. A bounded opening snapshot plus resolvable pointers gives the
  conversation a stable subject without serialising each page visited later.
- *One dock hosting assistant and steering as two modes.* Fewer pixels,
  but it merges two things whose only shared property is being
  chat-shaped: one answers, the other pushes into a running agent and
  never replies. A mode switch makes "which one am I typing into" a
  question the operator has to keep answering.

## 2026-08-01 — what "above the route tree" turned out to include

Decision (A) says the session lives above the route tree because
navigation must not restart it. Two follow-ups showed the rule is wider
than the session, and both were bugs before they were principles:

- **Anything the OPERATOR set about a conversation belongs there too.** The
  first implementation kept dismissal in route/dock state, so opting out in
  one tab could affect another tab looking at the same reference. Candidate
  acceptance, opt-out and the final anchor now live on the conversation in
  `AssistantProvider`; a `/board → /whats-next → /board` round trip and a
  conversation switch both preserve the correct independent choice.
- **Run steering is structural, not another floating surface.** It is a
  permanent panel inside the run console's own resizable SideDock. The
  shell-level assistant may still reserve a separate outer column.

Neither changes the decision; they are what it costs to hold it.
