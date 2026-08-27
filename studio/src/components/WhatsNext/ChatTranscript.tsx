import { useEffect, useMemo, useRef, type ReactNode } from "react";

import type { FirstClassBot } from "@/lib/whats-next/firstClassBots";
import type { WhatsNextMessage } from "@/lib/whats-next/messages";
import type { FormAnswer } from "@/lib/whats-next/questionForm";

import MarkdownText from "@/components/Runs/conversation/MarkdownText";

import HumanChatTurn from "./HumanChatTurn";
import { OperatorBubble } from "./OperatorBubble";
import NodeBanner from "./NodeBanner";

interface Props {
  messages: WhatsNextMessage[];
  // The active bot — used to look up nodeMap form specs for human
  // turns and to enrich rendering with bot-specific affordances.
  bot?: FirstClassBot;
  // Called when the user submits a reply to a pending human-question
  // message. The `messageId` is the id of the human-question message
  // being answered, so callers can route the submit back to the
  // matching interaction. `outcome.formAnswer` is populated when the
  // node has a rich form spec; otherwise the parent falls back to
  // text + approved.
  onHumanSubmit?: (
    messageId: string,
    outcome: {
      text: string;
      approved?: boolean;
      formAnswer?: FormAnswer;
    },
  ) => void;
  // True while a submit is in-flight; disables inputs on the pending
  // human-question turn.
  busyMessageId?: string | null;
  // When set, this pending human turn's INPUT is owned by the unified
  // composer below the transcript: the turn still renders inline (the
  // assistant bubble — Nexie's reply — must stay in the flow), but
  // its own textarea/buttons are suppressed.
  composerHandlesId?: string;
  // Rendered inside the LAST turn's assistant bubble. For something that turn
  // produced — an offer the assistant just made. Below the bubble it reads as
  // chrome and gets missed; inside it, it reads as part of what was said.
  bubbleSlot?: ReactNode;
}

export default function ChatTranscript({
  messages,
  bot,
  onHumanSubmit,
  busyMessageId = null,
  composerHandlesId,
  bubbleSlot,
}: Props) {
  const endRef = useRef<HTMLDivElement | null>(null);
  const scrollContainerRef = useRef<HTMLDivElement | null>(null);
  // Track whether the user is scrolled near the bottom. We only
  // auto-scroll on new messages when they are — otherwise reading older
  // turns gets yanked down every time a banner update arrives, which
  // is the bug F-TS-7 was about.
  const atBottomRef = useRef(true);

  const handleScroll = () => {
    const el = scrollContainerRef.current;
    if (!el) return;
    // Treat "within 48px of the bottom" as still pinned — small enough
    // that brief overshoot during smooth-scroll doesn't unpin, large
    // enough that the user only has to nudge up once to escape.
    const distanceFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight;
    atBottomRef.current = distanceFromBottom < 48;
  };

  // Re-pin to the bottom when the message count grows OR when the
  // composer takes/releases a pending turn (the footer height shifts).
  useEffect(() => {
    if (!atBottomRef.current) return;
    endRef.current?.scrollIntoView({ behavior: "smooth", block: "end" });
  }, [messages.length, composerHandlesId]);

  // ResizeObserver on the scroll container catches in-place height
  // changes that the deps array misses: the textarea growing as the
  // user types, a bubble expanding, etc. Only fires the re-pin when
  // the user is already at the bottom.
  useEffect(() => {
    const el = scrollContainerRef.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const obs = new ResizeObserver(() => {
      if (!atBottomRef.current) return;
      endRef.current?.scrollIntoView({ behavior: "auto", block: "end" });
    });
    obs.observe(el);
    return () => obs.disconnect();
  }, []);

  // An ask_user pause's prompt IS the agent's final narration text, so
  // the transcript would show the same paragraph twice: once as the
  // narration row, once as Nexie's question bubble. Keep the bubble
  // (it carries the answer affordance) and hide the narration twin.
  // Scoped to ask_user questions (the only kind whose prompt is the
  // narration itself) and memoized — this runs on every scroll frame
  // otherwise.
  const visible = useMemo(() => {
    const hiddenIds = new Set<string>();
    messages.forEach((m, i) => {
      if (m.kind !== "human-question" || !m.prompt) return;
      if (!m.questions || !("ask_user_response" in m.questions)) return;
      for (let j = i - 1; j >= 0; j--) {
        const prev = messages[j];
        if (!prev) break;
        if (prev.kind === "assistant-text") {
          if (prev.text.trim() === m.prompt.trim()) hiddenIds.add(prev.id);
          break;
        }
        if (prev.kind === "human-question" || prev.kind === "user-message") break;
      }
    });
    if (hiddenIds.size === 0) return messages;
    return messages.filter((m) => !hiddenIds.has(m.id));
  }, [messages]);

  // bubbleSlot only renders on a human-question row. Anchoring it to the
  // last visible message dropped the CTA whenever a banner or narration
  // landed after the question, or the moment the operator answered
  // (AnsweredTurn used to ignore the slot). Last hostable row, answered
  // or not (R98e430).
  const lastHostableId = [...visible]
    .reverse()
    .find((m) => m.kind === "human-question")?.id;

  return (
    <div
      ref={scrollContainerRef}
      onScroll={handleScroll}
      className="flex-1 overflow-y-auto px-4 py-3 space-y-4"
    >
      {visible.map((m) => (
        <MessageRow
          key={m.id}
          message={m}
          bubbleSlot={m.id === lastHostableId ? bubbleSlot : undefined}
          bot={bot}
          onHumanSubmit={onHumanSubmit}
          busy={m.kind === "human-question" && busyMessageId === m.id}
          inputHidden={m.id === composerHandlesId}
        />
      ))}
      {messages.length === 0 && (
        <p className="text-body text-fg-subtle italic">
          The conversation will start as soon as the first turn begins.
        </p>
      )}
      <div ref={endRef} />
    </div>
  );
}

