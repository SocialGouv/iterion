import type { DragEvent } from "react";
import type { LibraryItem, LibraryCategory } from "@/lib/library/types";
import { NODE_ICONS, NODE_COLORS } from "@/lib/constants";
import { LAYER_COLORS, LAYER_ICONS } from "@/lib/constants";

function getCategoryColor(category: LibraryCategory): string {
  if (category in NODE_COLORS) return NODE_COLORS[category as keyof typeof NODE_COLORS];
  if (category === "schema") return LAYER_COLORS.schemas;
  if (category === "prompt") return LAYER_COLORS.prompts;
  if (category === "var") return LAYER_COLORS.vars;
  return "#888";
}

function getCategoryIcon(category: LibraryCategory): string {
  if (category in NODE_ICONS) return NODE_ICONS[category as keyof typeof NODE_ICONS];
  if (category === "schema") return LAYER_ICONS.schemas;
  if (category === "prompt") return LAYER_ICONS.prompts;
  if (category === "var") return LAYER_ICONS.vars;
  return "?";
}

interface Props {
  item: LibraryItem;
  onAdd: (item: LibraryItem) => void;
}

export default function LibraryItemCard({ item, onAdd }: Props) {
  const color = getCategoryColor(item.category);
  const icon = getCategoryIcon(item.category);

  const onDragStart = (e: DragEvent) => {
    e.dataTransfer.setData("application/iterion-library", item.id);
    e.dataTransfer.effectAllowed = "move";
  };

  return (
    <div
      draggable
      onDragStart={onDragStart}
      onClick={() => onAdd(item)}
      className="flex items-start gap-2 px-2 py-2 rounded cursor-grab hover:bg-gray-700/50 transition-colors border-l-2 group"
      style={{ borderLeftColor: color }}
      title={item.description}
    >
      <span className="text-sm mt-0.5 shrink-0">{icon}</span>
      <div className="min-w-0 flex-1">
        <div className="text-xs font-medium text-gray-200 truncate">{item.name}</div>
        <div className="text-[10px] text-gray-500 line-clamp-2 leading-tight mt-0.5">{item.description}</div>
        {item.tags && item.tags.length > 0 && (
          <div className="flex gap-1 flex-wrap mt-1">
            {item.tags.slice(0, 3).map((tag) => (
              <span
                key={tag}
                className="text-[9px] px-1 py-0 rounded bg-gray-700 text-gray-400"
              >
                {tag}
              </span>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
