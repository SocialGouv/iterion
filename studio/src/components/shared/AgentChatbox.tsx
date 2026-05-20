import { useCallback, useEffect, useMemo, useState } from "react";
import { Cross1Icon, PlusIcon, TrashIcon } from "@radix-ui/react-icons";

import { Badge, Button, IconButton, Textarea } from "@/components/ui";
import type { BadgeVariant } from "@/components/ui";
import {
  cancelQueuedMessage,
  listQueuedMessages,
  listRunSkills,
  queueMessage,
  type BundleSkill,
} from "@/api/queueMessages";
import {
  useRunStore,
  type QueuedUserMessage,
} from "@/store/run";
import { useUIStore } from "@/store/ui";

interface Props {
  runId: string;
  // When true, hide the composer and show a muted placeholder. Used by
  // RunView/WhatsNextView once the run reaches a terminal status.
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
// Shared between RunView and WhatsNextView. The live message inbox is
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
  const chatEnterSubmits = useUIStore((s) => s.chatEnterSubmits);
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [showAll, setShowAll] = useState(false);
  // Skill attachment state: catalog fetched on mount, plus the
  // currently-selected names for the next outgoing message.
  const [skillCatalog, setSkillCatalog] = useState<BundleSkill[]>([]);
  const [attachedSkills, setAttachedSkills] = useState<string[]>([]);
  const [pickerOpen, setPickerOpen] = useState(false);
  const [pickerFilter, setPickerFilter] = useState("");

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

  // Fetch the bundle skill catalog on mount, with a module-level
  // cache keyed by runId so multiple tabs / re-mounts share a single
  // request. The catalog is immutable for a run's lifetime — skills
  // are mirrored from the bundle at run start and the bundle path
  // doesn't change mid-run. Failure is non-fatal: the picker stays
  // empty and the user can still send plain text messages.
  useEffect(() => {
    let cancelled = false;
    const cached = skillCatalogCache.get(runId);
    if (cached) {
      setSkillCatalog(cached);
      return;
    }
    listRunSkills(runId)
      .then((skills) => {
        if (cancelled) return;
        skillCatalogCache.set(runId, skills);
        setSkillCatalog(skills);
      })
      .catch(() => {
        /* swallow: catalog is best-effort */
      });
    return () => {
      cancelled = true;
    };
  }, [runId]);

