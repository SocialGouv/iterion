# ADR-086 — The pipeline card drawer stays a hand-rolled overlay, and owns its own focus trap

- Status: accepted
- Date: 2026-07-31
- Deciders: jo (direction), Claude (analysis + implementation)

## Context

The studio ships a Radix-backed right-side sheet primitive,
[`ui/Drawer`](../../studio/src/components/ui/Drawer.tsx), whose whole
selling point is that "focus trap, Escape, click-outside and aria wiring
come from Radix".

The pipeline board's card details drawer
([`PipelineCardDetails.tsx`](../../studio/src/views/PipelineBoard/PipelineCardDetails.tsx),
`presentation="overlay"`) does **not** use it. It hand-rolls a
`createPortal` to `document.body` with its own scrim, its own
`z-index` tokens, its own Escape listener and its own body-scroll lock.
The portal itself is not a matter of taste — the board tree sits under
`main { overflow: hidden }` and the cards animate with `transform`,
which traps `position: fixed` descendants and made an inline drawer
invisible.

Two things forced the question now:

1. **The drawer became resizable.** It was pinned at
   `w-[min(28rem,100vw)]`, so a multi-line prompt or a JSON blob could
   only be read ten lines at a time — operators left the board for the
   full-page route to do in-place triage. It now has a drag grip on its
   inner edge with a persisted width.
2. **The drawer gained a lot of focusable controls.** Every input and
   output value is now an `ExpandableValue` with a copy button, an
   expand toggle and (for JSON) a raw/pretty toggle — on top of the new
   resize grip.

That made a latent a11y bug load-bearing: the panel declared
`role="dialog" aria-modal="true"` but never moved focus into itself and
never trapped Tab. A keyboard user tabbed straight past the drawer into
the board behind the scrim — while a screen reader had been told that
board was inert.

So: migrate the overlay to `ui/Drawer` and inherit Radix's trap, or keep
it hand-rolled and write the trap ourselves?

## Decision

**Keep the hand-rolled overlay. Add the missing focus trap as a shared
helper rather than as a reason to migrate.**

- `focusableWithin` + `trapTabKey` live in
  [`lib/a11y.ts`](../../studio/src/lib/a11y.ts) next to
  `clickableRowProps`, so the next non-Radix `aria-modal` surface gets
  the trap for free instead of re-deriving it.
- `PipelineCardDetails` wires them: the `<aside>` takes `tabIndex={-1}`
  and an `onKeyDown` that cycles Tab/Shift+Tab within the panel; an
  effect moves focus in on open and restores it to the opener on close.
- The resize grip is a real **window splitter**, not a pointer-only
  affordance: `role="separator"` with `aria-valuenow/min/max`,
  `tabIndex={0}`, arrow keys, Home/End and double-click-to-reset. It is
  therefore *inside* the trap ring, which is the point.

Rejected: **migrating the overlay to `ui/Drawer`.** Radix `Dialog.Content`
would happily host the grip and an inline `style={{ width }}`, so the
resize feature is not what blocks it. What blocks it is that the
overlay's three deliberate deviations all have to be re-obtained against
Radix's own semantics:

- the **scrim-arming tick** (the scrim ignores the first click, because
  the portal mounts under the cursor that opened it and would otherwise
  close instantly) has no direct Radix equivalent — it would become a
  `onPointerDownOutside`/`onInteractOutside` dance with different timing;
- `presentation="page"` shares this component's header and body with the
  full-page card route, which is not a dialog at all — a Radix migration
  splits one component into two divergent trees;
- the board is a heavily-used surface with the `z-index`/portal
  interaction spelled out in a comment precisely because it was got
  wrong before.

The cost of that migration is a behaviour-change risk on the board's main
triage path; the benefit is ~40 lines of trap we now have as a tested,
reusable helper. Not worth it.

## Consequences

- The studio has **two** modal idioms: Radix (`ui/Dialog`, `ui/Drawer`)
  for everything that fits, and this one hand-rolled overlay. New modal
  surfaces should still reach for the Radix primitives — this ADR is the
  documented exception, not a licence.
- Any future hand-rolled `aria-modal` surface MUST use `trapTabKey`.
  An `aria-modal="true"` with no trap is worse than no `aria-modal` at
  all: it lies to assistive tech about what is reachable.
- The trap is regression-guarded by
  [`PipelineCardDrawer.test.tsx`](../../studio/src/views/PipelineBoard/PipelineCardDrawer.test.tsx),
  which asserts focus entry/restore, both wrap directions, and runs
  axe-core over `document.body` *through the portal* — the only way to
  see the real DOM the user gets.
- If a third non-Radix modal appears, revisit: at that point the right
  move is a `useFocusTrap` hook (or a Radix migration), not a third copy.
