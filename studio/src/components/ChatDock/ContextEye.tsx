// The quiet half of the page context: an eye in the composer row.
//
// It replaced a pinned strip that named the page above every conversation.
// The objection that retired it is sound — the operator can see what page
// they are on, so a permanent line repeating it buys nothing. But two
// things the strip carried were still worth keeping, and neither needs a
// line of its own:
//
//   the ANSWER — "what exactly is going out with my message?" It is a
//     question asked occasionally, not a fact needing continuous display,
//     so it lives in the tooltip and the accessible name.
//   the CONTROL — without it there is no way to say "not this page", and
//     a mechanism you cannot turn off is worse than one you cannot see.
//
// Sitting in the composer row is deliberate: the context rides the message
// being typed, so the control belongs beside the thing it affects, and the
// row already exists — this costs no vertical space at all.
//
// Renders nothing when the strip is speaking (see stripSpeaks): the
// dismissed and degraded cases already show a control, and two ways to
// dismiss one reference is the confusion this is supposed to remove.

import { EyeOpenIcon } from "@radix-ui/react-icons";

import type { AssistantPageContextSnapshot } from "@/lib/chatDock/pageContext";
import type { TypedReference } from "@/lib/chatDock/routeReference";

import { stripSpeaks } from "./ContextChip";

export default function ContextEye({
  reference,
  page,
  dismissed,
  onDismiss,
  includesEditorDocument = false,
}: {
  reference: TypedReference | null;
  page?: AssistantPageContextSnapshot | null;
  dismissed: boolean;
  onDismiss: () => void;
  includesEditorDocument?: boolean;
}) {
  if (!reference) return null;
  if (stripSpeaks(reference, dismissed)) return null;
  // Both the tooltip and the accessible name carry the label, and the
  // title carries the wire form as well: a sighted operator hovers, a
  // screen-reader user hears it on focus, and neither has to take the
  // pointer on trust.
  const details = [
    reference.ref,
    page?.route ? `route ${page.route}` : null,
    page?.section ? `section ${page.section}` : null,
    page?.state?.dirty === true ? "unsaved changes" : null,
    includesEditorDocument && page?.route.startsWith("/editor")
      ? "live editor document"
      : null,
  ].filter(Boolean);
  const said = `Sending this page as context: ${details.join("; ")}`;
  return (
    <button
      type="button"
      onClick={onDismiss}
      title={`${said} — click to stop`}
      aria-label={`${said}. Stop using it`}
      className="shrink-0 mb-1.5 rounded p-1 text-fg-subtle hover:text-fg-default hover:bg-surface-2 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent"
    >
      <EyeOpenIcon className="h-3.5 w-3.5" />
    </button>
  );
}
