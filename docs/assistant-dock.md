# The assistant dock

The studio's assistant is reachable from **every** authenticated route,
not just `/whats-next`. It rides in a dock mounted at shell level, and it
already knows which page you are on.

Design rationale and rejected alternatives:
[ADR-088](adr/088-ubiquitous-assistant-chat-dock.md).

## The three states

| State          | What you see                                                              |
| -------------- | ------------------------------------------------------------------------- |
| `closed`       | A bubble in the bottom-right corner, badged with messages you have not read |
| `floating`     | A resizable, **non-modal** panel — the page behind it stays live and clickable |
| `docked-right` | A full-height column at the right edge; the page reflows beside it, it never covers it |

Opening from closed picks `floating` on a wide viewport and
`docked-right` at or below Tailwind's `lg` breakpoint (1024px), where a
floating panel would swallow the screen. That is a point-in-time choice:
an already-open dock does not re-dock itself when you resize, and an
explicit dock/undock always wins afterwards.

The state is remembered **per user**, not per page — dock it on `/board`
and it is still docked on `/runs`.

`Escape` closes the floating panel. It is deliberately not a focus trap:
the dock is a helper you consult while working, not a modal.

## One session, everywhere

The dock and `/whats-next` are two views onto **one** session. Navigating
between routes cannot start a second run or lose the transcript — the
session is mounted above the route tree, so it is never unmounted by
navigation.

The draft you are typing is shared too: start a sentence in the dock,
open `/whats-next`, and it is still there.

On `/whats-next` itself the dock stands down — that route already renders
the same conversation full-width.

If no session exists yet, the composer is still live: your first message
starts one.

If the dock could not *check* whether one exists — the startup lookup
failed — it says so and offers a Retry instead of the usual invitation.
The two states look different on purpose: a live session may exist that
the dock cannot see, and here the next keystroke is what would launch a
second one over it.

## The context chip

The dock reports the page you are on as a **typed reference**:

| Where you are            | Reference             |
| ------------------------ | --------------------- |
| `/runs/019fbd46…`        | `run/019fbd46…`       |
| `/pipelines/cards/issue/native:abc` | `card/native:abc` |
| `/bots/review-pr`        | `bot/review-pr`       |
| `/editor?file=bots/x/main.bot` | `bot/bots/x/main.bot` |
| `/repos/acme%2Fwidgets`  | `repo/acme/widgets`   |
| `/board`, `/pipelines`, … | `view/board`, `view/pipelines`, … |

Two things about it matter:

- **It is a pointer, never the page's content.** The reference travels as
  one line on your message (`[page context: run/019fbd46…]`); the
  assistant then resolves it with the tools it already has. A run with
  thousands of events costs your prompt one line, and the assistant reads
  only what it decides it needs.

  What resolving costs depends on the kind. Nexie reads a `card/…`
  through the board tools it already declares; `run/…` and `node/…` it
  reads from the run store with its shell (`run.json`, `events.jsonl`) —
  **there is no run-inspection MCP surface yet**, so those are a
  well-formed hint rather than a one-call lookup. `repo/…` and `view/…`
  are scope only: they tell the assistant what you are looking at, with
  nothing to fetch. A reference that does not resolve on this host is
  reported as such rather than guessed at.
- **It is a reference the URL cannot forge.** Route params are
  attacker-supplied — an operator only has to open a link someone sent
  them — so the mint both strips what would break the one-line,
  bracket-delimited protocol AND requires each entity kind's id to have
  its known shape (`run`, `card`, `node`). A `/runs/<prose>` is not a
  run reference, so it is refused and the page degrades to `view/runs`;
  the crafted text never reaches the prompt. `bot/` and `repo/` carry
  paths rather than ids, so their shape is conservative rather than
  absent — a path has no whitespace and none of the punctuation an
  instruction needs — and their chip additionally shows the **whole
  value** rather than a basename, because a friendly stand-in would hide
  exactly the part an attacker controls. Visibility alone was too weak
  to stand on its own: the chip truncates inside a 380px column, so a
  crafted 200-character value was only recoverable from the tooltip.
  (Trade-off taken knowingly: a filename containing a space loses its
  chip and degrades to the plain view reference.)
- **It is always visible.** The chip above the composer names what the
  assistant is assumed to be looking at. Dismiss it with the ✕ and that
  reference stops being sent — including if you navigate away and back —
  while other pages still contribute their own. "Use this page as
  context" puts it back.

A route nobody mapped contributes **no** context rather than a guess.
Home and `/whats-next` deliberately contribute none.

Adding a route is a row in `ROUTE_RULES`
([`studio/src/lib/chatDock/routeReference.ts`](../studio/src/lib/chatDock/routeReference.ts)) —
there is no per-view wiring to forget.

`node/<run>/<node>` is part of the same vocabulary but is not derivable
from the URL today (node selection is component state, not a route
param), so it only arrives when something is dropped in explicitly.

## Assistant vs steering on `/runs/:id`

A run page shows two chat-shaped surfaces. They do opposite things, so
they are named apart, carry different icons, and sit in different corner
lanes:

| Surface       | What it does                                                                             |
| ------------- | ---------------------------------------------------------------------------------------- |
| **Assistant** | You ask, it answers. Follows you across routes. Bottom-right corner (lane 0)               |
| **Steering**  | You push. The text is queued into the run's **live agent** and picked up at its next turn — nothing replies. One lane over |

The assistant owns the canonical corner because it is the surface present
on every route: its position must not move under you. In the run
console's right-hand dock, the steering panel is the tab labelled
**Steering**.

Both can be docked right at once — the assistant's column pins to the
window edge and the run console's dock sits inside the page beside it.

A lane is a floor, not a fixed offset. The assistant occupies a `fixed`
band at the right edge whenever it is open, and padding does nothing for
another `fixed` element — so steering's bubble would sit *under* it and
take no clicks.

Two hooks, because these are two different questions and conflating them
is what made steering unreachable:

| Hook | Answers | Docked | Floating |
| --- | --- | --- | --- |
| `useAssistantReservedWidthPx` | how much LAYOUT to reserve (`AppShell` padding) | 380px | 0 — floating overlays on purpose |
| `useAssistantFixedInsetPx` | how much another FIXED surface must clear | 380px | 436px |

The dock sits on its own `--z-dock` rung (35), **below** the modal scrim
(`--z-overlay`, 40). A dialog therefore dims and covers it. The in-between
value is the one to avoid: above the scrim but below the dialog, the dock
paints undimmed beside a dimmed page while Radix has already made it inert —
it looks interactive and is not.

A floating assistant spans right 16 → 436 while steering's closed bubble
sits at right 80, so answering the second question with the first left
the bubble underneath it — in the persisted default configuration, which
is where most operators are.
