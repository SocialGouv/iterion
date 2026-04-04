import { type DragEvent, useMemo } from "react";
import type { NodeKind } from "@/api/types";
import type { LibraryCategory } from "@/lib/library/types";
import { NODE_ICONS, NODE_COLORS } from "@/lib/constants";
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
];

const CATEGORIES: { value: LibraryCategory | null; label: string }[] = [
  { value: null, label: "All" },
  { value: "agent", label: "Agent" },
  { value: "judge", label: "Judge" },
  { value: "router", label: "Router" },
  { value: "human", label: "Human" },
  { value: "tool", label: "Tool" },
  { value: "schema", label: "Schema" },
  { value: "prompt", label: "Prompt" },
  { value: "var", label: "Var" },
];

function CollapsedPalette({ onExpand }: { onExpand: () => void }) {
  const onDragStart = (e: DragEvent, kind: NodeKind) => {
    e.dataTransfer.setData("application/iterion-node", kind);
    e.dataTransfer.effectAllowed = "move";
  };

  return (
    <div className="flex flex-col items-center gap-2 py-3 px-1 h-full">
      <span className="text-[9px] text-gray-500 uppercase tracking-wider">Nodes</span>
      {NODE_TYPES.map(({ kind, label }) => (
        <div
          key={kind}
          draggable
          onDragStart={(e) => onDragStart(e, kind)}
          className="w-12 h-12 flex flex-col items-center justify-center rounded cursor-grab hover:brightness-125 transition-all border border-gray-600"
          style={{ backgroundColor: NODE_COLORS[kind] + "33", borderColor: NODE_COLORS[kind] }}
          title={label}
        >
          <span className="text-base">{NODE_ICONS[kind]}</span>
          <span className="text-[9px] text-gray-300">{label}</span>
        </div>
      ))}
      <div className="flex-1" />
      <button
        onClick={onExpand}
        className="w-10 h-10 flex items-center justify-center rounded hover:bg-gray-700 transition-colors text-gray-400 hover:text-gray-200"
        title="Expand library"
      >
        <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <polyline points="6,3 11,8 6,13" />
        </svg>
      </button>
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
      <div className="flex items-center justify-between px-3 py-2 border-b border-gray-700">
        <span className="text-xs font-semibold text-gray-300 uppercase tracking-wider">Library</span>
        <button
          onClick={onCollapse}
          className="w-6 h-6 flex items-center justify-center rounded hover:bg-gray-700 transition-colors text-gray-400 hover:text-gray-200"
          title="Collapse library"
        >
          <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <polyline points="11,3 6,8 11,13" />
          </svg>
        </button>
      </div>

      {/* Search */}
      <div className="px-3 py-2">
        <input
          type="text"
          value={searchQuery}
          onChange={(e) => setSearchQuery(e.target.value)}
          placeholder="Search library..."
          className="w-full bg-gray-800 border border-gray-600 rounded px-2 py-1 text-xs text-white placeholder-gray-500 focus:border-blue-500 focus:outline-none"
        />
      </div>

      {/* Category pills */}
      <div className="px-3 pb-2 flex flex-wrap gap-1">
        {CATEGORIES.map(({ value, label }) => (
          <button
            key={label}
            onClick={() => setActiveCategory(value)}
            className={`text-[10px] px-2 py-0.5 rounded-full border transition-colors ${
              activeCategory === value
                ? "bg-blue-500/20 border-blue-500 text-blue-300"
                : "bg-gray-800 border-gray-600 text-gray-400 hover:border-gray-500"
            }`}
          >
            {label}
          </button>
        ))}
      </div>

      {/* Quick Add — generic node types */}
      {!activeCategory && !searchQuery && (
        <div className="px-3 pb-2">
          <span className="text-[9px] text-gray-500 uppercase tracking-wider">Quick Add</span>
          <div className="grid grid-cols-3 gap-1 mt-1">
            {NODE_TYPES.map(({ kind, label }) => (
              <div
                key={kind}
                draggable
                onDragStart={(e) => onDragStart(e, kind)}
                className="h-10 flex flex-col items-center justify-center rounded cursor-grab hover:brightness-125 transition-all border border-gray-600"
                style={{ backgroundColor: NODE_COLORS[kind] + "33", borderColor: NODE_COLORS[kind] }}
                title={label}
              >
                <span className="text-sm">{NODE_ICONS[kind]}</span>
                <span className="text-[8px] text-gray-300">{label}</span>
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Divider */}
      <div className="border-t border-gray-700 mx-3" />

      {/* Library items */}
      <div className="flex-1 overflow-y-auto px-2 py-2 space-y-0.5">
        {filteredItems.length === 0 ? (
          <div className="text-[10px] text-gray-500 text-center py-4">
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
