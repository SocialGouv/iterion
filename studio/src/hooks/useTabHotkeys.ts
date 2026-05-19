import { useEffect } from "react";

import { HOME_TAB_ID, useTabsStore } from "@/store/tabs";
import { useUIStore } from "@/store/ui";
import { isTypingTarget } from "@/lib/keyboard";

// useTabHotkeys wires the window-level keyboard shortcuts for the tab
// system. Bindings:
//   - Cmd/Ctrl+T          → open command palette in "new tab" mode
//   - Cmd/Ctrl+W          → close the active tab (Home is non-closable)
//   - Cmd/Ctrl+1..9       → focus the nth tab (1-indexed)
//   - Cmd/Ctrl+Shift+]    → next tab
//   - Cmd/Ctrl+Shift+[    → previous tab
//   - Cmd/Ctrl+Shift+→/←  → move the active tab forward / backward
//
// All bindings respect isTypingTarget(): when an input/textarea/
// contenteditable is focused, the browser's default behaviour wins.
// Mounted once from AppShell; no-op when the studio is in focus mode
// only because no tab UI is visible — the bindings still resolve.
export function useTabHotkeys(): void {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (!(e.metaKey || e.ctrlKey)) return;
      if (isTypingTarget(e.target)) return;
      const key = e.key;
      const lower = key.toLowerCase();

      // Cmd+T → command palette (acts as "new tab" entry point)
      if (lower === "t" && !e.shiftKey) {
        e.preventDefault();
        useUIStore.getState().setCommandPaletteOpen(true);
        return;
      }

      // Cmd+W → close active tab
      if (lower === "w" && !e.shiftKey) {
        e.preventDefault();
        const { activeTabId, closeTab } = useTabsStore.getState();
        if (activeTabId && activeTabId !== HOME_TAB_ID) {
          closeTab(activeTabId);
        }
        return;
      }

      // Cmd+1..9 → focus nth tab
      if (!e.shiftKey && key >= "1" && key <= "9") {
        const idx = parseInt(key, 10) - 1;
        const { tabs, setActive } = useTabsStore.getState();
        const target = tabs[idx];
        if (target) {
          e.preventDefault();
          setActive(target.id);
        }
        return;
      }

      // Cmd+Shift+] / [ → cycle, Cmd+Shift+→/← → reorder
      if (e.shiftKey) {
        const { tabs, activeTabId, setActive, reorder } = useTabsStore.getState();
        if (!activeTabId) return;
        const idx = tabs.findIndex((t) => t.id === activeTabId);
        if (idx === -1) return;

        if (key === "]" || key === "}") {
          e.preventDefault();
          const next = tabs[(idx + 1) % tabs.length];
          if (next) setActive(next.id);
          return;
        }
        if (key === "[" || key === "{") {
          e.preventDefault();
          const prev = tabs[(idx - 1 + tabs.length) % tabs.length];
          if (prev) setActive(prev.id);
          return;
        }
        if (key === "ArrowRight") {
          e.preventDefault();
          if (idx < tabs.length - 1) reorder(idx, idx + 1);
          return;
        }
        if (key === "ArrowLeft") {
          e.preventDefault();
          if (idx > 0) reorder(idx, idx - 1);
          return;
        }
      }
    };

    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);
}
