// The run console's STEERING panel: text typed here is queued into the
// live agent's inbox and picked up at its next turn. Nothing replies —
// that is the shell-level assistant dock's job (see components/ChatDock).
// The two are named apart on purpose; see @/lib/chatDock/labels.
//
// Presentation (the non-modal closed/floating/docked-right shell) is
// ChatDockShell's. This file is a caller of it: it owns the run-specific
// pieces only — the transcript, the queue composer, the unread count and
// the auto-expand-on-pause rule.
import { useCallback, useEffect, useRef, useState } from "react";
import { PaperPlaneIcon } from "@radix-ui/react-icons";

import AgentChatboxInline from "@/components/shared/AgentChatboxInline";
import { useAssistantFixedInsetPx } from "@/components/ChatDock/AssistantProvider";
import {
  ChatDockBubble,
  ChatDockChrome,
  ChatDockFloating,
  ChatDockPanel,
} from "@/components/ChatDock/ChatDockShell";
import RunConversationView from "./conversation/RunConversationView";
import { openedDock, type DockState } from "@/lib/chatDock/dockState";
import {
  STEERING_HINT,
  STEERING_LANE,
  STEERING_TITLE,
} from "@/lib/chatDock/labels";
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

  // Same opened-by-operator tracking the assistant's shell does. The
  // extracted ChatDockFloating defaults focusOnMount to false, and this
  // call site not passing it silently cost keyboard users the
  // focus-on-open the panel had before the extraction — they landed back
  // on the page background and had to tab in. Passing it unconditionally
  // is not the fix either: this dock's state is persisted too
  // (CHAT_DOCK_KEY) and remounts on every navigation to /runs/:id, so it
  // would steal focus on restore exactly like the assistant did.
  const openedByOperator = useRef(false);
  const changeDock = useCallback(
    (next: ChatDock) => {
      openedByOperator.current = true;
      onDockChange(next);
    },
    [onDockChange],
  );
  // The assistant is on this route too, and when docked it owns a fixed
  // column at the right edge. Lane 1 alone does not clear it — the
  // steering bubble would sit under it and take no clicks.
  // The steering panel is a PEER fixed surface, so it must clear the
  // assistant's band in every state it occupies one — floating included.
  // Using the layout reservation here left the bubble under a floating
  // assistant, unclickable, in the default closed configuration.
  const rightInset = useAssistantFixedInsetPx();

  if (dock === "docked-right") {
    // RunView mounts ChatPanelContent inline; nothing to render here.
    return null;
  }
  if (dock === "closed") {
    return (
      <SteeringBubble
        runId={runId}
        status={status}
        rightInset={rightInset}
        onOpen={() => changeDock(openedDock())}
      />
    );
  }
  return (
    <ChatDockFloating
      label={STEERING_TITLE}
      lane={STEERING_LANE}
      rightInset={rightInset}
      focusOnMount={openedByOperator.current}
      onClose={() => changeDock("closed")}
    >
      <ChatDockChrome
        title={STEERING_TITLE}
        titleHint={STEERING_HINT}
        onDockRight={() => changeDock("docked-right")}
        onClose={() => changeDock("closed")}
      />
      <ChatPanelBody runId={runId} inputDisabled={inputDisabled} compact />
    </ChatDockFloating>
  );
}

// Subscribes to the message stream only when actually rendered (i.e.
// dock === "closed"), so the bubble doesn't pay the fold cost for
// other dock states. That subscription is why the bubble is a component
// here rather than an `unread` prop computed by the caller.
function SteeringBubble({
  runId,
  status,
  rightInset,
  onOpen,
}: {
  runId: string;
  status: RunStatus | undefined;
  rightInset: number;
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
    <ChatDockBubble
      label="Open run steering"
      title="Steer this run — queue a message into its live agent"
      attentionTitle="Run waiting for input — click to open steering"
      icon={<PaperPlaneIcon className="h-5 w-5" />}
      unread={unread}
      attention={needsAttention}
      lane={STEERING_LANE}
      rightInset={rightInset}
      onOpen={onOpen}
    />
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
    <ChatDockPanel
      title={STEERING_TITLE}
      titleHint={STEERING_HINT}
      onUndock={onUndock}
      onClose={onClose}
    >
      <ChatPanelBody runId={runId} inputDisabled={inputDisabled} compact={false} />
    </ChatDockPanel>
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
