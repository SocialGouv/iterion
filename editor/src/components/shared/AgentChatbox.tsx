import { useCallback, useEffect, useState } from "react";
import { TrashIcon } from "@radix-ui/react-icons";

import { Badge, Button, IconButton, Textarea } from "@/components/ui";
import type { BadgeVariant } from "@/components/ui";
import {
  cancelQueuedMessage,
  listQueuedMessages,
  queueMessage,
} from "@/api/queueMessages";
import {
  useRunStore,
  type QueuedUserMessage,
} from "@/store/run";

interface Props {
  runId: string;
  // When true, hide the composer and show a muted placeholder. Used by
  // RunView/PiloteView once the run reaches a terminal status.
  disabled?: boolean;
  // Maximum number of recent messages to surface. Older messages
  // collapse behind a "show all" link. Default 5.
  maxVisible?: number;
}

// AgentChatbox lets the operator queue chat messages against a
// running agent. Messages are delivered cooperatively at safe agent
// boundaries (between tool iterations for claw, at the next human
// pause for claude_code / codex) — there is no preemption.
//
// Shared between RunView and PiloteView. The live message inbox is
// kept on the run store (queuedMessages slice), populated through
// the user_message_* event family and seeded by a REST hydration on
// mount.
export default function AgentChatbox({
  runId,
  disabled = false,
  maxVisible = 5,
}: Props) {
  const messages = useRunStore((s) => s.queuedMessages);
  const setQueuedMessages = useRunStore((s) => s.setQueuedMessages);
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [showAll, setShowAll] = useState(false);

  // Initial REST hydration: needed because the WS only emits NEW
  // events from the moment we subscribe, but the run might already
  // have queued messages from a previous tab.
  useEffect(() => {
    let cancelled = false;
    listQueuedMessages(runId)
      .then((msgs) => {
        if (cancelled) return;
        setQueuedMessages(msgs);
      })
      .catch(() => {
        /* swallow: live events will heal the inbox */
      });
    return () => {
      cancelled = true;
    };
  }, [runId, setQueuedMessages]);

  const submit = useCallback(async () => {
    const text = draft.trim();
    if (text === "" || busy) return;
    setBusy(true);
    setError(null);
    try {
      await queueMessage(runId, text);
      setDraft("");
      // The user_message_queued event will flow through the WS and
      // populate the store; no optimistic insert here.
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }, [draft, runId, busy]);

  const cancel = useCallback(
    async (msgId: string) => {
      try {
        await cancelQueuedMessage(runId, msgId);
      } catch (e) {
        setError((e as Error).message);
      }
    },
    [runId],
  );

  const onKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
      e.preventDefault();
      void submit();
    }
  };

  // Hide cancelled and stale consumed messages from the default
  // view so a long-running run's chatbox doesn't accumulate stale
  // rows. "show all" exposes them on demand.
  const visible = filterVisible(messages, showAll, maxVisible);
  const hiddenCount = messages.length - visible.length;

  return (
    <div className="border-t border-border-subtle bg-surface-1">
      <div className="mx-auto max-w-3xl px-4 py-2 space-y-2">
        <div className="flex items-stretch gap-2">
          <Textarea
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={onKeyDown}
            placeholder={
              disabled
                ? "Run is not active"
                : "Queue a message to the running agent…"
            }
            rows={Math.max(2, Math.min(6, Math.ceil(draft.length / 60) + 1))}
            disabled={disabled || busy}
            className="flex-1 text-[12px]"
          />
          <Button
            variant="primary"
            size="sm"
            disabled={disabled || busy || draft.trim() === ""}
            onClick={() => void submit()}
            className="self-end"
          >
            {busy ? "…" : "Send"}
          </Button>
        </div>

        {error && (
          <p className="text-[11px] text-danger-fg" role="alert">
            {error}
          </p>
        )}

        {visible.length > 0 && (
          <ul className="space-y-1">
            {visible.map((m) => (
              <li
                key={m.id}
                className="flex items-start gap-2 rounded border border-border-subtle bg-surface-0 px-2 py-1.5"
              >
                <StatusBadge status={m.status} />
                <span className="flex-1 text-[12px] whitespace-pre-wrap break-words text-fg-default">
                  {m.text}
                </span>
                {m.status === "queued" && !disabled && (
                  <IconButton
                    aria-label="Cancel queued message"
                    size="sm"
                    onClick={() => void cancel(m.id)}
                  >
                    <TrashIcon />
                  </IconButton>
                )}
              </li>
            ))}
          </ul>
        )}

        {hiddenCount > 0 && (
          <button
            type="button"
            className="text-[10px] text-fg-subtle hover:text-fg-default"
            onClick={() => setShowAll((v) => !v)}
          >
            {showAll
              ? "Hide older messages"
              : `Show ${hiddenCount} older message${hiddenCount === 1 ? "" : "s"}`}
          </button>
        )}
      </div>
    </div>
  );
}

const statusVariant: Record<QueuedUserMessage["status"], BadgeVariant> = {
  queued: "warning",
  delivered: "accent",
  consumed: "success",
  cancelled: "neutral",
};

function StatusBadge({ status }: { status: QueuedUserMessage["status"] }) {
  return <Badge variant={statusVariant[status]}>{status}</Badge>;
}

// filterVisible hides messages the operator no longer needs to see:
// cancelled and consumed messages are folded behind "show all";
// queued / delivered are always rendered. Returns the most recent
// maxVisible entries in the active set when collapsed, or the full
// list when expanded.
function filterVisible(
  all: QueuedUserMessage[],
  showAll: boolean,
  maxVisible: number,
): QueuedUserMessage[] {
  if (showAll) return all;
  const live = all.filter(
    (m) => m.status === "queued" || m.status === "delivered",
  );
  return live.slice(-maxVisible);
}
