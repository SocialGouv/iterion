// The chat dock's presentation, with no idea what it is talking to.
//
// Deliberately NOT built on ui/Dialog (Radix): this is a NON-modal
// surface — the operator keeps interacting with the page behind it (no
// scrim, no focus trap, page stays live). Radix Dialog is modal by
// design, so the hand-rolled shell here is intentional.
//
// Extracted verbatim from Runs/FloatingChatPanel so the same three
// states serve both the shell-level assistant and the run console's
// steering panel. Everything session-specific (transcript, composer,
// unread count, attention state) is injected by the caller.

import { useCallback, useEffect, useRef, useState } from "react";
import type { DragEvent as ReactDragEvent, ReactNode } from "react";
import {
  ChatBubbleIcon,
  MinusIcon,
  PinRightIcon,
} from "@radix-ui/react-icons";

import { IconButton } from "@/components/ui";
import {
  DOCKED_WIDTH_DEFAULT_PX,
  FLOATING_MIN_HEIGHT_PX,
  FLOATING_MIN_WIDTH_PX,
  clampDockSize,
  openedDock,
  readDockSize,
  writeDockSize,
  type DockSize,
  type DockState,
} from "@/lib/chatDock/dockState";
import { hasReferenceDrag } from "@/lib/chatDock/dragReference";

// Two docks can be on the page at once (the assistant everywhere, the
// run console's steering panel on /runs/:id). A lane is the bottom-right
// slot each one owns so they never land on top of each other. Lane 0 is
// the canonical corner — it belongs to the surface that is present on
// EVERY route, so its position never moves under the operator.
export type DockLane = 0 | 1;

// Lanes are px offsets from the right edge, not Tailwind classes: they
// have to compose with `rightInset` below, and an interpolated class
// name is not something Tailwind can see.
const BUBBLE_LANE_PX: Record<DockLane, number> = { 0: 16, 1: 80 };
const FLOATING_LANE_PX: Record<DockLane, number> = { 0: 16, 1: 448 };

// Breathing room between a fixed corner surface and whatever it is
// clearing.
const EDGE_GUTTER_PX = 16;

// `bottom-4` in the panel's class, and the breathing room kept on the two
// edges it can grow toward. Named so the clamp and the CSS cannot drift.
const FLOATING_BOTTOM_PX = 16;
const FLOATING_EDGE_MARGIN_PX = 16;

const FLOATING_WIDTH_PX = 420;
const FLOATING_HEIGHT_PX = 520;

// Right edge a lane-0 floating panel occupies (its offset plus its width).
// Exported because a PEER fixed surface has to clear the band, not just the
// docked column: `rightInset` is about what another `fixed` element must step
// out of, and a floating panel is every bit as click-blocking as a docked one.
export const FLOATING_FOOTPRINT_PX = FLOATING_LANE_PX[0] + FLOATING_WIDTH_PX;

// A lane offset is a floor, not an absolute: when another surface has
// reserved the right edge (the assistant's docked column), a lane that
// falls inside that band must step out of it. `padding` on the layout
// root does nothing here — these are `fixed`, so they'd sit UNDER the
// column and take no clicks. A lane that already clears is left alone,
// so nothing moves for a reservation it never overlapped.
function laneRightPx(base: number, rightInset: number): number {
  return rightInset > 0 ? Math.max(base, rightInset + EDGE_GUTTER_PX) : base;
}

// Width of the self-rendered docked column. Exported because the shell
// (AppShell) reserves exactly this much so the dock pushes the page
// aside instead of covering it.
// Re-exported from the lib so the layout reservation, the persisted width and
// this panel cannot drift apart.
export const DOCKED_WIDTH_PX = DOCKED_WIDTH_DEFAULT_PX;

