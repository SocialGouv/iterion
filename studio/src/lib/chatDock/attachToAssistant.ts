// Attaching a reference to the assistant's composer WITHOUT a drag.
//
// dragReference.ts owns the gesture half: a source publishes a typed
// reference on a DataTransfer, the dock's drop target reads it back. The
// gesture works only because both ends of it live in the same panel — the
// general case does not have that luxury. The sources (a run-list row, a
// bots-gallery card, a node header) render far from the ChatDock whose
// composer owns the attached list, so the keyboard/single-pointer control
// #2770 adds cannot receive the dock's setAttached as a prop.
//
// This module is the channel between those ends, and the ONE attach action
// both paths call: the drop handler routes through it, and the future
// control calls it directly. A CustomEvent is the channel lib/chatDock
// already uses for cross-tree notices (missionGrant.ts, assistantActions.ts);
// the dock subscribes for as long as it is mounted and applies the reference
// to the message being composed.
//
// What it deliberately does NOT do is open the dock. The drop path springs
// the shell open during dragover (ChatDockShell's openOnReferenceDrag),
// before the drop lands; what an attach arriving at a closed dock should
// surface is part of #2770's placement decision, not of this channel.

import type { TypedReference } from "./routeReference";

export const ASSISTANT_ATTACH_EVENT = "iterion:assistant-attach-reference";

/** The ONE way a reference is attached to the assistant's composer, whatever
 *  the gesture: a drop onto the dock, or the keyboard/single-pointer control
 *  #2770 adds next to each drag grip. Callers mint through `mintReference`
 *  (or carry what `readReferenceDrop` minted), so every path produces the
 *  same `<kind>/<id>` wire form. */
export function attachReferenceToAssistant(reference: TypedReference): void {
  if (typeof window === "undefined") return;
  window.dispatchEvent(
    new CustomEvent<TypedReference>(ASSISTANT_ATTACH_EVENT, {
      detail: reference,
    }),
  );
}

/** Subscribe to attach requests; returns the unsubscribe. The ChatDock is
 *  the subscriber — it owns the attached list. */
export function onAssistantAttachRequest(
  listener: (reference: TypedReference) => void,
): () => void {
  const handler = (event: Event) => {
    const detail = (event as CustomEvent<unknown>).detail;
    // A CustomEvent is forgeable by any script on the page, so only the
    // shape the vocabulary produces is applied — the same posture
    // readReferenceDrop takes with a drop payload.
    if (!isTypedReferenceShape(detail)) return;
    listener(detail);
  };
  window.addEventListener(ASSISTANT_ATTACH_EVENT, handler);
  return () => window.removeEventListener(ASSISTANT_ATTACH_EVENT, handler);
}

function isTypedReferenceShape(value: unknown): value is TypedReference {
  if (!value || typeof value !== "object") return false;
  const candidate = value as Record<string, unknown>;
  return (
    typeof candidate.kind === "string" &&
    typeof candidate.ref === "string" &&
    typeof candidate.label === "string"
  );
}
