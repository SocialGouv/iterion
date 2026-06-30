import { type DragEvent, useMemo } from "react";
import type { NodeKind } from "@/api/types";
import type { LibraryCategory } from "@/lib/library/types";
import { NODE_COLORS, softColor } from "@/lib/constants";
import { NodeIcon } from "@/components/icons/NodeIcon";
import { IconButton } from "@/components/ui/IconButton";
import { Input } from "@/components/ui/Input";
import { useUIStore } from "@/store/ui";
import { useLibraryStore, selectAllItems } from "@/store/library";
import { useAddFromLibrary } from "@/hooks/useAddFromLibrary";
import LibraryItemCard from "./LibraryItemCard";

const NODE_TYPES: { kind: NodeKind; label: string }[] = [
  { kind: "agent", label: "Agent" },
  { kind: "judge", label: "Judge" },
  { kind: "router", label: "Router" },
  { kind: "human", label: "Human" },
  { kind: "tool", label: "Tool" },
  { kind: "compute", label: "Compute" },
];

// Shared draggable node-chip styling — tactile elevation + hover lift on the
// kind-coloured palette tiles. Single source so the collapsed rail and the
// expanded Quick-Add grid (which differ only in size) stay in sync.
const NODE_TILE_CLASS =
  "flex flex-col items-center justify-center gap-0.5 rounded-[var(--radius-md)] cursor-grab border border-border-strong shadow-[var(--shadow-sm)] transition-[transform,box-shadow,filter] duration-[var(--motion-fast)] ease-[var(--motion-ease)] hover:-translate-y-0.5 hover:brightness-110 hover:shadow-[var(--shadow-md)] active:translate-y-0";

const CATEGORIES: { value: LibraryCategory | null; label: string }[] = [
  { value: null, label: "All" },
  { value: "agent", label: "Agent" },
  { value: "judge", label: "Judge" },
  { value: "router", label: "Router" },
  { value: "human", label: "Human" },
  { value: "tool", label: "Tool" },
  { value: "compute", label: "Compute" },
  { value: "schema", label: "Schema" },
  { value: "prompt", label: "Prompt" },
  { value: "var", label: "Var" },
  { value: "pattern", label: "Pattern" },
];

function CollapsedPalette({ onExpand }: { onExpand: () => void }) {
  const onDragStart = (e: DragEvent, kind: NodeKind) => {
    e.dataTransfer.setData("application/iterion-node", kind);
    e.dataTransfer.effectAllowed = "move";
  };

  return (
    <div className="flex flex-col items-center gap-2 py-3 px-1 h-full">
      <span className="text-caption text-fg-subtle uppercase tracking-wider">Nodes</span>
      {NODE_TYPES.map(({ kind, label }) => (
        <div
          key={kind}
          draggable
          onDragStart={(e) => onDragStart(e, kind)}
          className={`w-12 h-12 ${NODE_TILE_CLASS}`}
          style={{ backgroundColor: softColor(NODE_COLORS[kind], 20), borderColor: NODE_COLORS[kind] }}
          title={label}
        >
          <NodeIcon kind={kind} size={16} />
          <span className="text-caption text-fg-muted">{label}</span>
        </div>
      ))}
      <div className="flex-1" />
      <IconButton
        label="Expand library"
        variant="ghost"
        size="md"
        onClick={onExpand}
      >
        <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <polyline points="6,3 11,8 6,13" />
        </svg>
      </IconButton>
    </div>
  );
}

function ExpandedPanel({ onCollapse }: { onCollapse: () => void }) {
  const searchQuery = useLibraryStore((s) => s.searchQuery);
  const setSearchQuery = useLibraryStore((s) => s.setSearchQuery);
  const activeCategory = useLibraryStore((s) => s.activeCategory);
  const setActiveCategory = useLibraryStore((s) => s.setActiveCategory);
  const allItems = useLibraryStore(selectAllItems);
  const filteredItems = useMemo(() => {
    let items = allItems;
    if (activeCategory) items = items.filter((i) => i.category === activeCategory);
    if (searchQuery.trim()) {
      const q = searchQuery.toLowerCase();
      items = items.filter(
        (i) =>
          i.name.toLowerCase().includes(q) ||
          i.description.toLowerCase().includes(q) ||
          i.tags?.some((t) => t.toLowerCase().includes(q)),
      );
    }
    return items;
  }, [allItems, activeCategory, searchQuery]);
  const addFromLibrary = useAddFromLibrary();

  const onDragStart = (e: DragEvent, kind: NodeKind) => {
    e.dataTransfer.setData("application/iterion-node", kind);
    e.dataTransfer.effectAllowed = "move";
  };

  return (
    <div className="flex flex-col h-full">
      {/* Header */}
      <div className="flex items-center justify-between px-3 py-2 border-b border-border-default">
        <span className="text-xs font-semibold text-fg-muted uppercase tracking-wider">Library</span>
        <IconButton
          label="Collapse library"
          variant="ghost"
          size="sm"
          onClick={onCollapse}
        >
          <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <polyline points="11,3 6,8 11,13" />
          </svg>
        </IconButton>
      </div>

      {/* Search */}
      <div className="px-3 py-2">
        <Input
          size="sm"
          type="text"
          value={searchQuery}
          onChange={(e) => setSearchQuery(e.target.value)}
          placeholder="Search library..."
        />
      </div>

      {/* Category pills */}
      <div className="px-3 pb-2 flex flex-wrap gap-1">
        {CATEGORIES.map(({ value, label }) => (
          <button
            key={label}
            onClick={() => setActiveCategory(value)}
            className={`text-caption px-2 py-0.5 rounded-full border transition-colors ${
              activeCategory === value
                ? "bg-accent/20 border-accent text-accent-text"
                : "bg-surface-1 border-border-strong text-fg-subtle hover:border-border-strong"
            }`}
          >
            {label}
          </button>
        ))}
      </div>

      {/* Quick Add — generic node types */}
      {!activeCategory && !searchQuery && (
        <div className="px-3 pb-2">
          <span className="text-caption text-fg-subtle uppercase tracking-wider">Quick Add</span>
          <div className="grid grid-cols-3 gap-1 mt-1">
            {NODE_TYPES.map(({ kind, label }) => (
              <div
                key={kind}
                draggable
                onDragStart={(e) => onDragStart(e, kind)}
                className={`h-10 ${NODE_TILE_CLASS}`}
                style={{ backgroundColor: softColor(NODE_COLORS[kind], 20), borderColor: NODE_COLORS[kind] }}
                title={label}
              >
                <NodeIcon kind={kind} size={14} />
                <span className="text-caption text-fg-muted">{label}</span>
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Divider */}
      <div className="border-t border-border-default mx-3" />

      {/* Library items */}
      <div className="flex-1 overflow-y-auto px-2 py-2 space-y-0.5">
        {filteredItems.length === 0 ? (
          <div className="text-caption text-fg-subtle text-center py-4">
            {searchQuery ? "No items match your search" : "No items in this category"}
          </div>
        ) : (
          filteredItems.map((item) => (
            <LibraryItemCard key={item.id} item={item} onAdd={addFromLibrary} />
          ))
        )}
      </div>
    </div>
  );
}

export default function LibraryPanel() {
  const libraryExpanded = useUIStore((s) => s.libraryExpanded);
  const toggleLibraryPanel = useUIStore((s) => s.toggleLibraryPanel);

  if (libraryExpanded) {
    return <ExpandedPanel onCollapse={toggleLibraryPanel} />;
  }
  return <CollapsedPalette onExpand={toggleLibraryPanel} />;
}
