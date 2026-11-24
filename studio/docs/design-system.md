# Iterion Studio — Design System

This is a working reference for studio contributors. The goal is **discipline of adoption**, not novelty: the primitives below already exist; reach for them before writing ad-hoc styling.

## Tokens

Single source of truth: [`studio/src/app.css`](../src/app.css). Everything below is generated from `@theme` custom properties — change a value here and every consumer (canvas, inspector, run console, board, dispatcher) follows.

| Family | Tokens | When to use |
|---|---|---|
| Fonts | `font-sans` (Geist Variable) / `font-mono` (Geist Mono Variable) | Self-hosted variable fonts (imported in `main.tsx`, defined in `app.css`). `font-sans` is the global default; `font-mono` for technical identifiers (run-ids / SHAs / node-ids / branch names). Both fall back to the system stack. See visual-identity.md § Typography. |
| Surfaces | `surface-0` / `surface-1` / `surface-2` / `surface-3` | See § Surface hierarchy below |
| Foreground | `fg-default`, `fg-muted`, `fg-subtle`, `fg-onAccent` | Text contrast tiers — `fg-subtle` holds AA on surfaces 0-2 and 3:1 on the surface-3 hover; `fg-onAccent` is white and only sits on `bg-accent`, a `bg-<severity>-solid` or the `bg-black/NN` scrim of a chip inside an accent row |
| Borders | `border-default`, `border-strong`, `border-subtle` · `border-control` | Dividers, card outlines. **`border-control`** is the boundary of a text control (input, textarea, select, tag input, combobox trigger; native checkboxes and radios draw their own box): held ≥ 3:1 against surfaces 0-2 in both themes (RGAA 3.3), decoupled from `border-strong` so chrome keeps its hairlines |
| Focus | `focus-ring` | The keyboard-focus indicator: the global `:focus-visible` outline and the xyflow / virtuoso counter-rules in `app.css`. Held ≥ 3:1 on surfaces 0-2 in both themes (RGAA 10.7); `accent` alone is 2.8:1 on dark |
| Accent | `accent`, `accent-hover`, `accent-soft`, `accent-fg` (bg/brand) · `accent-text` (links/icons) | **Decoupled**: `bg-accent` for button/brand backgrounds (white-on = AA); `text-accent-text` for accent-coloured text/links — AA on surfaces 0-2 and on `accent-soft` in both themes (indigo-300 on dark). See visual-identity.md § Primary accent. |
| Severity | `danger`, `warning`, `success`, `info` (+ `-solid`, `-soft` and `-fg` variants) · `live` | Status, validation, badges. Decoupled like accent / accent-text: the plain token is the text, icon and border colour (`text-danger`, `border-warning`) — AA on surfaces 0-1 in both themes (dark uses the -400/-500 family); `<severity>-solid` is the background under a white label (`bg-danger-solid text-fg-onAccent` is the danger button) — AA in both themes. `-fg` is the text on the `-soft` tint. `live` has no solid |
| Node-kind | `node-agent`, `node-judge`, `node-router`, `node-human`, `node-tool`, `node-compute`, `node-done`, `node-fail`, `node-start`, `node-join`, `node-group` | Canvas borders, form headers, library cards |
| Layer | `layer-schemas`, `layer-prompts`, `layer-vars` | Layer overlay + sub-node palette |
| Selection | `selected`, `sub-tool` | Selected highlights, sub-node tool kind |
| Library | `library-pattern` | "Pattern" library category (no node-kind equivalent) |
| Radii | `radius-sm`/`md`/`lg`/`xl` | Component corner radius |
| Type | `text-caption` (10px) / `text-micro` (11px) / `text-body` (12px) / `text-label` (13px) / `text-title` (14px) / `text-display` (16px) / `text-headline` (18px) | Use the generated utility directly — `text-body`, `text-title`, … — **not** `text-[12px]` arbitrary values. `text-headline` is the page-level `<h1>` size (used by `PageHeader`); `text-display` is for emphasised section / card headings. |
| Elevation | `shadow-sm` / `shadow-md` / `shadow-lg` / `shadow-popover` | Surface depth. Consume via `shadow-[var(--shadow-popover)]`. Light-mode alphas override automatically. |
| Motion | `motion-fast` (120ms) / `motion-base` (180ms) / `motion-slow` (280ms), `motion-ease` | Transitions, animations |
| Stacking | `z-canvas` (40, panel chrome) / `z-overlay` (40, modal backdrops) / `z-modal` (50) / `z-confirm` (60, confirm-on-modal + cmd-K) / `z-popover` (70) / `z-tooltip` (80) / `z-toast` (100, also focused skip-link) | Use via `z-[var(--z-modal)]` (Tailwind arbitrary value) or `style={{ zIndex: "var(--z-modal)" }}` |
| Scrim | `scrim-modal` / `scrim-popover` / `scrim-soft` | Modal / drawer / popover backdrops. Consume via `bg-scrim-modal`. Replaces ad-hoc `bg-black/N`; light theme softens automatically. |

