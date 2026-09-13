import { useCallback, useState } from "react";
import { addReferenceToDrag } from "@/lib/chatDock/dragReference";

import type { NativeIssue } from "@/api/native";

import { DRAG_MIME_ISSUE_IDS } from "../boardShared";

export interface UseBoardSelectionResult {
  selectedIds: Set<string>;
  setSelectedIds: React.Dispatch<React.SetStateAction<Set<string>>>;
  anchorId: string | null;
  setAnchorId: React.Dispatch<React.SetStateAction<string | null>>;
  setSingleSelection: (id: string | null) => void;
  toggleSelection: (id: string) => void;
  selectAllVisible: () => void;
  selectColumn: (stateName: string) => void;
  onCardClick: (iss: NativeIssue, e: React.MouseEvent) => void;
  onCardDragStart: (iss: NativeIssue, e: React.DragEvent) => void;
}

// Owns the multi-selection state + the click/drag-start selection
// logic. `selectedIds` is the full set; `anchorId` is the pivot for
// shift-range extension and the focal point for keyboard navigation.
// A plain click collapses both to {id}; ctrl/meta-click toggles
// membership; shift-click extends a range from the anchor to the
// clicked card.
export function useBoardSelection({
  filteredIssues,
  flatIssueIds,
  byState,
}: {
  filteredIssues: NativeIssue[];
  flatIssueIds: string[];
  byState: Map<string, NativeIssue[]>;
}): UseBoardSelectionResult {
  const [selectedIds, setSelectedIds] = useState<Set<string>>(() => new Set());
  const [anchorId, setAnchorId] = useState<string | null>(null);

  const setSingleSelection = useCallback((id: string | null) => {
    if (id === null) {
      setSelectedIds(new Set());
      setAnchorId(null);
    } else {
      setSelectedIds(new Set([id]));
      setAnchorId(id);
    }
  }, []);

  const onCardClick = useCallback(
    (iss: NativeIssue, e: React.MouseEvent) => {
      const meta = e.ctrlKey || e.metaKey;
      const shift = e.shiftKey;

      if (meta) {
        const wasSelected = selectedIds.has(iss.id);
        setSelectedIds((prev) => {
          const next = new Set(prev);
          if (next.has(iss.id)) next.delete(iss.id);
          else next.add(iss.id);
          return next;
        });
        // Anchor follows additions so a follow-up shift-click
        // extends from here. On deselect, keep the prior anchor —
        // unless it was this same card, which is no longer there.
        setAnchorId((prev) =>
          wasSelected ? (prev === iss.id ? null : prev) : iss.id,
        );
        return;
      }

      if (shift && anchorId && anchorId !== iss.id) {
        const startIdx = flatIssueIds.indexOf(anchorId);
        const endIdx = flatIssueIds.indexOf(iss.id);
        if (startIdx >= 0 && endIdx >= 0) {
          const [lo, hi] = startIdx <= endIdx ? [startIdx, endIdx] : [endIdx, startIdx];
          const range = flatIssueIds.slice(lo, hi + 1);
          setSelectedIds((prev) => {
            const next = new Set(prev);
            for (const id of range) next.add(id);
            return next;
          });
        }
        return;
      }

      // Plain click selects only (GitHub-style) — opening the modal is
      // a deliberate gesture (double-click the card or click the title).
      // Selection still updates so the keyboard hook has an anchor and
      // the single-card action bar appears.
      setSingleSelection(iss.id);
    },
    [anchorId, flatIssueIds, selectedIds, setSingleSelection],
  );

  const onCardDragStart = useCallback(
    (iss: NativeIssue, e: React.DragEvent) => {
      // Dragging an unselected card collapses the selection to
      // that card — the operator's intent is "move this one",
      // not "move the four I forgot were still selected".
      let ids: string[];
      if (selectedIds.has(iss.id) && selectedIds.size > 1) {
        ids = Array.from(selectedIds);
      } else {
        ids = [iss.id];
        setSingleSelection(iss.id);
      }
      e.dataTransfer.setData(DRAG_MIME_ISSUE_IDS, JSON.stringify(ids));
      // text/plain fallback so a drag-out into another app yields
      // the originating issue id rather than a JSON blob.
      e.dataTransfer.setData("text/plain", iss.id);
      e.dataTransfer.effectAllowed = "move";
      // The same gesture also serves the assistant's composer: a typed
      // reference rides alongside the move payload under its own MIME type,
      // so dragging a card onto the dock asks about it. Only the single-card
      // case — an assistant handed four cards is being given a project, and
      // the operator would not see which four.
      if (ids.length === 1) {
        addReferenceToDrag(e.dataTransfer, "card", iss.id, iss.title);
      }
    },
    [selectedIds, setSingleSelection],
  );

  // Toggle one card in/out of the multi-selection (keyboard `x` and the
  // per-column select-all share this). The anchor follows the acted-on
  // card so further arrow-key navigation continues from here.
  const toggleSelection = useCallback((id: string) => {
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
    setAnchorId(id);
  }, []);

  const selectAllVisible = useCallback(() => {
    const ids = filteredIssues.map((i) => i.id);
    setSelectedIds(new Set(ids));
    setAnchorId(ids[0] ?? null);
  }, [filteredIssues]);

  // Select every card in one state column (per-column select-all box).
  const selectColumn = useCallback(
    (stateName: string) => {
      const ids = (byState.get(stateName) ?? []).map((i) => i.id);
      setSelectedIds((prev) => {
        const next = new Set(prev);
        const allIn = ids.length > 0 && ids.every((id) => next.has(id));
        // Toggle: if the whole column is already selected, clear it;
        // otherwise add the column to the selection.
        for (const id of ids) {
          if (allIn) next.delete(id);
          else next.add(id);
        }
        return next;
      });
      const first = ids[0];
      if (first) setAnchorId(first);
    },
    [byState],
  );

  return {
    selectedIds,
    setSelectedIds,
    anchorId,
    setAnchorId,
    setSingleSelection,
    toggleSelection,
    selectAllVisible,
    selectColumn,
    onCardClick,
    onCardDragStart,
  };
}
