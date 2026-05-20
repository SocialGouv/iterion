import { useEffect, useRef } from "react";

import type { FirstClassBot } from "@/lib/whats-next/firstClassBots";
import type { WhatsNextMessage } from "@/lib/whats-next/messages";
import type { FormAnswer } from "@/lib/whats-next/questionForm";

import HumanChatTurn from "./HumanChatTurn";
import IssuesSummaryCard from "./IssuesSummaryCard";
import NodeBanner from "./NodeBanner";
import RoadmapCard from "./RoadmapCard";
import SurveyCard from "./SurveyCard";

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
  // When set, skip this message id while mapping — used by
  // WhatsNextView to lift the pending human turn out of the inline
  // transcript and into a fixed-footer slot below.
  excludeMessageId?: string;
}

export default function ChatTranscript({
  messages,
  bot,
  onHumanSubmit,
  busyMessageId = null,
  excludeMessageId,
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
  // footer toggles (excludeMessageId set ↔ unset). Without the second
  // dep the bottom panel growing/shrinking — e.g. AgentChatbox →
  // pending HumanChatTurn footer with a larger form — would shift the
  // visible region without firing a scrollIntoView, and the most
  // recent message slips below the fold.
  useEffect(() => {
    if (!atBottomRef.current) return;
    endRef.current?.scrollIntoView({ behavior: "smooth", block: "end" });
  }, [messages.length, excludeMessageId]);

  // ResizeObserver on the scroll container catches in-place height
  // changes that the deps array misses: the textarea growing as the
  // user types, the WizardForm expanding when "Other" is picked, etc.
  // Only fires the re-pin when the user is already at the bottom.
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

  const renderedMessages = excludeMessageId
    ? messages.filter((m) => m.id !== excludeMessageId)
    : messages;

  return (
    <div
      ref={scrollContainerRef}
      onScroll={handleScroll}
      className="flex-1 overflow-y-auto px-4 py-3 space-y-4"
    >
      {renderedMessages.map((m) => (
        <MessageRow
          key={m.id}
          message={m}
          bot={bot}
          onHumanSubmit={onHumanSubmit}
          busy={m.kind === "human-question" && busyMessageId === m.id}
        />
      ))}
      {renderedMessages.length === 0 && (
        <p className="text-[12px] text-fg-subtle italic">
          The session will start as soon as iterion finishes the first survey.
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
}: {
  message: WhatsNextMessage;
  bot?: FirstClassBot;
  onHumanSubmit?: Props["onHumanSubmit"];
  busy: boolean;
}) {
  switch (message.kind) {
    case "banner":
      return <NodeBanner message={message} />;
    case "human-question": {
      const form = bot?.nodeMap[message.nodeId]?.form;
      return (
        <HumanChatTurn
          message={message}
          form={form}
          onSubmit={
            onHumanSubmit
              ? (outcome) => onHumanSubmit(message.id, outcome)
              : undefined
          }
          busy={busy}
        />
      );
    }
    case "roadmap-card":
      return <RoadmapCard message={message} />;
    case "issues-summary":
      return <IssuesSummaryCard message={message} />;
    case "survey-card":
      return <SurveyCard message={message} />;
    case "session-closed":
      return <SessionClosedRow message={message} />;
    case "plan-handed-off":
      return <PlanHandedOffRow message={message} />;
  }
}

function SessionClosedRow({
  message,
}: {
  message: Extract<WhatsNextMessage, { kind: "session-closed" }>;
}) {
  // "finished" no longer means "plan handed off" — that's now the
  // dedicated PlanHandedOffRow milestone fired when emit_action lands.
  // A "finished" run reaches Done because the operator picked
  // action=done in the triage loop (or a bot with no triage loop
  // reached its terminal node). Either way: this is the actual
  // end-of-session marker.
  const label =
    message.reason === "finished"
      ? "Session ended."
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
      className={`text-[11px] text-center italic border-t border-border-subtle pt-3 ${cls}`}
    >
      {label}
    </div>
  );
}

function PlanHandedOffRow({
  message,
}: {
  message: Extract<WhatsNextMessage, { kind: "plan-handed-off" }>;
}) {
  const issueLabel =
    message.createdCount === 1 ? "1 issue" : `${message.createdCount} issues`;
  return (
    <div className="border-t border-success/40 pt-3 text-center">
      <div className="inline-flex items-center gap-2 rounded-full border border-success/40 bg-success-soft px-3 py-1 text-[12px] text-success-fg">
        <span aria-hidden="true">✓</span>
        <span>
          Plan handed off — {issueLabel} created on the board (in
          <code className="mx-1 px-1 rounded bg-bg-default/40">backlog</code>)
        </span>
      </div>
      {message.summary && (
        <div className="mt-1 text-[11px] italic text-fg-muted">
          {message.summary}
        </div>
      )}
    </div>
  );
}