### Surface hierarchy

Four tiers. Stick to the semantics; don't pick a surface by hue alone — pick by role.

| Token | Role | Examples |
|---|---|---|
| `surface-0` | App background — the deepest plane | RunView main pane, Editor canvas backdrop, body of full-screen views |
| `surface-1` | Panel / card body — the default container | Headers, side panels, popovers, dialogs, kanban columns |
| `surface-2` | Input + secondary-button background; toolbar chrome | `<Input>`, Button `secondary` variant, filter pills (inactive), Tooltip bg |
| `surface-3` | Hover state on `surface-2`; also "active filter pill" | Button `secondary:hover`, filter pills (active), context menu hover |

If a new component doesn't fit cleanly: it probably needs a soft-tinted variant (`accent-soft`, `warning-soft`, …) on top of `surface-1`, not a new neutral.

### Density scale

Don't redefine spacing — Tailwind already exposes a coherent 4px scale. Commit to it semantically:

| Tailwind class | Use for |
|---|---|
| `gap-1` / `p-1` (4px) | Inside a tight cluster (icon + label, chip internals) |
| `gap-2` / `p-2` (8px) | Inter-row inside a form, dense list items |
| `gap-3` / `p-3` (12px) | Section padding inside a panel |
| `gap-4` / `p-4` (16px) | Panel / card padding |
| `gap-6` / `p-6` (24px) | Between top-level sections on a page |

If you reach for `px-3 py-1.5` or other half-step inline padding, ask if the cluster belongs inside `<Button size="sm">` or `<IconButton>` instead.

### TypeScript-side mirrors

`lib/constants.ts` re-exposes the node/layer/selection palette as `var(--...)` strings:

```ts
import { NODE_COLORS, LAYER_COLORS, SUB_COLORS, SELECTED_BORDER, SELECTED_GLOW, softColor } from "@/lib/constants";

// NODE_COLORS.agent === "var(--color-node-agent)" — works in inline styles,
// xyflow markerEnd.color, SVG fills, anywhere CSS values are accepted.
```

**Never** write a raw hex literal in a canvas, form, or run-console component. If you reach for one, ask: which token does this mean? Add it to `app.css` first, then consume.

For semi-transparent overlays (the legacy `${hex}22` pattern), use the `softColor` helper:

```ts
style={{ backgroundColor: softColor(NODE_COLORS.tool) }}        // 13% (default — matches old "22")
style={{ background: softColor(color, 10) }}                    // 10% (matches old "1A")
```

It compiles to `color-mix(in srgb, var(--color-node-tool) 13%, transparent)`. Supported in all modern browsers (Chromium 111+, Firefox 113+, Safari 16.4+).

## Primitives

Use these first. If the use-case doesn't fit, **extend the primitive** rather than rolling your own.

### Page header

[`ui/PageHeader.tsx`](../src/components/ui/PageHeader.tsx) — the single shared header for content-titled / browse / destination views (Marketplace, Plugins, …). Renders `title` in the `text-headline` token, an optional muted `description`, an `eyebrow`, an accent-soft `icon` chip, and a right-aligned `actions` slot, all on a `surface-1` raised bar with a consistent `px-6 py-4` rhythm (centred `max-w-5xl`, or `width="wide"` for dashboards).

```tsx
<PageHeader
  icon={<ArchiveIcon className="h-5 w-5" />}
  title="Bot marketplace"
  description="Browse the hosted registry…"
  actions={<Button variant="secondary" size="sm" onClick={refresh}>Refresh</Button>}
/>
```

This is the **content** title — distinct from `ContextualHeaderBar` (the slim, always-present account strip above `<main>`, fed by `useHeaderSlot`). A view uses **one or the other**, not both for the same title. Dense working/config views (Board, Dispatcher, Settings, …) keep the slim `useHeaderSlot` title; don't convert them to `PageHeader`.

### Cards & elevation

