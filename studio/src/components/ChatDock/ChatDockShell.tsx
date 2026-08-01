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

import { useEffect, useRef } from "react";
import type { ReactNode } from "react";
import {
  ChatBubbleIcon,
  MinusIcon,
  PinRightIcon,
} from "@radix-ui/react-icons";

import { IconButton } from "@/components/ui";
import { openedDock, type DockState } from "@/lib/chatDock/dockState";

// Two docks can be on the page at once (the assistant everywhere, the
// run console's steering panel on /runs/:id). A lane is the bottom-right
// slot each one owns so they never land on top of each other. Lane 0 is
// the canonical corner — it belongs to the surface that is present on
// EVERY route, so its position never moves under the operator.
export type DockLane = 0 | 1;

// Tailwind needs literal class names, so the lanes are a lookup rather
// than an interpolated offset.
const BUBBLE_LANE: Record<DockLane, string> = {
  0: "right-4",
  1: "right-20",
};
const FLOATING_LANE: Record<DockLane, string> = {
  0: "right-4",
  1: "right-[28rem]",
};

const FLOATING_WIDTH_PX = 420;
const FLOATING_HEIGHT_PX = 520;

export interface ChatDockShellProps {
  dock: DockState;
  onDockChange: (next: DockState) => void;
  // Chrome title + accessible name. This is what makes an assistant
  // dock distinguishable from a steering panel at a glance.
  title: string;
  // Longer explanation of what typing here does, shown as the chrome's
  // tooltip. Optional but strongly encouraged when two docks coexist.
  titleHint?: string;
  lane?: DockLane;
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
  // "self": the shell renders the docked-right column itself (the
  // shell-level assistant, which owns a real layout slot).
  // "host": the shell renders nothing and the host lays the panel out
  // via ChatDockPanel (the run console, whose SideDock owns the column).
  dockedRightMode?: "self" | "host";
  children: ReactNode;
}

// ChatDockShell dispatches on the dock state. Callers keep the state
// (persisted per user) and hand it back down.
export function ChatDockShell({
  dock,
  onDockChange,
  title,
  titleHint,
  lane = 0,
  bubbleIcon,
  bubbleLabel,
  bubbleTitle,
  unread = 0,
  attention = false,
  attentionTitle,
  dockedRightMode = "host",
  children,
}: ChatDockShellProps) {
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
        onOpen={() => onDockChange(openedDock())}
      />
    );
  }
  if (dock === "docked-right") {
    if (dockedRightMode === "host") return null;
    return (
      <ChatDockPanel
        title={title}
        titleHint={titleHint}
        onUndock={() => onDockChange("floating")}
        onClose={() => onDockChange("closed")}
      >
        {children}
      </ChatDockPanel>
    );
  }
  return (
    <ChatDockFloating
      label={title}
      lane={lane}
      onClose={() => onDockChange("closed")}
    >
      <ChatDockChrome
        title={title}
        titleHint={titleHint}
        onDockRight={() => onDockChange("docked-right")}
        onClose={() => onDockChange("closed")}
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
  onClose,
  children,
}: {
  label: string;
  lane?: DockLane;
  onClose: () => void;
  children: ReactNode;
}) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
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
  }, []);
  return (
    <div
      ref={ref}
      tabIndex={-1}
      className={`fixed bottom-4 ${FLOATING_LANE[lane]} z-[var(--z-toast)] flex flex-col rounded-md border border-border-default bg-surface-1 shadow-[var(--shadow-popover)] resize overflow-hidden focus:outline-none`}
      style={{
        width: FLOATING_WIDTH_PX,
        height: FLOATING_HEIGHT_PX,
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
      {children}
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
  onOpen,
}: {
  label: string;
  title: string;
  icon?: ReactNode;
  unread?: number;
  attention?: boolean;
  attentionTitle?: string;
  lane?: DockLane;
  onOpen: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onOpen}
      className={`fixed bottom-4 ${BUBBLE_LANE[lane]} z-[var(--z-toast)] h-12 w-12 rounded-full border shadow-[var(--shadow-lg)] flex items-center justify-center transition-transform hover:scale-105 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent ${attention ? "border-warning bg-warning-soft animate-pulse" : "border-border-default bg-surface-2 text-fg-default"}`}
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
  onUndock,
  onClose,
  children,
}: {
  title: string;
  titleHint?: string;
  onUndock: () => void;
  onClose: () => void;
  children: ReactNode;
}) {
  return (
    <div className="h-full w-full flex flex-col bg-surface-1 border-l border-border-default">
      <ChatDockChrome title={title} titleHint={titleHint} onUndock={onUndock} onClose={onClose} />
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
  onDockRight,
  onUndock,
  onClose,
}: {
  title: string;
  titleHint?: string;
  onDockRight?: () => void;
  onUndock?: () => void;
  onClose: () => void;
}) {
  return (
    <div className="shrink-0 flex items-center justify-between px-3 py-1 border-b border-border-default bg-surface-2">
      <span
        className="text-micro font-medium text-fg-default uppercase tracking-wide"
        title={titleHint}
      >
        {title}
      </span>
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
