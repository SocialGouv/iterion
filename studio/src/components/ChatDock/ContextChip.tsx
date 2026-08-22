// The pinned, dismissible chip that says what the assistant is assumed
// to be looking at.
//
// It is always visible when a reference is in effect — the point of the
// implicit-context design is that context is never silent. Dismissing it
// leaves a restore affordance rather than nothing, so "stop looking at
// this page" is reversible without a reload.

import { Cross2Icon, EyeOpenIcon } from "@radix-ui/react-icons";

import type { TypedReference } from "@/lib/chatDock/routeReference";

interface Props {
  reference: TypedReference | null;
  dismissed: boolean;
  onDismiss: () => void;
  onRestore: () => void;
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
      <div className="shrink-0 px-3 py-1.5 border-b border-border-subtle bg-surface-0">
        <button
          type="button"
          onClick={onRestore}
          className="inline-flex items-center gap-1 text-micro text-fg-muted hover:text-fg-default focus:outline-none focus-visible:ring-2 focus-visible:ring-accent rounded"
          title={`Let the assistant look at ${reference.label} again`}
        >
          <EyeOpenIcon className="h-3 w-3" />
          Use this page as context
        </button>
      </div>
    );
  }

  return (
    <div className="shrink-0 px-3 py-1.5 border-b border-border-subtle bg-surface-0 flex items-center gap-2">
      <span className="text-micro text-fg-muted shrink-0">Looking at</span>
      <span
        className="inline-flex items-center gap-1 min-w-0 max-w-full rounded-full border border-border-default bg-surface-2 px-2 py-0.5 text-micro text-fg-default"
        title={reference.ref}
      >
        <span className="text-fg-muted uppercase tracking-wide shrink-0">
          {reference.kind}
        </span>
        <span className="truncate">{reference.label}</span>
      </span>
      <button
        type="button"
        onClick={onDismiss}
        aria-label={`Stop using ${reference.label} as context`}
        title="Dismiss page context"
        className="ml-auto shrink-0 text-fg-muted hover:text-fg-default focus:outline-none focus-visible:ring-2 focus-visible:ring-accent rounded"
      >
        <Cross2Icon className="h-3 w-3" />
      </button>
    </div>
  );
}
