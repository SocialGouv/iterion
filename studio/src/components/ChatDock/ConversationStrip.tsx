// The dock's conversation tabs, plus the way back to what each one is about.
//
// Two things the operator loses without this, and both were the reason for
// asking: a second thread costs them the first, and a conversation they come
// back to gives no clue what it concerns.
//
// "What it concerns" is its WORKPLACE, not its birthplace — the workflow a
// conversation drafted, not the board you happened to be on when you opened
// the tab. See WorkplaceLink; the origin is only the fallback.

import { Cross2Icon, PlusIcon } from "@radix-ui/react-icons";
import { Link } from "wouter";

import { IconButton } from "@/components/ui";
import type { Conversation } from "@/lib/chatDock/conversations";
import { hrefForReference } from "@/lib/chatDock/routeReference";

export function ConversationStrip({
  conversations,
  activeId,
  atLimit,
  onSelect,
  onClose,
  onOpen,
  labelFor,
}: {
  conversations: readonly Conversation[];
  activeId: string | null;
  atLimit: boolean;
  onSelect: (id: string) => void;
  onClose: (id: string) => void;
  onOpen: () => void;
  /** Display name of the bot answering a conversation. */
  labelFor: (c: Conversation) => string;
}) {
  // One conversation needs no strip: a tab bar with a single tab is chrome
  // that explains nothing. It appears when there is a choice to make.
  if (conversations.length < 2) {
    return (
      <IconButton
        label="New conversation"
        tooltip={atLimit ? "Too many conversations open" : "New conversation"}
        size="sm"
        variant="ghost"
        disabled={atLimit}
        onClick={onOpen}
      >
        <PlusIcon className="h-3.5 w-3.5" />
      </IconButton>
    );
  }
  return (
    <div className="flex min-w-0 items-center gap-1 overflow-x-auto">
      {conversations.map((c) => {
        const isActive = c.id === activeId;
        return (
          <span
            key={c.id}
            className={`group inline-flex shrink-0 items-center gap-1 rounded px-1.5 py-0.5 text-micro ${
              isActive
                ? "bg-surface-1 text-fg-default"
                : "text-fg-muted hover:text-fg-default"
            }`}
          >
            <button
              type="button"
              className="max-w-[9rem] truncate"
              aria-current={isActive ? "true" : undefined}
              onClick={() => onSelect(c.id)}
              title={
                c.originLabel
                  ? `${labelFor(c)} — opened from ${c.originLabel}`
                  : labelFor(c)
              }
            >
              {labelFor(c)}
              {c.originLabel ? (
                <span className="text-fg-subtle"> · {c.originLabel}</span>
              ) : null}
            </button>
            <button
              type="button"
              aria-label={`Close ${labelFor(c)} conversation`}
              className="opacity-0 group-hover:opacity-100 focus:opacity-100 text-fg-subtle hover:text-fg-default"
              onClick={() => onClose(c.id)}
            >
              <Cross2Icon className="h-3 w-3" />
            </button>
          </span>
        );
      })}
      <IconButton
        label="New conversation"
        tooltip={atLimit ? "Too many conversations open" : "New conversation"}
        size="sm"
        variant="ghost"
        disabled={atLimit}
        onClick={onOpen}
      >
        <PlusIcon className="h-3.5 w-3.5" />
      </IconButton>
    </div>
  );
}

/**
 * WorkplaceLink offers the page a conversation is ABOUT.
 *
 * Two candidates, and the order matters. A conversation that drafted a
 * workflow is about that workflow — coming back to it means the editor with
 * the draft loaded, not the board you happened to be on when you opened the
 * tab. Where it was born is only the fallback, for a conversation that has
 * produced nothing to return to.
 *
 * A link, never a jump: the same rule as every other move the assistant
 * proposes. Absent when there is nowhere to go, or when the operator is
 * already there — a link to where you stand is noise.
 */
export function WorkplaceLink({
  conversation,
  runId,
  hasDraft,
  currentPath,
  currentSearch,
}: {
  conversation: Conversation | null;
  runId: string | null;
  hasDraft: boolean;
  currentPath: string;
  currentSearch: string;
}) {
  if (hasDraft && runId) {
    const href = `/editor?draft=${encodeURIComponent(runId)}`;
    // Already looking at this draft: the canvas IS the answer.
    if (
      currentPath === "/editor" &&
      new URLSearchParams(currentSearch).get("draft") === runId
    ) {
      return null;
    }
    return (
      <LinkRow href={href} label="the workflow it drafted" />
    );
  }
  const href = conversation?.origin ? hrefForReference(conversation.origin) : null;
  if (!href || !conversation?.originLabel) return null;
  if (href === currentPath) return null;
  return <LinkRow href={href} label={conversation.originLabel} />;
}

function LinkRow({ href, label }: { href: string; label: string }) {
  return (
    <div className="px-3 pt-2">
      <Link
        href={href}
        className="text-caption text-accent-text hover:underline"
      >
        ← Back to {label}
      </Link>
    </div>
  );
}
