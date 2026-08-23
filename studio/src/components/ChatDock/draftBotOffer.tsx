// The assistant's offer to move the work to the editor, and the consent that
// answers it. Extracted from ChatDock so both halves are testable on their own —
// the click path was reported broken from a real session and had no test.

import { useEffect, useRef, useState } from "react";
import { ArrowRightIcon, Pencil2Icon } from "@radix-ui/react-icons";
import { Link, useLocation } from "wouter";

import { useDraftState } from "@/hooks/useDraftBot";

// What the "Open the editor" button says on the operator's behalf. The click
// is a CONSENT event — the assistant asked to move, the operator agreed — so it
// has to reach the run as an answer to the pending turn, not merely as a
// navigation. Parked at its human node, the run would otherwise sit there
// forever while the operator waited on a canvas nothing was going to fill.
//
// A fixed English sentinel rather than prose in the operator's language: the
// bot is taught this exact string (see the `design` posture in
// bots/copilot/main.bot), and one short line cannot drag a French conversation
// into English the way a paragraph would.
export const EDITOR_OPENED_CONFIRMATION = "Opened the editor — go ahead.";

/**
 * useEditorConsent turns the click into an answer to the paused turn.
 *
 * The composer stamps the page context at SEND time, so the answer waits for
 * the route to settle on the editor: sending from the page they just left
 * would tell the bot they are still there, and it would orient them twice.
 */
export function useEditorConsent(
  submitPending: (text: string) => Promise<unknown>,
): { accept: () => void } {
  const [pending, setPending] = useState(false);
  const [route] = useLocation();
  useEffect(() => {
    if (!pending || !route.startsWith("/editor")) return;
    setPending(false);
    void submitPending(EDITOR_OPENED_CONFIRMATION).catch(() => {});
  }, [pending, route, submitPending]);
  return { accept: () => setPending(true) };
}

// The assistant's one write-shaped affordance, and it writes nothing.
//
// TWO offers, in the order the operator asked for — settle WHERE the work
// happens, then do it there:
//
//   designing, nothing to show yet → "Open the editor". The venue first, so
//     that when the build lands it lands somewhere the operator is looking.
//   a draft in hand → "Open this draft in the editor".
//
// A link rather than an automatic redirect, deliberately. The assistant may
// propose a change of page; it may not perform one — a view that swaps itself
// under the operator steals the control the dock exists to keep (it is
// non-modal for the same reason).
//
// The one thing that happens without a click is the LOAD, and only once the
// operator is already on /editor: they consented to the venue by going there,
// and the alternative is asking a second time for a page they are on. The
// draft has been compiled by the run's own deterministic validator before it
// is offered at all.
//
// The INVITATION is deliberately not rendered here. It is the bot's last
// sentence (see the `design` posture in bots/copilot/main.bot), so it lands in
// the operator's own language and in the assistant's voice — a static caption
// underneath read as a disclaimer bolted to the message rather than part of it.
// This component contributes the button; the bot contributes the offer.
export function DraftBotOffer({
  runId,
  revision,
  onOpenEditor,
}: {
  runId: string | null;
  revision: number;
  // Called when the operator accepts the move. Only for the venue offer — a
  // finished draft needs no permission to be looked at.
  onOpenEditor: () => void;
}) {
  const { source, designing } = useDraftState(runId, revision);
  const [location, setLocation] = useLocation();
  const onEditor = location.startsWith("/editor");
  const hasDraft = !!source;

  // Auto-load, once per draft. The ref is what keeps this from fighting the
  // operator: if they close the tab or navigate within the editor, we do not
  // re-open it behind them.
  const loadedRef = useRef<string | null>(null);
  useEffect(() => {
    if (!runId || !hasDraft || !onEditor) return;
    if (loadedRef.current === runId) return;
    loadedRef.current = runId;
    // `replace` — this is not a place the operator navigated to, so it must
    // not cost them a Back press to leave.
    setLocation(`/editor?draft=${encodeURIComponent(runId)}`, { replace: true });
  }, [runId, hasDraft, onEditor, setLocation]);

  if (!runId || (!hasDraft && !designing)) return null;
  // Already on the editor with the draft loaded: the canvas IS the answer, so
  // a button pointing at what is on screen would be noise.
  if (hasDraft && onEditor) return null;

  const href = hasDraft
    ? `/editor?draft=${encodeURIComponent(runId)}`
    : "/editor";
  const label = hasDraft ? "Open this draft in the editor" : "Open the editor";

  return (
    <div className="mt-3 border-t border-border-subtle pt-2.5">
      <Link
        href={href}
        onClick={() => {
          if (!hasDraft) onOpenEditor();
        }}
        className="inline-flex w-full items-center justify-center gap-2 rounded-md bg-accent px-3 py-2 text-label font-medium text-accent-contrast hover:brightness-110 focus:outline-none focus:ring-2 focus:ring-accent-emphasis"
      >
        <Pencil2Icon className="h-4 w-4 shrink-0" aria-hidden="true" />
        {label}
        <ArrowRightIcon className="h-4 w-4 shrink-0" aria-hidden="true" />
      </Link>
    </div>
  );
}

