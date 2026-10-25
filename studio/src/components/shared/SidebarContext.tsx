import { MagnifyingGlassIcon, CaretSortIcon } from "@radix-ui/react-icons";

import { Icon } from "@/components/ui/Icon";
import { useProjectInfo } from "@/hooks/useProjectInfo";
import { isMacOS } from "@/lib/keyboard";
import { useUIStore } from "@/store/ui";

// Inline open-folder glyph. Radix has no folder icon and lucide would
// pull its full catalog along, so we keep this 13×13 SVG self-contained.
function FolderOpenGlyph({ className = "h-3.5 w-3.5" }: { className?: string }) {
  return (
    <svg
      viewBox="0 0 15 15"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.2"
      strokeLinecap="round"
      strokeLinejoin="round"
      className={`shrink-0 ${className}`}
      aria-hidden="true"
    >
      <path d="M1.5 4.5h4l1 1h7v6.5a.5.5 0 0 1-.5.5h-11a.5.5 0 0 1-.5-.5v-7.5z" />
      <path d="M2 12l1.5-4.5h11l-1.5 4.5" />
    </svg>
  );
}

interface Props {
  collapsed: boolean;
}

// SidebarContext renders two visually-separate controls below the logo
// and above the nav:
//   1. Project chip — current workspace context. Click opens the
//      project switcher (Ctrl+P). Hidden in cloud mode.
//   2. Command launcher — action surface. Click opens the command
//      palette (Ctrl/Cmd+K).
//
// They sit close together but are NOT visually merged: one is a
// passive context indicator, the other is an active launcher. Merging
// them confused the two themes.
//
// Collapsed mode degrades each to an icon-only square button; tooltips
// preserve the labels and shortcuts.
export default function SidebarContext({ collapsed }: Props) {
  const { name, dir, source } = useProjectInfo();
  const toggleCommandPalette = useUIStore((s) => s.toggleCommandPalette);

  const clickable = source === "desktop" || source === "server";
  const hasProject = !!name && clickable;

  const mac = isMacOS();
  const kbdK = mac ? "⌘K" : "Ctrl+K";
  const kbdP = mac ? "⌘P" : "Ctrl+P";

  const onOpenSwitcher = () => {
    window.dispatchEvent(new CustomEvent("iterion:open-project-switcher"));
  };

  const projectTooltip = name
    ? dir
      ? `Project: ${name}\n${dir}\n\nClick to switch (${kbdP})`
      : `Project: ${name}\nClick to switch (${kbdP})`
    : null;

  return (
    <div className="flex flex-col gap-2">
      {hasProject &&
        (collapsed ? (
          <button
            type="button"
            onClick={onOpenSwitcher}
            title={projectTooltip ?? undefined}
            aria-label={`Project: ${name ?? ""} — switch`}
            className="flex items-center justify-center h-8 w-full rounded border border-border-default bg-surface-2 text-fg-muted hover:text-fg-default hover:bg-surface-3 transition-colors"
          >
            <FolderOpenGlyph />
          </button>
        ) : (
          <button
            type="button"
            onClick={onOpenSwitcher}
            title={projectTooltip ?? undefined}
            className="flex items-center gap-2 px-2 py-1.5 rounded border border-border-default bg-surface-2 text-xs text-fg-default hover:bg-surface-3 transition-colors group"
          >
            <FolderOpenGlyph className="h-3.5 w-3.5 text-fg-muted group-hover:text-fg-default" />
            <span className="truncate flex-1 text-left font-medium">{name}</span>
            <Icon
              icon={CaretSortIcon}
              className="h-3.5 w-3.5 text-fg-subtle group-hover:text-fg-muted shrink-0"
            />
          </button>
        ))}

      {collapsed ? (
        <button
          type="button"
          onClick={toggleCommandPalette}
          title={`Search or run a command (${kbdK})`}
          aria-label={`Open command palette (${kbdK})`}
          className="flex items-center justify-center h-8 w-10 rounded border border-border-default bg-surface-2 text-fg-muted hover:text-fg-default hover:bg-surface-3 transition-colors"
        >
          <Icon icon={MagnifyingGlassIcon} className="h-3.5 w-3.5" />
        </button>
      ) : (
        <button
          type="button"
          onClick={toggleCommandPalette}
          title={`Open command palette (${kbdK})`}
          className="flex items-center gap-2 px-2 py-1.5 rounded border border-border-default bg-surface-2 text-xs text-fg-muted hover:text-fg-default hover:bg-surface-3 transition-colors group"
        >
          <Icon
            icon={MagnifyingGlassIcon}
            className="h-3.5 w-3.5 shrink-0 group-hover:text-fg-default"
          />
          <span className="flex-1 text-left truncate">Search or run…</span>
          <kbd className="font-mono text-caption text-fg-subtle group-hover:text-fg-muted px-1 py-px rounded border border-border-default bg-surface-1 shrink-0">
            {kbdK}
          </kbd>
        </button>
      )}
    </div>
  );
}
