import type { SessionClosedMessage } from "@/lib/runChat/types";

interface Props {
  message: SessionClosedMessage;
}

// SessionClosedCard marks the end of the chat transcript. Three
// shades — finished (muted), failed (danger), cancelled (warning).
// Patterned after the same row in
// `studio/src/components/WhatsNext/ChatTranscript.tsx`.
export default function SessionClosedCard({ message }: Props) {
  const label =
    message.reason === "finished"
      ? "Run finished."
      : message.reason === "failed"
        ? "Run failed."
        : "Run cancelled.";
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
