# The assistant dock

The studio's assistant is reachable from **every** authenticated route,
not just `/whats-next`. It rides in a dock mounted at shell level, and it
already knows which page you are on.

Design rationale and rejected alternatives:
[ADR-087](adr/087-ubiquitous-assistant-chat-dock.md).

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
  assistant then resolves it with the tools it already has — the control
  and board MCP servers. A run with thousands of events costs your prompt
  one line, and the assistant reads only what it decides it needs.
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
