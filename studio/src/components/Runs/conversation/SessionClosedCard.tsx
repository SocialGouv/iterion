import type { SessionClosedMessage } from "@/lib/runChat/types";

interface Props {
  message: SessionClosedMessage;
}

// SessionClosedCard marks the end of the chat transcript. Three
// shades — finished (muted), failed (danger), cancelled (warning).
// Patterned after the same row in
// `studio/src/components/WhatsNext/ChatTranscript.tsx`.
//
// Copy is tuned to give the operator the obvious next step rather
// than a bare status word: open the report on success, follow the
// hint banner on failure, resume from the header on cancel.
export default function SessionClosedCard({ message }: Props) {
  const label =
    message.reason === "finished"
      ? "Run finished. Pick a node above to see its output, or open the Report tab."
      : message.reason === "failed"
        ? "Run failed. Check the Hint banner above the timeline for the recommended next step."
        : "Run cancelled. Use Resume in the header to pick up from the last checkpoint.";
  const cls =
    message.reason === "finished"
      ? "text-fg-muted"
      : message.reason === "failed"
        ? "text-danger-fg"
        : "text-warning-fg";
  return (
    <div
      className={`text-[11px] text-center italic border-t border-border-subtle pt-2 ${cls}`}
      role="status"
    >
      {label}
    </div>
  );
}