  const submit = useCallback(async () => {
    const text = draft.trim();
    if (text === "" || busy) return;
    setBusy(true);
    setError(null);
    try {
      await queueMessage(runId, text, { skills: attachedSkills });
      setDraft("");
      setAttachedSkills([]);
      setPickerOpen(false);
      // The user_message_queued event will flow through the WS and
      // populate the store; no optimistic insert here.
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }, [draft, runId, busy, attachedSkills]);

  const toggleSkill = useCallback((name: string) => {
    setAttachedSkills((prev) =>
      prev.includes(name) ? prev.filter((n) => n !== name) : [...prev, name],
    );
  }, []);

  const filteredCatalog = useMemo(() => {
    const q = pickerFilter.trim().toLowerCase();
    if (q === "") return skillCatalog;
    return skillCatalog.filter(
      (s) =>
        s.name.toLowerCase().includes(q) ||
        (s.description ?? "").toLowerCase().includes(q),
    );
  }, [pickerFilter, skillCatalog]);

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
    if (e.key !== "Enter") return;
    if (chatEnterSubmits) {
      // Default: Enter submits, Shift+Enter inserts newline. Cmd/Ctrl+Enter
      // also submits for muscle-memory parity with the legacy binding.
      if (e.shiftKey) return;
      e.preventDefault();
      void submit();
    } else {
      // Legacy: Cmd/Ctrl+Enter submits, Enter inserts newline.
      if (!(e.metaKey || e.ctrlKey)) return;
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
        {/* Skill chip row: visible only when at least one skill is
            attached. Each chip is removable. */}
        {attachedSkills.length > 0 && (
          <div className="flex flex-wrap gap-1">
            {attachedSkills.map((name) => (
              <span
                key={name}
                className="inline-flex items-center gap-1 rounded-full border border-info/40 bg-info-soft px-2 py-0.5 text-[10px] font-mono text-fg-default"
              >
                ⑂ {name}
                <button
                  type="button"
                  onClick={() => toggleSkill(name)}
                  className="text-fg-subtle hover:text-fg-default focus:outline-none"
                  aria-label={`Remove skill ${name}`}
                >
                  <Cross1Icon className="h-2.5 w-2.5" />
                </button>
              </span>
            ))}
          </div>
        )}
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
            rows={Math.max(2, Math.min(10, Math.ceil(draft.length / 60) + 1))}
            disabled={disabled || busy}
            className="flex-1 text-[12px]"
          />
          <div className="flex flex-col items-stretch gap-1 self-end">
            {skillCatalog.length > 0 && (
              <Button
                variant="ghost"
                size="sm"
                disabled={disabled || busy}
                onClick={() => setPickerOpen((v) => !v)}
                title="Attach a SKILL.md from the bundle (sticky for the rest of the run)"
              >
                <PlusIcon className="mr-1 h-3 w-3" />
                Skill
              </Button>
            )}
            <Button
              variant="primary"
              size="sm"
              disabled={disabled || busy || draft.trim() === ""}
              onClick={() => void submit()}
            >
              {busy ? "…" : "Send"}
            </Button>
          </div>
        </div>

        {pickerOpen && skillCatalog.length > 0 && (
          <div className="rounded border border-border-default bg-surface-0 p-2 text-[11px] shadow-sm">
            <input
              type="text"
              value={pickerFilter}
              onChange={(e) => setPickerFilter(e.target.value)}
              placeholder="Filter skills…"
              className="mb-1 w-full rounded border border-border-default bg-surface-1 px-2 py-1 text-[11px]"
              autoFocus
            />
            <ul className="max-h-48 overflow-y-auto">
              {filteredCatalog.length === 0 && (
                <li className="px-1 py-0.5 text-fg-subtle">No matches.</li>
              )}
              {filteredCatalog.map((s) => {
                const selected = attachedSkills.includes(s.name);
                return (
                  <li key={s.name}>
                    <button
                      type="button"
                      onClick={() => toggleSkill(s.name)}
                      className={`w-full rounded px-2 py-1 text-left hover:bg-surface-2 ${selected ? "bg-info-soft" : ""}`}
                    >
                      <span className="inline-flex items-center gap-1 font-mono">
                        <span className="text-[10px] text-fg-subtle">
                          {selected ? "☑" : "☐"}
                        </span>
                        {s.name}
                      </span>
                      {s.description && (
                        <span className="ml-5 block text-[10px] text-fg-subtle">
                          {s.description}
                        </span>
                      )}
                    </button>
                  </li>
                );
              })}
            </ul>
            <div className="mt-1 flex justify-between text-[10px] text-fg-subtle">
              <span>{attachedSkills.length} attached</span>
              <button
                type="button"
                className="hover:text-fg-default"
                onClick={() => setPickerOpen(false)}
              >
                Done
              </button>
            </div>
          </div>
        )}

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
                  {m.skill_refs && m.skill_refs.length > 0 && (
                    <span className="ml-1 font-mono text-[10px] text-fg-subtle">
                      [skill: {m.skill_refs.join(", ")}]
                    </span>
                  )}
                </span>
                {m.status === "queued" && !disabled && (
                  <IconButton
                    label="Cancel queued message"
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

// skillCatalogCache memoises the bundle skill catalog per runId.
// Module-scoped so multiple AgentChatbox instances (e.g. the same run
// open in multiple tabs) share a single fetch. Bundle skills are
// immutable for a run's lifetime, so we never need to invalidate.
const skillCatalogCache = new Map<string, BundleSkill[]>();

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
