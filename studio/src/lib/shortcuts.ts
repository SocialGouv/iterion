// The single-key shortcut registry (RGAA 12.10): every shortcut a bare
// keypress triggers, listed for the help dialog and gated by the user's
// single-key setting. A new bare-key branch registers here AND consults
// shortcutsEnabled() — the source canary refuses raw single-key branches.
import { useUIStore } from "@/store/ui";

export interface SingleKeyShortcut {
  id: string;
  key: string;
  label: string;
  surface: "canvas" | "window" | "run" | "board";
}

export const SINGLE_KEY_SHORTCUTS: SingleKeyShortcut[] = [
  { id: "search", key: "/", label: "Search nodes", surface: "canvas" },
  { id: "tool-select", key: "v", label: "Select tool", surface: "canvas" },
  { id: "tool-pan", key: "h", label: "Pan tool", surface: "canvas" },
  { id: "step-next", key: "j", label: "Step to the next node by start order", surface: "run" },
  { id: "step-prev", key: "k", label: "Step to the previous node", surface: "run" },
  { id: "chords", key: "g", label: "Command chords: g f (failed), g l (live), g n / g p (step)", surface: "run" },
  { id: "shortcuts-help", key: "?", label: "Show keyboard shortcuts", surface: "window" },
  { id: "board-help", key: "?", label: "Board: show shortcuts help", surface: "board" },
  { id: "board-create", key: "c", label: "Board: create a card (n works too)", surface: "board" },
  { id: "board-edit", key: "e", label: "Board: edit the selected card", surface: "board" },
  { id: "board-toggle-select", key: "x", label: "Board: toggle card selection", surface: "board" },
];

// The gate every bare-key branch consults: with the setting off, a
// keypress belongs to whatever has focus, never to a global shortcut.
export function shortcutsEnabled(): boolean {
  return useUIStore.getState().singleKeyShortcuts;
}