function MessageRow({
  message,
  bot,
  onHumanSubmit,
  busy,
  inputHidden,
  bubbleSlot,
}: {
  message: WhatsNextMessage;
  bot?: FirstClassBot;
  onHumanSubmit?: Props["onHumanSubmit"];
  busy: boolean;
  inputHidden: boolean;
  bubbleSlot?: ReactNode;
}) {
  switch (message.kind) {
    case "banner":
      return <NodeBanner message={message} />;
    case "human-question": {
      const form = bot?.nodeMap[message.nodeId]?.form;
      return (
        <HumanChatTurn
          message={message}
          persona={bot?.label ?? ""}
          bubbleSlot={bubbleSlot}
          form={form}
          inputHidden={inputHidden}
          onSubmit={
            onHumanSubmit
              ? (outcome) => onHumanSubmit(message.id, outcome)
              : undefined
          }
          busy={busy}
        />
      );
    }
    case "session-closed":
      return <SessionClosedRow message={message} />;
    case "user-message":
      return <UserMessageRow message={message} />;
    case "assistant-text":
      return <NarrationRow message={message} />;
  }
}

// NarrationRow renders the agent's mid-turn narration (assistant_text
// events) as Nexie's speech bubble — left-aligned, quieter than a
// structured card, markdown-capable.
function NarrationRow({
  message,
}: {
  message: Extract<WhatsNextMessage, { kind: "assistant-text" }>;
}) {
  if (!message.text) return null;
  return (
    <div className="max-w-[92%] rounded-md border border-border-subtle/60 bg-surface-1/60 px-3 py-2">
      <MarkdownText value={message.text} />
    </div>
  );
}

// UserMessageRow renders an operator-queued chat message inline in
// the transcript, anchored to the chronological position of the
// originating `user_message_queued` event. The status pill makes the
// lifecycle explicit — operators saw "delivered" and assumed the bot
// had acted on the request, when in fact it only meant "now in the
// agent's conversation context". The labels distinguish "in agent's
// context" from "agent read it" so the contract is clear.
function UserMessageRow({
  message,
}: {
  message: Extract<WhatsNextMessage, { kind: "user-message" }>;
}) {
  const meta = userStatusMeta(message.status);
  return (
    <OperatorBubble
      text={message.text}
      // A settled message is just a message. The chip only earns its place
      // while the message is still in flight or has failed — which is exactly
      // when the operator needs to know it has not landed.
      badge={
        meta.transient ? (
          <span
            className={`inline-flex items-center gap-1 rounded-full px-1.5 py-0.5 text-caption font-medium ${meta.tone}`}
            title={meta.hint}
          >
            {meta.label}
          </span>
        ) : null
      }
    />
  );
}

function userStatusMeta(
  status: Extract<WhatsNextMessage, { kind: "user-message" }>["status"],
): { label: string; tone: string; hint: string; transient: boolean } {
  switch (status) {
    case "queued":
      return {
        label: "Queued",
        transient: true,
        tone: "bg-warning-soft text-warning-fg",
        hint: "Waiting for the agent's next turn. The agent has not seen it yet.",
      };
    case "delivered":
      return {
        label: "In agent's context",
        transient: true,
        tone: "bg-info-soft text-info-fg",
        hint: "Injected into the agent's conversation. The next LLM turn will read it — but the agent has not processed it yet.",
      };
    case "consumed":
      return {
        label: "Read by agent",
        transient: false,
        tone: "bg-success-soft text-success-fg",
        hint: "The agent finished a turn that included this message. Note: this does not mean the agent acted on it — only that it had the chance to.",
      };
    case "cancelled":
      return {
        label: "Cancelled",
        transient: true,
        tone: "bg-surface-2 text-fg-muted",
        hint: "Removed before delivery.",
      };
    default: {
      // Compile-time exhaustiveness: any new UserMessageStatus added
      // upstream surfaces here as a type error. The runtime fallback
      // keeps the row renderable instead of crashing on a missing
      // mapping.
      const _exhaustive: never = status;
      return {
        label: String(_exhaustive),
        tone: "bg-surface-2 text-fg-muted",
        hint: "",
        transient: true,
      };
    }
  }
}

function SessionClosedRow({
  message,
}: {
  message: Extract<WhatsNextMessage, { kind: "session-closed" }>;
}) {
  // A "finished" run reaches Done because the operator EXPLICITLY
  // closed the session; standby keeps the run paused and reachable,
  // so it never lands here. The composer below stays live (it
  // re-seeds a fresh session), so frame this as a soft close, not a
  // dead end.
  const label =
    message.reason === "finished"
      ? "Session closed — send a message to start a fresh one."
      : message.reason === "failed"
        ? "Session failed."
        : "Session cancelled.";
  const cls =
    message.reason === "finished"
      ? "text-fg-muted"
      : message.reason === "failed"
        ? "text-danger-fg"
        : "text-warning-fg";
  return (
    <div
      className={`text-micro text-center italic border-t border-border-subtle pt-3 ${cls}`}
    >
      {label}
    </div>
  );
}
