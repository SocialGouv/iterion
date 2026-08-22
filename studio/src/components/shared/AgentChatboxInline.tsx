import { errorMessage } from "@/lib/errorHints";
import { useCallback, useEffect, useMemo, useState } from "react";
import { Cross1Icon, PlusIcon, TrashIcon } from "@radix-ui/react-icons";

import { Button, IconButton, Input, StatusBadge, Textarea } from "@/components/ui";
import type { BadgeVariant } from "@/components/ui";
import MarkdownText from "@/components/Runs/conversation/MarkdownText";
import { useAsyncAction } from "@/hooks/useAsyncAction";
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
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";

interface Props {
  runId: string;
  disabled?: boolean;
  maxVisible?: number;
  // When true, the textarea grows up to ~10 rows; when false, capped
  // at 4 rows for use in tight containers (FloatingChatPanel).
  compact?: boolean;
  // When the parent transcript renders queued messages inline (the
  // user-message card variant the runChat fold emits), pass `embedded`
  // so this chatbox hides its own duplicate queue list. The composer
  // and skill picker stay visible — only the list of past+pending
  // queued messages below the textarea disappears.
  embedded?: boolean;
  // Placeholder override for the composer textarea. Defaults to the
  // "Queue a message to the running agent…" copy.
  placeholder?: string;
  // When provided, the composer calls this instead of the built-in
  // queueMessage on submit. Used by WhatsNextView to route a typed
  // message through its own continuation logic (queue into a live
  // run, or re-seed a fresh run when the previous one closed). The
  // draft + attached skills are cleared on a resolved send.
  onSend?: (text: string, opts: { skills: string[] }) => Promise<void> | void;
}

