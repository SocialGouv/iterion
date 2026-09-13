// Persistent conversation-anchor banner below the assistant header.
// The current page is only a candidate while the conversation is empty. Once
// the first message is accepted, the stored anchor is immutable and doubles
// as the way back to the page where the question started.

import { Cross2Icon, EyeNoneIcon, EyeOpenIcon } from "@radix-ui/react-icons";
import { Link } from "wouter";

import type { TypedReference } from "@/lib/chatDock/routeReference";
import type { ConversationContextState } from "@/lib/chatDock/conversations";

interface Props {
  state: ConversationContextState;
  reference: TypedReference | null;
  backHref?: string | null;
  onDismiss?: () => void;
  onRestore?: () => void;
}

export default function ContextChip({
  state,
  reference,
  backHref,
  onDismiss,
  onRestore,
}: Props) {
  if (state === "disabled") {
    return (
      <Strip>
        <EyeNoneIcon className="h-3.5 w-3.5 shrink-0 text-fg-subtle" />
        <span className="text-micro text-fg-muted min-w-0">
          Conversation without page context
        </span>
        {onRestore && reference ? (
          <button
            type="button"
            onClick={onRestore}
            className="ml-auto shrink-0 inline-flex items-center gap-1 rounded px-1.5 py-0.5 text-micro font-medium text-accent-text hover:bg-accent-soft focus:outline-none focus-visible:ring-2 focus-visible:ring-accent"
            title={`Let the first message use ${reference.label}`}
          >
            Use this page for the first message
          </button>
        ) : null}
      </Strip>
    );
  }

  if (state === "unknown") {
    return (
      <Strip>
        <EyeNoneIcon className="h-3.5 w-3.5 shrink-0 text-fg-subtle" />
        <span className="text-micro text-fg-muted">
          Original context unavailable
        </span>
      </Strip>
    );
  }

  if (!reference) return null;

  if (state === "pending") {
    return (
      <Strip>
        <EyeOpenIcon
          className={`h-3.5 w-3.5 shrink-0 ${
            reference.degraded ? "text-warning-fg" : "text-fg-subtle"
          }`}
        />
        <span className="text-micro text-fg-muted shrink-0">
          {reference.degraded
            ? "Limited context for first message:"
            : "Context for first message:"}
        </span>
        <ReferencePill reference={reference} />
        {onDismiss ? (
          <RemoveButton reference={reference} onDismiss={onDismiss} />
        ) : null}
      </Strip>
    );
  }

  return (
    <Strip>
      <EyeOpenIcon className="h-3.5 w-3.5 shrink-0 text-fg-subtle" />
      <span className="text-micro text-fg-muted shrink-0">
        Conversation context:
      </span>
      <ReferencePill reference={reference} />
      {backHref ? (
        <Link
          href={backHref}
          className="ml-auto shrink-0 rounded px-1.5 py-0.5 text-micro font-medium text-accent-text hover:bg-accent-soft focus:outline-none focus-visible:ring-2 focus-visible:ring-accent"
        >
          Back to {reference.label}
        </Link>
      ) : null}
    </Strip>
  );
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

function RemoveButton({
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
      aria-label={`Remove ${reference.label} from the first message context`}
      title={`Remove ${reference.label} from the first message context`}
      className="ml-auto shrink-0 inline-flex items-center gap-1 rounded px-1.5 py-0.5 text-micro font-medium text-fg-muted hover:text-fg-default hover:bg-surface-2 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent"
    >
      <Cross2Icon className="h-3 w-3" />
      <span>Remove</span>
    </button>
  );
}
