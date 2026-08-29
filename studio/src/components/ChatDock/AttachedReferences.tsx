// The chips for what the operator DROPPED in, above the composer.
//
// Sibling of ContextChip, which shows the implicit page reference. They are
// deliberately distinct surfaces: one says "you are here" and can be
// dismissed for the session, the other says "you asked about these" and is
// cleared when the message is sent. Same visible-context rule governs both —
// whatever the assistant is handed, the operator can read.

import { Cross2Icon } from "@radix-ui/react-icons";

import type { TypedReference } from "@/lib/chatDock/routeReference";

export default function AttachedReferences({
  references,
  onDetach,
}: {
  references: readonly TypedReference[];
  onDetach: (ref: string) => void;
}) {
  if (references.length === 0) return null;
  return (
    <div
      className="shrink-0 flex flex-wrap items-center gap-1.5 border-t border-border-default bg-surface-1 px-3 py-1.5"
      // Announced as a group so a screen-reader user hears the whole
      // attachment set rather than a stream of unrelated buttons.
      role="group"
      aria-label="Attached references"
    >
      <span className="text-micro uppercase tracking-wide text-fg-muted">
        Attached
      </span>
      {references.map((r) => (
        <span
          key={r.ref}
          // The full wire form in the title: the chip's label is shortened
          // for runs, and the operator must be able to check exactly what
          // pointer is going out.
          title={r.ref}
          className="inline-flex items-center gap-1 rounded-[var(--radius-sm)] border border-border-default bg-surface-2 pl-1.5 pr-0.5 py-0.5 text-caption text-fg-default max-w-full"
        >
          <span className="text-fg-muted">{r.kind}</span>
          <span className="truncate">{r.label}</span>
          <button
            type="button"
            onClick={() => onDetach(r.ref)}
            aria-label={`Remove ${r.ref}`}
            title="Remove"
            className="shrink-0 rounded-[var(--radius-sm)] p-0.5 text-fg-muted hover:text-fg-default hover:bg-surface-3"
          >
            <Cross2Icon className="w-3 h-3" />
          </button>
        </span>
      ))}
    </div>
  );
}
