import { SINGLE_KEY_SHORTCUTS } from "@/lib/shortcuts";
import { Dialog } from "@/components/ui/Dialog";
import { Kbd } from "@/components/ui/Kbd";
import { isMacOS } from "@/lib/keyboard";

interface Props {
  open: boolean;
  onClose: () => void;
}

export default function ShortcutsHelp({ open, onClose }: Props) {
  // The modifier glyph differs by platform — macOS users expect ⌘
  // (Command), every other platform sees "Ctrl". Computed inside the
  // component body (not at module scope) so SSR / test environments
  // where `navigator` is unavailable still resolve to a sensible
  // default at mount time.
  const mod = isMacOS() ? "⌘" : "Ctrl";
  const shortcuts = [
    { keys: `${mod}+Z`, desc: "Undo" },
    { keys: `${mod}+Y / ${mod}+Shift+Z`, desc: "Redo" },
    { keys: `${mod}+S`, desc: "Save" },
    { keys: `${mod}+C`, desc: "Copy selected node" },
    { keys: `${mod}+V`, desc: "Paste copied node" },
    { keys: "Delete / Backspace", desc: "Delete selected node or edge" },
    { keys: "Right-click node", desc: "Context menu (set entry, duplicate, delete)" },
    { keys: "Drag from handle", desc: "Quick-add node with auto-connect" },
    { keys: "Escape", desc: "Clear selection / close dialogs" },
  ];
  // The bare-keypress shortcuts come from the registry, not hand copies:
  // the list here and the gated branches cannot drift apart.
  const singleKeyRows = SINGLE_KEY_SHORTCUTS.map(({ key, label }) => ({ keys: key, desc: label }));
  const rows = [...shortcuts.slice(0, shortcuts.length - 1), ...singleKeyRows, shortcuts[shortcuts.length - 1]!];
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
      title="Keyboard Shortcuts"
      widthClass="max-w-md"
    >
      <div className="px-4 py-3 space-y-2">
        {rows.map(({ keys, desc }) => (
          <div key={keys} className="flex items-center justify-between gap-4">
            <span className="text-xs text-fg-muted">{desc}</span>
            <Kbd size="md" className="whitespace-nowrap">
              {keys}
            </Kbd>
          </div>
        ))}
        <p className="pt-3 mt-3 border-t border-border-default text-caption text-fg-subtle text-center">
          Press Escape to close
        </p>
      </div>
    </Dialog>
  );
}