[`ui/Card.tsx`](../src/components/ui/Card.tsx) — the canonical raised container: `bg-surface-1` + hairline border + `shadow-[var(--shadow-sm)]` at rest. `interactive` adds the hover affordance (subtle lift + `hover:shadow-[var(--shadow-md)]` + `hover:border-border-strong`) for clickable cards/rows; `flush` drops the default `p-4`.

When a list item must stay a semantic `<li>`/`<button>` (so `Card`'s `<div>` doesn't fit), apply the same classes inline: `rounded-[var(--radius-lg)] border border-border-default bg-surface-1 shadow-[var(--shadow-sm)]` (+ the hover trio for interactive rows). Reach for the elevation tokens (`--shadow-sm|md|lg`) — never a raw `shadow-lg`/`shadow-xl` utility (§ Don'ts).

### Buttons & icon buttons

[`ui/Button.tsx`](../src/components/ui/Button.tsx) — `variant: primary | secondary | ghost | danger`, `size: sm | md`, `loading` prop, leading/trailing icons. Spinner is wired automatically when `loading={true}`.

[`ui/IconButton.tsx`](../src/components/ui/IconButton.tsx) — square icon-only variant for toolbars.

### Confirmation dialogs

**Never** call `window.confirm()`. It bypasses the theme, breaks focus management, and looks alien in a Tailwind UI.

Use the [`useConfirm`](../src/hooks/useConfirm.tsx) hook:

```tsx
const { confirm, dialog } = useConfirm();

const handleDestructive = async () => {
  const ok = await confirm({
    title: "Discard unsaved changes?",
    message: "You have unsaved changes that will be lost.",
    confirmLabel: "Discard",
    confirmVariant: "danger",
  });
  if (!ok) return;
  // ... do the thing
};

return <>... {dialog}</>;
```

For dialogs with custom bodies or non-binary outcomes, drop down to [`shared/ConfirmDialog`](../src/components/shared/ConfirmDialog.tsx) directly (it supports a third "secondary action" button).

For non-confirmation modals (file picker, forms, multi-step wizards), use [`ui/Dialog`](../src/components/ui/Dialog.tsx) (Radix-backed).

### Empty / loading / error states

Use [`ui/EmptyState`](../src/components/ui/EmptyState.tsx) for **every list, tree, or panel that may have no data**. Three branches, one component:

```tsx
{error ? <EmptyState message={<span className="text-danger">{error}</span>} />
 : loading ? <div className="flex items-center gap-2"><Spinner /> <span>Loading…</span></div>
 : items.length === 0 ? <EmptyState message="No commits yet" />
 : <List items={items} />}
```

For "shimmer while data loads" rather than a centered message, use [`ui/Skeleton`](../src/components/ui/Skeleton.tsx).

### Spinner

[`ui/Spinner.tsx`](../src/components/ui/Spinner.tsx) — `size: xs | sm | md`. Use for **pure indeterminate loading**:

```tsx
<Spinner size="sm" /> Loading…
```

Note: the existing pattern of rotating an icon (e.g. `<ReloadIcon className="animate-spin" />`) is a **different design choice** and intentionally kept where it shows that a specific operation (reload, refresh) is in flight. Don't blanket-replace those with `Spinner` — the icon carries semantics.

### LiveDot

[`ui/LiveDot.tsx`](../src/components/ui/LiveDot.tsx) — small coloured pulsing dot for "something is live / in flight":

```tsx
<LiveDot tone="info" size="sm" />     // workflow running
<LiveDot tone="success" size="sm" />  // data flowing / connected
<LiveDot tone="warning" pulse />      // reconnecting
<LiveDot tone="danger" pulse={false}/>// disconnected (steady)
```

Tones disambiguate the meaning so the same shape can express different states without inventing new visuals. `pulse={false}` for steady terminal states (WSStatusDot uses this for "connected" and "disconnected" — only intermediate states pulse).

**Don't use for**:
- Generic loading shimmer (use `Skeleton`).
- Urgent attention badges (apply `animate-pulse` directly on the badge — `DiagnosticBadge` is the reference).
- The AI "thinking" glyph in `ThinkingFooter` (intentional bespoke).
- Row-level pulse on in-flight events (`NodeDetailPanel` applies `animate-pulse` to the entire event row — a different design choice).

### Toasts

`useUIStore.addToast(message, level)` with four levels: `info`, `warning`, `error`, `success`. Optional `{ persistent: true }`. Use for **transient asynchronous feedback** (save complete, reload failed, etc.). Don't swallow API errors — toast them.

### Feedback surfaces

Four surfaces, and only four. Pick by the message's **lifetime and locality**, not by its tone.

| Surface | Lifetime | Locality | Use for |
|---|---|---|---|
| **Toast** (`useUIStore.addToast`) | transient (auto-dismiss) | global | result of a discrete user action — "Saved", "Dispatched 3 issues", "Save failed" |
| **InlineBanner** ([`ui/InlineBanner`](../src/components/ui/InlineBanner.tsx)) | persistent until resolved / dismissed | in-place | a contextual error/warning attached to the surface that caused it — form-level validation, "Dispatcher paused", "Tracker error" |
| **ConfirmDialog** ([`useConfirm`](../src/hooks/useConfirm.tsx)) | blocking | modal | a yes/no decision the user must make before continuing — destructive or expensive ops |
| **In-stream indicator** (e.g. `EventLog`'s error-jump chip) | as long as the data is in view | inside the data surface | per-row errors inside a list / log / table — annotate the row, don't surface globally |

**Never silent.** Every `catch` lifts its error to one of the four above. Two narrow exceptions, each of which must say so in a comment:
- **Best-effort decoration** with no analog — e.g. `NodeRunsChip`'s reverse-nav chip just hides on failure, because the outage surfaces loudly from the Runs view that owns that data.
- **Background telemetry** the user didn't initiate (heartbeats, analytics).

Don't **double-announce**: a `setError(…)` that already renders an InlineBanner shouldn't *also* `addToast` the same failure — they compete for attention.

Don't hand-roll a `<div className="bg-danger-soft text-danger-fg …">` error strip — that's exactly `<InlineBanner tone="danger">`. This is a **review-time** convention, not a scanner rule: the severity-soft tokens are legitimate on badges, status rows, event-log tints, and the dedicated banner primitives (`HeaderBanner`, `WSDisconnectBanner`, …), so a regex can't tell a hand-rolled strip from a tinted badge.

### Fetching data

Studio uses **[TanStack Query](https://tanstack.com/query)** (`@tanstack/react-query`) as the canonical fetch + cache layer. The provider is mounted in `main.tsx` with sensible defaults (`staleTime: 0`, `retry: 1`, `refetchOnWindowFocus: false` because the run console reacts to WebSocket events).

For a fresh fetch site, reach for `useQuery` directly:

```tsx
import { useQuery } from "@tanstack/react-query";

const { data, isLoading, error } = useQuery<MyThing[]>({
  queryKey: ["my-things", filter],
  queryFn: () => api.listMyThings(filter),
});

if (isLoading) return <Skeleton className="h-6" />;
if (error) return <EmptyState message={<span className="text-danger">{(error as Error).message}</span>} />;
if (!data || data.length === 0) return <EmptyState message="No things yet" />;
return <List items={data} />;
```

The library handles latest-wins race guards, deduplication across consumers of the same key, and a stable cache so the `EmptyState` / `Skeleton` consumer code stays straight-line.

**Patterns the studio uses on top of `useQuery`:**

- [`useRuns`](../src/hooks/useRuns.ts) — `refetchInterval` returns 3s vs 8s based on queue depth, `refetchIntervalInBackground: false` pauses polling when the tab is hidden.
- [`useGlobalActiveRuns`](../src/hooks/useGlobalActiveRuns.ts) — fixed 8s poll for cross-store run discovery.
- [`useRunFiles`](../src/hooks/useRunFiles.ts) / [`useRunCommits`](../src/hooks/useRunCommits.ts) — watch the in-memory event stream from `useRunStore` and call `queryClient.invalidateQueries()` on a 300ms debounce when a `node_finished` / `run_finished` / etc. event lands. No polling.
- [`useEffortCapabilities`](../src/hooks/useEffortCapabilities.ts) / [`useResolvedEffort`](../src/hooks/useResolvedEffort.ts) — `staleTime: Infinity` because the values don't change during a session. Helpers (`getCachedEffortCapabilities`, `fetchAndCacheEffortCapabilities`) wrap `queryClient.getQueryData` / `queryClient.fetchQuery` for imperative seeds; `useEffortCapabilitiesClient` binds them to the active query client.

**WebSocket lives outside the query cache.** [`useRunWebSocket`](../src/hooks/useRunWebSocket.ts) manages the connection + reconnect logic and pushes events into `useRunStore`. Components that need to react to those events watch the store directly; React Query only sees the consequent `queryClient.invalidateQueries()` calls.

### Tabs, Inputs, Selects, Checkboxes, Radios, Badges

See [`ui/index.ts`](../src/components/ui/index.ts) for the full export list — including `Checkbox`, `Radio` / `RadioGroup` (native controls, brand-accent check via `accent-accent`), `FieldLabel` (the canonical block label), and `BrandWordmark`. All wired with proper tokens, focus rings, and disabled states.

## Patterns

### Disabled controls

Always pair `disabled:opacity-*` (or color change) with `disabled:cursor-not-allowed`. Opacity alone makes the control look interactive while being inert — that's a a11y faux-positive.

The Button primitive uses the canonical pattern:

```css
disabled:bg-surface-2 disabled:text-fg-subtle disabled:cursor-not-allowed
```

For text/icon-only buttons where a background swap is too heavy, use:

```css
disabled:opacity-50 disabled:cursor-not-allowed disabled:hover:text-fg-subtle
```

### Form headers (inspector)

Each node-kind form (`AgentForm`, `RouterForm`, `ToolForm`, etc.) renders a colored header so the user knows what they're editing at a glance. The header color **must** equal the node-kind token — that's how the canvas → inspector visual link works.

```tsx
const headerColor = NODE_COLORS[kind]; // "var(--color-node-agent)"
<div
  className="flex items-center gap-2 px-2 py-1.5 rounded mb-2 -mx-1"
  style={{
    backgroundColor: softColor(headerColor),
    borderLeft: `3px solid ${headerColor}`,
  }}
>
  <span style={{ color: headerColor }}>{headerLabel}</span>
</div>
```

### Status colors

Stick to the severity palette. Red is **reserved** for `node-fail` and diagnostic errors — don't use red for anything else. Amber/`warning` is the second-tier "needs attention" token; reuse it for entry markers, loop edges, schema mismatches.

### Canvas edges (xyflow)

`markerEnd.color` accepts CSS variables — pass `"var(--color-warning)"` rather than `"#F59E0B"`. xyflow renders the marker as SVG `<polygon fill={color}>`, which resolves the variable against `:root`.

```ts
markerEnd: { type: MarkerType.ArrowClosed, color: "var(--color-fg-subtle)", width: 16, height: 16 }
```

### Themes

The studio supports dark and light themes via `[data-theme="..."]` on `<html>` (managed by [`store/theme.ts`](../src/store/theme.ts)). The full token palette — including the **canvas tokens** (`node-*`, `layer-*`, `selected`, `sub-tool`, `library-pattern`) — has light-mode overrides using the Tailwind-700 family for contrast against the lighter surface.

When adding a new color token to `@theme`, **always** add the matching `[data-theme="light"]` override. If you forget, light mode falls back to the dark default and prints poorly on white surfaces.

## Accessibility

What's already wired:
- `:focus-visible` global outline ([app.css](../src/app.css), `:focus-visible` block) on the `focus-ring` token, plus counter-rules for the libraries that reset it: `@xyflow/react` (stylesheet, nodes / edges / selection rects) and `react-virtuoso` (inline `outline: none` on its scroller, so `!important`). Wiring and token values are held by `__tests__/a11y/focus-ring.test.ts`.
- Contrast is audited, not eyeballed: `__tests__/a11y/contrast.test.ts` holds the text tiers, the severity pairs, `accent-text`, `focus-ring` and `border-control` to their WCAG bars in both themes, and `tokens-mirror.test.ts` holds that audit's mirror (`_tokens.ts`) equal to `app.css`. `source-discipline.test.ts` reads the JSX (`_jsx.ts`, every className shape, per tag) and refuses the shapes the RGAA audit found: a static `opacity-N` of any step or arbitrary value (or `hover:opacity`, a `text-…/NN` alpha, an inline `style={{ opacity }}`) on an element, a chrome border token on a native control or a control primitive, white `accent-fg` text off `bg-accent` / a `bg-<severity>-solid`, `accent-text` on a `bg-accent/NN` alpha tint, `fg-subtle` on a `-soft` tint, a className whose variant product overflows the reader. Pairing rules are judged per className variant (every branch of a ternary, every key of a `clsx` object, one value of a variant map).
- All `disabled:` controls pair their visual change with `disabled:cursor-not-allowed` (Q4).
- `ConfirmDialog` traps focus and exits on Escape.
- `useConfirm` returns a Promise so call-sites stay synchronous-shaped.
- `Skeleton` renders `aria-hidden` so screen readers skip the shimmer.
- `LiveDot` accepts an optional `label` for screen reader announcement.
- **Reduced motion**: a global `@media (prefers-reduced-motion: reduce)` block in `app.css` removes decorative pulses/fades and collapses transitions to ~instant (WCAG 2.3.3 / 2.2.2). Status is always carried by colour/text, never motion alone.
- `Checkbox` / `Radio` / `RadioGroup` are native controls (free keyboard + SR semantics); `FieldLabel` owns the block-label + `?` help affordance.
- `IconButton` mandates a `label` prop and applies it as `aria-label`.
- Toast component is announced via the toast bus.
- FormField inputs wire `aria-describedby` to their help icon and error message via the `FieldRow` wrapper. Set the `error` prop to render `<p role="alert">` and add `aria-invalid` on the input.
- **Skip-link** from `AppShell` (`<a href="#main-content">`) becomes visible on keyboard focus and jumps to the main work surface. Implemented on Home, Editor, RunList, RunView, Board, Dispatcher — pages without an `id="main-content"` anchor degrade gracefully.

### Clickable rows and cards

A row, card or table row that reacts to a click takes one of the two shapes
[`lib/a11y.ts`](../src/lib/a11y.ts) offers, and the choice is decided by ONE
question: does the row hold anything focusable?

| The row | Shape | Why |
|---|---|---|
| holds nothing focusable (text, badges, layout) | `clickableRowProps(onActivate, label?, opts?)` — `role="button"` + `tabindex` + Enter/Space | the row IS the control, and one tab stop is the whole row |
| holds a control (a button, a link, a file input) | `pointerRowProps(onActivate, opts?)` — a click, nothing else | a `role="button"` around a control is a button inside a button (axe `nested-interactive`, WCAG 4.1.2), and the row's Enter races the inner control's |

A pointer row is legal only because a real control inside it carries the
keyboard path (RGAA 7.3) — the title button of a run card, the Open button of
a table row, the `<label>` of a file drop zone ([`FileDropZone`](../src/components/shared/FileDropZone.tsx),
whose sibling [`HiddenFileInput`](../src/components/shared/HiddenFileInput.tsx)
is for the input a visible button drives instead). Neither shape takes a
second `onClick` / `onKeyDown` / `role` / `tabIndex` beside the spread: a
later JSX prop wins, so the helper's handler silently stops running. What a
site needs travels through the helper — the activation carries the cursor
position, or null when a key activated the row.

Both halves are a gate, not a convention: the scan in
[`src/__tests__/a11y/_clickableRows.ts`](../src/__tests__/a11y/_clickableRows.ts)
follows every call site's descendants (components included, to their own
definition) and `source-discipline.test.ts` fails on a row that breaks either
rule. A descendant it cannot follow is reported until somebody decides it.

### Axe-core a11y tests

A regression-trap for the shared primitives lives at [`src/__tests__/a11y/primitives.test.tsx`](../src/__tests__/a11y/primitives.test.tsx). It boots jsdom, renders Button / IconButton / EmptyState / Spinner / LiveDot / Badge / Skeleton / Checkbox / Radio / FieldLabel / BrandWordmark in every variant, and asserts zero axe-core violations against `wcag2a`, `wcag2aa`, `wcag21a`, `wcag21aa` rule sets. Add a new test there when you ship a new primitive.

```bash
pnpm -F iterion-studio test
```

The jsdom environment is opt-in per file via `// @vitest-environment jsdom` so pure-function tests stay on the fast Node runner.

#### SC 2.5.8 target-size: the real-engine half

`wcag22aa` is in the jsdom net's declared scope, and its only rule today is `target-size` — which jsdom cannot judge (no layout: every element measures 0×0 and axe reads that as a PASS). The rule gets its verdict in a real browser from [`e2e/a11y/target-size.spec.ts`](../e2e/a11y/target-size.spec.ts) (helper: [`e2e/a11y/targetSize.ts`](../e2e/a11y/targetSize.ts)), driven by its own [`playwright.a11y.config.ts`](../playwright.a11y.config.ts) — deliberately OUTSIDE the default `e2e/specs` sweep: the decided trigger (#2765) is the `studio-e2e-advisory` workflow (weekly schedule + `workflow_dispatch`), never the per-PR gate. Run it on demand:

```bash
devbox run -- task test:a11y:target-size    # builds the binary; skips cleanly without chromium
corepack pnpm -C studio test:a11y:target-size
```

The policy is decided (#2765): the studio's floor is SC 2.5.8's **24px AA minimum**, and WCAG's exceptions are accepted where one honestly applies. The spec walks a declared surface list (`SURFACES`), runs axe's `target-size` rule, and measures every sub-24px node it saw — including the ones axe PASSES only through the spacing exception, since accepting that exception is a decision to record, not a pass to swallow. A finding survives only behind an entry in `TARGET_SIZE_WAIVERS` (per surface, per selector/tag/container, `maxNodes`-bounded so a class cannot grow unexamined) whose `decision` names the WCAG exception it rests on — `spacing`, `inline` or `equivalent` — with a justification giving the measured facts. A waiver without a decision fails the run, a waiver that matches nothing fails, and so does an unwaivered finding. The list is the studio's documented-exception ledger for the criterion.

Run a manual axe browser-extension sweep on `/`, `/editor`, `/runs/:id`, `/board`, `/dispatcher` (application settings are a dialog, not a route) before any large UI release — jsdom can't model the canvas, the WebSocket flows, or the full layout, so the unit suite is a floor not a ceiling.

Open items still requiring human / browser verification:
- Light-mode canvas variants on `softColor(color, 10)` backgrounds (contrast).
- `role="button"` divs in `LogLinesView`, `Canvas/DetailSubNode`, `Canvas/AuxiliaryNode`, `Canvas/SubNodePalette` — each has the keyboard handler (`clickableRowProps`, scanned by `source-discipline`), so what is left for a browser is the focus ring's visibility over the canvas and the drag affordance of a `draggable` row (#2770).
- Full keyboard reachability of the canvas — cycling between root nodes via `Tab` alone.

## Don'ts

| Pattern | Why | Use instead |
|---|---|---|
| `window.confirm("…")` | Bypasses theme, no focus mgmt, alien UX | `useConfirm()` |
| `style={{ color: "#3B82F6" }}` | Drifts from tokens, breaks theme switch | `style={{ color: NODE_COLORS.agent }}` or `text-node-agent` utility |
| `${hex}22` for soft bg | Doesn't work with `var()` strings | `softColor(token)` |
| `<div>Loading…</div>` ad-hoc | Visual drift across panels | `<EmptyState message="Loading…" />` |
| Hand-rolled `<div className="bg-danger-soft text-danger-fg …">` error strip | Re-implements InlineBanner; drifts on spacing / role / aria-live | `<InlineBanner tone="danger">` (§ Feedback surfaces) |
| `<span className="animate-spin border-2 …" />` ad-hoc | Reinvents Button's spinner | `<Spinner size="sm" />` |
| `shadow-lg` / `shadow-2xl` raw utility | Drifts from the elevation ladder; `shadow-2xl` isn't even a token | `shadow-[var(--shadow-popover)]` (or `-lg` / `-md`) |
| `z-10` / `z-20` / `z-30` raw utility | Ad-hoc stacking races when surfaces nest | `z-[var(--z-canvas)]` … per the stacking ladder |
| `text-[11px]` / `text-[12px]` arbitrary size | Drifts from the type scale | `text-micro` / `text-body` … (semantic type tokens) |
| `disabled:opacity-50` alone on a button | a11y faux-positive (still looks clickable) | Add `disabled:cursor-not-allowed` |
| `text-error`, `border-error` | These classes don't exist (no `--color-error` token) | `text-danger`, `border-danger` |
| A static `opacity-N` literal on an element (`opacity-70` on a `<span>`, `opacity-25` or `opacity-[.6]` on a row), a `hover:opacity-80`, a `text-fg-subtle/70` alpha, an inline `style={{ opacity: .75 }}` | The text's contrast drops with the alpha (the audit measured 4.39:1 and below, RGAA 3.2); `source-discipline` reads every className shape and refuses it | `text-fg-muted` / `text-fg-subtle` for de-emphasised text; `disabled:opacity-60` or `data-[disabled]:opacity-60` + the attribute for an inactive state (prefixed by a state, exempt); `saturate-50` / `grayscale` to mute a block without touching its text |
| `border-border-strong` / `-default` / `-subtle` on an input / textarea / select | Under 2.5:1 against the surface: the field has no perceptible boundary (RGAA 3.3) | `border-border-control` |
| `text-accent-fg` / `text-fg-onAccent` on `bg-accent-soft`, `bg-accent/20` or a bare surface | Both are white: 1.0-1.5:1 in the light theme (RGAA 3.2) | `text-accent-text` — white only on `bg-accent` or a `bg-<severity>-solid` |
| `bg-danger text-fg-onAccent` (a white label on the plain severity) | The plain severity is the text colour: dark red-400 … cyan-500 hold white at 2.2-2.8:1 (RGAA 3.2) | `bg-danger-solid text-fg-onAccent` — the `-solid` twin holds the label at AA in both themes |
| `text-accent-text` on `bg-accent/NN` (any ad-hoc alpha) | A pair no contrast row measures — accent/20 read 4.4-4.6:1 over the light panels (RGAA 3.2) | `bg-accent-soft text-accent-text` — the one tint the audit holds (5.0+) |
| `text-fg-subtle` on a `-soft` tint (`bg-warning-soft text-fg-subtle`) | 4.0-4.2:1 over the light panels; under 4.5 in dark on `live-soft` or on surface-2 (RGAA 3.2) | `text-fg-muted`, or the tint's own `-fg` |
| New CSS-in-JS hex outside `app.css` / `lib/constants.ts` | Bypasses palette, breaks future theme variants | Add a token to `app.css`, expose via constants |
| Inventing a secondary accent for "in-flight / live" | One accent only — `info` already carries the running semantic, `accent-soft` covers "soft primary" needs | Reuse `info` (LiveDot tone) for live state; `accent-soft` for muted primary surfaces |
| Adopting an external design library (shadcn/ui, Mantine, Park UI) | The studio already _is_ a Radix + Tailwind + token system; bringing another duplicates primitives and breaks the contract above. | Extend the existing primitives in `ui/`; if a use-case really wants a peer library, raise it at design review first |

## When to extend vs roll your own

Before adding a new primitive, ask:

1. **Is there already a primitive that almost fits?** Extend its props (add a `variant`, a size) instead of forking.
2. **Is this a one-off vs reusable?** A one-off form layout is fine inline; a pattern repeated 3+ times deserves a primitive.
3. **Where does this fit in the tokens?** If you can't name the design tokens you'd use, the design isn't finished yet — pause and define them.

## Responsive scope

The studio is **desktop-first**. The Editor (xyflow canvas with drag-drop, multi-select, library palette) and RunView (resizable panels, scrubber, dock) are not designed for touch and remain effectively desktop-only.

What does work on narrow viewports (tested ≥ 360 px wide):
- The Sidebar collapses gracefully — NavLinks become icon-only below `sm` (640 px), `ProjectLabel` caps at 140 px, the user-team chip drops the email line. Brand + right-actions stay shrink-0.
- HomeView grid switches from 2-col to 1-col below `md` (768 px).
- RunListView table scrolls horizontally inside its container.
- Board kanban already scrolls horizontally by design.
- Skip-link is keyboard-only — no mobile impact.

The pragmatic mental model: read-only flows (Home, RunList, Board, Dispatcher, Settings) are reachable on a phone for monitoring a run in flight; authoring flows (Editor, Launch form) expect a real desktop. When a feature crosses that line, hide it on `< sm` with a clear notice rather than reflowing it badly.

Use [`ui/DesktopOnlyNotice`](../src/components/ui/DesktopOnlyNotice.tsx) as the gate:

```tsx
<DesktopOnlyNotice
  feature="the workflow editor"
  lsKey="iterion.editor.mobile-optin"
>
  {/* desktop UI */}
</DesktopOnlyNotice>
```

It renders the children at `sm` and above, otherwise a centered notice with a "Continue anyway" button (persisted in localStorage via `lsKey`). Currently mounted on `LaunchView` and `EditorView`. Read-only views (RunList card mode, Settings two-up→stack) reflow inline — they do not use this primitive.

## Open items

- **Pulse semantics** — `animate-pulse` is used for several meanings beyond the `LiveDot` dot pattern (Skeleton shimmer, DiagnosticBadge urgency, ThinkingFooter glyph, NodeDetailPanel row in-flight). Documented in `LiveDot.tsx` but consider a follow-up audit if more callers appear.
- **WCAG AA contrast audit on every theme** — the axe-core unit suite covers primitives; light-mode canvas variants on `softColor(color, 10)` backgrounds still need a manual browser sweep.
- **Touch / mobile authoring** — explicitly out of scope until a use-case demands it.
- **EventLog fresh-arrival pulse** — designed (rate-limited burst-aware pulse-flash on most recently arrived rows). Deferred — Virtuoso mount/unmount semantics need a per-seq "seen" registry to avoid re-pulsing on scroll. Pick up when the run-console restructure lands a node-focus breadcrumb (Phase 6).
