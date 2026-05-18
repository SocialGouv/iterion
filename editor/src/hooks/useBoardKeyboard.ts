import { useEffect } from "react";

import type { NativeIssue, NativeBoard } from "@/api/native";
import { isTypingTarget } from "@/lib/keyboard";

interface Args {
  board: NativeBoard | null;
  byState: Map<string, NativeIssue[]>;
  selectedId: string | null;
  modalOpen: boolean;
  onSelect: (id: string | null) => void;
  onCreate: () => void;
  onEdit: (id: string) => void;
  onDelete: (id: string) => void;
  onTransition: (id: string, toState: string) => void;
  onShowHelp: () => void;
}

// useBoardKeyboard wires keyboard navigation for the kanban board.
//   c / n        : open the New Issue modal
//   ? or shift+/ : toggle the keyboard-help overlay
//   Esc          : clear selection
//   ↑ / ↓        : navigate within the current column
//   ← / →        : move the selected card to the previous/next column
//   Enter / e    : open the selected card
//   Del / Bksp   : delete the selected card
//
// The handler is window-scoped but skips inputs/textareas/contenteditable
// targets, and bails out entirely while any modal is open so the modal
// keeps Escape / Enter for its own form actions.
export function useBoardKeyboard({
  board,
  byState,
  selectedId,
  modalOpen,
  onSelect,
  onCreate,
  onEdit,
  onDelete,
  onTransition,
  onShowHelp,
}: Args): void {
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (modalOpen) return;
      if (isTypingTarget(e.target)) return;
      if (e.metaKey || e.ctrlKey || e.altKey) return;

      // "?" is shift+/ on most layouts. Accept both the produced glyph
      // and the raw key+shift combo so AZERTY/QWERTZ users don't lose
      // the affordance.
      if (e.key === "?" || (e.key === "/" && e.shiftKey)) {
        e.preventDefault();
        onShowHelp();
        return;
      }

      if (e.key === "c" || e.key === "n") {
        e.preventDefault();
        onCreate();
        return;
      }

      if (e.key === "Escape") {
        if (selectedId) {
          e.preventDefault();
          onSelect(null);
        }
        return;
      }

      // The remaining shortcuts act on the active selection. Without a
      // selection there's no target so let arrow keys scroll the page
      // normally.
      if (!selectedId || !board) return;

      if (e.key === "e" || e.key === "Enter") {
        e.preventDefault();
        onEdit(selectedId);
        return;
      }

      if (e.key === "Delete" || e.key === "Backspace") {
        e.preventDefault();
        onDelete(selectedId);
        return;
      }

      const { state, column, index } = locateSelection(board, byState, selectedId);
      if (!column) return;

      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        const next = e.key === "ArrowDown" ? index + 1 : index - 1;
        const target = column[next];
        if (target) onSelect(target.id);
        return;
      }

      if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
        e.preventDefault();
        const stateIdx = board.states.findIndex((s) => s.name === state);
        if (stateIdx < 0) return;
        const dir = e.key === "ArrowRight" ? 1 : -1;
        const nextState = board.states[stateIdx + dir];
        if (!nextState) return;
        onTransition(selectedId, nextState.name);
        return;
      }
    };

    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [
    board,
    byState,
    selectedId,
    modalOpen,
    onSelect,
    onCreate,
    onEdit,
    onDelete,
    onTransition,
    onShowHelp,
  ]);
}

// locateSelection returns the column the selected issue lives in, plus
// its index, so the arrow-key handlers can navigate without recomputing
// the layout.
function locateSelection(
  board: NativeBoard,
  byState: Map<string, NativeIssue[]>,
  selectedId: string,
): { state: string; column: NativeIssue[] | null; index: number } {
  for (const s of board.states) {
    const col = byState.get(s.name);
    if (!col) continue;
    const idx = col.findIndex((i) => i.id === selectedId);
    if (idx >= 0) return { state: s.name, column: col, index: idx };
  }
  // unmapped bucket (rare — fallthrough to keep arrow keys working)
  const orphan = byState.get("__unmapped__");
  if (orphan) {
    const idx = orphan.findIndex((i) => i.id === selectedId);
    if (idx >= 0) return { state: "__unmapped__", column: orphan, index: idx };
  }
  return { state: "", column: null, index: -1 };
}

