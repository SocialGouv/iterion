import { memo } from "react";

import type { AssistantTextMessage } from "@/lib/runChat/types";

import MarkdownText from "./MarkdownText";

interface Props {
  message: AssistantTextMessage;
}

// AssistantTextCard renders the agent's mid-turn narration (an
// `assistant_text` event) as a speech bubble, so a long working step
// reads as the agent talking through what it does instead of a frozen
// banner. Deliberately quieter than NodeOutputCard — narration is
// commentary, the structured output card remains the result.
function AssistantTextCardImpl({ message }: Props) {
  if (!message.text) return null;
  return (
    <div className="ml-5 mt-1 max-w-[92%] rounded-md border border-border-subtle/60 bg-surface-1/60 px-3 py-2">
      <MarkdownText value={message.text} />
    </div>
  );
}

const AssistantTextCard = memo(AssistantTextCardImpl);
export default AssistantTextCard;
