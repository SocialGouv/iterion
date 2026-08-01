// Deliberately NOT built on ui/Dialog (Radix): this is a NON-modal
// floating panel — the operator keeps interacting with the run view
// behind it (no scrim, no focus trap, page stays live). Radix Dialog is
// modal by design, so the hand-rolled shell here is intentional.
import { useEffect, useRef, useState } from "react";
import {
  ChatBubbleIcon,
  MinusIcon,
  PinRightIcon,
} from "@radix-ui/react-icons";

import AgentChatboxInline from "@/components/shared/AgentChatboxInline";
import { IconButton } from "@/components/ui";
import RunConversationView from "./conversation/RunConversationView";
import { openedDock, type DockState } from "@/lib/chatDock/dockState";
import { useRunChatMessages } from "@/lib/runChat/useRunChatMessages";
import { useRunStore } from "@/store/run";
import type { RunStatus } from "@/api/runs";

// The dock vocabulary now lives in @/lib/chatDock/dockState — it is
// shared with the shell-level assistant dock. Kept as an alias so the
// run console's existing `ChatDock` imports keep resolving.
export type ChatDock = DockState;

interface Props {
  runId: string;
  dock: ChatDock;
  onDockChange: (next: ChatDock) => void;
  inputDisabled: boolean;
}

// Auto-expand idempotency: the ref in useAutoExpandOnPause remembers
// the last paused status we reacted to, so closing the bubble while
// the run stays paused does NOT re-open it. A fresh pause (after a
// resume) re-arms the trigger.
export default function FloatingChatPanel({
  runId,
  dock,
  onDockChange,
  inputDisabled,
}: Props) {
  const status = useRunStore((s) => s.snapshot?.run.status);
  useAutoExpandOnPause(status, dock, onDockChange);

  if (dock === "docked-right") {
    // RunView mounts ChatPanelContent inline; nothing to render here.
    return null;
  }
  if (dock === "closed") {
    return (
      <ClosedBubble
        runId={runId}
        status={status}
        onOpen={() => onDockChange(openedDock())}
      />
    );
  }
  return (
    <FloatingDialogShell
      label="Conversation"
      onClose={() => onDockChange("closed")}
    >
      <ChatPanelChrome
        onDockRight={() => onDockChange("docked-right")}
        onClose={() => onDockChange("closed")}
      />
      <ChatPanelBody runId={runId} inputDisabled={inputDisabled} compact />
    </FloatingDialogShell>
  );
}

// Non-blocking floating dialog: labelled, keyboard-reachable, and
// dismissable via Escape. Focus moves into the panel on mount so a
// keyboard user can immediately Tab through the chrome + body without
// reaching the page background first. Intentionally NOT a focus trap —
// the page underneath remains interactive (this is a docked helper,
// not a modal).
function FloatingDialogShell({
  label,
  onClose,
  children,
}: {
  label: string;
  onClose: () => void;
  children: React.ReactNode;
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
      className="fixed bottom-4 right-4 z-[var(--z-toast)] flex flex-col rounded-md border border-border-default bg-surface-1 shadow-[var(--shadow-popover)] resize overflow-hidden focus:outline-none"
      style={{ width: 420, height: 520, minWidth: 320, minHeight: 280 }}
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

// Subscribes to the message stream only when actually rendered (i.e.
// dock === "closed"), so the bubble doesn't pay the fold cost for
// other dock states.
function ClosedBubble({
  runId,
  status,
  onOpen,
}: {
  runId: string;
  status: RunStatus | undefined;
  onOpen: () => void;
}) {
  const messages = useRunChatMessages(runId);
  // Baseline frozen at mount: anything that arrives after this counts
  // as unread until the bubble opens (the component unmounts on open
  // and re-mounts with a fresh baseline on close).
  const [baseline] = useState(messages.length);
  const unread = Math.max(0, messages.length - baseline);
  const needsAttention =
    status === "paused_waiting_human" || status === "paused_operator";
  return (
    <button
      type="button"
      onClick={onOpen}
      className={`fixed bottom-4 right-4 z-[var(--z-toast)] h-12 w-12 rounded-full border shadow-[var(--shadow-lg)] flex items-center justify-center transition-transform hover:scale-105 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent ${needsAttention ? "border-warning bg-warning-soft animate-pulse" : "border-border-default bg-surface-2 text-fg-default"}`}
      aria-label={`Open conversation${unread > 0 ? ` (${unread} new)` : ""}`}
      title={
        needsAttention
          ? "Run waiting for input — click to open conversation"
          : "Open conversation"
      }
    >
      <ChatBubbleIcon className="h-5 w-5" />
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

// Mounted by RunView inside a resizable Panel when dock is "docked-right".
// Shares chrome with the floating mode but swaps "Dock right" for
// "Undock" (back to floating).
export function ChatPanelContent({
  runId,
  inputDisabled,
  onUndock,
  onClose,
}: {
  runId: string;
  inputDisabled: boolean;
  onUndock: () => void;
  onClose: () => void;
}) {
  return (
    <div className="h-full w-full flex flex-col bg-surface-1 border-l border-border-default">
      <ChatPanelChrome onUndock={onUndock} onClose={onClose} />
      <ChatPanelBody runId={runId} inputDisabled={inputDisabled} compact={false} />
    </div>
  );
}

function ChatPanelChrome({
  onDockRight,
  onUndock,
  onClose,
}: {
  onDockRight?: () => void;
  onUndock?: () => void;
  onClose: () => void;
}) {
  return (
    <div className="shrink-0 flex items-center justify-between px-3 py-1 border-b border-border-default bg-surface-2">
      <span className="text-micro font-medium text-fg-default uppercase tracking-wide">
        Conversation
      </span>
      <div className="flex items-center gap-0.5">
        {onDockRight && (
          <IconButton
            label="Dock conversation to right side"
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
          label="Minimise conversation"
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

function ChatPanelBody({
  runId,
  inputDisabled,
  compact,
}: {
  runId: string;
  inputDisabled: boolean;
  compact: boolean;
}) {
  return (
    <>
      <div className="flex-1 min-h-0 overflow-hidden">
        <RunConversationView runId={runId} />
      </div>
      {!inputDisabled && (
        <div className="shrink-0 border-t border-border-default bg-surface-0 px-3 py-2">
          <AgentChatboxInline runId={runId} compact={compact} />
        </div>
      )}
    </>
  );
}

function useAutoExpandOnPause(
  status: RunStatus | undefined,
  dock: ChatDock,
  onDockChange: (next: ChatDock) => void,
) {
  const lastReactedRef = useRef<RunStatus | null>(null);
  // Latest dock + onDockChange held in refs so the effect only depends
  // on `status` — closing the bubble while paused must NOT re-trigger
  // the effect, but reading them through closure would otherwise stale.
  const dockRef = useRef(dock);
  const onDockChangeRef = useRef(onDockChange);
  dockRef.current = dock;
  onDockChangeRef.current = onDockChange;
  useEffect(() => {
    if (!status) return;
    const isPaused =
      status === "paused_waiting_human" || status === "paused_operator";
    if (!isPaused) {
      lastReactedRef.current = null;
      return;
    }
    if (lastReactedRef.current === status) return;
    lastReactedRef.current = status;
    if (dockRef.current === "closed") {
      onDockChangeRef.current(openedDock());
    }
  }, [status]);
}
