import { useEffect } from "react";

import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { Kbd } from "@/components/ui/Kbd";

export function BoardKeyboardHelp({ onClose }: { onClose: () => void }) {
  // Esc is handled by Dialog; this hook still intercepts "?" so a second
  // press of the help shortcut also closes the panel.
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.key === "?") {
        e.preventDefault();
        onClose();
      }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [onClose]);

  return (
    <Dialog
      open
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
      title="Keyboard shortcuts"
      widthClass="max-w-sm"
      footer={
        <Button variant="secondary" size="sm" onClick={onClose}>
          Close
        </Button>
      }
    >
      <ul className="space-y-1.5 text-fg-default text-sm">
        <ShortcutRow keys="c / n" desc="New issue" />
        <ShortcutRow keys="click" desc="Select issue" />
        <ShortcutRow keys="title / double-click" desc="Open issue" />
        <ShortcutRow keys="Ctrl/⌘+click" desc="Toggle card in selection" />
        <ShortcutRow keys="Shift+click" desc="Extend selection range" />
        <ShortcutRow keys="Ctrl/⌘+A" desc="Select all visible cards" />
        <ShortcutRow keys="x" desc="Toggle selected card" />
        <ShortcutRow keys="drag selection" desc="Move all selected cards" />
        <ShortcutRow keys="↑ ↓" desc="Navigate cards in column" />
        <ShortcutRow keys="← →" desc="Move card to previous/next column" />
        <ShortcutRow keys="Enter / e" desc="Open selected issue" />
        <ShortcutRow keys="Del / Bksp" desc="Delete selected issue" />
        <ShortcutRow keys="Esc" desc="Clear selection or close" />
      </ul>
    </Dialog>
  );
}

function ShortcutRow({ keys, desc }: { keys: string; desc: string }) {
  return (
    <li className="flex items-center justify-between gap-4">
      <Kbd>{keys}</Kbd>
      <span className="text-fg-muted text-xs">{desc}</span>
    </li>
  );
}