export interface ChatDockShellProps {
  dock: DockState;
  onDockChange: (next: DockState) => void;
  // Chrome title + accessible name. This is what makes an assistant
  // dock distinguishable from a steering panel at a glance.
  title: string;
  // Longer explanation of what typing here does, shown as the chrome's
  // tooltip. Optional but strongly encouraged when two docks coexist.
  titleHint?: string;
  // Rendered in the header strip next to the title. The assistant puts its
  // bot switcher here; a host with one fixed correspondent passes nothing.
  headerSlot?: ReactNode;
  lane?: DockLane;
  // Right-edge width another surface has reserved (see laneRightPx).
  rightInset?: number;
  // Left-edge width the layout chrome owns (the sidebar). The floating
  // panel grows LEFTWARD from its top-left resize handle, so without this
  // its clamp is the raw viewport and an enlarged panel slides over the
  // nav. Mirrored from rightInset; 0 when the chrome is gone (focus mode).
  leftInset?: number;
  // Closed-bubble presentation.
  bubbleIcon?: ReactNode;
  bubbleLabel?: string;
  bubbleTitle?: string;
  // Unread messages accumulated while closed (badge on the bubble).
  unread?: number;
  // Something needs the operator (a pause, a question) — pulses the
  // bubble and swaps its tooltip.
  attention?: boolean;
  attentionTitle?: string;
  // A reference source can be dragged from the page while the dock is
  // minimised. Spring the assistant open on entry so its full-body drop
  // target is available before the pointer is released.
  openOnReferenceDrag?: boolean;
  // "self": the shell renders the docked-right column itself (the
  // shell-level assistant, which owns a real layout slot).
  // "host": the shell renders nothing and the host lays the panel out
  // via ChatDockPanel (the run console, whose SideDock owns the column).
  dockedRightMode?: "self" | "host";
  // Width of the self-hosted docked column, and the setter that makes it
  // resizable. Owned by the caller because the SAME number drives the host
  // page's layout reservation — a width the panel kept to itself would let
  // the page and the dock disagree about where the edge is. Omitting
  // onWidthChange simply leaves the column fixed.
  dockedWidth?: number;
  onWidthChange?: (px: number) => void;
  // localStorage key under which the FLOATING panel remembers the size the
  // operator dragged it to. Omitted = the panel resizes but forgets.
  floatingSizeKey?: string;
  children: ReactNode;
}

// ChatDockShell dispatches on the dock state. Callers keep the state
// (persisted per user) and hand it back down.
export function ChatDockShell({
  dock,
  onDockChange,
  title,
  titleHint,
  headerSlot,
  lane = 0,
  rightInset = 0,
  leftInset = 0,
  bubbleIcon,
  bubbleLabel,
  bubbleTitle,
  unread = 0,
  attention = false,
  attentionTitle,
  openOnReferenceDrag = false,
  dockedRightMode = "host",
  dockedWidth = DOCKED_WIDTH_DEFAULT_PX,
  onWidthChange,
  floatingSizeKey,
  children,
}: ChatDockShellProps) {
  // Did the operator just open this, or is it restoring a persisted
  // state? The floating panel moves focus into itself on mount, which
  // was right for a panel you had just clicked open on ONE route. Now
  // the dock is shell-level and its state is persisted per user, so an
  // unguarded mount steals focus on every cold page load whose stored
  // state is `floating`, and again on every navigation back from
  // /whats-next (where it unmounts and remounts) — landing on an
  // autofocused search field and yanking the caret out of it.
  const openedByOperator = useRef(false);
  const changeDock = useCallback(
    (next: DockState) => {
      openedByOperator.current = true;
      onDockChange(next);
    },
    [onDockChange],
  );

  if (dock === "closed") {
    return (
      <ChatDockBubble
        label={bubbleLabel ?? `Open ${title.toLowerCase()}`}
        title={bubbleTitle ?? title}
        icon={bubbleIcon}
        unread={unread}
        attention={attention}
        attentionTitle={attentionTitle}
        lane={lane}
        rightInset={rightInset}
        onOpen={() => changeDock(openedDock())}
        onDragEnter={(e) => {
          if (!openOnReferenceDrag || !hasReferenceDrag(e.dataTransfer)) return;
          e.preventDefault();
          changeDock(openedDock());
        }}
      />
    );
  }
  if (dock === "docked-right") {
    if (dockedRightMode === "host") return null;
    // "self": the dock is mounted outside the layout tree, so it pins
    // its own full-height column at the right edge. AppShell reserves
    // DOCKED_WIDTH_PX of padding so this pushes the page aside rather
    // than covering it — a dock that hides what you're asking about
    // would defeat the point.
    return (
      <div
        className="fixed top-0 right-0 bottom-0 z-[var(--z-dock)]"
        style={{ width: `min(${dockedWidth}px, 100vw)` }}
      >
        {onWidthChange && (
          <DockResizeHandle width={dockedWidth} onWidthChange={onWidthChange} />
        )}
        <ChatDockPanel
          title={title}
          titleHint={titleHint}
          headerSlot={headerSlot}
          onUndock={() => changeDock("floating")}
          onClose={() => changeDock("closed")}
        >
          {children}
        </ChatDockPanel>
      </div>
    );
  }
  return (
    <ChatDockFloating
      label={title}
      lane={lane}
      rightInset={rightInset}
      leftInset={leftInset}
      focusOnMount={openedByOperator.current}
      sizeKey={floatingSizeKey}
      onClose={() => changeDock("closed")}
    >
      <ChatDockChrome
        title={title}
        titleHint={titleHint}
        headerSlot={headerSlot}
        onDockRight={() => changeDock("docked-right")}
        onClose={() => changeDock("closed")}
      />
      {children}
    </ChatDockFloating>
  );
}

