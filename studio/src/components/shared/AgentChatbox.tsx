import AgentChatboxInline from "./AgentChatboxInline";

interface Props {
  runId: string;
  disabled?: boolean;
  maxVisible?: number;
}

// AgentChatbox is the legacy banner-style chatbox: a full-width strip
// with a top border and a centered max-w-3xl content column. Still
// used by WhatsNextView's flush-bottom layout.
//
// New surfaces (FloatingChatPanel in RunView) should render
// `AgentChatboxInline` directly without this chrome.
export default function AgentChatbox({
  runId,
  disabled = false,
  maxVisible = 5,
}: Props) {
  return (
    <div className="border-t border-border-subtle bg-surface-1">
      <div className="mx-auto max-w-3xl px-4 py-2">
        <AgentChatboxInline
          runId={runId}
          disabled={disabled}
          maxVisible={maxVisible}
        />
      </div>
    </div>
  );
}
