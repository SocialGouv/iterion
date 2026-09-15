// The operator's own turn in a transcript.
//
// One shape, used by BOTH paths that render what the operator said: the
// opening message (a launch var, folded in as a user-message) and every reply
// after it (the answer on a human turn). They used to render differently — the
// first as a right-aligned tinted card with a status chip, the rest as a "You"
// bubble — so a conversation looked like it changed format after its first
// line. Same person, same shape.
//
// It mirrors the assistant's bubble deliberately: a label chip, then the text.
// A conversation reads as an exchange between two speakers, which only works
// if both are drawn the same way.

import type { ReactNode } from "react";

import MarkdownText from "@/components/Runs/conversation/MarkdownText";
import { withoutPageContext } from "@/lib/chatDock/contextMessage";

export function OperatorBubble({
  text,
  empty,
  badge,
}: {
  text: string;
  /** Rendered instead of the text when there is none (an approve/reject). */
  empty?: ReactNode;
  /** Only for a state worth naming — queued, failed. A settled message is
   *  just a message, and a chip on every one of them is noise. */
  badge?: ReactNode;
}) {
  // The context lines are protocol, not speech: stripped for display only,
  // never from what was sent.
  const body = withoutPageContext(text).trim();
  return (
    <div className="flex items-start gap-2 ml-6">
      <span
        className="mt-1 px-2 py-0.5 rounded-full bg-surface-3 text-fg-default text-caption font-bold flex items-center justify-center shrink-0"
        aria-hidden="true"
      >
        You
      </span>
      <div className="flex-1 rounded-lg bg-surface-1 border border-border-subtle px-3 py-2 text-label text-fg-default">
        {body ? (
          <MarkdownText value={body} size="sm" />
        ) : (
          <span className="italic text-fg-subtle">{empty ?? "(empty reply)"}</span>
        )}
        {badge ? <div className="mt-1 flex justify-end">{badge}</div> : null}
      </div>
    </div>
  );
}
