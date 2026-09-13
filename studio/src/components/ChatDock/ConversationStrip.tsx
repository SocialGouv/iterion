// The dock's conversation tabs, plus the way back to what each one is about.
//
// Two things the operator loses without this, and both were the reason for
// asking: a second thread costs them the first, and a conversation they come
// back to gives no clue what it concerns.
//
// The immutable first-message anchor is rendered by ContextChip. A workflow
// produced later is a separate destination, never a replacement for it.

import { Cross2Icon, PlusIcon } from "@radix-ui/react-icons";
import { Link } from "wouter";

import { IconButton } from "@/components/ui";
import type { Conversation } from "@/lib/chatDock/conversations";

export function ConversationStrip({
  conversations,
  activeId,
  atLimit,
  onSelect,
  onClose,
  onOpen,
  labelFor,
  closingIds = new Set<string>(),
  waitingIds = new Set<string>(),
  unreadWatchIds = new Set<string>(),
}: {
  conversations: readonly Conversation[];
  activeId: string | null;
  atLimit: boolean;
  onSelect: (id: string) => void;
  onClose: (id: string) => void | Promise<void>;
  onOpen: () => void;
  /** Display name of the bot answering a conversation. */
  labelFor: (c: Conversation) => string;
  /** Conversation ids waiting for server acceptance of their cancellation. */
  closingIds?: ReadonlySet<string>;
  /** Conversation ids whose run is parked on a human gate. */
  waitingIds?: ReadonlySet<string>;
  /** Conversation ids carrying a finalized automatic watch result. */
  unreadWatchIds?: ReadonlySet<string>;
}) {
  // Render the single tab too. Hiding the strip when there was only one
  // conversation also hid the ONLY control whose contract is "close and stop
  // the run"; the panel's minus correctly means minimise and must stay safe.
  return (
    <div className="flex min-w-0 items-center gap-1 overflow-x-auto">
      {conversations.map((c) => {
        const isActive = c.id === activeId;
        const isClosing = closingIds.has(c.id);
        const isWaiting = waitingIds.has(c.id);
        const hasUnreadWatch = unreadWatchIds.has(c.id);
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
              disabled={isClosing}
              onClick={() => onSelect(c.id)}
              title={
                c.originLabel
                  ? `${labelFor(c)} — about ${c.originLabel}`
                  : labelFor(c)
              }
            >
              {labelFor(c)}
              {c.originLabel ? (
                <span className="text-fg-subtle"> · {c.originLabel}</span>
              ) : null}
              {hasUnreadWatch || isWaiting ? (
                <span
                  className={`ml-1 inline-block h-1.5 w-1.5 rounded-full align-middle ${
                    hasUnreadWatch ? "bg-accent" : "bg-warning"
                  }`}
                  aria-label={
                    hasUnreadWatch
                      ? "New automatic watch result"
                      : "Ready for your next message"
                  }
                  title={
                    hasUnreadWatch
                      ? "New automatic watch result"
                      : "Ready for your next message"
                  }
                />
              ) : null}
            </button>
            <button
              type="button"
              aria-label={
                isClosing
                  ? `Closing ${labelFor(c)} conversation`
                  : `Close ${labelFor(c)} conversation and stop its run`
              }
              title="Close conversation and stop its run"
              disabled={isClosing}
              className={`${
                conversations.length === 1
                  ? "opacity-100"
                  : "opacity-0 group-hover:opacity-100 focus:opacity-100"
              } text-fg-subtle hover:text-fg-default disabled:cursor-wait disabled:opacity-50`}
              onClick={() => void onClose(c.id)}
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
 * WorkplaceLink offers a workflow the conversation produced. The original
 * question remains independently reachable through the anchor banner.
 *
 * A link, never a jump: the same rule as every other move the assistant
 * proposes. Absent when there is nowhere to go, or when the operator is
 * already there — a link to where you stand is noise.
 */
export function WorkplaceLink({
  runId,
  hasDraft,
  currentPath,
  currentSearch,
}: {
  runId: string | null;
  hasDraft: boolean;
  currentPath: string;
  currentSearch: string;
}) {
  if (!hasDraft || !runId) return null;
  const href = `/editor?draft=${encodeURIComponent(runId)}`;
  // Path alone is insufficient: /editor?draft=A and ?draft=B are different
  // workplaces.
  if (
    currentPath === "/editor" &&
    new URLSearchParams(currentSearch).get("draft") === runId
  ) {
    return null;
  }
  return <LinkRow href={href} label="the workflow it drafted" />;
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
