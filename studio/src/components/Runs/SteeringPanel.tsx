// The run console's STEERING panel: text typed here is queued into the
// live agent's inbox and picked up at its next turn. Nothing replies —
// that is the shell-level assistant dock's job (see components/ChatDock).
//
// Steering is intentionally a permanent part of the run console's right
// dock. It has no floating, minimise, or closed presentation: leaving the
// run is how the operator leaves this run-scoped conversation.
import AgentChatboxInline from "@/components/shared/AgentChatboxInline";
import { ChatDockPanel } from "@/components/ChatDock/ChatDockShell";
import { STEERING_HINT, STEERING_TITLE } from "@/lib/chatDock/labels";

import RunConversationView from "./conversation/RunConversationView";

export function SteeringPanel({
  runId,
  inputDisabled,
}: {
  runId: string;
  inputDisabled: boolean;
}) {
  return (
    <ChatDockPanel title={STEERING_TITLE} titleHint={STEERING_HINT}>
      <div className="flex-1 min-h-0 overflow-hidden">
        <RunConversationView runId={runId} />
      </div>
      {!inputDisabled && (
        <div className="shrink-0 border-t border-border-default bg-surface-0 px-3 py-2">
          <AgentChatboxInline runId={runId} compact={false} />
        </div>
      )}
    </ChatDockPanel>
  );
}
