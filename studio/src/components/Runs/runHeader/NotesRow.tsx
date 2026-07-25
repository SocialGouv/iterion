// Extracted from RunHeader.tsx to keep that file focused.
// Per-run operator notes: a small chronological list plus an "add note"
// input, backed by GET/POST /api/runs/:id/notes. Notes persist with the
// run (filesystem locally, Mongo in cloud) and are visible to the whole
// team — the durable "flaky, re-ran" / "root cause was X" annotations.

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { addNote, listNotes, type RunNote } from "@/api/runs";
import { Button, InlineBanner, Textarea } from "@/components/ui";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { errorMessage } from "@/lib/errorHints";
import { formatRelative } from "@/lib/format";

// NotesRow loads the run's notes once on mount (they're durable and
// change only through this input — no WS stream), renders them oldest
// first, and lets the operator append one. Notes are immutable in this
// first cut, so there's no edit/delete affordance.
export default function NotesRow({ runId }: { runId: string }) {
  const queryClient = useQueryClient();
  const notesQuery = useQuery({
    queryKey: ["run-notes", runId],
    queryFn: ({ signal }) => listNotes(runId, { signal }),
  });
  const notes = notesQuery.data ?? [];
  const [draft, setDraft] = useState("");
  const addAction = useAsyncAction();
  // One banner serves both surfaces: the add-flow error takes precedence,
  // else the load error (hidden once dismissed — nothing refetches notes,
  // so it would otherwise stick around forever).
  const [loadErrorDismissed, setLoadErrorDismissed] = useState(false);
  const loadError =
    notesQuery.error && !loadErrorDismissed
      ? errorMessage(notesQuery.error)
      : null;
  const error = addAction.error ?? loadError;

  const onAdd = async () => {
    const body = draft.trim();
    if (!body || addAction.busy) return;
    // Submitting clears whatever error was on display (run() resets the
    // add-flow error; the load error is dismissed the same way).
    setLoadErrorDismissed(true);
    const created = await addAction.run(() => addNote(runId, body));
    if (created) {
      // The POST returned the authoritative record — fold it into the
      // cached list rather than refetching the whole run's notes.
      queryClient.setQueryData<RunNote[]>(["run-notes", runId], (prev) => [
        ...(prev ?? []),
        created,
      ]);
      setDraft("");
    }
  };

  // Cmd/Ctrl+Enter submits — matches the chatbox convention so a plain
  // Enter can still add newlines to a multi-line note.
  const onKeyDown = (e: React.KeyboardEvent) => {
    if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
      e.preventDefault();
      void onAdd();
    }
  };

  return (
    <div className="shrink-0 px-3 sm:px-4 py-2 border-b border-border-default flex flex-col gap-2 text-sm">
      <div className="text-micro font-medium text-fg-muted uppercase tracking-wide">
        Notes
      </div>
      {notes.length > 0 ? (
        <ul className="flex flex-col gap-1.5">
          {notes.map((n) => (
            <li key={n.seq} className="flex flex-col gap-0.5">
              <div className="whitespace-pre-wrap break-words text-fg-default">
                {n.body}
              </div>
              <div className="text-micro text-fg-subtle">
                {n.author || "operator"} · {formatRelative(n.ts)}
              </div>
            </li>
          ))}
        </ul>
      ) : (
        <div className="text-micro text-fg-subtle">No notes yet.</div>
      )}
      <div className="flex items-end gap-2">
        <Textarea
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={onKeyDown}
          placeholder="Add a note… (⌘/Ctrl+Enter to save)"
          rows={2}
          className="flex-1 min-h-0"
          aria-label="Add a run note"
        />
        <Button
          variant="secondary"
          size="sm"
          onClick={() => void onAdd()}
          disabled={addAction.busy || draft.trim() === ""}
        >
          Add note
        </Button>
      </div>
      {error && (
        <InlineBanner
          tone="danger"
          dismissable
          onDismiss={() => {
            addAction.clearError();
            setLoadErrorDismissed(true);
          }}
        >
          {error}
        </InlineBanner>
      )}
    </div>
  );
}