// Renders the composer without outer chrome — the parent container
// is responsible for borders, background, and width framing. The
// legacy `AgentChatbox` is a banner-style wrapper around this.
export default function AgentChatboxInline({
  runId,
  disabled = false,
  maxVisible = 5,
  compact = false,
  embedded = false,
  placeholder,
  onSend,
}: Props) {
  const messages = useRunStore((s) => s.queuedMessages);
  const setQueuedMessages = useRunStore((s) => s.setQueuedMessages);
  const chatEnterSubmits = useUIStore((s) => s.chatEnterSubmits);
  // Cloud runs resolve their bundle on the runner pod, so the skill
  // catalog endpoint returns empty on this server pod — the picker would
  // otherwise silently vanish. Signal the cloud gap explicitly instead.
  const isCloud = useServerInfoStore((s) => s.info?.mode === "cloud");
  // Draft lives in the run store so the WhatsNextView swap between
  // AgentChatbox and the HumanChatTurn footer (which unmounts this
  // component when the bot raises a question) doesn't discard the
  // operator's in-flight text.
  const draft = useRunStore((s) => s.chatDraft);
  const setDraft = useRunStore((s) => s.setChatDraft);
  const { busy, error, run: runAction, setError } = useAsyncAction();
  const [showAll, setShowAll] = useState(false);
  const [skillCatalog, setSkillCatalog] = useState<BundleSkill[]>([]);
  const [attachedSkills, setAttachedSkills] = useState<string[]>([]);
  const [pickerOpen, setPickerOpen] = useState(false);
  const [pickerFilter, setPickerFilter] = useState("");

  useEffect(() => {
    let cancelled = false;
    // No run yet (the assistant dock's composer is live before its first
    // session exists — the first message launches one). There is no
    // inbox to fetch, and asking for /runs//messages only produces a
    // spurious error banner.
    if (!runId) {
      setQueuedMessages([]);
      return;
    }
    listQueuedMessages(runId)
      .then((msgs) => {
        if (cancelled) return;
        setQueuedMessages(msgs);
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        // The initial inbox snapshot may fail (run not found yet, WS
        // mid-reconnect). Live events still heal it, but a stale
        // composer with no past messages is confusing — surface the
        // error in the existing error slot so the operator knows.
        setError(
          `Could not load queued messages: ${
            e instanceof Error ? e.message : String(e)
          }`,
        );
      });
    return () => {
      cancelled = true;
    };
  }, [runId, setQueuedMessages, setError]);

  useEffect(() => {
    let cancelled = false;
    if (!runId) return;
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
    await runAction(async () => {
      if (onSend) {
        await onSend(text, { skills: attachedSkills });
      } else {
        await queueMessage(runId, text, { skills: attachedSkills });
      }
      setDraft("");
      setAttachedSkills([]);
      setPickerOpen(false);
    });
  }, [draft, runId, busy, attachedSkills, onSend, setDraft, runAction]);

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
        setError(errorMessage(e));
      }
    },
    [runId, setError],
  );

  const onKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key !== "Enter") return;
    if (chatEnterSubmits) {
      if (e.shiftKey) return;
      e.preventDefault();
      void submit();
    } else {
      if (!(e.metaKey || e.ctrlKey)) return;
      e.preventDefault();
      void submit();
    }
  };

  const visible = filterVisible(messages, showAll, maxVisible);
  const hiddenCount = messages.length - visible.length;
  const maxRows = compact ? 4 : 10;

  return (
    <div className="space-y-2">
      {attachedSkills.length > 0 && (
        <div className="flex flex-wrap gap-1">
          {attachedSkills.map((name) => (
            <span
              key={name}
              className="inline-flex items-center gap-1 rounded-full border border-info/40 bg-info-soft px-2 py-0.5 text-caption font-mono text-fg-default"
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
          // An explicit placeholder wins even when disabled: the caller
          // that disables the composer is usually the one with something
          // to say about WHY. The assistant dock disables during startup
          // discovery and wants "Starting a session…", which the old
          // order made unreachable — the operator saw "Run is not
          // active" while a run was being looked for, i.e. the opposite
          // of what was happening, on the surface added to explain it.
          placeholder={
            placeholder ??
            (disabled ? "Run is not active" : "Queue a message to the running agent…")
          }
          rows={Math.max(2, Math.min(maxRows, Math.ceil(draft.length / 60) + 1))}
          disabled={disabled || busy}
          className="flex-1 text-body"
        />
        <div className="flex flex-col items-stretch gap-1 self-end">
          {skillCatalog.length > 0 ? (
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
          ) : (
            isCloud && (
              // Empty catalog on a cloud server: the bundle (and its
              // skills) live on the runner pod, unreachable from here.
              // Render a disabled affordance with a reason rather than
              // nothing, so the gap reads as "cloud limitation" not "bug".
              <Button
                variant="ghost"
                size="sm"
                disabled
                title="Skills aren't available for cloud runs — the bundle is resolved on the runner pod, not this server."
              >
                <PlusIcon className="mr-1 h-3 w-3" />
                No skills
              </Button>
            )
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
        <div className="rounded border border-border-default bg-surface-0 p-2 text-micro shadow-sm">
          <Input
            type="text"
            value={pickerFilter}
            onChange={(e) => setPickerFilter(e.target.value)}
            placeholder="Filter skills…"
            aria-label="Filter skills"
            className="mb-1"
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
                      <span className="text-caption text-fg-subtle">
                        {selected ? "☑" : "☐"}
                      </span>
                      {s.name}
                    </span>
                    {s.description && (
                      <span className="ml-5 block text-caption text-fg-subtle">
                        {s.description}
                      </span>
                    )}
                  </button>
                </li>
              );
            })}
          </ul>
          <div className="mt-1 flex justify-between text-caption text-fg-subtle">
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
        <p className="text-micro text-danger-fg" role="alert">
          {error}
        </p>
      )}

      {!embedded && visible.length > 0 && (
        <ul className="space-y-1">
          {visible.map((m) => (
            <li
              key={m.id}
              className="flex items-start gap-2 rounded border border-border-subtle bg-surface-0 px-2 py-1.5"
            >
              <StatusBadge variant={statusVariant[m.status]} label={m.status} />
              <div className="flex-1 text-body text-fg-default">
                <MarkdownText value={m.text} size="sm" />
                {m.skill_refs && m.skill_refs.length > 0 && (
                  <span className="font-mono text-caption text-fg-subtle">
                    [skill: {m.skill_refs.join(", ")}]
                  </span>
                )}
              </div>
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

      {!embedded && hiddenCount > 0 && (
        <button
          type="button"
          className="text-caption text-fg-subtle hover:text-fg-default"
          onClick={() => setShowAll((v) => !v)}
        >
          {showAll
            ? "Hide older messages"
            : `Show ${hiddenCount} older message${hiddenCount === 1 ? "" : "s"}`}
        </button>
      )}
    </div>
  );
}

const skillCatalogCache = new Map<string, BundleSkill[]>();

const statusVariant: Record<QueuedUserMessage["status"], BadgeVariant> = {
  queued: "warning",
  delivered: "accent",
  consumed: "success",
  cancelled: "neutral",
};

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
