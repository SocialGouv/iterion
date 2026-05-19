import {
  HomeIcon,
  Pencil2Icon,
  PlayIcon,
  PaperPlaneIcon,
  ViewGridIcon,
  RocketIcon,
  GearIcon,
  PersonIcon,
  Cross2Icon,
  PlusIcon,
} from "@radix-ui/react-icons";

import {
  HOME_TAB_ID,
  useTabsStore,
  type Tab,
  type TabKind,
} from "@/store/tabs";
import { useUIStore } from "@/store/ui";

const KIND_ICON: Record<TabKind, typeof HomeIcon> = {
  home: HomeIcon,
  editor: Pencil2Icon,
  run: PlayIcon,
  "whats-next": PaperPlaneIcon,
  board: ViewGridIcon,
  dispatcher: RocketIcon,
  settings: GearIcon,
  team: PersonIcon,
};

// TabBar — horizontal strip above the contextual header bar. Renders
// one button per tab (icon + truncated label + close affordance) plus
// a trailing "+" that opens the command palette in new-tab mode.
//
// The active tab is highlighted; switching is single-click. Home is
// pinned and non-closable. Reordering by drag is out of scope; the
// keyboard shortcut Cmd+Shift+→/← handles it (see useTabHotkeys).
export default function TabBar() {
  const tabs = useTabsStore((s) => s.tabs);
  const activeTabId = useTabsStore((s) => s.activeTabId);
  const setActive = useTabsStore((s) => s.setActive);
  const closeTab = useTabsStore((s) => s.closeTab);
  const openCommandPalette = useUIStore((s) => s.setCommandPaletteOpen);

  return (
    <div
      className="shrink-0 flex items-stretch gap-px h-9 bg-surface-1 border-b border-border-default overflow-x-auto"
      role="tablist"
      aria-label="Open tabs"
    >
      {tabs.map((tab) => (
        <TabButton
          key={tab.id}
          tab={tab}
          active={tab.id === activeTabId}
          onSelect={() => setActive(tab.id)}
          onClose={tab.id === HOME_TAB_ID ? null : () => closeTab(tab.id)}
        />
      ))}
      <button
        type="button"
        onClick={() => openCommandPalette(true)}
        className="shrink-0 inline-flex items-center justify-center h-full w-9 text-fg-subtle hover:text-fg-default hover:bg-surface-2"
        title="Open command palette"
        aria-label="Open command palette"
      >
        <PlusIcon className="w-3.5 h-3.5" />
      </button>
    </div>
  );
}

interface TabButtonProps {
  tab: Tab;
  active: boolean;
  onSelect: () => void;
  onClose: (() => void) | null;
}

function TabButton({ tab, active, onSelect, onClose }: TabButtonProps) {
  const Icon = KIND_ICON[tab.kind];
  const stateCls = active
    ? "bg-surface-0 text-fg-default border-t-2 border-t-accent"
    : "bg-surface-1 text-fg-muted hover:bg-surface-2 hover:text-fg-default border-t-2 border-t-transparent";
  return (
    <div
      role="tab"
      aria-selected={active}
      className={`shrink-0 inline-flex items-center gap-1.5 pl-2.5 pr-1 max-w-[180px] text-xs border-r border-border-default group ${stateCls}`}
      title={tab.label}
    >
      <button
        type="button"
        onClick={onSelect}
        className="inline-flex items-center gap-1.5 min-w-0 py-1 focus:outline-none"
      >
        <Icon className="w-3.5 h-3.5 shrink-0" />
        <span className="truncate">{tab.label}</span>
      </button>
      {onClose ? (
        <button
          type="button"
          onClick={(e) => {
            e.stopPropagation();
            onClose();
          }}
          className="inline-flex items-center justify-center w-5 h-5 rounded text-fg-subtle hover:text-fg-default hover:bg-surface-3 opacity-0 group-hover:opacity-100 focus:opacity-100"
          title="Close tab"
          aria-label={`Close tab ${tab.label}`}
        >
          <Cross2Icon className="w-3 h-3" />
        </button>
      ) : (
        <span className="w-5 h-5" aria-hidden />
      )}
    </div>
  );
}
