import { useEffect } from "react";

import type { ExecutionState } from "@/api/runs";

interface Args {
  selectedNodeId: string | null;
  executions: ExecutionState[];
  iterationByNode: Map<string, number>;
  onSelectNode: (id: string | null) => void;
  onSelectIteration: (nodeId: string, iteration: number) => void;
  onScrubLive?: () => void;
  onJumpToFailed?: () => void;
}

// useRunKeyboard wires the run console's keyboard navigation:
//   ←/→  : step through iterations of the currently-selected node
//   Esc  : deselect
//   g f  : jump to first failed (chord — press g then f)
//   g l  : back to live (sort of scrubber)
//
// Listens at window level so it works regardless of focus, but skips
// inputs and contenteditable elements so users can still type.
export function useRunKeyboard({
  selectedNodeId,
  executions,
  iterationByNode,
  onSelectNode,
  onSelectIteration,
  onScrubLive,
  onJumpToFailed,
}: Args): void {
  useEffect(() => {
    let chord: "g" | null = null;
    let chordTimer: ReturnType<typeof setTimeout> | null = null;
    const clearChord = () => {
      chord = null;
      if (chordTimer) {
        clearTimeout(chordTimer);
        chordTimer = null;
      }
    };

    const handler = (e: KeyboardEvent) => {
      if (isTypingTarget(e.target)) return;
      if (e.metaKey || e.ctrlKey || e.altKey) return;

      // Chord support: "g" then "f" within 750ms, etc.
      if (chord === "g") {
        if (e.key === "f") {
          e.preventDefault();
          onJumpToFailed?.();
          clearChord();
          return;
        }
        if (e.key === "l") {
          e.preventDefault();
          onScrubLive?.();
          clearChord();
          return;
        }
        clearChord();
      }
      if (e.key === "g") {
        chord = "g";
        if (chordTimer) clearTimeout(chordTimer);
        chordTimer = setTimeout(clearChord, 750);
        return;
      }

      if (e.key === "Escape") {
        if (selectedNodeId) {
          e.preventDefault();
          onSelectNode(null);
        }
        return;
      }

      if (e.key === "ArrowLeft" || e.key === "ArrowRight") {
        if (!selectedNodeId) return;
        const matching = executions
          .filter((ex) => ex.ir_node_id === selectedNodeId)
          .map((ex) => ex.loop_iteration)
          .sort((a, b) => a - b);
        if (matching.length === 0) return;
        e.preventDefault();
        const current = iterationByNode.get(selectedNodeId) ?? matching[matching.length - 1]!;
        const idx = matching.indexOf(current);
        if (idx < 0) return;
        const nextIdx =
          e.key === "ArrowLeft"
            ? Math.max(0, idx - 1)
            : Math.min(matching.length - 1, idx + 1);
        const next = matching[nextIdx]!;
        if (next !== current) onSelectIteration(selectedNodeId, next);
      }
    };

    window.addEventListener("keydown", handler);
    return () => {
      if (chordTimer) clearTimeout(chordTimer);
      window.removeEventListener("keydown", handler);
    };
  }, [
    selectedNodeId,
    executions,
    iterationByNode,
    onSelectNode,
    onSelectIteration,
    onScrubLive,
    onJumpToFailed,
  ]);
}

function isTypingTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  const tag = target.tagName;
  if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return true;
  if (target.isContentEditable) return true;
  return false;
}
