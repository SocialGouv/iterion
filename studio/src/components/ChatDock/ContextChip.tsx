// The context strip — which now speaks only when it has something to say.
//
// It used to be pinned open whenever a reference was in effect, on the
// reasoning that context must never be silent. That reasoning was half
// right. What must never be silent is what the assistant was TOLD; on the
// ordinary route those two are the same fact, and the operator is already
// looking at it. "You are looking at the board", pinned above the board,
// is a tautology occupying a permanent strip. The route table already
// applied this rule in one place — /whats-next contributes no reference
// because "you are looking at the conversation" is noise — and this is
// that same rule applied consistently.
//
// So the strip renders in the two cases where it is NEWS:
//
//   dismissed — the absence of context is invisible by nature, and
//               without a way back the only escape is a reload.
//   degraded  — the route addressed an entity and the pointer fell back
//               to the surrounding view. The screen still shows the run;
//               only the assistant lost it. Nothing else would tell you.
//
// Everything else is the quiet control in the composer row: ContextEye,
// which keeps "what am I sending" answerable on demand and costs no
// vertical space to ask.

import { Cross2Icon, EyeOpenIcon } from "@radix-ui/react-icons";

import type { TypedReference } from "@/lib/chatDock/routeReference";

interface Props {
  reference: TypedReference | null;
  dismissed: boolean;
  onDismiss: () => void;
  onRestore: () => void;
}

/**
 * stripSpeaks is the whole "is this news" rule, in one place.
 *
 * Exported because ContextEye is its exact complement: the strip and the
 * eye are two presentations of one control, and both showing at once
 * would give the operator two ways to dismiss the same reference. Sharing
 * the predicate is what keeps them from drifting into that.
 */
export function stripSpeaks(
  reference: TypedReference | null,
  dismissed: boolean,
): boolean {
  if (!reference) return false;
  return dismissed || reference.degraded === true;
}

export default function ContextChip({
  reference,
  dismissed,
  onDismiss,
  onRestore,
}: Props) {
  // Nothing to point at (home, the assistant's own route, an unmapped
  // route) — render no strip at all rather than an empty one.
  if (!reference) return null;

  if (dismissed) {
    return (
      <Strip>
        <button
          type="button"
          onClick={onRestore}
          className="inline-flex items-center gap-1 text-micro text-fg-muted hover:text-fg-default focus:outline-none focus-visible:ring-2 focus-visible:ring-accent rounded"
          title={`Let the assistant look at ${reference.label} again`}
        >
          <EyeOpenIcon className="h-3 w-3" />
          Use this page as context
        </button>
      </Strip>
    );
  }

  if (reference.degraded) {
    return (
      <Strip>
        {/* Said plainly: the pointer is coarser than the page. The
            operator can act on it — name the run in their message — but
            only if they know, and this is the only surface that knows. */}
        <span className="text-micro text-fg-muted min-w-0">
          Couldn&apos;t identify this page — the assistant only has
        </span>
        <ReferencePill reference={reference} />
        <DismissButton reference={reference} onDismiss={onDismiss} />
      </Strip>
    );
  }

  // The ordinary case: the operator can see the page, so the strip says
  // nothing. ContextEye holds the answer and the control.
  return null;
}

function Strip({ children }: { children: React.ReactNode }) {
  return (
    <div className="shrink-0 px-3 py-1.5 border-b border-border-subtle bg-surface-0 flex items-center gap-2">
      {children}
    </div>
  );
}

export function ReferencePill({ reference }: { reference: TypedReference }) {
  return (
    <span
      className="inline-flex items-center gap-1 min-w-0 max-w-full rounded-full border border-border-default bg-surface-2 px-2 py-0.5 text-micro text-fg-default"
      title={reference.ref}
    >
      <span className="text-fg-muted uppercase tracking-wide shrink-0">
        {reference.kind}
      </span>
      <span className="truncate">{reference.label}</span>
    </span>
  );
}

function DismissButton({
  reference,
  onDismiss,
}: {
  reference: TypedReference;
  onDismiss: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onDismiss}
      aria-label={`Stop using ${reference.label} as context`}
      title="Dismiss page context"
      className="ml-auto shrink-0 text-fg-muted hover:text-fg-default focus:outline-none focus-visible:ring-2 focus-visible:ring-accent rounded"
    >
      <Cross2Icon className="h-3 w-3" />
    </button>
  );
}