// Non-blocking floating dialog: labelled, keyboard-reachable, and
// dismissable via Escape. Focus moves into the panel on mount so a
// keyboard user can immediately Tab through the chrome + body without
// reaching the page background first. Intentionally NOT a focus trap —
// the page underneath remains interactive (this is a docked helper,
// not a modal).
export function ChatDockFloating({
  label,
  lane = 0,
  rightInset = 0,
  leftInset = 0,
  focusOnMount = false,
  sizeKey,
  onClose,
  children,
}: {
  sizeKey?: string;
  label: string;
  lane?: DockLane;
  rightInset?: number;
  // Width the left chrome (sidebar) owns. The panel grows leftward, so its
  // width budget stops at the sidebar's right edge, not at the viewport's.
  leftInset?: number;
  // Move focus into the panel on mount. Only true when the operator
  // just opened it — a restore-from-localStorage mount must leave the
  // page's own focus alone.
  focusOnMount?: boolean;
  onClose: () => void;
  children: ReactNode;
}) {
  const ref = useRef<HTMLDivElement>(null);

  // The panel is anchored BOTTOM-RIGHT, which is what made the browser's own
  // `resize` handle useless here: it sits at the bottom-right corner, so
  // dragging it grows the panel off the screen and the operator can only ever
  // make it smaller. The handle below is at the TOP-LEFT instead, where
  // dragging outward grows it into the viewport.
  //
  // Driven by state rather than read back off the element, so the persisted
  // size and what is on screen cannot drift.
  const [size, setSize] = useState<DockSize>(() =>
    sizeKey
      ? readDockSize(sizeKey, {
          width: FLOATING_WIDTH_PX,
          height: FLOATING_HEIGHT_PX,
        })
      : { width: FLOATING_WIDTH_PX, height: FLOATING_HEIGHT_PX },
  );
  // The panel is pinned bottom-right, so the space it may grow into is the
  // viewport MINUS its own offsets and the left chrome — not the whole
  // viewport. Clamping against the raw viewport let a full-width panel start
  // at a negative x and run off the left edge, which is the one direction it
  // can escape; clamping short of `leftInset` only kept it on-screen but ON
  // the sidebar, which is the surface a leftward drag meets first.
  const right = laneRightPx(FLOATING_LANE_PX[lane], rightInset);
  const available = useCallback(
    (): { width: number; height: number } => ({
      width: Math.max(
        FLOATING_MIN_WIDTH_PX,
        (typeof window === "undefined" ? 1280 : window.innerWidth) -
          leftInset -
          right -
          FLOATING_EDGE_MARGIN_PX,
      ),
      height: Math.max(
        FLOATING_MIN_HEIGHT_PX,
        (typeof window === "undefined" ? 800 : window.innerHeight) -
          FLOATING_BOTTOM_PX -
          FLOATING_EDGE_MARGIN_PX,
      ),
    }),
    [leftInset, right],
  );

  const resizeTo = useCallback(
    (next: DockSize) => {
      const clamped = clampDockSize(next, available());
      setSize(clamped);
      if (sizeKey) writeDockSize(sizeKey, clamped);
    },
    [sizeKey, available],
  );

  // A window that SHRANK must not leave the panel hanging off the edge. Same
  // clamp, so what is restored and what is on screen agree.
  useEffect(() => {
    const onResize = () => setSize((current) => clampDockSize(current, available()));
    onResize();
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, [available]);

  useEffect(() => {
    if (!focusOnMount) return;
    // Defer focus so any layout / autofocus inside children settles first.
    const t = window.setTimeout(() => {
      const root = ref.current;
      if (!root) return;
      if (root.contains(document.activeElement)) return;
      const focusable = root.querySelector<HTMLElement>(
        'button, [href], input, textarea, select, [tabindex]:not([tabindex="-1"])',
      );
      (focusable ?? root).focus();
    }, 0);
    return () => window.clearTimeout(t);
  }, [focusOnMount]);
  return (
    <div
      ref={ref}
      tabIndex={-1}
      className="fixed bottom-4 z-[var(--z-dock)] flex flex-col rounded-md border border-border-default bg-surface-1 shadow-[var(--shadow-popover)] overflow-hidden focus:outline-none"
      style={{
        right: laneRightPx(FLOATING_LANE_PX[lane], rightInset),
        width: size.width,
        height: size.height,
        minWidth: 320,
        minHeight: 280,
      }}
      role="dialog"
      aria-label={label}
      onKeyDown={(e) => {
        if (e.key === "Escape") {
          e.stopPropagation();
          onClose();
        }
      }}
    >
      <FloatingResizeHandle size={size} onResize={resizeTo} />
      {children}
    </div>
  );
}

// Grow the floating panel from its TOP-LEFT corner.
//
// That corner and not the browser's default: the panel is pinned bottom-right,
// so the native handle could only push it off-screen. Dragging up and to the
// left grows it into the page, which is the direction the operator means.
//
// Same pointer-capture approach as the docked column's handle, and keyboard
// reachable for the same reason: a control that only answers to a drag is
// unusable to anyone who cannot make one.
function FloatingResizeHandle({
  size,
  onResize,
}: {
  size: DockSize;
  onResize: (next: DockSize) => void;
}) {
  const teardownRef = useRef<(() => void) | null>(null);
  useEffect(() => () => teardownRef.current?.(), []);
  const onPointerDown = (e: React.PointerEvent<HTMLDivElement>) => {
    e.preventDefault();
    teardownRef.current?.();
    const startX = e.clientX;
    const startY = e.clientY;
    const start = size;
    // Listen on the WINDOW, not on the handle.
    //
    // setPointerCapture on a 16px corner looked equivalent and was not: a real
    // drag leaves that square within a frame, and any move the capture failed
    // to redirect went to whatever was underneath — so the panel never
    // resized. Window listeners cannot miss, whatever the pointer does or how
    // fast it leaves.
    const onMove = (ev: PointerEvent) => {
      onResize({
        width: start.width + (startX - ev.clientX),
        height: start.height + (startY - ev.clientY),
      });
    };
    const teardown = () => {
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", onUp);
      window.removeEventListener("pointercancel", onUp);
      teardownRef.current = null;
    };
    const onUp = () => teardown();
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", onUp);
    window.addEventListener("pointercancel", onUp);
    teardownRef.current = teardown;
  };
  return (
    <div
      role="separator"
      aria-label="Resize the assistant panel"
      tabIndex={0}
      onPointerDown={onPointerDown}
      onKeyDown={(e) => {
        const step = e.shiftKey ? 64 : 16;
        const delta: Partial<DockSize> = {};
        if (e.key === "ArrowLeft") delta.width = size.width + step;
        else if (e.key === "ArrowRight") delta.width = size.width - step;
        else if (e.key === "ArrowUp") delta.height = size.height + step;
        else if (e.key === "ArrowDown") delta.height = size.height - step;
        else return;
        e.preventDefault();
        onResize({ ...size, ...delta });
      }}
      title="Drag to resize"
      className="absolute left-0 top-0 z-10 h-5 w-5 cursor-nwse-resize focus:outline-none group"
    >
      {/* A visible grip. The previous corner was a 16px hitbox that only
          appeared on hover, in the same corner as the title — findable only by
          someone who already knew it was there. */}
      <svg
        viewBox="0 0 10 10"
        aria-hidden="true"
        className="h-full w-full text-fg-subtle group-hover:text-accent-text"
      >
        <path
          d="M9 1 H1 V9"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
        />
      </svg>
    </div>
  );
}

// The closed state: a corner bubble carrying the unread badge and the
// needs-attention pulse.
export function ChatDockBubble({
  label,
  title,
  icon,
  unread = 0,
  attention = false,
  attentionTitle,
  lane = 0,
  rightInset = 0,
  onOpen,
  onDragEnter,
}: {
  label: string;
  title: string;
  icon?: ReactNode;
  unread?: number;
  attention?: boolean;
  attentionTitle?: string;
  lane?: DockLane;
  rightInset?: number;
  onOpen: () => void;
  onDragEnter?: (e: ReactDragEvent<HTMLButtonElement>) => void;
}) {
  return (
    <button
      type="button"
      onClick={onOpen}
      onDragEnter={onDragEnter}
      style={{ right: laneRightPx(BUBBLE_LANE_PX[lane], rightInset) }}
      className={`fixed bottom-4 z-[var(--z-dock)] h-12 w-12 rounded-full border shadow-[var(--shadow-lg)] flex items-center justify-center transition-transform hover:scale-105 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent ${attention ? "border-warning bg-warning-soft animate-pulse" : "border-border-default bg-surface-2 text-fg-default"}`}
      aria-label={`${label}${unread > 0 ? ` (${unread} new)` : ""}`}
      title={attention ? attentionTitle ?? title : title}
    >
      {icon ?? <ChatBubbleIcon className="h-5 w-5" />}
      {unread > 0 && (
        <span
          className="absolute -top-1 -right-1 min-w-[18px] h-[18px] px-1 rounded-full bg-accent text-fg-onAccent text-caption font-semibold flex items-center justify-center"
          aria-hidden
        >
          {unread > 99 ? "99+" : unread}
        </span>
      )}
    </button>
  );
}

// The docked-right column: chrome + body filling whatever slot the host
// (or the shell itself, in "self" mode) gives it.
export function ChatDockPanel({
  title,
  titleHint,
  headerSlot,
  onUndock,
  onClose,
  children,
}: {
  title: string;
  titleHint?: string;
  headerSlot?: ReactNode;
  onUndock: () => void;
  onClose: () => void;
  children: ReactNode;
}) {
  return (
    <div className="h-full w-full flex flex-col bg-surface-1 border-l border-border-default">
      <ChatDockChrome
        title={title}
        titleHint={titleHint}
        headerSlot={headerSlot}
        onUndock={onUndock}
        onClose={onClose}
      />
      {children}
    </div>
  );
}

// The header strip. `title` is the one word that tells the operator what
// this surface does — "Assistant" answers you, "Steering" pushes into a
// live agent.
export function ChatDockChrome({
  title,
  titleHint,
  headerSlot,
  onDockRight,
  onUndock,
  onClose,
}: {
  title: string;
  titleHint?: string;
  headerSlot?: ReactNode;
  onDockRight?: () => void;
  onUndock?: () => void;
  onClose: () => void;
}) {
  return (
    <div className="shrink-0 flex items-center justify-between gap-2 px-3 py-1 border-b border-border-default bg-surface-2">
      <div className="flex items-center gap-2 min-w-0">
        <span
          className="text-micro font-medium text-fg-default uppercase tracking-wide shrink-0"
          title={titleHint}
        >
          {title}
        </span>
        {headerSlot}
      </div>
      <div className="flex items-center gap-0.5">
        {onDockRight && (
          <IconButton
            label={`Dock ${title.toLowerCase()} to right side`}
            tooltip="Dock to right"
            size="sm"
            variant="ghost"
            onClick={onDockRight}
          >
            <PinRightIcon className="h-3.5 w-3.5" />
          </IconButton>
        )}
        {onUndock && (
          <IconButton
            label="Undock to floating panel"
            tooltip="Float (undock)"
            size="sm"
            variant="ghost"
            onClick={onUndock}
          >
            <PinRightIcon
              className="h-3.5 w-3.5"
              style={{ transform: "scaleX(-1)" }}
            />
          </IconButton>
        )}
        <IconButton
          label={`Minimise ${title.toLowerCase()}`}
          tooltip="Minimise"
          size="sm"
          variant="ghost"
          onClick={onClose}
        >
          <MinusIcon className="h-3.5 w-3.5" />
        </IconButton>
      </div>
    </div>
  );
}

// Drag the docked column wider or narrower from its inner edge.
//
// Pointer events rather than mouse: one code path covers trackpad, mouse and
// touch, and setPointerCapture keeps the drag alive when the cursor outruns
// the 6px strip — which it always does. Width is committed on every move so
// the page reflows WITH the drag; the clamp lives in setDockWidth, so a drag
// past the ceiling simply stops widening.
//
// Keyboard-reachable on purpose: a separator that only answers to a drag is
// unusable to anyone who cannot make one.
function DockResizeHandle({
  width,
  onWidthChange,
}: {
  width: number;
  onWidthChange: (px: number) => void;
}) {
  const teardownRef = useRef<(() => void) | null>(null);
  useEffect(() => () => teardownRef.current?.(), []);
  const onPointerDown = (e: React.PointerEvent<HTMLDivElement>) => {
    e.preventDefault();
    teardownRef.current?.();
    const startX = e.clientX;
    const startWidth = width;
    // Window listeners rather than pointer capture — see FloatingResizeHandle.
    const onMove = (ev: PointerEvent) => {
      // The panel is pinned to the RIGHT edge, so dragging left (a smaller
      // clientX) makes it wider.
      onWidthChange(startWidth + (startX - ev.clientX));
    };
    const teardown = () => {
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", onUp);
      window.removeEventListener("pointercancel", onUp);
      teardownRef.current = null;
    };
    const onUp = () => teardown();
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", onUp);
    window.addEventListener("pointercancel", onUp);
    teardownRef.current = teardown;
  };
  return (
    <div
      role="separator"
      aria-orientation="vertical"
      aria-label="Resize the assistant panel"
      tabIndex={0}
      onPointerDown={onPointerDown}
      onKeyDown={(e) => {
        const step = e.shiftKey ? 64 : 16;
        if (e.key === "ArrowLeft") {
          e.preventDefault();
          onWidthChange(width + step);
        } else if (e.key === "ArrowRight") {
          e.preventDefault();
          onWidthChange(width - step);
        }
      }}
      title="Drag to resize"
      className="absolute left-0 top-0 bottom-0 z-10 w-1.5 -ml-0.5 cursor-col-resize hover:bg-accent/40 focus:bg-accent/60 focus:outline-none"
    />
  );
}
