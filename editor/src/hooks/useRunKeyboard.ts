import { useEffect } from "react";

import type { ExecutionState } from "@/api/runs";
import { isTypingTarget } from "@/lib/keyboard";

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
//   j/k  : step through nodes (next/prev by start order, vim-style)
//   Esc  : deselect
//   g f  : jump to first failed (chord — press g then f)
//   g l  : back to live (sort of scrubber)
//   g n  : next failed (chord) — sibling of g f, cycles
//   g p  : previous failed (chord) — cycles
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
    let failedCursor = -1;
    const clearChord = () => {
      chord = null;
      if (chordTimer) {
        clearTimeout(chordTimer);
        chordTimer = null;
      }
    };

    // stepFailed cycles through every failed execution in start order.
    // Repeated `g n` advances to the next; `g p` walks back. The cursor
    // lives in closure scope so the user's repeated cycles don't reset
    // every render — selectedNodeId is also used as the anchor when the
    // cursor falls behind (e.g. user clicked into a non-failed node
    // between presses).
    const stepFailed = (backward: boolean) => {
      const failed = executions
        .filter((ex) => ex.status === "failed")
        .sort((a, b) => a.first_seq - b.first_seq);
      if (failed.length === 0) return;
      if (failedCursor < 0 || failedCursor >= failed.length) {
        // Anchor on the currently-selected node when we can find it in
        // the failed list, otherwise start at the first/last entry.
        const anchor = failed.findIndex((ex) => ex.ir_node_id === selectedNodeId);
        failedCursor = anchor >= 0 ? anchor : backward ? failed.length - 1 : 0;
      }
      failedCursor = backward
        ? (failedCursor - 1 + failed.length) % failed.length
        : (failedCursor + 1) % failed.length;
      const target = failed[failedCursor];
      if (target) onSelectNode(target.ir_node_id);
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
        if (e.key === "n" || e.key === "p") {
          e.preventDefault();
          stepFailed(e.key === "p");
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

      // j / k step between nodes by start order (vim-idiom: j=down, k=up).
      // The "down" axis is "later in the run"; "up" is "earlier".
      if (e.key === "j" || e.key === "k") {
        const ordered = uniqueNodesByStart(executions);
        if (ordered.length === 0) return;
        e.preventDefault();
        const idx = selectedNodeId ? ordered.indexOf(selectedNodeId) : -1;
        let next: number;
        if (idx < 0) {
          next = e.key === "j" ? 0 : ordered.length - 1;
        } else {
          next = e.key === "j" ? idx + 1 : idx - 1;
        }
        if (next < 0 || next >= ordered.length) return;
        const target = ordered[next];
        if (target) onSelectNode(target);
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
        // Sort by first_seq (start order) to match RunCanvasIR /
        // RunView ordering. Stepping is by INDEX in this list, the
        // same semantic iterationByNode stores — see RunCanvasIR's
        // defaultIterationFor comment for the motivation.
        const matching = executions
          .filter((ex) => ex.ir_node_id === selectedNodeId)
          .sort((a, b) => a.first_seq - b.first_seq);
        if (matching.length === 0) return;
        e.preventDefault();
        const last = matching.length - 1;
        const current = iterationByNode.get(selectedNodeId) ?? last;
        const clamped = Math.min(Math.max(current, 0), last);
        const next =
          e.key === "ArrowLeft"
            ? Math.max(0, clamped - 1)
            : Math.min(last, clamped + 1);
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

// uniqueNodesByStart collapses the executions array — which may contain
// multiple attempts per node — to a stable, start-ordered list of node
// IDs. Used by j/k navigation so re-running a node doesn't make it
// "skip ahead" in the ordering on every retry.
function uniqueNodesByStart(executions: ExecutionState[]): string[] {
  const seenSeq = new Map<string, number>();
  for (const ex of executions) {
    const prev = seenSeq.get(ex.ir_node_id);
    if (prev === undefined || ex.first_seq < prev) {
      seenSeq.set(ex.ir_node_id, ex.first_seq);
    }
  }
  return Array.from(seenSeq.entries())
    .sort((a, b) => a[1] - b[1])
    .map((e) => e[0]);
}

