import { useRunStore } from "@/store/run";
import { useRunChatMessages } from "@/lib/runChat/useRunChatMessages";
import { useScrollPin } from "@/lib/runChat/useScrollPin";
import type { RunChatMessage } from "@/lib/runChat/types";

import BannerCard from "./BannerCard";
import HumanQuestionCard from "./HumanQuestionCard";
import NodeOutputCard from "./NodeOutputCard";
import SessionClosedCard from "./SessionClosedCard";

interface Props {
  runId: string;
}

// RunConversationView renders the generic chat-style transcript:
// banners for agent/judge/compute steps, markdown cards for their
// outputs, inline form on the pending human pause turn, terminal
// session-closed marker.
export default function RunConversationView({ runId }: Props) {
  const messages = useRunChatMessages(runId);
  const pending = useRunStore((s) => s.pendingHumanInput);
  const runStatus = useRunStore((s) => s.snapshot?.run.status);

  const { scrollRef, endRef, onScroll } = useScrollPin([messages.length]);

  const activeHumanNodeId =
    runStatus === "paused_waiting_human" ? pending?.node_id ?? null : null;

  return (
    <div
      ref={scrollRef}
      onScroll={onScroll}
      className="h-full w-full overflow-y-auto px-4 py-3 space-y-4 bg-surface-0"
    >
      {messages.length === 0 ? (
        <p className="text-[12px] text-fg-subtle italic">
          Waiting for the run to start…
        </p>
      ) : (
        messages.map((m) => (
          <MessageRow
            key={m.id}
            runId={runId}
            message={m}
            activeHumanNodeId={activeHumanNodeId}
          />
        ))
      )}
      <div ref={endRef} />
    </div>
  );
}

function MessageRow({
  runId,
  message,
  activeHumanNodeId,
}: {
  runId: string;
  message: RunChatMessage;
  activeHumanNodeId: string | null;
}) {
  switch (message.kind) {
    case "banner":
      return <BannerCard message={message} />;
    case "node-output":
      return <NodeOutputCard message={message} />;
    case "human-question":
      return (
        <HumanQuestionCard
          runId={runId}
          message={message}
          isActive={activeHumanNodeId === message.nodeId}
        />
      );
    case "session-closed":
      return <SessionClosedCard message={message} />;
    case "extension":
      // Bot-specific resolvers (whats-next) lift these into typed
      // cards before they reach this renderer. Defensive fallback —
      // an unprocessed extension here just renders blank rather
      // than crashing.
      return null;
  }
}
